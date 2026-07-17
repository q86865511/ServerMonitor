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
	p         *provision.Provisioner
	cacheRoot string // 共用快取根;CurseForge 被擋模組的手動匯入目錄由此衍生(<cacheRoot>/cf-imports,#3)
}

var _ provisionRunner = (*provisionAdapter)(nil)

// cfImportSubdir 是 CurseForge 被擋模組(downloadUrl=null)的固定手動匯入子目錄(相對共用快取根)。
// 使用者把手動下載的被擋模組放此,重試安裝時 CurseForgeProvider 依檔名(＋長度)自動補齊(#3/R14)。
const cfImportSubdir = "cf-imports"

// NewProvisionAdapter 以共用快取根建立 provision adapter;opts 透傳給 provision.New(供測試注入
// httptest client/API base)。
func NewProvisionAdapter(cacheRoot string, opts ...provision.Option) *provisionAdapter {
	return &provisionAdapter{p: provision.New(cacheRoot, opts...), cacheRoot: cacheRoot}
}

// EnsureJava 委派 JavaProvisioner 供應指定 major 版 JRE,回傳 java 執行檔絕對路徑(R4)。
func (a *provisionAdapter) EnsureJava(ctx context.Context, major int, progress func(protocol.ProvisionProgress)) (string, error) {
	return a.p.Java.Ensure(ctx, major, wrapProgress(progress))
}

// InstallServer 依 loader 委派 Minecraft 伺服器安裝至實例目錄,回傳啟動來源產物(R5/R7)。
// MCVersion 優先取 payload 值(ServerInstallRequest.MCVersion,由 T9 buildSpec 經 native.go 傳入);
// 缺值時才由 Variant 依 "<loader>-<version>" 慣例導出(見 deriveMCVersion)作為 fallback。
// Forge/NeoForge 的啟動腳本/args 檔(InstalledServer.StartScript/ArgsFile)一併透出,供 native.go
// 持久化進 native.json 並以 {start_script} token 展開啟動命令。
func (a *provisionAdapter) InstallServer(ctx context.Context, req ServerInstallRequest, progress func(protocol.ProvisionProgress)) (ServerInstallResult, error) {
	installed, err := a.p.InstallServerByLoader(ctx, req.Loader, provision.InstallRequest{
		MCVersion: mcVersionOrDerive(req),
		TargetDir: req.InstanceDir,
		JavaExe:   req.JavaPath,
	}, wrapProgress(progress))
	if err != nil {
		return ServerInstallResult{}, err
	}
	return ServerInstallResult{
		ServerJar:   installed.ServerJar,
		StartScript: installed.StartScript,
		ArgsFile:    installed.ArgsFile,
	}, nil
}

// mcVersionOrDerive 取 InstallServer 所需的 Minecraft 版本:優先 payload 明確值,缺值退回 Variant
// 慣例導出(deriveMCVersion)。
func mcVersionOrDerive(req ServerInstallRequest) string {
	if v := strings.TrimSpace(req.MCVersion); v != "" {
		return v
	}
	return deriveMCVersion(req.Variant, req.Loader)
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

// InstallModpack 依 req.Type 分派模組包安裝(native-backend R11/R14)。req.ArchivePath 非空時安裝
// 既有本機封存檔(手動上傳路徑,略過遠端解析);否則以 req.Ref 委由對應 ModProvider 解析遠端來源。
// "curseforge" 僅在建置內嵌或設定覆蓋了 API key 時才註冊 ModProvider(R14);未啟用時回明確的
// 「未啟用」錯誤——刻意不落入下方 map 查無的通用訊息,使呼叫端(GUI/使用者)能立即理解原因。
func (a *provisionAdapter) InstallModpack(ctx context.Context, req ModpackInstallRequest, progress func(protocol.ProvisionProgress)) error {
	provider, ok := a.p.ModProvider(req.Type)
	if !ok {
		if req.Type == "curseforge" {
			return fmt.Errorf("agent: CurseForge 模組包未啟用(建置未內嵌 API key 且設定未覆蓋;native-backend R14)")
		}
		return fmt.Errorf("agent: 不支援的模組包來源 %q", req.Type)
	}
	var ref *provision.ModpackRef
	if req.ArchivePath == "" {
		ref = &provision.ModpackRef{Type: req.Type, Ref: req.Ref}
	}
	// #3:CurseForge 被擋模組的手動匯入目錄接線——過去 ImportDir 恆空,CurseForgeProvider 永遠無從自動
	// 補齊,使用者手動下載後重試仍拿到同一批 BlockedModsError。此處填固定目錄 <cacheRoot>/cf-imports
	// (預先建好,不存在則自動建),使重試能匹配該目錄內手動下載的檔案。僅 curseforge 路徑需要。
	importDir := a.curseForgeImportDir(req.Type)
	return provider.InstallModpack(ctx, provision.ModpackInstallRequest{
		Ref:         ref,
		ArchivePath: req.ArchivePath,
		TargetDir:   req.TargetDir,
		MCVersion:   req.MCVersion,
		Loader:      req.Loader,
		ImportDir:   importDir,
	}, wrapProgress(progress))
}

// curseForgeImportDir 回傳 CurseForge 被擋模組的固定手動匯入目錄(<cacheRoot>/cf-imports),並確保其存在。
// 非 curseforge 型別或 cacheRoot 為空時回空字串(不匯入)。建立失敗僅記為回空字串降級(不阻斷安裝——
// 匯入本為選用的降級補齊路徑,失敗最多退回 BlockedModsError 引導手動下載)。
func (a *provisionAdapter) curseForgeImportDir(modpackType string) string {
	if modpackType != "curseforge" || a.cacheRoot == "" {
		return ""
	}
	dir := filepath.Join(a.cacheRoot, cfImportSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// CurseForgeEnabled 回報供應器是否啟用 CurseForge 模組包(native-backend R14:建置內嵌或設定覆蓋
// 了 API key)。NativeBackend 以 curseForgeCapable 可選介面查詢之,一路透出至 GUI 綁定。
func (a *provisionAdapter) CurseForgeEnabled() bool {
	return a.p.CurseForgeEnabled()
}

// SetCurseForgeKey 委派供應器熱替換使用者覆蓋的 CurseForge API 金鑰(GUI 設定;native-backend R14)。
// 空字串=清除覆蓋、回退內嵌金鑰。NativeBackend 以 curseForgeConfigurable 可選介面查詢之。
func (a *provisionAdapter) SetCurseForgeKey(key string) {
	a.p.SetCurseForgeKey(key)
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
