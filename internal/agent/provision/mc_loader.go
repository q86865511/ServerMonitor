package provision

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InstallerExecFunc 執行官方 loader installer(Fabric/Forge/NeoForge 的 installer jar),
// 將每一行 stdout/stderr 經 onLine 回呼傳出,行程結束後回傳其結果(exit code 非 0 時回非 nil
// error)。與 steamcmd.go 的 SteamCMDExecFunc 同一注入模式:生產環境跑真 JVM,測試注入假行程
// (免真的執行 installer / 免真 JRE)。java 為 java.exe 絕對路徑,args 已含 -jar <installer> ...。
type InstallerExecFunc func(ctx context.Context, java string, args []string, onLine func(string)) error

// defaultInstallerExec 是 InstallerExecFunc 的預設實作:以 os/exec 啟動 java、合併 stdout/stderr
// 逐行讀出並經 onLine 傳出,回傳行程結束結果(非 0 exit code 為 error)。比照 defaultSteamCMDExec。
func defaultInstallerExec(ctx context.Context, java string, args []string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, java, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("provision: 建立 installer stdout pipe 失敗: %w", err)
	}
	cmd.Stderr = cmd.Stdout // 合併 stderr,單一 scanner 逐行讀取。

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("provision: 啟動 installer 失敗: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
	scanErr := scanner.Err()

	waitErr := cmd.Wait()
	if scanErr != nil {
		return fmt.Errorf("provision: 讀取 installer 輸出失敗: %w", scanErr)
	}
	return waitErr
}

// loaderInstallParams 描述一次「下載 installer jar → 執行 → 驗證輸出 → 原子升格至 TargetDir」流程
// 的參數。buildArgs/validate 由各 loader 提供以吸收差異(Fabric 的 server 子命令 vs Forge/NeoForge
// 的 --installServer;輸出檔名不同)。
type loaderInstallParams struct {
	java          string       // 執行 installer 用的 java.exe 絕對路徑(req.JavaExe)
	installerURL  string       // installer jar 下載 URL
	checksum      checksumSpec // installer jar 校驗(maven .sha1 旁檔;無旁檔時為 skip)
	targetDir     string       // 最終落地目錄
	downloadStage string       // 下載階段的進度 stage 名
	execStage     string       // 執行階段的進度 stage 名
	// buildArgs 依 installer jar 路徑與 staging 目錄產生 java 參數(不含 java 執行檔本身)。
	buildArgs func(installerPath, stagingDir string) []string
	// validate 檢查 installer 是否於 stagingDir 產出預期結構;不符時回錯(視為安裝失敗)。
	validate func(stagingDir string) error
}

// runLoaderInstaller 執行 loaderInstallParams 描述的安裝流程,並保證「失敗不留半成品」:
// 全程在與 targetDir 同磁碟區的暫存工作目錄內下載/執行/暫存,僅在 installer 成功且輸出結構
// 通過驗證後,才把 staging 內容升格(rename)進 targetDir。任一步失敗時工作目錄整個被清除,
// targetDir 不會出現半成品(對齊 java.go 的 staging→jreDir 原子升格慣例)。
//
// 工作目錄刻意建在 filepath.Dir(targetDir) 之下(而非系統 TEMP):Windows 上 os.Rename 不能
// 跨磁碟區,staging 與 targetDir 必須同卷才能原子升格。
func runLoaderInstaller(ctx context.Context, client *http.Client, execFn InstallerExecFunc, p loaderInstallParams, progress ProgressFunc) error {
	if p.java == "" {
		return fmt.Errorf("provision: loader 安裝需要 JavaExe(執行官方 installer)但未提供")
	}

	parent := filepath.Dir(p.targetDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("provision: 建立安裝目的父目錄失敗: %w", err)
	}
	work, err := os.MkdirTemp(parent, ".mc-loader-*")
	if err != nil {
		return fmt.Errorf("provision: 建立 installer 工作目錄失敗: %w", err)
	}
	defer os.RemoveAll(work) // 成功已把 staging 內容 rename 走;失敗則連同半成品一併清除。

	installerPath := filepath.Join(work, "installer.jar")
	dlReq := downloadRequest{
		URL:      p.installerURL,
		DestPath: installerPath,
		Checksum: p.checksum,
		Stage:    p.downloadStage,
		Progress: progress,
	}
	if err := download(ctx, client, dlReq); err != nil {
		return fmt.Errorf("provision: 下載 installer 失敗: %w", err)
	}

	staging := filepath.Join(work, "server")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("provision: 建立 installer staging 目錄失敗: %w", err)
	}

	var lastLine string
	onLine := func(line string) {
		if s := strings.TrimSpace(line); s != "" {
			lastLine = s
		}
		progress.report(ProvisionProgress{Stage: p.execStage, Percent: -1, Detail: line})
	}

	if runErr := execFn(ctx, p.java, p.buildArgs(installerPath, staging), onLine); runErr != nil {
		if lastLine != "" {
			return fmt.Errorf("provision: installer 執行失敗(最後輸出:%s): %w", lastLine, runErr)
		}
		return fmt.Errorf("provision: installer 執行失敗: %w", runErr)
	}

	if err := p.validate(staging); err != nil {
		return err // validate 已含足夠語意的錯誤訊息。
	}

	if err := promoteDir(staging, p.targetDir); err != nil {
		return fmt.Errorf("provision: 升格 installer 輸出至目的目錄失敗: %w", err)
	}
	return nil
}

// promoteDir 把 staging 內的頂層條目逐一 rename 進 target(target 不存在時先建立)。以逐條目
// rename 併入而非整目錄 rename,以相容 target 已存在其他檔案(如先寫入的 eula.txt)的情況。
// staging 與 target 需同磁碟區(呼叫端保證,見 runLoaderInstaller)。目標已存在同名條目時回錯
// (fresh 安裝不應發生;視為異常)。
func promoteDir(staging, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("provision: 建立目的目錄失敗: %w", err)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("provision: 讀取 staging 目錄失敗: %w", err)
	}
	for _, e := range entries {
		src := filepath.Join(staging, e.Name())
		dst := filepath.Join(target, e.Name())
		if _, statErr := os.Stat(dst); statErr == nil {
			return fmt.Errorf("provision: 目的目錄已存在同名條目 %q,拒絕覆寫", e.Name())
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("provision: 移入 %q 失敗: %w", e.Name(), err)
		}
	}
	return nil
}

// installServerRunScript 與 installServerJVMArgs 是 Forge/NeoForge installer 於 --installServer
// 模式產出的頂層啟動檔(現代結構,MC 1.17+)。啟動經 run.bat/run.sh,無單一 loader server jar;
// user_jvm_args.txt 為使用者可調的 JVM 參數檔(supervisor 注入記憶體上限的落點)。
const (
	installServerRunScript = "run.bat"
	installServerJVMArgs   = "user_jvm_args.txt"
)

// validateInstallServerOutput 驗證 Forge/NeoForge installer 於 stagingDir 是否產出現代
// --installServer 結構:run.bat + user_jvm_args.txt(頂層)+ argsRelPath 指向的 win_args.txt
// (libraries 下,依 loader 座標)。三者齊備才算成功;缺任一即視為安裝失敗或不支援的舊版結構
// (design.md 風險節:MC 基線 1.21.x 只支援現代結構,舊版 forge-universal jar 明確不支援)。
// loaderName 用於錯誤訊息(如 "Forge"/"NeoForge")。
func validateInstallServerOutput(stagingDir, argsRelPath, loaderName string) error {
	required := []string{
		filepath.Join(stagingDir, installServerRunScript),
		filepath.Join(stagingDir, installServerJVMArgs),
		filepath.Join(stagingDir, filepath.FromSlash(argsRelPath)),
	}
	for _, p := range required {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("provision: %s installer 輸出缺少 %q(可能為不支援的舊版結構或安裝失敗):%v",
				loaderName, filepath.Base(p), err)
		}
	}
	return nil
}

// fetchSHA1Sidecar 嘗試取得 maven 制品的 .sha1 旁檔內容(hex 摘要)。maven 倉庫慣例:每個制品旁
// 有 <artifact>.sha1 純文字檔。回傳 (hex, true, nil) 表示取得;(_, false, nil) 表示旁檔不存在
// (404,呼叫端據此跳過校驗並記錄偏離);網路等錯誤回 (_, false, err)。
func fetchSHA1Sidecar(ctx context.Context, client *http.Client, artifactURL string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL+".sha1", nil)
	if err != nil {
		return "", false, fmt.Errorf("provision: 建立 sha1 旁檔請求失敗: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("provision: 取得 sha1 旁檔失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("provision: sha1 旁檔回應非預期狀態 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", false, fmt.Errorf("provision: 讀取 sha1 旁檔失敗: %w", err)
	}
	// 旁檔內容常為「<hex>  <filename>」或純 <hex>,取首個 whitespace 分隔 token。
	sum := strings.TrimSpace(string(body))
	if i := strings.IndexAny(sum, " \t\r\n"); i >= 0 {
		sum = sum[:i]
	}
	if sum == "" {
		return "", false, fmt.Errorf("provision: sha1 旁檔內容為空")
	}
	return sum, true, nil
}

// installerChecksum 依 artifactURL 的 .sha1 旁檔組出 checksumSpec(以 sha1 校驗)。官方 maven
// (Fabric/Forge/NeoForge)對每個制品必附 .sha1 旁檔,故旁檔缺失(404)代表載點異常、被竄改或非
// 官方鏡像——不再靜默跳過校驗(雙審 #12:未校驗即安裝是完整性缺口),改為硬失敗回明確錯誤。
// 網路錯誤上拋(installer 下載本就需要網路)。
func installerChecksum(ctx context.Context, client *http.Client, artifactURL string) (checksumSpec, error) {
	sum, ok, err := fetchSHA1Sidecar(ctx, client, artifactURL)
	if err != nil {
		return checksumSpec{}, err
	}
	if !ok {
		return checksumSpec{}, fmt.Errorf("provision: installer %s 缺 .sha1 旁檔(旁檔回 404);官方 maven 應必附,拒絕未校驗安裝", artifactURL)
	}
	return checksumSpec{Algo: "sha1", Value: sum}, nil
}
