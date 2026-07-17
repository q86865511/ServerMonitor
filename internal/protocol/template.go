package protocol

import (
	"fmt"

	"github.com/pelletier/go-toml/v2"
)

// GameTemplate 是版本化的宣告式遊戲範本(R1)。新增「沿用既有 adapter」的遊戲
// 主要是新增一份 templates/*.toml,而非改核心。欄位對齊 design「遊戲範本 schema」。
//
// 可選頂層區段(Docker/Health/PlayersQuery/Mods)以指標表示,方便日後範本引擎
// (T7)區分「未提供」與「零值」。範本合法性驗證(缺欄位/版本/ID/adapter 不存在→
// 拒載並記 TEMPLATE_LOAD_FAILED)不在本套件,屬 core 範本引擎職責。
type GameTemplate struct {
	SchemaVersion    int               `toml:"schema_version"`
	ID               string            `toml:"id"`
	Name             string            `toml:"name"`
	Runtime          string            `toml:"runtime"` // 預設 runtime:"docker" | "native"(能力另由 [docker]/[native] 區段存在推導)
	DataDirs         []string          `toml:"data_dirs"`
	Docker           *DockerImage      `toml:"docker"`
	Variants         []Variant         `toml:"variants"`
	Ports            []PortSpec        `toml:"ports"`
	Params           []ParamSpec       `toml:"params"`
	Secrets          []SecretSpec      `toml:"secrets"`
	CommandProtocols []CommandProtocol `toml:"command_protocols"`
	Hooks            Hooks             `toml:"hooks"`
	Health           *HealthProbe      `toml:"health"`
	PlayersQuery     *PlayersQuery     `toml:"players_query"`
	Mods             *ModsSpec         `toml:"mods"`
	Native           *NativeSpec       `toml:"native"` // native-backend R3:本機行程模式的供應/啟動/設定映射
}

// SupportsDocker 回報範本是否宣告 docker 執行能力(能力由 [docker] 區段存在推導;native-backend R3)。
func (t *GameTemplate) SupportsDocker() bool { return t.Docker != nil }

// SupportsNative 回報範本是否宣告 native 執行能力(能力由 [native] 區段存在推導;native-backend R3)。
func (t *GameTemplate) SupportsNative() bool { return t.Native != nil }

// DockerImage 對應 [docker]:鎖定映像來源(R11 鎖 tag 或 digest)。
type DockerImage struct {
	Image       string `toml:"image"`
	ImageDigest string `toml:"image_digest"`
}

// NativeSpec 對應 [native]:免 Docker 本機行程模式的宣告(native-backend R3)。
// 供應(JRE/SteamCMD)、啟動命令模板、params→遊戲設定檔映射皆在此宣告;實際供應與啟動
// 由 agent 側 NativeBackend 執行(T2 以降)。
type NativeSpec struct {
	Provision ProvisionSpec   `toml:"provision"`
	Launch    LaunchSpec      `toml:"launch"`
	Config    []ConfigMapping `toml:"config"`
	Mods      *NativeModsSpec `toml:"mods"` // native 模組落位目錄等(可選)
}

// ProvisionSpec 對應 [native.provision]:本機執行環境/伺服器檔案的供應方式(native-backend R4/R5/R6)。
type ProvisionSpec struct {
	Kind          string `toml:"kind"`            // "java" | "steamcmd"
	JavaMajor     int    `toml:"java_major"`      // kind=java 時所需 Java major 版(如 21)
	SteamAppID    string `toml:"steam_app_id"`    // kind=steamcmd 時的 Steam App ID(如 "2394010")
	UpdateOnStart bool   `toml:"update_on_start"` // R6:啟動前重跑 app_update 檢查更新
}

// LaunchSpec 對應 [native.launch]:啟動命令模板(native-backend R7)。
// Command 為 argv 模板,支援 token:{java} {server_jar} {memory_mb} {instance_dir} {port:<name>};
// 由 NativeBackend 於啟動時展開為實際參數。
type LaunchSpec struct {
	Command    []string `toml:"command"`     // argv 模板
	WorkingDir string   `toml:"working_dir"` // 相對 instance 資料根;空=資料根
}

// ConfigMapping 對應 [[native.config]]:一個 params→遊戲設定檔的映射(native-backend R3)。
// Format 決定編碼器:"properties"(k=v 逐行)與 "palworld-ini"(單行 OptionSettings=(K=V,...) 打包)。
type ConfigMapping struct {
	File    string            `toml:"file"`    // 設定檔名(如 "server.properties" | "PalWorldSettings.ini")
	Format  string            `toml:"format"`  // "properties" | "palworld-ini"
	Section string            `toml:"section"` // palworld-ini 用(ini section 名)
	Map     map[string]string `toml:"map"`     // paramKey(索引 Env) -> configKey
	// Set 是不經 param 的固定/衍生設定值(configKey -> 字面值或 {port:<name>} token),由
	// NativeBackend 於寫檔時展開埠 token。用於「非使用者參數但 native 執行需要」的設定:如
	// Minecraft server.properties 的 enable-rcon/rcon.port/server-port(docker 由 itzg 代勞注入,
	// native 無 itzg 故於此宣告)。configKey 與 Map 的產出鍵相同時,Set 優先(後寫覆蓋)。
	Set map[string]string `toml:"set"`
}

// NativeModsSpec 對應 [native.mods]:native 模式下模組/模組包的落位設定(native-backend R11)。
// 首版僅最小欄位(落位目錄);完整 native 模組安裝(Modrinth 解析、依賴下載)於 T10 擴充。
type NativeModsSpec struct {
	ModsDir string `toml:"mods_dir"` // 模組落位目錄(相對 instance 資料根),如 "mods"
}

// Variant 對應 [[variants]]:一種可選的伺服器變體(如 Paper/Forge),env 於建立時透傳。
// Loader 宣告此變體的 loader 家族(如 paper/vanilla/forge/fabric/quilt/neoforge),供 R11
// 模組包前置相容檢查:模組包只能套用在 [mods].modpack_loaders 允許的 loader 上(T14 增量)。
type Variant struct {
	ID     string            `toml:"id"`
	Loader string            `toml:"loader"`
	Env    map[string]string `toml:"env"`
}

// PortSpec 對應 [[ports]]:埠宣告,含 host binding。
// 衝突鍵為 (bind_ip, protocol, host_port);host_port 可為 0 表示動態分配(R2)。
type PortSpec struct {
	Name      string `toml:"name"`
	Container int    `toml:"container"`
	HostPort  int    `toml:"host_port"` // 0 = 動態
	BindIP    string `toml:"bind_ip"`   // wildcard(0.0.0.0)與具體 IP 視為重疊
	Protocol  string `toml:"protocol"`  // tcp | udp
	Required  bool   `toml:"required"`
}

// ParamSpec 對應 [[params]]:使用者可填參數的宣告。
// Default 型別依 Type 而定(string/bool/int...),故以 any 承接 TOML 原生值。
type ParamSpec struct {
	Key      string `toml:"key"`
	Label    string `toml:"label"`
	Type     string `toml:"type"` // string | bool | int | ...
	Default  any    `toml:"default"`
	Required bool   `toml:"required"`
}

// SecretSpec 對應 [[secrets]]:敏感輸入的宣告。輸入即入金鑰庫、不進 params_json(R12)。
type SecretSpec struct {
	Key   string `toml:"key"`
	Label string `toml:"label"`
}

// CommandProtocol 對應 [[command_protocols]],是 kind 判別的 tagged union(R7)。
// 以「欄位超集 + kind 判別」建模:kind="rcon" 用 HostPortRef/PasswordRef(raw console);
// kind="rest" 用 Auth/Actions(具名動作)。Legacy 供 Palworld RCON 標記(首版不啟用)。
type CommandProtocol struct {
	ProtocolID string `toml:"protocol_id"`
	Kind       string `toml:"kind"` // "rcon" | "rest"

	// rcon 欄位
	HostPortRef string `toml:"host_port_ref"` // 指向 [[ports]].name
	PasswordRef string `toml:"password_ref"`  // 指向 [[secrets]].key
	Legacy      bool   `toml:"legacy"`        // Palworld RCON legacy:首版不啟用

	// rest 欄位
	Auth     string          `toml:"auth"`     // 如 "basic"
	Username string          `toml:"username"` // basic auth 帳號;可選,未填時 core 回退 "admin"(相容 Palworld 慣例)
	Actions  []CommandAction `toml:"actions"`
}

// CommandAction 對應 rest 協定的 [[command_protocols.actions]]:一個具名 REST 動作(R7)。
// 憑證/埠參照可於動作層覆寫協定層設定。
type CommandAction struct {
	ActionID    string `toml:"action_id"`
	Method      string `toml:"method"` // GET | POST | ...
	Path        string `toml:"path"`   // 如 /v1/api/players
	Input       string `toml:"input"`  // typed input(型別或 schema 名)
	Output      string `toml:"output"` // typed output
	HostPortRef string `toml:"host_port_ref"`
	PasswordRef string `toml:"password_ref"`
	Auth        string `toml:"auth"`
}

// Hooks 對應 [hooks]:生命週期指令映射(R3/R8/R9)。核心不需硬編遊戲語意。
type Hooks struct {
	Stop     *Hook `toml:"stop"`
	Announce *Hook `toml:"announce"`
	Quiesce  *Hook `toml:"quiesce"`
	Resume   *Hook `toml:"resume"`
}

// Hook 以 (protocol_id, action_id 或 raw command) 映射一個生命週期動作。
// rcon 協定用 Command(自由字串,可含 {msg} 佔位);rest 協定用 ActionID + Args(靜態參數,
// 隨動作固定送出,如 Palworld shutdown 的 waittime)。MessageKey 指定動態訊息(如 Announce 的
// msg)要寫入 Args 的哪個欄位名,預設 "message"(對齊 Palworld REST announce/shutdown)。
type Hook struct {
	ProtocolID string            `toml:"protocol_id"`
	Command    string            `toml:"command"`
	ActionID   string            `toml:"action_id"`
	Args       map[string]string `toml:"args"`
	MessageKey string            `toml:"message_key"`
}

// HealthProbe 對應 [health]:健康/就緒探針(R8)。未定義時以容器 running 為就緒。
type HealthProbe struct {
	Kind    string `toml:"kind"`     // tcp | rcon | rest | docker
	PortRef string `toml:"port_ref"` // 指向 [[ports]].name
	// ActionID(可選,僅 kind=rest)指定經指令協定送出的具名 REST 動作(如 Palworld "info"):
	// 探測複用該 rest 協定的 Basic Auth 認證,避免未認證裸 GET 對「全端點需認證」的伺服器恆非 2xx。
	// 省略時 rest 探針退回未認證 GET port_ref 根路徑(僅適合無認證的簡單服務)。
	ActionID string `toml:"action_id"`
}

// PlayersQuery 對應 [players_query]:玩家數查詢方式(R6)。不支援者省略,GUI 顯示「不適用」。
type PlayersQuery struct {
	Kind     string `toml:"kind"` // rcon | rest
	Command  string `toml:"command"`
	ActionID string `toml:"action_id"`
}

// ModsSpec 對應 [mods]:模組/模組包設定(R11)。itzg 為 /data 的唯一安裝擁有者。
type ModsSpec struct {
	Owner         string   `toml:"owner"` // 如 "image-native"
	PluginDir     string   `toml:"plugin_dir"`
	ModpackEnv    []string `toml:"modpack_env"`    // 如 ["MODRINTH", "AUTO_CURSEFORGE"]
	ManualFormats []string `toml:"manual_formats"` // 如 ["mrpack", "curseforge-zip"]
	ManualMount   string   `toml:"manual_mount"`   // 手動檔掛載處,交由 itzg 對應 env 取用

	// ModpackLoaders 是「允許套用模組包的變體 loader 集合」(T14 增量)。模組包(Modrinth/
	// CurseForge)需 mod loader(forge/fabric/quilt/neoforge)或由 itzg 自管(vanilla);外掛平台
	// (paper/bukkit)無法載入模組,故不列於此。前置檢查:選模組包但變體 loader 不在此集合 → 建立前擋。
	ModpackLoaders []string `toml:"modpack_loaders"`
}

// ParseTemplate 將範本 TOML 位元組解析為 GameTemplate。
// 本函式只負責解析語法,不做語意驗證(缺欄位/版本/adapter 檢查屬 core 範本引擎)。
func ParseTemplate(data []byte) (*GameTemplate, error) {
	var t GameTemplate
	if err := toml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("解析遊戲範本失敗: %w", err)
	}
	return &t, nil
}
