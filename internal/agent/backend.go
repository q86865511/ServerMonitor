package agent

import (
	"context"
	"time"

	"servermonitor/internal/protocol"
)

// RuntimeBackend 是執行後端(容器)層的抽象(R4)。core 對容器的所有生命週期、
// 資料封存與執行事件監看操作皆經此介面,不散落直呼 Docker SDK,使 Docker、Mock、
// 未來 Native 後端可等價替換。實作:MockBackend(本套件)、DockerBackend(T4)。
//
// 兩個刻意的邊界:
//   - Restart 不是介面方法:由 core 以 Stop+Start 編排並持 per-instance lock(R3/R8),
//     agent 亦不開 /restart。
//   - 遊戲內指令不經 ExecProcess(容器內程序),而經獨立的 GameCommandAdapter(R7)。
type RuntimeBackend interface {
	// Create 依規格建立一個 runtime 實例(容器),回傳其 RuntimeID;不自動啟動。
	Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error)
	// Start 啟動已建立的實例。
	Start(ctx context.Context, id protocol.RuntimeID) error
	// Stop 優雅停機(呼叫端先跑 hooks.stop,再於此以 opts.Grace 寬限期停止)。
	Stop(ctx context.Context, id protocol.RuntimeID, opts StopOpts) error
	// Status 回傳 runtime 層即時狀態。
	Status(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeStatus, error)
	// List 列舉本後端所有 runtime(R13 對帳:比對 gsm.* 標籤)。
	List(ctx context.Context) ([]protocol.RuntimeRef, error)
	// Inspect 回傳單一 runtime 的詳細資訊(R13)。
	Inspect(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeInfo, error)
	// Remove 移除實例;預設保留 data/backups,opts.Purge 才一併刪除。
	Remove(ctx context.Context, id protocol.RuntimeID, opts RemoveOpts) error
	// Logs 回傳 log 串流(R6);opts 控制 follow/tail/since。
	Logs(ctx context.Context, id protocol.RuntimeID, opts LogOpts) (LogStream, error)
	// ExecProcess 在容器內執行程序(非遊戲指令;遊戲指令見 GameCommandAdapter)。
	ExecProcess(ctx context.Context, id protocol.RuntimeID, cmd ExecCmd) (ExecResult, error)
	// Stats 取樣資源使用(R6)。
	Stats(ctx context.Context, id protocol.RuntimeID) (protocol.ResourceStats, error)
	// Archive 產生一致快照(呼叫端先 planned-stop),回傳對外 opaque 的 BackupID(R9)。
	Archive(ctx context.Context, id protocol.RuntimeID) (protocol.BackupID, error)
	// Restore 以備份還原,回傳「新的」RuntimeID(R9 原子還原語意)。
	Restore(ctx context.Context, id protocol.RuntimeID, b protocol.BackupID) (protocol.RuntimeID, error)
	// Events 回傳執行事件串流(die/health 等,R4/R8)。since 為上次游標:
	// 空字串=只收訂閱後的新事件;非空=先補送游標之後的緩衝事件(漏事件對帳)再轉為即時。
	Events(ctx context.Context, since protocol.Cursor) (EventStream, error)
}

// StopOpts 是 Stop 的選項。
//
// Grace 為優雅停機寬限期(對應代理 API 的 grace_seconds,R3)。
// PlannedStopToken 標記計畫性停止,供 core 端狀態機對帳隨後的 die 事件、避免誤判崩潰(R8);
// 後端本身不解讀此 token——Mock 與 Docker 的 die 事件皆不攜帶它,以維持後端等價,
// 對帳由 core 依 runtime + operation generation 追蹤(見 design「planned-stop token」)。
type StopOpts struct {
	Grace            time.Duration
	PlannedStopToken string
}

// RemoveOpts 是 Remove 的選項。Purge=true 時一併刪除 data/backups。
type RemoveOpts struct {
	Purge bool
}

// LogOpts 是 Logs 的選項。
type LogOpts struct {
	Follow bool      // 持續追蹤新輸出
	Tail   int       // 起始回溯行數(0=後端預設)
	Since  time.Time // 只取此時間之後的 log(零值=不限)
}

// LogLine 是一行容器 log(亦為 WS /instances/{id}/logs 的 wire 型別,故帶 snake_case 標籤)。
type LogLine struct {
	TsUTC  time.Time `json:"ts_utc"`
	Stream string    `json:"stream"` // "stdout" | "stderr"
	Line   string    `json:"line"`
}

// LogStream 是 log 串流(R6)。消費端 range Lines();串流因 ctx 取消、Close() 或來源
// 結束而關閉,關閉後 Err() 回傳原因(nil=正常結束)。
type LogStream interface {
	Lines() <-chan LogLine
	Close() error
	Err() error
}

// ExecCmd 是 ExecProcess 的輸入(容器內程序)。
type ExecCmd struct {
	Cmd     []string
	Env     []string
	WorkDir string
}

// ExecResult 是 ExecProcess 的結果(緩衝式;首版不串流)。
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// 執行事件型別(RuntimeEventKind/RuntimeEvent 與其常數)已上移至 internal/protocol:
// 它是節點代理 WS /events 的 wire 契約,須供核心跨 HTTP 邊界反序列化而不相依 agent。
// 以下別名維持 agent 內既有引用不變,並使 EventStream 直接串流可上線序列化的型別。
type (
	RuntimeEvent     = protocol.RuntimeEvent
	RuntimeEventKind = protocol.RuntimeEventKind
)

const (
	RuntimeEventStart  = protocol.RuntimeEventStart
	RuntimeEventDie    = protocol.RuntimeEventDie
	RuntimeEventHealth = protocol.RuntimeEventHealth
	RuntimeEventOOM    = protocol.RuntimeEventOOM
	RuntimeEventResync = protocol.RuntimeEventResync
)

// EventStream 是執行事件串流(R4/R8)。消費端 range Events();語意同 LogStream:
// 因 ctx 取消、Close() 或後端關閉而結束,Err() 回傳原因(nil=正常關閉)。
type EventStream interface {
	Events() <-chan RuntimeEvent
	Close() error
	Err() error
}
