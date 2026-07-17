package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
)

// defaultMojangManifestURL 是 Mojang version manifest v2 的預設 URL(測試以 NewVanillaInstaller
// 的 manifestURL 參數注入 httptest;欄位路徑經官方端點實測確認,2026-07-16)。
const defaultMojangManifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"

// VanillaInstaller 安裝官方 Mojang Vanilla 伺服器(R5)。
type VanillaInstaller struct {
	client      *http.Client
	manifestURL string
}

// NewVanillaInstaller 建構 Vanilla 安裝器。client 為 nil 時使用零值 http.Client;manifestURL
// 為空字串時採用官方預設(測試注入 httptest server URL)。
func NewVanillaInstaller(client *http.Client, manifestURL string) *VanillaInstaller {
	if client == nil {
		client = &http.Client{}
	}
	if manifestURL == "" {
		manifestURL = defaultMojangManifestURL
	}
	return &VanillaInstaller{client: client, manifestURL: manifestURL}
}

var _ ServerInstaller = (*VanillaInstaller)(nil)

// mojangVersionManifest 是 version_manifest_v2.json 的最小子集(欄位路徑經官方端點實測確認,
// 2026-07-16:https://piston-meta.mojang.com/mc/game/version_manifest_v2.json)。
type mojangVersionManifest struct {
	Versions []struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"versions"`
}

// mojangVersionDetail 是單一版本 JSON(manifest 內 versions[].url 指向)的最小子集,
// downloads.server 含官方 server.jar 下載連結與 sha1(欄位路徑同上實測確認)。
type mojangVersionDetail struct {
	Downloads struct {
		Server struct {
			SHA1 string `json:"sha1"`
			URL  string `json:"url"`
		} `json:"server"`
	} `json:"downloads"`
}

// Install 依 Mojang version manifest 找出 req.MCVersion 對應版本,取得該版本 JSON 的
// downloads.server 後下載並以 sha1 校驗,通過才落地至 TargetDir/server.jar。
func (v *VanillaInstaller) Install(ctx context.Context, req InstallRequest, progress ProgressFunc) (InstalledServer, error) {
	progress.report(ProvisionProgress{Stage: "查詢 Minecraft 版本清單", Percent: -1, Detail: "Mojang manifest"})

	manifest, err := v.fetchManifest(ctx)
	if err != nil {
		return InstalledServer{}, err
	}

	var versionURL string
	for _, ver := range manifest.Versions {
		if ver.ID == req.MCVersion {
			versionURL = ver.URL
			break
		}
	}
	if versionURL == "" {
		return InstalledServer{}, fmt.Errorf("provision: Mojang version manifest 找不到 Minecraft 版本 %q", req.MCVersion)
	}

	detail, err := v.fetchVersionDetail(ctx, versionURL)
	if err != nil {
		return InstalledServer{}, err
	}
	if detail.Downloads.Server.URL == "" {
		return InstalledServer{}, fmt.Errorf("provision: Minecraft %s 版本 JSON 缺少 downloads.server", req.MCVersion)
	}
	if detail.Downloads.Server.SHA1 == "" {
		return InstalledServer{}, fmt.Errorf("provision: Minecraft %s 的 server.jar 缺少 sha1,拒絕未校驗下載", req.MCVersion)
	}

	dest := filepath.Join(req.TargetDir, "server.jar")
	dlReq := downloadRequest{
		URL:      detail.Downloads.Server.URL,
		DestPath: dest,
		Checksum: checksumSpec{Algo: "sha1", Value: detail.Downloads.Server.SHA1},
		Stage:    fmt.Sprintf("下載 Minecraft %s server.jar", req.MCVersion),
		Progress: progress,
	}
	if err := download(ctx, v.client, dlReq); err != nil {
		return InstalledServer{}, fmt.Errorf("provision: 下載 Vanilla %s server.jar 失敗: %w", req.MCVersion, err)
	}

	return InstalledServer{ServerJar: dest}, nil
}

// fetchManifest 查詢 Mojang version manifest。
func (v *VanillaInstaller) fetchManifest(ctx context.Context) (mojangVersionManifest, error) {
	var manifest mojangVersionManifest
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, v.manifestURL, nil)
	if err != nil {
		return manifest, fmt.Errorf("provision: 建立 Mojang version manifest 請求失敗: %w", err)
	}
	resp, err := v.client.Do(httpReq)
	if err != nil {
		return manifest, fmt.Errorf("provision: 查詢 Mojang version manifest 失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return manifest, fmt.Errorf("provision: Mojang version manifest 回應非預期狀態 %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("provision: 解析 Mojang version manifest 失敗: %w", err)
	}
	return manifest, nil
}

// fetchVersionDetail 查詢單一版本 JSON。
func (v *VanillaInstaller) fetchVersionDetail(ctx context.Context, versionURL string) (mojangVersionDetail, error) {
	var detail mojangVersionDetail
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, versionURL, nil)
	if err != nil {
		return detail, fmt.Errorf("provision: 建立 Minecraft 版本 JSON 請求失敗: %w", err)
	}
	resp, err := v.client.Do(httpReq)
	if err != nil {
		return detail, fmt.Errorf("provision: 查詢 Minecraft 版本 JSON 失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return detail, fmt.Errorf("provision: Minecraft 版本 JSON 回應非預期狀態 %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return detail, fmt.Errorf("provision: 解析 Minecraft 版本 JSON 失敗: %w", err)
	}
	return detail, nil
}
