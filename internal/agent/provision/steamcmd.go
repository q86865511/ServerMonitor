package provision

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// defaultSteamCMDBaseURL 是 SteamCMD 的 Valve 官方匿名載點(Windows 版)。
//
// 偏離說明(R6 非功能需求「所有自動下載一律 HTTPS+官方來源+checksum 校驗」):
// Valve 不公布 steamcmd.zip 的官方 checksum,無值可校驗。本供應器以「HTTPS + 官方網域
// (steamcdn-a.akamaihd.net) + 解壓後 steamcmd.exe 存在性檢查」替代逐位元組校驗,
// 為刻意的、有文件記錄的偏離,非疏漏。
const defaultSteamCMDBaseURL = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"

// steamCMDErrorMarker 是 SteamCMD 輸出中表示安裝/更新失敗的標記字串(大小寫不敏感比對)。
const steamCMDErrorMarker = "error!"

// steamCMDFirstRunMarker 是「全新下載的 SteamCMD 尚未自我更新即跑 app_update」的失敗特徵
// (輸出含 "Missing configuration"、exit 7)。SteamCMD 於此失敗後才在同次行程自我更新,故重試同
// 一命令即成功——屬 SteamCMD 已知首次行為,非設定錯誤。實機重現:首台/清快取後首跑 app_update
// 2394010 得此錯、隨即自我更新,重試即順利下載。以此特徵限定重試一次,不影響 No subscription 等
// 真實永久錯誤(不重試)。
const steamCMDFirstRunMarker = "missing configuration"

// steamCMDInstallAttempts 是 InstallApp 的最大嘗試次數:僅在首次自我更新特徵(見
// steamCMDFirstRunMarker)下用到第 2 次;其餘錯誤第 1 次即返回。
const steamCMDInstallAttempts = 2

// steamProgressRe 解析 SteamCMD 輸出中的下載/驗證進度行,如:
// "Update state (0x61) downloading, progress: 32.15 (1234 / 5678)"。
var steamProgressRe = regexp.MustCompile(`progress:\s*([0-9]+(?:\.[0-9]+)?)`)

// SteamCMDExecFunc 執行 steamcmd.exe 並將每一行 stdout/stderr 輸出經 onLine 回呼傳出;
// 行程結束後回傳其結果(exit code 非 0 時回非 nil error)。由 InstallApp 用來解析安裝進度,
// 亦為測試注入假行程的接縫(不需真的跑 SteamCMD)。
type SteamCMDExecFunc func(ctx context.Context, exe string, args []string, onLine func(string)) error

// SteamCMDProvisioner 供應 SteamCMD 至共用快取,並以其安裝/更新 Steam 專用伺服器(R6)。
// 快取佈局:<cacheRoot>/steamcmd/steamcmd.exe。
type SteamCMDProvisioner struct {
	cacheRoot string           // 共用快取根
	client    *http.Client     // 下載 steamcmd.zip 共用
	baseURL   string           // steamcmd.zip 下載來源(可注入,測試用 httptest)
	exec      SteamCMDExecFunc // 執行 steamcmd.exe 的實作(可注入,測試用假行程)
	locks     *keyedMutex      // per-key 併發鎖:Ensure 用固定 key 序列化下載;InstallApp 用 appID+installDir 序列化安裝
}

// SteamCMDOption 以函式選項調整 SteamCMDProvisioner 建構參數。
type SteamCMDOption func(*SteamCMDProvisioner)

// WithSteamCMDHTTPClient 注入自訂 http client(測試以 httptest server 的 client 注入)。
func WithSteamCMDHTTPClient(c *http.Client) SteamCMDOption {
	return func(s *SteamCMDProvisioner) {
		if c != nil {
			s.client = c
		}
	}
}

// WithSteamCMDBaseURL 覆寫 steamcmd.zip 下載來源(測試以 httptest server URL 注入)。
func WithSteamCMDBaseURL(base string) SteamCMDOption {
	return func(s *SteamCMDProvisioner) {
		if base != "" {
			s.baseURL = base
		}
	}
}

// WithSteamCMDExec 覆寫執行 steamcmd.exe 的實作(測試注入假行程,避免真的跑 SteamCMD)。
func WithSteamCMDExec(fn SteamCMDExecFunc) SteamCMDOption {
	return func(s *SteamCMDProvisioner) {
		if fn != nil {
			s.exec = fn
		}
	}
}

// NewSteamCMDProvisioner 建構 SteamCMDProvisioner。cacheRoot 為共用快取根;opts 依序套用。
func NewSteamCMDProvisioner(cacheRoot string, opts ...SteamCMDOption) *SteamCMDProvisioner {
	s := &SteamCMDProvisioner{
		cacheRoot: cacheRoot,
		client:    &http.Client{}, // 無整體逾時:大型下載由 context 控制生命週期,同 Provisioner 慣例。
		baseURL:   defaultSteamCMDBaseURL,
		exec:      defaultSteamCMDExec,
		locks:     newKeyedMutex(),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// steamCMDDir 回傳 SteamCMD 快取目錄。
func (s *SteamCMDProvisioner) steamCMDDir() string {
	return filepath.Join(s.cacheRoot, "steamcmd")
}

// Ensure 確保本機快取存在 SteamCMD 並回傳 steamcmd.exe 絕對路徑。快取命中時直接回傳、
// 不打網路;未命中則下載官方 zip 並解壓入快取。progress 可為 nil。
func (s *SteamCMDProvisioner) Ensure(ctx context.Context, progress ProgressFunc) (string, error) {
	dir := s.steamCMDDir()
	exePath := filepath.Join(dir, "steamcmd.exe")

	// 快取命中(免鎖快路徑)。
	if _, err := os.Stat(exePath); err == nil {
		return exePath, nil
	}

	// 序列化,避免併發重複下載。
	unlock := s.locks.lock("steamcmd")
	defer unlock()

	// 取得鎖後再查一次:可能已被前一個持鎖者供應完成。
	if _, err := os.Stat(exePath); err == nil {
		return exePath, nil
	}

	return s.provision(ctx, dir, progress)
}

// provision 執行實際供應:下載 zip(無官方 checksum,見套件註解)→ 解壓至 staging →
// 原子升格為快取目錄。任一步失敗時,快取目錄 dir 不會出現(半成品留在被清除的暫存/staging)。
func (s *SteamCMDProvisioner) provision(ctx context.Context, dir string, progress ProgressFunc) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", fmt.Errorf("provision: 建立 SteamCMD 快取父目錄失敗: %w", err)
	}

	tmpZip := dir + ".zip.part"
	defer os.Remove(tmpZip)
	dlReq := downloadRequest{
		URL:      s.baseURL,
		DestPath: tmpZip,
		Checksum: checksumSpec{}, // 無官方 checksum 可校驗,見套件層註解偏離說明。
		Stage:    "下載 SteamCMD",
		Progress: progress,
	}
	if err := download(ctx, s.client, dlReq); err != nil {
		return "", fmt.Errorf("provision: 下載 SteamCMD 失敗: %w", err)
	}

	staging := dir + ".partial"
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("provision: 清理殘留 staging 失敗: %w", err)
	}
	defer os.RemoveAll(staging) // 成功已 rename 掉,no-op;失敗則清除半成品。

	if err := extractZip(tmpZip, staging, "解壓 SteamCMD", progress); err != nil {
		return "", fmt.Errorf("provision: 解壓 SteamCMD 失敗: %w", err)
	}

	// 校驗替代:確認解壓後確實產出 steamcmd.exe(見套件層 checksum 偏離說明)。
	stagedExe, ok := findSteamCMDExe(staging)
	if !ok {
		return "", fmt.Errorf("provision: SteamCMD 解壓後找不到 steamcmd.exe")
	}

	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("provision: 清理舊 SteamCMD 快取失敗: %w", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", fmt.Errorf("provision: 升格 SteamCMD 快取目錄失敗: %w", err)
	}

	rel, err := filepath.Rel(staging, stagedExe)
	if err != nil {
		return "", fmt.Errorf("provision: 換算 steamcmd.exe 路徑失敗: %w", err)
	}
	return filepath.Join(dir, rel), nil
}

// InstallApp 以 SteamCMD 安裝或更新(同一命令,SteamCMD 具冪等性)指定 appID 至 installDir。
// 同 appID+installDir 併發呼叫序列化(per-key mutex),避免同一目錄被兩個 SteamCMD 行程同時寫入。
func (s *SteamCMDProvisioner) InstallApp(ctx context.Context, appID string, installDir string, progress ProgressFunc) error {
	exe, err := s.Ensure(ctx, progress)
	if err != nil {
		return err
	}

	unlock := s.locks.lock(appID + "|" + installDir)
	defer unlock()

	args := []string{
		"+force_install_dir", installDir,
		"+login", "anonymous",
		"+app_update", appID, "validate",
		"+quit",
	}

	stage := fmt.Sprintf("安裝 Steam App %s", appID)

	// 首次自我更新特徵下重試一次(見 steamCMDFirstRunMarker):首跑 app_update 於 SteamCMD 自我
	// 更新完成前失敗,重試即成功。重試前 lastErrorLine 重置;失敗一次不下載任何內容,故無重複下載。
	var lastErr error
	for attempt := 1; attempt <= steamCMDInstallAttempts; attempt++ {
		var lastErrorLine string
		onLine := func(line string) {
			if strings.Contains(strings.ToLower(line), steamCMDErrorMarker) {
				lastErrorLine = line
			}
			pct, detail := parseSteamCMDLine(line)
			progress.report(ProvisionProgress{Stage: stage, Percent: pct, Detail: detail})
		}

		runErr := s.exec(ctx, exe, args, onLine)
		if lastErrorLine == "" && runErr == nil {
			return nil
		}
		if lastErrorLine != "" {
			if runErr != nil {
				lastErr = fmt.Errorf("provision: SteamCMD 安裝 App %s 失敗(%s): %w", appID, lastErrorLine, runErr)
			} else {
				lastErr = fmt.Errorf("provision: SteamCMD 安裝 App %s 回報錯誤: %s", appID, lastErrorLine)
			}
		} else {
			lastErr = fmt.Errorf("provision: SteamCMD 安裝 App %s 失敗: %w", appID, runErr)
		}

		// 僅對首次自我更新特徵重試;真實錯誤(No subscription 等)與 ctx 取消不重試。
		if attempt < steamCMDInstallAttempts && isSteamCMDFirstRunError(lastErrorLine) && ctx.Err() == nil {
			continue
		}
		return lastErr
	}
	return lastErr
}

// isSteamCMDFirstRunError 判斷錯誤行是否為 SteamCMD 首次自我更新未就緒特徵(見 steamCMDFirstRunMarker)。
func isSteamCMDFirstRunError(errorLine string) bool {
	return strings.Contains(strings.ToLower(errorLine), steamCMDFirstRunMarker)
}

// parseSteamCMDLine 從 SteamCMD 輸出行嘗試解析百分比進度;解析不到時 percent 回 -1
// (呼叫端仍以固定 stage 回報,detail 帶原始行文字)。
func parseSteamCMDLine(line string) (percent int, detail string) {
	if m := steamProgressRe.FindStringSubmatch(line); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			return int(f), line
		}
	}
	return -1, line
}

// defaultSteamCMDExec 是 SteamCMDExecFunc 的預設實作:以 os/exec 啟動 steamcmd.exe,
// 合併 stdout/stderr 逐行讀出並經 onLine 回呼傳出,回傳行程結束結果(非 0 exit code 為 error)。
func defaultSteamCMDExec(ctx context.Context, exe string, args []string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, exe, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("provision: 建立 SteamCMD stdout pipe 失敗: %w", err)
	}
	cmd.Stderr = cmd.Stdout // 合併 stderr 至同一 pipe,方便單一 scanner 逐行讀取。

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("provision: 啟動 SteamCMD 失敗: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
	scanErr := scanner.Err()

	waitErr := cmd.Wait()
	if scanErr != nil {
		return fmt.Errorf("provision: 讀取 SteamCMD 輸出失敗: %w", scanErr)
	}
	return waitErr
}

// findSteamCMDExe 在 root 下尋找 steamcmd.exe,回傳其絕對路徑。Valve 官方 zip 解壓後
// steamcmd.exe 位於頂層,遞迴尋找以兼容佈局變動。
func findSteamCMDExe(root string) (string, bool) {
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 目錄不存在等錯誤視為未命中,繼續。
		}
		if found != "" {
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(d.Name(), "steamcmd.exe") {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", false
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return found, true
	}
	return abs, true
}
