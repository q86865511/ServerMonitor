package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"servermonitor/internal/agent/provision"
	"servermonitor/internal/protocol"
)

// provisionAdapter 把 internal/agent/provision 子套件的 Provisioner 接到 NativeBackend 的窄
// provisionRunner 介面上(native-backend R4/R5/R6)。NativeBackend 刻意只依賴窄介面以便獨立單元測;
// 本 adapter 是生產路徑上真正委派供應工作的一層,並負責 provision 與 agent 兩側進度型別的轉譯
// (provision.ProvisionProgress ↔ protocol.ProvisionProgress)。
//
// 與 provision 子套件的接縫:EnsureJava→Java.Ensure、InstallServer→InstallServerByLoader、
// InstallSteamApp→SteamCMD.InstallApp 皆已接上真實入口(T4/T5/T6 已合流)。WriteEula 為純檔案系統
// 操作,就地實作。唯一殘留 seam 見 InstallServer:native 側 ServerInstallRequest 帶 Variant 而非
// bare MCVersion,由本 adapter 依 "<loader>-<version>" 慣例橋接(最終由 T9 buildSpec 定案)。
type provisionAdapter struct {
	p *provision.Provisioner
}

var _ provisionRunner = (*provisionAdapter)(nil)

// NewProvisionAdapter 以共用快取根建立 provision adapter;opts 透傳給 provision.New(供測試注入
// httptest client/API base)。
func NewProvisionAdapter(cacheRoot string, opts ...provision.Option) *provisionAdapter {
	return &provisionAdapter{p: provision.New(cacheRoot, opts...)}
}

// EnsureJava 委派 JavaProvisioner 供應指定 major 版 JRE,回傳 java 執行檔絕對路徑(R4)。
func (a *provisionAdapter) EnsureJava(ctx context.Context, major int, progress func(protocol.ProvisionProgress)) (string, error) {
	return a.p.Java.Ensure(ctx, major, wrapProgress(progress))
}

// InstallServer 依 loader 委派 Minecraft 伺服器安裝至實例目錄,回傳啟動用 server jar 路徑(R5)。
// MCVersion 由 native 側的 Variant 依 "<loader>-<version>" 慣例導出(見 deriveMCVersion)。
// Forge/NeoForge 的啟動腳本/args 檔(InstalledServer.StartScript/ArgsFile)不經此窄回傳值透出——
// 屬 native.go 啟動來源的後續接線(seam),此處僅回 ServerJar。
func (a *provisionAdapter) InstallServer(ctx context.Context, req ServerInstallRequest, progress func(protocol.ProvisionProgress)) (string, error) {
	installed, err := a.p.InstallServerByLoader(ctx, req.Loader, provision.InstallRequest{
		MCVersion: deriveMCVersion(req.Variant, req.Loader),
		TargetDir: req.InstanceDir,
		JavaExe:   req.JavaPath,
	}, wrapProgress(progress))
	if err != nil {
		return "", err
	}
	return installed.ServerJar, nil
}

// InstallSteamApp 委派 SteamCMD 供應並安裝/更新指定 Steam App 至實例目錄(R6)。
func (a *provisionAdapter) InstallSteamApp(ctx context.Context, appID, instanceDir string, progress func(protocol.ProvisionProgress)) error {
	return a.p.SteamCMD.InstallApp(ctx, appID, instanceDir, wrapProgress(progress))
}

// WriteEula 於實例目錄寫入 eula.txt(native 無 itzg 代勞;R5)。accepted=false 時為 no-op。
// eula 落檔是純檔案系統操作、不需下載器,故就地實作(非 provision 子套件職責)。
func (a *provisionAdapter) WriteEula(ctx context.Context, instanceDir string, accepted bool) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if !accepted {
		return nil
	}
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		return fmt.Errorf("建立實例目錄失敗: %w", err)
	}
	path := filepath.Join(instanceDir, "eula.txt")
	if err := os.WriteFile(path, []byte("eula=true\n"), 0o644); err != nil {
		return fmt.Errorf("寫入 eula.txt 失敗: %w", err)
	}
	return nil
}

// deriveMCVersion 由 native 側的 Variant 導出 provision 需要的 Minecraft 版本號。native 的 Variant
// 慣例為 "<loader>-<version>"(如 "paper-1.21");去除 loader 前綴後即版本號。無前綴可去時原樣回傳
// (讓 provision 端以明確錯誤反映版本缺失,而非在此臆測)。
func deriveMCVersion(variant, loader string) string {
	variant = strings.TrimSpace(variant)
	loader = strings.TrimSpace(loader)
	if loader != "" {
		if v := strings.TrimPrefix(variant, loader+"-"); v != variant {
			return v
		}
	}
	return variant
}

// wrapProgress 把 NativeBackend 傳入的 protocol 進度回呼轉為 provision 子套件的 ProgressFunc;
// provision 的 Percent 以 -1 表未知,轉為 protocol 的 0(不確定)。progress 為 nil 時回 nil(不回報)。
func wrapProgress(progress func(protocol.ProvisionProgress)) provision.ProgressFunc {
	if progress == nil {
		return nil
	}
	return func(pp provision.ProvisionProgress) {
		percent := float64(pp.Percent)
		if pp.Percent < 0 {
			percent = 0
		}
		progress(protocol.ProvisionProgress{Stage: pp.Stage, Percent: percent, Detail: pp.Detail})
	}
}
