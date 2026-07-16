package provision

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// curseforgeAPIKey 是編譯期以下列 ldflags 注入的專案專屬 CurseForge API 金鑰:
//
//	-ldflags "-X servermonitor/internal/agent/provision.curseforgeAPIKey=<key>"
//
// 比照 Prism Launcher(key 專屬本專案、不進 repo,衍生作品須自行申請並替換——見 requirements
// R14/Q1 與 README「CurseForge 模組包」節)。預設空字串=無內嵌 key(CurseForge 模組包功能停用)。
// 使用者設定(core.AppConfig.CurseForgeAPIKey)非空時覆蓋之(見 provision.New 的 WithCurseForgeAPIKey)。
// 本值刻意不落任何 log/事件;僅於 API 請求以 x-api-key header 傳遞。
var curseforgeAPIKey string

// defaultCurseForgeAPIBase 是 CurseForge 官方 API 的預設 base URL(測試以 WithCurseForgeAPIBase 注入 httptest)。
//
// 查證(2026-07-17,https://docs.curseforge.com/rest-api/):
//   - GET /v1/mods/{modId}/files/{fileId} → { "data": File }。File 含 id/modId/fileName/fileLength/
//     downloadUrl(作者停用第三方 API 散布時為 null)/hashes[](各含 value 字串 + algo 整數,1=sha1、2=md5)。
//   - GET /v1/mods/{modId} → { "data": Mod }。Mod.links.websiteUrl 為專案頁 URL(被擋模組手動下載引導)。
//   - 認證:x-api-key 請求 header。
//   - CurseForge 模組包(cfzip)的 manifest.json:minecraft{version, modLoaders[]{id,primary}}、
//     files[]{projectID,fileID,required}、overrides(overrides 資料夾名,複製到實例根;通常 "overrides")。
//     manifest 的 files[] 僅含 ID,實際下載連結須逐檔查上述 files 端點。
const defaultCurseForgeAPIBase = "https://api.curseforge.com"

// cfHashAlgoSha1 是 CurseForge File.hashes[].algo 的 sha1 列舉值(2=md5,download() 不支援故忽略)。
const cfHashAlgoSha1 = 1

// BlockedMod 是一個因作者停用第三方 API 散布(downloadUrl=null)而無法自動下載的模組
// (native-backend R14)。清單經 provision 進度事件(Stage="blocked-mods",Detail=JSON)回報 GUI,
// 引導使用者以瀏覽器手動下載後匯入。
type BlockedMod struct {
	ProjectID  int    `json:"project_id"`
	FileID     int    `json:"file_id"`
	FileName   string `json:"file_name"`
	FileLength int64  `json:"file_length,omitempty"` // API 回報的檔案長度(匯入匹配用;0=API 未提供)
	URL        string `json:"url"`                   // 手動下載頁(links.websiteUrl + "/files/" + fileID;查不到時退回數字專案頁)
}

// BlockedModsError 表示 CurseForge 模組包含至少一個作者停用第三方散布的模組:可下載的部分與 overrides
// 已安裝,但清單所列模組須使用者手動下載後重試(native-backend R14 降級方案)。以 errors.As 取出清單。
type BlockedModsError struct {
	Mods []BlockedMod
}

func (e *BlockedModsError) Error() string {
	names := make([]string, 0, len(e.Mods))
	for _, m := range e.Mods {
		names = append(names, m.FileName)
	}
	return fmt.Sprintf("provision: CurseForge 模組包含 %d 個作者停用第三方散布的模組,須手動下載: %s",
		len(e.Mods), strings.Join(names, ", "))
}

// cfManifest 對映 CurseForge 模組包的 manifest.json(查證來源見 defaultCurseForgeAPIBase 註)。
type cfManifest struct {
	Minecraft struct {
		Version    string `json:"version"`
		ModLoaders []struct {
			ID      string `json:"id"`
			Primary bool   `json:"primary"`
		} `json:"modLoaders"`
	} `json:"minecraft"`
	Name      string           `json:"name"`
	Version   string           `json:"version"`
	Files     []cfManifestFile `json:"files"`
	Overrides string           `json:"overrides"`
}

// cfManifestFile 是 manifest.json 的 files[] 元素(僅 ID,下載連結須逐檔查 API)。
type cfManifestFile struct {
	ProjectID int  `json:"projectID"`
	FileID    int  `json:"fileID"`
	Required  bool `json:"required"`
}

// cfFileResponse / cfFile / cfHash 對映 GET /v1/mods/{modId}/files/{fileId} 回應的最小子集。
type cfFileResponse struct {
	Data cfFile `json:"data"`
}
type cfFile struct {
	ID          int      `json:"id"`
	ModID       int      `json:"modId"`
	FileName    string   `json:"fileName"`
	FileLength  int64    `json:"fileLength"`
	DownloadURL *string  `json:"downloadUrl"` // 作者停用第三方散布時為 null
	Hashes      []cfHash `json:"hashes"`
}
type cfHash struct {
	Value string `json:"value"`
	Algo  int    `json:"algo"`
}

// cfModResponse 對映 GET /v1/mods/{modId} 回應中組被擋模組手動頁所需的最小子集。
type cfModResponse struct {
	Data struct {
		Links struct {
			WebsiteURL string `json:"websiteUrl"`
		} `json:"links"`
	} `json:"data"`
}

// CurseForgeProvider 依 CurseForge 官方 API 解析 cfzip 模組包 manifest、逐檔取下載連結並安裝至實例
// (native-backend R14)。作者停用第三方散布(downloadUrl=null)的模組收集為 BlockedMod 清單,不視為
// 立即失敗:先安裝可下載部分與 overrides,再(視匯入資料夾)嘗試自動補齊、仍缺者回 BlockedModsError
// 引導手動下載(降級方案;Create 為同步阻塞,不採「暫停等待監看」以免與 Create 逾時衝突)。
type CurseForgeProvider struct {
	client  *http.Client
	apiBase string
	apiKey  string
}

// NewCurseForgeProvider 建構 CurseForgeProvider。client 為 nil 時用零值 http.Client;apiBase 為空
// 時採官方預設(測試以 httptest server URL 注入);apiKey 為 API 金鑰(x-api-key header)。
func NewCurseForgeProvider(client *http.Client, apiBase, apiKey string) *CurseForgeProvider {
	if client == nil {
		client = &http.Client{}
	}
	if apiBase == "" {
		apiBase = defaultCurseForgeAPIBase
	}
	return &CurseForgeProvider{client: client, apiBase: apiBase, apiKey: apiKey}
}

var _ ModProvider = (*CurseForgeProvider)(nil)

// InstallModpack 安裝一個 CurseForge 模組包:req.ArchivePath 非空時直接安裝該本機 cfzip(手動上傳
// 路徑,略過遠端解析);否則依 req.Ref 取得 cfzip(見 obtainArchive 的 Ref 語意)後安裝。
func (c *CurseForgeProvider) InstallModpack(ctx context.Context, req ModpackInstallRequest, progress ProgressFunc) error {
	archivePath := req.ArchivePath
	if archivePath == "" {
		if req.Ref == nil || strings.TrimSpace(req.Ref.Ref) == "" {
			return fmt.Errorf("provision: CurseForge 安裝請求缺少來源(Ref/ArchivePath 皆空)")
		}
		resolved, cleanup, err := c.obtainArchive(ctx, req.Ref.Ref, progress)
		if err != nil {
			return err
		}
		defer cleanup()
		archivePath = resolved
	}
	return c.installArchive(ctx, archivePath, req, progress)
}

// obtainArchive 依 Ref 取得 cfzip 封存檔並落地至暫存檔。Ref 語意(native-backend R14):
//   - HTTPS URL:直接下載該 cfzip(無官方 checksum,略過校驗)。
//   - "<projectID>:<fileID>"(或以 "/" 分隔):經 API 查該模組包檔的 downloadUrl 後下載。
//
// 回傳暫存檔路徑與清理函式(呼叫端 defer 清理,不論成敗)。
func (c *CurseForgeProvider) obtainArchive(ctx context.Context, ref string, progress ProgressFunc) (string, func(), error) {
	tmp, err := os.CreateTemp("", "curseforge-*.zip")
	if err != nil {
		return "", func() {}, fmt.Errorf("provision: 建立暫存檔失敗: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	cleanup := func() { os.Remove(tmpPath) }

	var dlURL string
	var sum checksumSpec
	if isHTTPURL(ref) {
		dlURL = ref
	} else {
		projID, fileID, perr := parseCurseForgeRef(ref)
		if perr != nil {
			cleanup()
			return "", func() {}, perr
		}
		f, ferr := c.fetchFile(ctx, projID, fileID)
		if ferr != nil {
			cleanup()
			return "", func() {}, ferr
		}
		if f.DownloadURL == nil {
			cleanup()
			return "", func() {}, fmt.Errorf("provision: CurseForge 模組包檔案(project %d file %d)本身停用第三方散布,無法自動下載", projID, fileID)
		}
		dlURL = *f.DownloadURL
		sum = cfChecksum(f.Hashes)
	}
	if err := download(ctx, c.client, downloadRequest{
		URL:      dlURL,
		DestPath: tmpPath,
		Checksum: sum,
		Stage:    "下載 CurseForge 模組包",
		Progress: progress,
	}); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("provision: 下載 CurseForge 模組包失敗: %w", err)
	}
	return tmpPath, cleanup, nil
}

// installArchive 是 cfzip 安裝核心:解析 manifest → 逐檔查 API 取下載連結 → 下載(sha1 可用時校驗)落於
// mods/ → 解 overrides 至實例根 → 被擋模組(downloadUrl=null)嘗試匯入補齊,仍缺者回 BlockedModsError。
// req.TargetDir 為實例根(mods 為其子目錄;與 mrpack 安裝的 targetDir 語意一致——見 modprovider.go)。
func (c *CurseForgeProvider) installArchive(ctx context.Context, archivePath string, req ModpackInstallRequest, progress ProgressFunc) error {
	manifest, err := readCurseForgeManifest(archivePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(req.TargetDir, 0o755); err != nil {
		return fmt.Errorf("provision: 建立模組落位目錄失敗: %w", err)
	}

	var blocked []BlockedMod
	total := len(manifest.Files)
	for i, mf := range manifest.Files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f, ferr := c.fetchFile(ctx, mf.ProjectID, mf.FileID)
		if ferr != nil {
			return ferr
		}
		if f.DownloadURL == nil {
			blocked = append(blocked, BlockedMod{
				ProjectID:  mf.ProjectID,
				FileID:     mf.FileID,
				FileName:   f.FileName,
				FileLength: f.FileLength,
				URL:        c.blockedModURL(ctx, mf.ProjectID, mf.FileID),
			})
			continue
		}
		dest, derr := safeModpackFilePath(req.TargetDir, path.Join("mods", f.FileName))
		if derr != nil {
			return derr
		}
		if err := download(ctx, c.client, downloadRequest{
			URL:      *f.DownloadURL,
			DestPath: dest,
			Checksum: cfChecksum(f.Hashes),
			Stage:    fmt.Sprintf("下載模組 %d/%d", i+1, total),
			Progress: progress,
		}); err != nil {
			return fmt.Errorf("provision: 下載模組檔 %q 失敗: %w", f.FileName, err)
		}
	}

	if err := extractCurseForgeOverrides(archivePath, manifest.Overrides, req.TargetDir); err != nil {
		return err
	}

	// 被擋模組:先嘗試從匯入資料夾自動補齊(降級方案的重試/監看匯入路徑,見 importBlockedMods);
	// 仍缺者以 blocked-mods 事件回報 GUI 並回 BlockedModsError(引導手動下載後重試)。
	if len(blocked) > 0 && strings.TrimSpace(req.ImportDir) != "" {
		remaining, ierr := importBlockedMods(req.ImportDir, req.TargetDir, blocked, progress)
		if ierr != nil {
			return ierr
		}
		blocked = remaining
	}
	if len(blocked) > 0 {
		reportBlockedMods(blocked, progress)
		return &BlockedModsError{Mods: blocked}
	}

	progress.report(ProvisionProgress{Stage: "modpack", Percent: 100, Detail: "安裝完成"})
	return nil
}

// readCurseForgeManifest 於 archivePath 內尋找頂層 manifest.json 並解析。
func readCurseForgeManifest(archivePath string) (cfManifest, error) {
	rc, err := zip.OpenReader(archivePath)
	if err != nil {
		return cfManifest{}, fmt.Errorf("provision: 無法以 zip 開啟 CurseForge 模組包 %q: %w", archivePath, err)
	}
	defer rc.Close()
	for _, f := range rc.File {
		if f.Name != "manifest.json" {
			continue
		}
		data, err := readZipEntryLimited(f, maxMrpackIndexBytes)
		if err != nil {
			return cfManifest{}, err
		}
		var m cfManifest
		if err := json.Unmarshal(data, &m); err != nil {
			return cfManifest{}, fmt.Errorf("provision: 解析 manifest.json 失敗: %w", err)
		}
		return m, nil
	}
	return cfManifest{}, fmt.Errorf("provision: CurseForge 模組包 %q 缺少 manifest.json", archivePath)
}

// extractCurseForgeOverrides 把 cfzip 內 overrides 資料夾(manifest.overrides 命名,預設 "overrides")
// 複製到實例根(targetDir)。zip-slip 防禦沿用 mrpack overrides 的落地邏輯(extractMrpackOverrideEntry)。
func extractCurseForgeOverrides(archivePath, overridesDir, targetDir string) error {
	overridesDir = strings.TrimSpace(overridesDir)
	if overridesDir == "" {
		overridesDir = "overrides"
	}
	prefix := strings.TrimSuffix(overridesDir, "/") + "/"
	rc, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("provision: 無法以 zip 開啟 CurseForge 模組包 %q: %w", archivePath, err)
	}
	defer rc.Close()
	for _, f := range rc.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || rel == "" {
			continue
		}
		if err := extractMrpackOverrideEntry(f, rel, targetDir); err != nil {
			return err
		}
	}
	return nil
}

// fetchFile 查 GET /v1/mods/{modId}/files/{fileId}(帶 x-api-key)取單一檔案中繼。
func (c *CurseForgeProvider) fetchFile(ctx context.Context, projectID, fileID int) (cfFile, error) {
	u := fmt.Sprintf("%s/v1/mods/%d/files/%d", strings.TrimRight(c.apiBase, "/"), projectID, fileID)
	var resp cfFileResponse
	if err := c.getJSON(ctx, u, &resp); err != nil {
		return cfFile{}, fmt.Errorf("provision: 查詢 CurseForge 檔案(project %d file %d)失敗: %w", projectID, fileID, err)
	}
	return resp.Data, nil
}

// blockedModURL 組出被擋模組的手動下載頁 URL:查 GET /v1/mods/{modId} 取 links.websiteUrl,附
// "/files/{fileID}"。查詢失敗或無 websiteUrl 時退回官方數字專案頁(仍可導向使用者手動搜尋)。
func (c *CurseForgeProvider) blockedModURL(ctx context.Context, projectID, fileID int) string {
	u := fmt.Sprintf("%s/v1/mods/%d", strings.TrimRight(c.apiBase, "/"), projectID)
	var resp cfModResponse
	if err := c.getJSON(ctx, u, &resp); err == nil {
		if ws := strings.TrimSpace(resp.Data.Links.WebsiteURL); ws != "" {
			return strings.TrimRight(ws, "/") + "/files/" + strconv.Itoa(fileID)
		}
	}
	return fmt.Sprintf("https://www.curseforge.com/projects/%d", projectID)
}

// getJSON 對 u 發 GET(帶 x-api-key 與 Accept)並解析 JSON 至 out。非 200 回錯——刻意只回狀態碼與
// URL,不回 body/header(杜絕金鑰或敏感回應內容外洩至錯誤訊息/log)。
func (c *CurseForgeProvider) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("provision: 建立 CurseForge API 請求失敗: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("provision: 查詢 CurseForge API 失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provision: CurseForge API 回應非預期狀態 %d(%s)", resp.StatusCode, u)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("provision: 解析 CurseForge API 回應失敗: %w", err)
	}
	return nil
}

// cfChecksum 由 CurseForge File.hashes 取校驗規格:採 sha1(algo=1);僅 md5(algo=2)或無 hash 時回空
// (download() 不支援 md5,略過校驗——來源為官方 CDN + HTTPS,自負風險,與 mrpack 缺 hash 同策略)。
func cfChecksum(hashes []cfHash) checksumSpec {
	for _, h := range hashes {
		if h.Algo == cfHashAlgoSha1 && strings.TrimSpace(h.Value) != "" {
			return checksumSpec{Algo: "sha1", Value: h.Value}
		}
	}
	return checksumSpec{}
}

// importBlockedMods 於匯入資料夾以「檔名(＋API 回報的檔案長度,若有)」匹配使用者手動下載的被擋模組,
// 匹配者複製進實例 mods/ 目錄(native-backend R14 降級方案的自動匯入/重試路徑)。回傳仍未補齊的清單。
func importBlockedMods(importDir, targetDir string, blocked []BlockedMod, progress ProgressFunc) ([]BlockedMod, error) {
	var remaining []BlockedMod
	for _, m := range blocked {
		src := filepath.Join(importDir, m.FileName)
		info, err := os.Stat(src)
		if err != nil || info.IsDir() {
			remaining = append(remaining, m)
			continue
		}
		if m.FileLength > 0 && info.Size() != m.FileLength {
			remaining = append(remaining, m) // 長度不符:疑似下載不完整/錯檔,不匯入
			continue
		}
		dest, derr := safeModpackFilePath(targetDir, path.Join("mods", m.FileName))
		if derr != nil {
			return nil, derr
		}
		if err := copyFileInto(src, dest); err != nil {
			return nil, fmt.Errorf("provision: 匯入被擋模組 %q 失敗: %w", m.FileName, err)
		}
		progress.report(ProvisionProgress{Stage: "import-blocked", Percent: -1, Detail: "已匯入 " + m.FileName})
	}
	return remaining, nil
}

// copyFileInto 複製 src 至 dest(建立父目錄,覆寫既有)。
func copyFileInto(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// reportBlockedMods 以 provision 進度事件回報被擋模組清單:Stage="blocked-mods"、Detail=JSON 清單,
// 供 GUI 解析並顯示 BlockedMods 對話框(native-backend R14)。progress 為 nil 時為 no-op。
func reportBlockedMods(blocked []BlockedMod, progress ProgressFunc) {
	if progress == nil {
		return
	}
	data, err := json.Marshal(blocked)
	if err != nil {
		return
	}
	progress.report(ProvisionProgress{Stage: "blocked-mods", Percent: -1, Detail: string(data)})
}

// parseCurseForgeRef 解析 "<projectID>:<fileID>"(或以 "/" 分隔)為兩個整數 ID。
func parseCurseForgeRef(ref string) (projectID, fileID int, err error) {
	sep := ":"
	if strings.Contains(ref, "/") && !strings.Contains(ref, ":") {
		sep = "/"
	}
	parts := strings.SplitN(ref, sep, 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("provision: 無法解析 CurseForge 模組包 ref %q(需 \"<projectID>:<fileID>\" 或 cfzip URL)", ref)
	}
	projectID, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("provision: CurseForge 模組包 ref %q 的 projectID 非整數", ref)
	}
	fileID, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("provision: CurseForge 模組包 ref %q 的 fileID 非整數", ref)
	}
	return projectID, fileID, nil
}

// isHTTPURL 回報 s 是否為 http(s) URL。
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
