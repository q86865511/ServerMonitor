package protocol

import (
	"fmt"
	"time"
)

// 識別碼型別(跨 core/agent 共享)。
type (
	// RuntimeID 是執行後端(容器)層的識別碼。
	RuntimeID string
	// BackupID 是對外 opaque 的備份識別碼;agent 擁有備份儲存根,core 只存此參照(R9)。
	BackupID string
	// Cursor 是事件串流游標,供 reconnect 與漏事件對帳(R4)。
	Cursor string
)

// RuntimeState 是執行後端(容器)層的狀態,有別於 core 實例狀態機(見 InstanceState)。
type RuntimeState string

const (
	RuntimeStateUnknown RuntimeState = "unknown"
	RuntimeStateCreated RuntimeState = "created"
	RuntimeStateRunning RuntimeState = "running"
	RuntimeStateStopped RuntimeState = "stopped"
	RuntimeStateExited  RuntimeState = "exited"
	RuntimeStatePaused  RuntimeState = "paused"
	RuntimeStateDead    RuntimeState = "dead"
)

// InstanceState 是 core 每實例狀態機的狀態集(R8);轉移表與轉移邏輯於 core 實作(T8)。
// 在此定義為共享詞彙,供 DB(desired_state/observed_state)與 GUI 一致引用。
type InstanceState string

const (
	InstanceStateCreated   InstanceState = "Created"
	InstanceStateStarting  InstanceState = "Starting"
	InstanceStateRunning   InstanceState = "Running"
	InstanceStateStopping  InstanceState = "Stopping"
	InstanceStateStopped   InstanceState = "Stopped"
	InstanceStateBackingUp InstanceState = "BackingUp"
	InstanceStateRestoring InstanceState = "Restoring"
	InstanceStateCrashed   InstanceState = "Crashed"
	InstanceStateError     InstanceState = "Error"
	InstanceStateOffline   InstanceState = "Offline" // 節點層
)

// PortBinding 是解析後的實際埠綁定(範本 PortSpec + 使用者選擇 → 建立時的具體綁定)。
type PortBinding struct {
	Name      string `json:"name"`
	Container int    `json:"container"`
	HostPort  int    `json:"host_port"`
	BindIP    string `json:"bind_ip"`
	Protocol  string `json:"protocol"`
}

// InstanceSpec 是建立一個 runtime 實例所需的規格(core → RuntimeBackend.Create)。
// 參數已解析為 Env;機密值依 R12「runtime 明文例外」於此邊界注入 Env(loopback 信任域內)。
type InstanceSpec struct {
	UUID       string            `json:"uuid"`
	TemplateID string            `json:"template_id"`
	Variant    string            `json:"variant"`
	Image      string            `json:"image"` // 鎖定映像(tag 或 digest)
	Env        map[string]string `json:"env"`
	Ports      []PortBinding     `json:"ports"`
	DataDirs   []string          `json:"data_dirs"`        // 需掛載的資料目錄(R9 備份範圍)
	Mounts     []MountSpec       `json:"mounts,omitempty"` // 非備份範圍的具名掛載(R11 手動模組包檔傳輸)
	Labels     map[string]string `json:"labels"`           // gsm.uuid / gsm.managed-by / gsm.node / gsm.schema
	Node       string            `json:"node"`
}

// MountSpec 描述一個「備份範圍之外」的具名掛載點(R11 手動模組包檔傳輸)。agent 為每個
// Mount 於實例資料根下建 mounts/<Name>/ 宿主子目錄並 bind mount 到 ContainerPath,再經
// PUT /instances/{id}/mounts/{name} 端點接收檔案位元組寫入其中。此 namespace 刻意排除於
// Archive/Restore 的打包與還原範圍外——手動模組包檔屬「建立時提供的輸入」而非「執行產生的
// 狀態」,不應隨備份漂移,且跨備份還原時保持原樣(見 DockerBackend.tarInstanceData)。
type MountSpec struct {
	Name          string `json:"name"`
	ContainerPath string `json:"container_path"`
}

// RuntimeStatus 是 RuntimeBackend.Status 回傳的 runtime 層即時狀態。
type RuntimeStatus struct {
	ID         RuntimeID    `json:"id"`
	State      RuntimeState `json:"state"`
	Running    bool         `json:"running"`
	ExitCode   *int         `json:"exit_code,omitempty"` // 已結束時的退出碼
	Health     string       `json:"health,omitempty"`    // docker healthcheck:starting/healthy/unhealthy/none
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
}

// ResourceStats 是 RuntimeBackend.Stats 的資源取樣(R6)。無法採集的欄位以指標留空,
// 由 GUI 呈現「不適用」。CPUPercent 正規化為容器 CPU 使用率 / 可用核心。
type ResourceStats struct {
	TsUTC         time.Time `json:"ts_utc"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryBytes   uint64    `json:"memory_bytes"`
	MemoryLimit   uint64    `json:"memory_limit"`
	DataDiskBytes *uint64   `json:"data_disk_bytes,omitempty"` // 資料磁碟已用量(data_dirs 宿主路徑)
	PlayerCount   *int      `json:"player_count,omitempty"`
	Online        *bool     `json:"online,omitempty"`
}

// RuntimeRef 是 RuntimeBackend.List 的輕量條目(R13 對帳:比對 gsm.* 標籤)。
type RuntimeRef struct {
	ID     RuntimeID         `json:"id"`
	State  RuntimeState      `json:"state"`
	Labels map[string]string `json:"labels"`
}

// RuntimeInfo 是 RuntimeBackend.Inspect 的詳細資訊(R13)。
type RuntimeInfo struct {
	ID        RuntimeID         `json:"id"`
	State     RuntimeState      `json:"state"`
	Image     string            `json:"image"`
	Labels    map[string]string `json:"labels"`
	Ports     []PortBinding     `json:"ports"`
	CreatedAt time.Time         `json:"created_at"`
	Status    RuntimeStatus     `json:"status"`
}

// RuntimeEventKind 是執行事件種類(容器層,有別於 R14 的事件封套 Event)。
type RuntimeEventKind string

const (
	RuntimeEventStart  RuntimeEventKind = "start"  // 容器啟動
	RuntimeEventDie    RuntimeEventKind = "die"    // 容器結束(崩潰與計畫停止皆屬之)
	RuntimeEventHealth RuntimeEventKind = "health" // 健康狀態變更(starting/healthy/unhealthy)
	RuntimeEventOOM    RuntimeEventKind = "oom"    // 記憶體不足
	// RuntimeEventResync 表示串流偵測到緩衝已逐出、發生漏事件:消費端應改走完整對帳(R4/R13)。
	RuntimeEventResync RuntimeEventKind = "resync"
)

// RuntimeEvent 是 RuntimeBackend.Events 串流上的一則執行事件,亦為節點代理 WS /events 的
// wire 契約——故與同為 RuntimeBackend 輸出的 RuntimeStatus/ResourceStats 並置於此,供核心
// 跨 HTTP 邊界反序列化而不需相依 agent 內部型別。Cursor 單調遞增,供 reconnect 對帳;
// Kind=die 時 ExitCode 有值,health 時 Health 有值。
//
// 此型別刻意不攜帶 planned-stop token:崩潰與計畫停止在事件層不可區分(與 Docker 的 die
// 事件一致),planned 與否由核心依 token 對帳。
type RuntimeEvent struct {
	Cursor   Cursor           `json:"cursor,omitempty"`
	ID       RuntimeID        `json:"id,omitempty"`
	Kind     RuntimeEventKind `json:"kind"`
	TsUTC    time.Time        `json:"ts_utc"`
	ExitCode *int             `json:"exit_code,omitempty"` // die 時的退出碼
	Health   string           `json:"health,omitempty"`    // health 時的健康狀態
}

// GameCommand 是一則遊戲指令(R7):rcon 用 Raw 自由字串;rest 用 ActionID + Args 具名動作。
type GameCommand struct {
	ProtocolID string            `json:"protocol_id"`
	Raw        string            `json:"raw,omitempty"`
	ActionID   string            `json:"action_id,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
}

// CommandResult 是遊戲指令的回顯結果。
type CommandResult struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
}

// BackupMeta 是備份中繼(R9)。core DB 存 BackupID 與中繼,不存原始路徑。
type BackupMeta struct {
	BackupID     BackupID  `json:"backup_id"`
	InstanceUUID string    `json:"instance_uuid"`
	Game         string    `json:"game"`
	TsUTC        time.Time `json:"ts_utc"`
	Checksum     string    `json:"checksum"`
}

// ---- 代理 API 契約 DTO(R5;版本化 + bearer;{id}=instance UUID)----

// 代理 API 契約相關常數。
const (
	// AgentAPIVersion 是節點代理 API 的版本前綴(R5 版本化契約)。
	AgentAPIVersion = "v1"
	// IdempotencyKeyHeader 是寫入端點的冪等鍵標頭;重播於 TTL 內回原結果(R5)。
	IdempotencyKeyHeader = "Idempotency-Key"
	// IdempotencyKeyTTL 是冪等鍵重播窗口。
	IdempotencyKeyTTL = 10 * time.Minute
)

// ErrorCode 是代理 API 的統一錯誤碼(R5)。
type ErrorCode string

const (
	ErrPortConflict ErrorCode = "ERR_PORT_CONFLICT"
	ErrNotFound     ErrorCode = "ERR_NOT_FOUND"
	ErrLocked       ErrorCode = "ERR_LOCKED"
	ErrUnauthorized ErrorCode = "ERR_UNAUTHORIZED"
	ErrConflict     ErrorCode = "ERR_CONFLICT"
	ErrBadRequest   ErrorCode = "ERR_BAD_REQUEST"
	ErrInternal     ErrorCode = "ERR_INTERNAL"
)

// APIError 是代理 API 的統一錯誤回應主體。
type APIError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// Error 讓 APIError 滿足 error 介面。
func (e *APIError) Error() string {
	return string(e.Code) + ": " + e.Message
}

// CreateInstanceRequest 是 POST /instances 的請求(R5)。
type CreateInstanceRequest struct {
	Spec InstanceSpec `json:"spec"`
}

// CreateInstanceResponse 是 POST /instances 的回應。
type CreateInstanceResponse struct {
	UUID      string    `json:"uuid"`
	RuntimeID RuntimeID `json:"runtime_id"`
}

// InstanceSummary 是 GET /instances 清單條目。
type InstanceSummary struct {
	UUID      string       `json:"uuid"`
	RuntimeID RuntimeID    `json:"runtime_id"`
	State     RuntimeState `json:"state"`
}

// ListInstancesResponse 是 GET /instances 的回應。
type ListInstancesResponse struct {
	Instances []InstanceSummary `json:"instances"`
}

// StopInstanceRequest 是 POST /instances/{id}/stop 的請求。
// GraceSeconds 為優雅停機寬限期(預設 30s,R3);PlannedStopToken 標記計畫性停止,
// 使 core 狀態機不把隨後的 die 事件誤判為崩潰(R8)。
type StopInstanceRequest struct {
	GraceSeconds     int    `json:"grace_seconds"`
	PlannedStopToken string `json:"planned_stop_token,omitempty"`
}

// RemoveInstanceRequest 是 DELETE /instances/{id} 的請求。預設保留 data/backups,Purge 才刪。
type RemoveInstanceRequest struct {
	Purge bool `json:"purge"`
}

// RestActionSpec 是一個 rest 具名動作的線上定義(R7):core 由範本 CommandAction 轉出,
// 隨請求送達 agent,供 PalworldRestAdapter 依 action_id 選取並執行。刻意與範本層
// CommandAction(TOML 結構)分離,使 wire 契約不相依範本內部欄位(如 input/output/ref)。
type RestActionSpec struct {
	ActionID string `json:"action_id"`
	Method   string `json:"method"`
	Path     string `json:"path"`
}

// CommandTarget 描述一則遊戲指令的送達目標(R7)。由 core 解析範本 command_protocol、實例埠
// 映射與金鑰庫密碼實值組出,隨請求越過 loopback 送達 agent 供 GameCommandAdapter 執行。
//
// Password 為**明文**——這是與 InstanceSpec.Env 同一個 R12「runtime 明文例外」:機密實值
// 只在 loopback 信任域內注入,故 command 端點僅供本機呼叫、且此型別不做 redaction(有別於
// SecretRef)。Kind=rcon 用 Host/Port/Password;Kind=rest 另用 Username/Actions。
type CommandTarget struct {
	ProtocolID string           `json:"protocol_id"`
	Kind       string           `json:"kind"` // "rcon" | "rest"
	Host       string           `json:"host"`
	Port       int              `json:"port"`
	Username   string           `json:"username,omitempty"` // rest basic auth 帳號(Palworld 固定 "admin")
	Password   string           `json:"password,omitempty"` // 明文(loopback 例外):rcon 認證 / rest basic auth 密碼
	Actions    []RestActionSpec `json:"actions,omitempty"`  // rest 具名動作定義
}

// String / GoString 遮罩 Password,避免 %v/%s/%#v(如誤入 log/錯誤訊息)洩漏明文機密。
// 這與 wire 契約無關:JSON 序列化不走 Stringer 路徑,MarshalJSON 刻意不覆寫,故經
// NodeClient 送達 agent 的請求主體仍為明文(R12 loopback 例外)。
func (t CommandTarget) String() string   { return t.masked() }
func (t CommandTarget) GoString() string { return t.masked() }

// masked 回傳遮罩 Password 後的可讀表示;非空密碼一律顯示為 [REDACTED],不透露長度。
func (t CommandTarget) masked() string {
	pw := ""
	if t.Password != "" {
		pw = "[REDACTED]"
	}
	return fmt.Sprintf("CommandTarget{ProtocolID:%q Kind:%q Host:%q Port:%d Username:%q Password:%s Actions:%v}",
		t.ProtocolID, t.Kind, t.Host, t.Port, t.Username, pw, t.Actions)
}

// CommandRequest 是 POST /instances/{id}/command 的請求。Target 由 core 解析範本+埠+機密後填入,
// agent 依 Target.Kind 選 adapter 執行(不再由 agent 解析範本)。
type CommandRequest struct {
	Command GameCommand   `json:"command"`
	Target  CommandTarget `json:"target"`
}

// CommandResponse 是 POST /instances/{id}/command 的回應。
type CommandResponse struct {
	Result CommandResult `json:"result"`
}

// ListBackupsResponse 是 GET /instances/{id}/backups 的回應。
type ListBackupsResponse struct {
	Backups []BackupMeta `json:"backups"`
}

// BackupResponse 是 POST /instances/{id}/backup 的回應。
type BackupResponse struct {
	Backup BackupMeta `json:"backup"`
}

// RestoreRequest 是 POST /instances/{id}/restore 的請求。
type RestoreRequest struct {
	BackupID BackupID `json:"backup_id"`
}

// RestoreResponse 是 POST /instances/{id}/restore 的回應;還原回傳新 runtime ID(R9)。
type RestoreResponse struct {
	RuntimeID RuntimeID `json:"runtime_id"`
}

// HealthResponse 是 GET /health 的回應。
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}
