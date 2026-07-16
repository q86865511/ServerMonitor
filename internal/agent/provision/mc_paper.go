package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// defaultPaperAPIBase 是 PaperMC 官方 API 的預設 base URL。
//
// 注意(2026-07-16 實測查證):規格原文描述的 v2 端點
// (https://api.papermc.io/v2/projects/paper/versions/{ver}/builds)已由 PaperMC 下線,
// 目前回應 {"ok":false,"error":"sunset","message":"This API version has been sunset..."}。
// 現行公開端點為 v3「Fill」API(https://fill.papermc.io/v3/projects/{project}/versions/{version}/builds,
// OpenAPI 規格:https://fill.papermc.io/openapi.yaml),本檔改對接 v3。base URL 仍可由
// NewPaperInstaller 的 apiBase 參數注入以利測試。
const defaultPaperAPIBase = "https://fill.papermc.io"

// paperDownloadKey 是 v3 API downloads map 中「官方預設打包」安裝檔的鍵名(另有
// "server:mojang" 等變體,本任務僅取預設打包)。
const paperDownloadKey = "server:default"

// PaperInstaller 安裝 PaperMC 伺服器(R5)。
type PaperInstaller struct {
	client  *http.Client
	apiBase string
}

// NewPaperInstaller 建構 Paper 安裝器。client 為 nil 時使用零值 http.Client;apiBase 為空
// 字串時採用官方預設(測試注入 httptest server URL)。
func NewPaperInstaller(client *http.Client, apiBase string) *PaperInstaller {
	if client == nil {
		client = &http.Client{}
	}
	if apiBase == "" {
		apiBase = defaultPaperAPIBase
	}
	return &PaperInstaller{client: client, apiBase: apiBase}
}

var _ ServerInstaller = (*PaperInstaller)(nil)

// paperChecksums 對應 v3 BuildResponse.downloads[*].checksums(欄位路徑經官方 OpenAPI 文件與
// 端點實測確認,2026-07-16:https://fill.papermc.io/openapi.yaml)。
type paperChecksums struct {
	SHA256 string `json:"sha256"`
}

// paperDownload 對應 v3 BuildResponse.downloads 的 map value(Download schema)。
type paperDownload struct {
	Name      string         `json:"name"`
	Checksums paperChecksums `json:"checksums"`
	URL       string         `json:"url"`
}

// paperBuild 是 v3 BuildResponse 的最小子集。channel 實測值為 "ALPHA"/"BETA"/"STABLE"/
// "RECOMMENDED"(非規格原文提及的 "default"/"experimental",該命名屬已下線的 v2 API)。
type paperBuild struct {
	ID        int                      `json:"id"`
	Channel   string                   `json:"channel"`
	Downloads map[string]paperDownload `json:"downloads"`
}

// Install 取得 req.MCVersion 的最新 build(優先 STABLE channel,無 STABLE build 時退回任意
// channel 的最新 build)並下載 server:default 安裝檔,以 sha256 校驗後落地至 TargetDir/server.jar。
func (p *PaperInstaller) Install(ctx context.Context, req InstallRequest, progress ProgressFunc) (InstalledServer, error) {
	progress.report(ProvisionProgress{Stage: "查詢 Paper build", Percent: -1, Detail: req.MCVersion})

	build, err := p.latestBuild(ctx, req.MCVersion)
	if err != nil {
		return InstalledServer{}, err
	}

	dl, ok := build.Downloads[paperDownloadKey]
	if !ok || dl.URL == "" {
		return InstalledServer{}, fmt.Errorf("provision: Paper %s build %d 缺少 %s 下載項", req.MCVersion, build.ID, paperDownloadKey)
	}
	if dl.Checksums.SHA256 == "" {
		return InstalledServer{}, fmt.Errorf("provision: Paper %s build %d 缺少 sha256,拒絕未校驗下載", req.MCVersion, build.ID)
	}

	dest := filepath.Join(req.TargetDir, "server.jar")
	dlReq := downloadRequest{
		URL:      dl.URL,
		DestPath: dest,
		Checksum: checksumSpec{Algo: "sha256", Value: dl.Checksums.SHA256},
		Stage:    fmt.Sprintf("下載 Paper %s build %d", req.MCVersion, build.ID),
		Progress: progress,
	}
	if err := download(ctx, p.client, dlReq); err != nil {
		return InstalledServer{}, fmt.Errorf("provision: 下載 Paper %s server.jar 失敗: %w", req.MCVersion, err)
	}

	return InstalledServer{ServerJar: dest}, nil
}

// latestBuild 依序嘗試「僅 STABLE channel」與「不篩 channel」兩次查詢,取清單首個元素
// (端點回傳依 build 新→舊排序,實測確認見上)。兩次查詢皆無結果時回明確錯誤。
func (p *PaperInstaller) latestBuild(ctx context.Context, mcVersion string) (paperBuild, error) {
	builds, err := p.fetchBuilds(ctx, mcVersion, "STABLE")
	if err != nil {
		return paperBuild{}, err
	}
	if len(builds) == 0 {
		builds, err = p.fetchBuilds(ctx, mcVersion, "")
		if err != nil {
			return paperBuild{}, err
		}
	}
	if len(builds) == 0 {
		return paperBuild{}, fmt.Errorf("provision: Paper 版本 %s 沒有任何可用 build", mcVersion)
	}
	return builds[0], nil
}

// fetchBuilds 查詢指定版本的 build 清單;channel 為空字串時不加篩選查詢參數。
func (p *PaperInstaller) fetchBuilds(ctx context.Context, mcVersion, channel string) ([]paperBuild, error) {
	u := fmt.Sprintf("%s/v3/projects/paper/versions/%s/builds", strings.TrimRight(p.apiBase, "/"), url.PathEscape(mcVersion))
	if channel != "" {
		u += "?channel=" + url.QueryEscape(channel)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("provision: 建立 PaperMC build 查詢請求失敗: %w", err)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provision: 查詢 PaperMC build 失敗: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("provision: PaperMC 找不到 Minecraft 版本 %q", mcVersion)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provision: PaperMC build 查詢回應非預期狀態 %d", resp.StatusCode)
	}

	var builds []paperBuild
	if err := json.NewDecoder(resp.Body).Decode(&builds); err != nil {
		return nil, fmt.Errorf("provision: 解析 PaperMC build 回應失敗: %w", err)
	}
	return builds, nil
}
