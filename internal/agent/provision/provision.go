// Package provision 負責 native 後端(免 Docker)的執行環境供應:JRE、Minecraft 伺服器
// 檔案、SteamCMD、模組包等。Docker 模式下這些由映像內建,native 模式必須自行下載、校驗、
// 落地。本套件只做「取得位元組並安全落地」,不碰 Job Object / PID(那是 supervisor 的職責),
// 也刻意不依賴 agent 內部型別——進度以自定義 ProgressFunc 回呼傳出,便於 httptest 獨立單元測。
//
// 對應規格:specs/native-backend R4(本檔與 java.go 實作 JRE 供應);R5/R6 安裝器見
// mc_*.go/steamcmd.go;R11 模組包安裝器見 modprovider.go/modrinth.go(T10)。
package provision

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// ProvisionProgress 是一次供應步驟的進度快照。刻意與 agent 的事件型別解耦,由呼叫端
// (NativeBackend)轉譯為 protocol.RuntimeEvent 後 emit 至 eventHub。
type ProvisionProgress struct {
	// Stage 是當前階段的人類可讀描述(如「下載 Java 21」「解壓」)。
	Stage string
	// Percent 是完成百分比 0~100;無法得知總量時為 -1(不確定)。
	Percent int
	// Detail 是可選的附加細節(如已下載位元組數)。
	Detail string
}

// ProgressFunc 是供應方法接收的進度回呼。各供應方法的 progress 參數皆可為 nil(不回報)。
type ProgressFunc func(ProvisionProgress)

// report 安全地回報一則進度;p 為 nil 時為 no-op。以值接收器定義,對 nil func 值呼叫亦安全。
func (p ProgressFunc) report(pp ProvisionProgress) {
	if p != nil {
		p(pp)
	}
}

// defaultAdoptiumBase 是 Adoptium API v3 的預設 base URL(測試以 WithJavaAPIBase 注入 httptest)。
const defaultAdoptiumBase = "https://api.adoptium.net"

// Provisioner 聚合各類供應器,持共用的快取根與 http client。建構後由 NativeBackend 在
// Create 期間依範本 [native].Provision 呼叫對應供應器。
type Provisioner struct {
	// CacheRoot 是共用快取根目錄(JRE、SteamCMD 等跨實例共用,不隨實例重複)。
	CacheRoot string
	// client 是所有下載/API 呼叫共用的 http client;預設不設整體逾時(供應可能長時間,
	// 逾時交由 context 控制,見 design.md 風險節「長時供應 vs HTTP 逾時」)。
	client *http.Client
	// javaAPIBase 是 Adoptium API base URL,經 New 選項決定後傳入 JavaProvisioner。
	javaAPIBase string

	// Java 供應 Adoptium JRE(R4)。
	Java *JavaProvisioner

	// Minecraft 五 loader 安裝器(R5),依 loader 名經 InstallServerByLoader 選取。以 ServerInstaller
	// 介面型別持有,便於測試以假實作替換(路由測試)與 T8 agent adapter 接線。
	Vanilla  ServerInstaller
	Paper    ServerInstaller
	Fabric   ServerInstaller
	Forge    ServerInstaller
	NeoForge ServerInstaller

	// SteamCMD 供應 SteamCMD 並安裝 Steam 專用伺服器(R6,如 Palworld)。
	SteamCMD *SteamCMDProvisioner

	// ModProviders 依模組包來源型別("modrinth"｜"curseforge")分派模組包解析與安裝(R11/R14)。
	// "modrinth" 於 New 註冊首發實作;"curseforge" 留空位(T14,需 CF API key 才啟用,見
	// requirements.md R14),呼叫端(provisionAdapter)對此型別回明確的「尚未支援」錯誤。
	ModProviders map[string]ModProvider

	// modrinthAPIBase 是 Modrinth API v2 base URL,經 WithModrinthAPIBase 選項決定後傳入
	// ModrinthProvider(測試以 httptest server URL 注入)。
	modrinthAPIBase string

	// curseforgeAPIBase 是 CurseForge API base URL,經 WithCurseForgeAPIBase 選項決定後傳入
	// CurseForgeProvider(測試以 httptest server URL 注入)。
	curseforgeAPIBase string
	// curseforgeKey 是有效的 CurseForge API 金鑰:預設取編譯期內嵌值(curseforge.go 的
	// curseforgeAPIKey),經 WithCurseForgeAPIKey 以使用者設定覆蓋(非空才覆蓋)。空=CF 模組包停用。
	curseforgeKey string
}

// Option 以函式選項調整 Provisioner 建構參數。
type Option func(*Provisioner)

// WithHTTPClient 注入自訂 http client(測試以 httptest server 的 client 注入)。
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provisioner) {
		if c != nil {
			p.client = c
		}
	}
}

// WithJavaAPIBase 覆寫 Adoptium API base URL(測試以 httptest server URL 注入)。
func WithJavaAPIBase(base string) Option {
	return func(p *Provisioner) {
		if base != "" {
			p.javaAPIBase = base
		}
	}
}

// WithModrinthAPIBase 覆寫 Modrinth API base URL(測試以 httptest server URL 注入)。
func WithModrinthAPIBase(base string) Option {
	return func(p *Provisioner) {
		if base != "" {
			p.modrinthAPIBase = base
		}
	}
}

// WithCurseForgeAPIBase 覆寫 CurseForge API base URL(測試以 httptest server URL 注入)。
func WithCurseForgeAPIBase(base string) Option {
	return func(p *Provisioner) {
		if base != "" {
			p.curseforgeAPIBase = base
		}
	}
}

// WithCurseForgeAPIKey 以使用者設定覆蓋內嵌的 CurseForge API 金鑰(native-backend R14:比照 Prism,
// 使用者可在設定填自己的 key 覆蓋專案內嵌 key)。key 為空時不覆蓋(維持內嵌值,可能仍為空=停用)。
func WithCurseForgeAPIKey(key string) Option {
	return func(p *Provisioner) {
		if key != "" {
			p.curseforgeKey = key
		}
	}
}

// New 建構 Provisioner。cacheRoot 為共用快取根;opts 依序套用後再建構各子供應器。
func New(cacheRoot string, opts ...Option) *Provisioner {
	p := &Provisioner{
		CacheRoot:     cacheRoot,
		client:        &http.Client{}, // 無整體逾時:大型下載(JRE/SteamCMD)由 context 控制生命週期。
		javaAPIBase:   defaultAdoptiumBase,
		curseforgeKey: curseforgeAPIKey, // 內嵌值(ldflags);WithCurseForgeAPIKey 選項可覆蓋。
	}
	for _, o := range opts {
		o(p)
	}
	p.Java = newJavaProvisioner(cacheRoot, p.client, p.javaAPIBase)

	// 五安裝器共用 Provisioner 的 http client(無整體逾時,長時供應由 context 控制);
	// 各 base URL/exec 採官方預設(生產路徑),測試以直接建構或替換欄位注入。
	p.Vanilla = NewVanillaInstaller(p.client, "")
	p.Paper = NewPaperInstaller(p.client, "")
	p.Fabric = NewFabricInstaller(p.client, "", nil)
	p.Forge = NewForgeInstaller(p.client, "", "", nil)
	p.NeoForge = NewNeoForgeInstaller(p.client, "", nil)

	// SteamCMD 共用同一 client;快取根與 Provisioner 一致。
	p.SteamCMD = NewSteamCMDProvisioner(cacheRoot, WithSteamCMDHTTPClient(p.client))

	// 模組包提供者:"modrinth" 首發(R11);"curseforge" 僅在有有效 key 時註冊(R14)——無 key 時
	// 不註冊,adapter 遂對該型別回明確的「未啟用」錯誤,且 GUI 據 CurseForgeEnabled 隱藏該選項。
	p.ModProviders = map[string]ModProvider{
		"modrinth": NewModrinthProvider(p.client, p.modrinthAPIBase),
	}
	if p.CurseForgeEnabled() {
		p.ModProviders["curseforge"] = NewCurseForgeProvider(p.client, p.curseforgeAPIBase, p.curseforgeKey)
	}

	return p
}

// CurseForgeEnabled 回報 CurseForge 模組包功能是否啟用:有內嵌或設定覆蓋的 API key 即啟用
// (native-backend R14)。供 agent/app 一路透出至 GUI,決定是否顯示 native CurseForge 選項。
func (p *Provisioner) CurseForgeEnabled() bool {
	return strings.TrimSpace(p.curseforgeKey) != ""
}

// InstallServerByLoader 依 loader 名(vanilla|paper|fabric|forge|neoforge,大小寫不敏感)分派至
// 對應安裝器並安裝伺服器至 req.TargetDir。這是 agent 端 provisionRunner adapter(T8)呼叫供應的
// 統一入口——新增 loader 只需在此與 New 各加一列。未知 loader 回明確錯誤(不 panic、不預設猜測)。
func (p *Provisioner) InstallServerByLoader(ctx context.Context, loader string, req InstallRequest, progress ProgressFunc) (InstalledServer, error) {
	inst := p.installerFor(loader)
	if inst == nil {
		return InstalledServer{}, fmt.Errorf("provision: 不支援的 loader %q(支援:vanilla/paper/fabric/forge/neoforge)", loader)
	}
	return inst.Install(ctx, req, progress)
}

// installerFor 回傳 loader 名對應的安裝器;未知 loader 回 nil。
func (p *Provisioner) installerFor(loader string) ServerInstaller {
	switch strings.ToLower(strings.TrimSpace(loader)) {
	case "vanilla":
		return p.Vanilla
	case "paper":
		return p.Paper
	case "fabric":
		return p.Fabric
	case "forge":
		return p.Forge
	case "neoforge":
		return p.NeoForge
	default:
		return nil
	}
}
