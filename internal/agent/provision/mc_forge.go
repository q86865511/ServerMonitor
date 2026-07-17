package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

// Forge 供應的兩個官方端點(spike 查證,2026-07-16):
//   - promotions:GET {promoBase}/net/minecraftforge/forge/promotions_slim.json
//     → {homepage, promos:{"<mc>-recommended":"<forgever>","<mc>-latest":"<forgever>",...}};
//     優先 recommended,無則退回 latest。
//   - installer 制品:{mavenBase}/net/minecraftforge/forge/<mc>-<forgever>/forge-<mc>-<forgever>-installer.jar
//     (旁有 .sha1 供校驗)。
//   - installer 以 `--installServer <dir>` 產出現代結構(MC 1.17+):run.bat/run.sh/user_jvm_args.txt
//   - libraries/net/minecraftforge/forge/<mc>-<forgever>/win_args.txt(unix_args.txt);無單一
//     forge server jar,啟動經 run.bat。MC 基線 1.21.x 只支援此結構,舊版 forge-universal 不支援。
const (
	defaultForgePromotionsBase = "https://files.minecraftforge.net"
	defaultForgeMavenBase      = "https://maven.minecraftforge.net"
)

// ForgeInstaller 安裝 Minecraft Forge 伺服器:解析 promotions 取推薦 Forge 版本後,執行官方
// installer 的 --installServer 產出啟動配置(R5)。
type ForgeInstaller struct {
	client    *http.Client
	promoBase string
	mavenBase string
	exec      InstallerExecFunc
}

// NewForgeInstaller 建構 Forge 安裝器。client/exec 為 nil 及 base 為空皆採預設(測試注入
// httptest URL 與假行程)。promoBase 與 mavenBase 分屬不同官方主機,測試時可指向同一 httptest。
func NewForgeInstaller(client *http.Client, promoBase, mavenBase string, execFn InstallerExecFunc) *ForgeInstaller {
	if client == nil {
		client = &http.Client{}
	}
	if promoBase == "" {
		promoBase = defaultForgePromotionsBase
	}
	if mavenBase == "" {
		mavenBase = defaultForgeMavenBase
	}
	if execFn == nil {
		execFn = defaultInstallerExec
	}
	return &ForgeInstaller{client: client, promoBase: promoBase, mavenBase: mavenBase, exec: execFn}
}

var _ ServerInstaller = (*ForgeInstaller)(nil)

// forgePromotions 是 promotions_slim.json 的最小子集。
type forgePromotions struct {
	Promos map[string]string `json:"promos"`
}

// Install 解析 req.MCVersion 的推薦 Forge 版本,下載 installer(.sha1 校驗)並以 --installServer
// 產出啟動配置至 TargetDir。
func (f *ForgeInstaller) Install(ctx context.Context, req InstallRequest, progress ProgressFunc) (InstalledServer, error) {
	progress.report(ProvisionProgress{Stage: "查詢 Forge 版本", Percent: -1, Detail: req.MCVersion})

	forgeVer, err := f.resolveVersion(ctx, req.MCVersion)
	if err != nil {
		return InstalledServer{}, err
	}

	// installer 制品座標:<mc>-<forgever>。
	coord := req.MCVersion + "-" + forgeVer
	installerURL := fmt.Sprintf("%s/net/minecraftforge/forge/%s/forge-%s-installer.jar",
		strings.TrimRight(f.mavenBase, "/"), coord, coord)

	checksum, err := installerChecksum(ctx, f.client, installerURL)
	if err != nil {
		return InstalledServer{}, err
	}

	// 現代結構的 libraries 啟動參數檔相對路徑(以 / 分隔,validate 內轉平台分隔)。
	argsRel := fmt.Sprintf("libraries/net/minecraftforge/forge/%s/win_args.txt", coord)

	params := loaderInstallParams{
		java:          req.JavaExe,
		installerURL:  installerURL,
		checksum:      checksum,
		targetDir:     req.TargetDir,
		downloadStage: fmt.Sprintf("下載 Forge %s installer", forgeVer),
		execStage:     fmt.Sprintf("執行 Forge installer(MC %s / Forge %s)", req.MCVersion, forgeVer),
		buildArgs: func(installerPath, stagingDir string) []string {
			return []string{"-jar", installerPath, "--installServer", stagingDir}
		},
		validate: func(stagingDir string) error {
			return validateInstallServerOutput(stagingDir, argsRel, "Forge")
		},
	}
	if err := runLoaderInstaller(ctx, f.client, f.exec, params, progress); err != nil {
		return InstalledServer{}, err
	}

	return InstalledServer{
		StartScript: filepath.Join(req.TargetDir, installServerRunScript),
		ArgsFile:    filepath.Join(req.TargetDir, installServerJVMArgs),
	}, nil
}

// resolveVersion 由 promotions 取 req.MCVersion 的推薦 Forge 版本(優先 -recommended,退回 -latest)。
func (f *ForgeInstaller) resolveVersion(ctx context.Context, mcVersion string) (string, error) {
	u := fmt.Sprintf("%s/net/minecraftforge/forge/promotions_slim.json", strings.TrimRight(f.promoBase, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("provision: 建立 Forge promotions 請求失敗: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("provision: 查詢 Forge promotions 失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("provision: Forge promotions 回應非預期狀態 %d", resp.StatusCode)
	}
	var promos forgePromotions
	if err := json.NewDecoder(resp.Body).Decode(&promos); err != nil {
		return "", fmt.Errorf("provision: 解析 Forge promotions 失敗: %w", err)
	}

	if v := promos.Promos[mcVersion+"-recommended"]; v != "" {
		return v, nil
	}
	if v := promos.Promos[mcVersion+"-latest"]; v != "" {
		return v, nil
	}
	return "", fmt.Errorf("provision: Forge promotions 無 Minecraft 版本 %q 的推薦或最新版本", mcVersion)
}
