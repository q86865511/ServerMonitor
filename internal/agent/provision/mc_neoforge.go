package provision

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultNeoForgeMavenBase 是 NeoForged maven 倉庫的預設 base URL(spike 查證,2026-07-16):
//   - maven-metadata:GET {base}/releases/net/neoforged/neoforge/maven-metadata.xml
//     → versioning/versions/version 清單。NeoForge 版本格式為 <mcMinor>.<mcPatch>.<build>
//     (MC 1.21.1 → 21.1.x;MC 1.21(即 1.21.0)→ 21.0.x);與 Forge 不同,不含 MC 版本前綴。
//   - installer 制品:{base}/releases/net/neoforged/neoforge/<ver>/neoforge-<ver>-installer.jar
//     (旁有 .sha1)。
//   - installer 以 `--installServer <dir>` 產出與 Forge 同形的現代結構:run.bat/run.sh/
//     user_jvm_args.txt + libraries/net/neoforged/neoforge/<ver>/win_args.txt;啟動經 run.bat。
const defaultNeoForgeMavenBase = "https://maven.neoforged.net"

// NeoForgeInstaller 安裝 NeoForge 伺服器:自 maven-metadata 選對應 MC 版本的最新穩定 NeoForge
// 版本後,執行官方 installer 的 --installServer 產出啟動配置(R5)。
type NeoForgeInstaller struct {
	client    *http.Client
	mavenBase string
	exec      InstallerExecFunc
}

// NewNeoForgeInstaller 建構 NeoForge 安裝器。client/exec 為 nil 及 base 為空皆採預設。
func NewNeoForgeInstaller(client *http.Client, mavenBase string, execFn InstallerExecFunc) *NeoForgeInstaller {
	if client == nil {
		client = &http.Client{}
	}
	if mavenBase == "" {
		mavenBase = defaultNeoForgeMavenBase
	}
	if execFn == nil {
		execFn = defaultInstallerExec
	}
	return &NeoForgeInstaller{client: client, mavenBase: mavenBase, exec: execFn}
}

var _ ServerInstaller = (*NeoForgeInstaller)(nil)

// neoForgeMetadata 是 maven-metadata.xml 的最小子集。
type neoForgeMetadata struct {
	Versioning struct {
		Versions struct {
			Version []string `xml:"version"`
		} `xml:"versions"`
	} `xml:"versioning"`
}

// Install 選定 req.MCVersion 對應的最新穩定 NeoForge 版本,下載 installer(.sha1 校驗)並以
// --installServer 產出啟動配置至 TargetDir。
func (n *NeoForgeInstaller) Install(ctx context.Context, req InstallRequest, progress ProgressFunc) (InstalledServer, error) {
	progress.report(ProvisionProgress{Stage: "查詢 NeoForge 版本", Percent: -1, Detail: req.MCVersion})

	neoVer, err := n.resolveVersion(ctx, req.MCVersion)
	if err != nil {
		return InstalledServer{}, err
	}

	installerURL := fmt.Sprintf("%s/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar",
		strings.TrimRight(n.mavenBase, "/"), neoVer, neoVer)

	checksum, err := installerChecksum(ctx, n.client, installerURL)
	if err != nil {
		return InstalledServer{}, err
	}

	argsRel := fmt.Sprintf("libraries/net/neoforged/neoforge/%s/win_args.txt", neoVer)

	params := loaderInstallParams{
		java:          req.JavaExe,
		installerURL:  installerURL,
		checksum:      checksum,
		targetDir:     req.TargetDir,
		downloadStage: fmt.Sprintf("下載 NeoForge %s installer", neoVer),
		execStage:     fmt.Sprintf("執行 NeoForge installer(MC %s / NeoForge %s)", req.MCVersion, neoVer),
		buildArgs: func(installerPath, stagingDir string) []string {
			return []string{"-jar", installerPath, "--installServer", stagingDir}
		},
		validate: func(stagingDir string) error {
			return validateInstallServerOutput(stagingDir, argsRel, "NeoForge")
		},
	}
	if err := runLoaderInstaller(ctx, n.client, n.exec, params, progress); err != nil {
		return InstalledServer{}, err
	}

	return InstalledServer{
		StartScript: filepath.Join(req.TargetDir, installServerRunScript),
		ArgsFile:    filepath.Join(req.TargetDir, installServerJVMArgs),
	}, nil
}

// resolveVersion 由 maven-metadata 挑出 mcVersion 對應前綴下 build 號最高的穩定版本。
// 優先非 beta(版本字串不含 "-");同前綴全為 beta 時退回其中 build 號最高者。
func (n *NeoForgeInstaller) resolveVersion(ctx context.Context, mcVersion string) (string, error) {
	prefix, err := neoForgePrefix(mcVersion)
	if err != nil {
		return "", err
	}

	u := fmt.Sprintf("%s/releases/net/neoforged/neoforge/maven-metadata.xml", strings.TrimRight(n.mavenBase, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("provision: 建立 NeoForge metadata 請求失敗: %w", err)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("provision: 查詢 NeoForge metadata 失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("provision: NeoForge metadata 回應非預期狀態 %d", resp.StatusCode)
	}
	var meta neoForgeMetadata
	if err := xml.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", fmt.Errorf("provision: 解析 NeoForge metadata 失敗: %w", err)
	}

	var bestStable, bestBeta string
	var bestStableBuild, bestBetaBuild = -1, -1
	for _, v := range meta.Versioning.Versions.Version {
		if !strings.HasPrefix(v, prefix) {
			continue
		}
		build, ok := neoForgeBuild(v, prefix)
		if !ok {
			continue
		}
		if strings.Contains(v, "-") { // beta/rc 等預發行。
			if build > bestBetaBuild {
				bestBetaBuild, bestBeta = build, v
			}
			continue
		}
		if build > bestStableBuild {
			bestStableBuild, bestStable = build, v
		}
	}
	if bestStable != "" {
		return bestStable, nil
	}
	if bestBeta != "" {
		return bestBeta, nil
	}
	return "", fmt.Errorf("provision: NeoForge metadata 無 Minecraft 版本 %q(前綴 %q)的可用版本", mcVersion, prefix)
}

// neoForgePrefix 由 Minecraft 版本推得 NeoForge 版本前綴:MC "1.A.B" → "A.B.";MC "1.A" → "A.0."。
// 非 1.x 版本回錯(NeoForge 只服務 MC 1.x)。
func neoForgePrefix(mcVersion string) (string, error) {
	parts := strings.Split(mcVersion, ".")
	if len(parts) < 2 || parts[0] != "1" {
		return "", fmt.Errorf("provision: 無法由 Minecraft 版本 %q 推得 NeoForge 版本前綴", mcVersion)
	}
	minor := parts[1]
	patch := "0"
	if len(parts) >= 3 && parts[2] != "" {
		patch = parts[2]
	}
	return minor + "." + patch + ".", nil
}

// neoForgeBuild 取版本字串在 prefix 之後、下一個 "." 或 "-" 之前的 build 號(整數)。
// 例:prefix "21.1.",v "21.1.238" → 238;v "21.1.240-beta" → 240。
func neoForgeBuild(v, prefix string) (int, bool) {
	rest := strings.TrimPrefix(v, prefix)
	if i := strings.IndexAny(rest, ".-"); i >= 0 {
		rest = rest[:i]
	}
	build, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return build, true
}
