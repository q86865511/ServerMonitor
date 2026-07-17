package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// defaultFabricMetaBase 是 FabricMC meta API 的預設 base URL(可由 NewFabricInstaller 注入
// httptest)。
//
// spike 查證(2026-07-16,https://meta.fabricmc.net):
//   - GET {base}/v2/versions/loader/{mcversion} → 陣列,元素含 loader{version,stable,build,...};
//     陣列依 loader 新→舊排序,取首個 stable==true 者為推薦 loader 版本。
//   - GET {base}/v2/versions/installer → 陣列,元素含 {url,maven,version,stable};url 為 installer
//     jar 直接下載連結(maven.fabricmc.net),取首個 stable==true 者。
//   - installer 以 server 子命令產出啟動 jar:
//     `{java} -jar installer.jar server -dir <dir> -mcversion <ver> -loader <ver> -downloadMinecraft`
//     於 <dir> 產出 fabric-server-launch.jar(可直接 java -jar 啟動);-downloadMinecraft 令 installer
//     自行下載 vanilla server.jar 至同目錄。
//
// 另註(spike 附帶發現,未採用):meta 另有
// `GET {base}/v2/versions/loader/{game}/{loader}/{installer}/server/jar` 可直接下載 bootstrap
// 啟動 jar、免執行 Java。本實作依 design.md/tasks T4 採「執行官方 installer」路線,與 Forge/NeoForge
// 統一(共用 exec 注入與真 JRE 整合測),不走該捷徑。
const defaultFabricMetaBase = "https://meta.fabricmc.net"

// fabricServerLaunchJar 是 Fabric installer server 模式於目的目錄產出的啟動 jar 檔名(spike 確認)。
const fabricServerLaunchJar = "fabric-server-launch.jar"

// FabricInstaller 安裝 Fabric 伺服器:自 meta API 選定 loader/installer 版本後,執行官方 installer
// jar 的 server 子命令產出可啟動配置(R5)。
type FabricInstaller struct {
	client   *http.Client
	metaBase string
	exec     InstallerExecFunc
}

// NewFabricInstaller 建構 Fabric 安裝器。client 為 nil 用零值 http.Client;metaBase 為空用官方
// 預設;exec 為 nil 用 defaultInstallerExec(測試注入假行程)。
func NewFabricInstaller(client *http.Client, metaBase string, execFn InstallerExecFunc) *FabricInstaller {
	if client == nil {
		client = &http.Client{}
	}
	if metaBase == "" {
		metaBase = defaultFabricMetaBase
	}
	if execFn == nil {
		execFn = defaultInstallerExec
	}
	return &FabricInstaller{client: client, metaBase: metaBase, exec: execFn}
}

var _ ServerInstaller = (*FabricInstaller)(nil)

// fabricLoaderEntry 是 /v2/versions/loader/{mcversion} 陣列元素的最小子集(欄位路徑經 meta API
// 實測確認,2026-07-16)。
type fabricLoaderEntry struct {
	Loader struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	} `json:"loader"`
}

// fabricInstallerEntry 是 /v2/versions/installer 陣列元素的最小子集。
type fabricInstallerEntry struct {
	URL     string `json:"url"`
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

// Install 選定 req.MCVersion 對應的穩定 loader 與穩定 installer,下載 installer jar 並以 server
// 子命令產出 fabric-server-launch.jar 至 TargetDir。
func (f *FabricInstaller) Install(ctx context.Context, req InstallRequest, progress ProgressFunc) (InstalledServer, error) {
	progress.report(ProvisionProgress{Stage: "查詢 Fabric 版本", Percent: -1, Detail: req.MCVersion})

	loaderVer, err := f.stableLoader(ctx, req.MCVersion)
	if err != nil {
		return InstalledServer{}, err
	}
	installerURL, err := f.stableInstallerURL(ctx)
	if err != nil {
		return InstalledServer{}, err
	}

	checksum, err := installerChecksum(ctx, f.client, installerURL)
	if err != nil {
		return InstalledServer{}, err
	}

	params := loaderInstallParams{
		java:          req.JavaExe,
		installerURL:  installerURL,
		checksum:      checksum,
		targetDir:     req.TargetDir,
		downloadStage: "下載 Fabric installer",
		execStage:     fmt.Sprintf("執行 Fabric installer(MC %s / loader %s)", req.MCVersion, loaderVer),
		buildArgs: func(installerPath, stagingDir string) []string {
			return []string{
				"-jar", installerPath,
				"server",
				"-dir", stagingDir,
				"-mcversion", req.MCVersion,
				"-loader", loaderVer,
				"-downloadMinecraft",
			}
		},
		validate: func(stagingDir string) error {
			launch := filepath.Join(stagingDir, fabricServerLaunchJar)
			if _, statErr := os.Stat(launch); statErr != nil {
				return fmt.Errorf("provision: Fabric installer 未產出 %s(MC %s):%v", fabricServerLaunchJar, req.MCVersion, statErr)
			}
			return nil
		},
	}
	if err := runLoaderInstaller(ctx, f.client, f.exec, params, progress); err != nil {
		return InstalledServer{}, err
	}

	return InstalledServer{ServerJar: filepath.Join(req.TargetDir, fabricServerLaunchJar)}, nil
}

// stableLoader 取得 mcVersion 的首個 stable loader 版本;無 stable 時退回清單首元素(最新),
// 皆無則回明確錯誤。
func (f *FabricInstaller) stableLoader(ctx context.Context, mcVersion string) (string, error) {
	u := fmt.Sprintf("%s/v2/versions/loader/%s", strings.TrimRight(f.metaBase, "/"), mcVersion)
	var entries []fabricLoaderEntry
	if err := f.getJSON(ctx, u, &entries); err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("provision: Fabric 無 Minecraft 版本 %q 的 loader", mcVersion)
	}
	for _, e := range entries {
		if e.Loader.Stable && e.Loader.Version != "" {
			return e.Loader.Version, nil
		}
	}
	if entries[0].Loader.Version == "" {
		return "", fmt.Errorf("provision: Fabric loader 清單缺 version 欄位(MC %s)", mcVersion)
	}
	return entries[0].Loader.Version, nil // 無 stable 時取最新。
}

// stableInstallerURL 取得首個 stable installer 的下載 URL;無 stable 時退回清單首元素,皆無則回錯。
func (f *FabricInstaller) stableInstallerURL(ctx context.Context) (string, error) {
	u := fmt.Sprintf("%s/v2/versions/installer", strings.TrimRight(f.metaBase, "/"))
	var entries []fabricInstallerEntry
	if err := f.getJSON(ctx, u, &entries); err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Stable && e.URL != "" {
			return e.URL, nil
		}
	}
	if len(entries) > 0 && entries[0].URL != "" {
		return entries[0].URL, nil
	}
	return "", fmt.Errorf("provision: Fabric 無可用 installer 版本")
}

// getJSON 對 url 發 GET 並解析 JSON 至 out。
func (f *FabricInstaller) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("provision: 建立 Fabric meta 請求失敗: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("provision: 查詢 Fabric meta 失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provision: Fabric meta 回應非預期狀態 %d(%s)", resp.StatusCode, url)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("provision: 解析 Fabric meta 回應失敗: %w", err)
	}
	return nil
}
