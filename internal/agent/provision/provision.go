// Package provision 負責 native 後端(免 Docker)的執行環境供應:JRE、Minecraft 伺服器
// 檔案、SteamCMD、模組包等。Docker 模式下這些由映像內建,native 模式必須自行下載、校驗、
// 落地。本套件只做「取得位元組並安全落地」,不碰 Job Object / PID(那是 supervisor 的職責),
// 也刻意不依賴 agent 內部型別——進度以自定義 ProgressFunc 回呼傳出,便於 httptest 獨立單元測。
//
// 對應規格:specs/native-backend R4(本檔與 java.go 實作 JRE 供應);R5/R6/R11 的安裝器
// 於後續任務(T4~T6、T11)加入,New 已預留組合位。
package provision

import "net/http"

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

	// TODO(T5/R5):ServerInstaller 五實作(Vanilla/Paper/Fabric/Forge/NeoForge),依 Variant.Loader 選取。
	// TODO(T6/R6):SteamCMDProvisioner——Valve 匿名載點下載 SteamCMD、app_update 安裝 Palworld。
	// TODO(T11/R11、R14):ModProvider——Modrinth(首發)/CurseForge(R14)模組包解析與落位。
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

// New 建構 Provisioner。cacheRoot 為共用快取根;opts 依序套用後再建構各子供應器。
func New(cacheRoot string, opts ...Option) *Provisioner {
	p := &Provisioner{
		CacheRoot:   cacheRoot,
		client:      &http.Client{}, // 無整體逾時:大型下載(JRE/SteamCMD)由 context 控制生命週期。
		javaAPIBase: defaultAdoptiumBase,
	}
	for _, o := range opts {
		o(p)
	}
	p.Java = newJavaProvisioner(cacheRoot, p.client, p.javaAPIBase)
	return p
}
