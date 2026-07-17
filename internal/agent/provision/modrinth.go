package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// defaultModrinthAPIBase 是 Modrinth API v2 的預設 base URL。
//
// 查證(2026-07-16):
//   - https://docs.modrinth.com/api/operations/getprojectversions/ ——
//     GET /project/{id|slug}/version(query loaders、game_versions,值為 JSON 陣列字串如
//     `["fabric"]`/`["1.20.1"]`;回應依新→舊排序);version 物件含 id/game_versions/loaders/files,
//     files[] 含 filename/primary/hashes(sha1、sha512)/url。
//   - 直接以版本 ID 取單一版本:GET /version/{id}。
const defaultModrinthAPIBase = "https://api.modrinth.com/v2"

// ModrinthProvider 依 Modrinth API 解析模組包來源(slug/專案 ID,或直接版本 ID)取得 mrpack 封存檔
// 並安裝(native-backend R11)。
type ModrinthProvider struct {
	client  *http.Client
	apiBase string
}

// NewModrinthProvider 建構 ModrinthProvider。client 為 nil 時用零值 http.Client;apiBase 為空
// 字串時採官方預設(測試以 httptest server URL 注入)。
func NewModrinthProvider(client *http.Client, apiBase string) *ModrinthProvider {
	if client == nil {
		client = &http.Client{}
	}
	if apiBase == "" {
		apiBase = defaultModrinthAPIBase
	}
	return &ModrinthProvider{client: client, apiBase: apiBase}
}

var _ ModProvider = (*ModrinthProvider)(nil)

// modrinthVersionFile 是 GET .../version 回應中 files[] 元素的最小子集(查證來源同套件頂註)。
type modrinthVersionFile struct {
	Filename string            `json:"filename"`
	Primary  bool              `json:"primary"`
	URL      string            `json:"url"`
	Hashes   map[string]string `json:"hashes"`
}

// modrinthVersion 是 GET /version/{id} 與 GET /project/{ref}/version 回應元素的最小子集。
type modrinthVersion struct {
	ID           string                `json:"id"`
	GameVersions []string              `json:"game_versions"`
	Loaders      []string              `json:"loaders"`
	Files        []modrinthVersionFile `json:"files"`
}

// InstallModpack 安裝一個 Modrinth 模組包:req.ArchivePath 非空時直接安裝該本機既有封存檔
// (手動上傳路徑,略過遠端解析);否則依 req.Ref 解析(先試直接版本 ID,失敗才試 slug/專案 ID 查
// 相容版本清單)、下載 mrpack 至暫存檔後安裝,結束後清除暫存檔。
func (m *ModrinthProvider) InstallModpack(ctx context.Context, req ModpackInstallRequest, progress ProgressFunc) error {
	archivePath := req.ArchivePath
	if archivePath == "" {
		if req.Ref == nil || strings.TrimSpace(req.Ref.Ref) == "" {
			return fmt.Errorf("provision: Modrinth 安裝請求缺少來源(Ref/ArchivePath 皆空)")
		}
		resolved, cleanup, err := m.resolve(ctx, *req.Ref, req.MCVersion, req.Loader, progress)
		if err != nil {
			return err
		}
		defer cleanup()
		archivePath = resolved
	}
	return installMrpackArchive(ctx, m.client, archivePath, req.TargetDir, req.MCVersion, req.Loader, progress)
}

// resolve 解析 ref 取得 mrpack 下載來源並落地至暫存檔,回傳其路徑與清理函式(呼叫端 defer 呼叫,
// 不論安裝成敗都清除暫存檔)。
func (m *ModrinthProvider) resolve(ctx context.Context, ref ModpackRef, mcVersion, loader string, progress ProgressFunc) (path string, cleanup func(), err error) {
	ver, verr := m.fetchVersionByID(ctx, ref.Ref)
	if verr != nil {
		ver, verr = m.selectVersion(ctx, ref.Ref, mcVersion, loader)
		if verr != nil {
			return "", func() {}, verr
		}
	}
	file, ok := mrpackFileOf(ver)
	if !ok {
		return "", func() {}, fmt.Errorf("provision: Modrinth 版本 %s 未提供 .mrpack 檔", ver.ID)
	}

	tmp, terr := os.CreateTemp("", "modrinth-*.mrpack")
	if terr != nil {
		return "", func() {}, fmt.Errorf("provision: 建立暫存檔失敗: %w", terr)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	cleanup = func() { os.Remove(tmpPath) }

	if err := download(ctx, m.client, downloadRequest{
		URL:      file.URL,
		DestPath: tmpPath,
		Checksum: mrpackChecksum(file.Hashes),
		Stage:    "下載模組包",
		Progress: progress,
	}); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("provision: 下載模組包 %q 失敗: %w", ref.Ref, err)
	}
	return tmpPath, cleanup, nil
}

// fetchVersionByID 以直接版本 ID 查詢(GET /version/{id});404 視為「非版本 ID」而非錯誤,
// 回傳 error 讓呼叫端改走 selectVersion(slug/專案 ID 查詢)。
func (m *ModrinthProvider) fetchVersionByID(ctx context.Context, id string) (modrinthVersion, error) {
	var v modrinthVersion
	u := fmt.Sprintf("%s/version/%s", strings.TrimRight(m.apiBase, "/"), url.PathEscape(id))
	found, err := m.getJSON(ctx, u, &v)
	if err != nil {
		return modrinthVersion{}, err
	}
	if !found {
		return modrinthVersion{}, fmt.Errorf("provision: Modrinth 版本 %q 不存在", id)
	}
	return v, nil
}

// selectVersion 查詢專案(slug 或專案 ID)的版本清單,依 mcVersion/loader 篩選相容版本,取首個
// (API 依新→舊排序,查證同套件頂註)。
func (m *ModrinthProvider) selectVersion(ctx context.Context, projectRef, mcVersion, loader string) (modrinthVersion, error) {
	u := fmt.Sprintf("%s/project/%s/version", strings.TrimRight(m.apiBase, "/"), url.PathEscape(projectRef))
	q := url.Values{}
	if mcVersion != "" {
		q.Set("game_versions", fmt.Sprintf("[%q]", mcVersion))
	}
	if loader != "" {
		q.Set("loaders", fmt.Sprintf("[%q]", strings.ToLower(loader)))
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var versions []modrinthVersion
	found, err := m.getJSON(ctx, u, &versions)
	if err != nil {
		return modrinthVersion{}, err
	}
	if !found || len(versions) == 0 {
		return modrinthVersion{}, fmt.Errorf("provision: Modrinth 專案 %q 無相容版本(MC %s / loader %s)", projectRef, mcVersion, loader)
	}
	return versions[0], nil
}

// getJSON 對 u 發 GET 並解析 JSON 至 out;404 回 (false, nil)(呼叫端依此判斷「不存在」而非
// 「查詢失敗」),其餘非 200 回錯。
func (m *ModrinthProvider) getJSON(ctx context.Context, u string, out any) (bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("provision: 建立 Modrinth API 請求失敗: %w", err)
	}
	resp, err := m.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("provision: 查詢 Modrinth API 失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("provision: Modrinth API 回應非預期狀態 %d(%s)", resp.StatusCode, u)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("provision: 解析 Modrinth API 回應失敗: %w", err)
	}
	return true, nil
}

// mrpackFileOf 從版本的 files[] 挑出 .mrpack 檔(優先 primary 標記者)。
func mrpackFileOf(v modrinthVersion) (modrinthVersionFile, bool) {
	for _, f := range v.Files {
		if f.Primary && strings.HasSuffix(strings.ToLower(f.Filename), ".mrpack") {
			return f, true
		}
	}
	for _, f := range v.Files {
		if strings.HasSuffix(strings.ToLower(f.Filename), ".mrpack") {
			return f, true
		}
	}
	return modrinthVersionFile{}, false
}
