package app

import (
	"context"
	"time"

	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// 本檔集中「綁定層(main.App)需要的、對 core 服務的薄轉發」。回傳 core/protocol 型別;
// DTO 轉換與 Wails 特定行為(ctx、事件推送、dialog)留在 main.App。

// Templates 回傳所有已載入範本(供 GUI 列出可建立的遊戲類型)。
func (r *Runtime) Templates() []*protocol.GameTemplate { return r.engine.List() }

// TemplateIconPath 回傳範本 id 的 icon 檔案路徑與是否可服務(R14);供 AssetServer handler 服務
// GET /tpl-icons/{id} 與 TemplateDTO.HasIcon 判定共用同一路徑安全判定(見 TemplateEngine.IconPath)。
func (r *Runtime) TemplateIconPath(id string) (string, bool) { return r.engine.IconPath(id) }

// Create 建立一個實例(R2 原子建立),回傳登錄的記錄。ctx 衍生自 rootCtx 並計入 in-flight
// (關閉時取消/等待,T15 #3)。
func (r *Runtime) Create(_ context.Context, opts core.CreateOptions) (core.InstanceRecord, error) {
	ctx, done := r.trackOp()
	defer done()
	return r.instances.Create(ctx, opts)
}

// Instances 回傳所有實例記錄(含 desired/observed)。
func (r *Runtime) Instances() ([]core.InstanceRecord, error) { return r.store.ListInstances() }

// PortsByInstance 一次查詢所有埠預留並依 instance_uuid 分組(R12:免對每個實例各查一次的 N+1)。
// 供綁定層組 InstanceDTO.Ports 時共用同一份查詢結果。
func (r *Runtime) PortsByInstance() (map[string][]core.PortReservation, error) {
	ports, err := r.store.ListPortReservations()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]core.PortReservation, len(ports))
	for _, p := range ports {
		out[p.InstanceUUID] = append(out[p.InstanceUUID], p)
	}
	return out, nil
}

// Instance 依 uuid 取單一實例記錄。
func (r *Runtime) Instance(uuid string) (core.InstanceRecord, error) {
	return r.store.GetInstance(uuid)
}

// Snapshot 回傳一個實例的聚合監控快照;第二回傳值表示是否在監控中。
func (r *Runtime) Snapshot(uuid string) (core.MonitorSnapshot, bool) { return r.monitor.Snapshot(uuid) }

// QueryMetrics 回傳某實例自 since(含)起的聚合指標時序,按時間升冪(R13;NULL 欄位透傳)。
func (r *Runtime) QueryMetrics(uuid string, since time.Time) ([]core.MetricPoint, error) {
	return r.store.QueryMetrics(uuid, since)
}

// QueryMetricsSummary 回傳自 since(含)起、各 15s bucket 的全體平均(R13;供總覽全體趨勢)。
func (r *Runtime) QueryMetricsSummary(since time.Time) ([]core.MetricPoint, error) {
	return r.store.QueryMetricsSummary(since)
}

// SendCommand 送出一則遊戲指令並回顯(R7)。
func (r *Runtime) SendCommand(ctx context.Context, uuid string, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	return r.commands.Send(ctx, uuid, cmd)
}

// CommandCapability 回傳實例「啟用中」指令協定(供主控台決定渲染 rcon/rest/停用);
// 範本無可用協定→ErrCommandNotEnabled(綁定層轉為 kind=none)。
func (r *Runtime) CommandCapability(uuid string) (protocol.CommandProtocol, error) {
	return r.commands.Capability(uuid)
}

// ListBackups 回傳實例備份清單(agent 為權威,R9)。
func (r *Runtime) ListBackups(ctx context.Context, uuid string) ([]protocol.BackupMeta, error) {
	return r.backups.ListBackups(ctx, uuid)
}

// RestoreBackup 以指定備份還原實例(R9 原子還原)。ctx 衍生自 rootCtx 並計入 in-flight
// (關閉時取消/等待,T15 #3)。
func (r *Runtime) RestoreBackup(_ context.Context, uuid string, backupID protocol.BackupID) error {
	ctx, done := r.trackOp()
	defer done()
	return r.backups.Restore(ctx, uuid, backupID)
}

// Schedules 回傳實例的排程(uuid 空回全部)。
func (r *Runtime) Schedules(uuid string) ([]core.ScheduleRecord, error) {
	return r.sched.ListSchedules(uuid)
}

// CreateSchedule 新增排程,回傳新 ID。
func (r *Runtime) CreateSchedule(uuid, kind string, spec core.ScheduleSpec, enabled bool) (string, error) {
	return r.sched.CreateSchedule(uuid, kind, spec, enabled)
}

// UpdateSchedule 覆寫既有排程。
func (r *Runtime) UpdateSchedule(id, uuid, kind string, spec core.ScheduleSpec, enabled bool) error {
	return r.sched.UpdateSchedule(id, uuid, kind, spec, enabled)
}

// DeleteSchedule 刪除排程。
func (r *Runtime) DeleteSchedule(id string) error { return r.sched.DeleteSchedule(id) }

// QueryEvents 依 filter 查詢事件(R14)。
func (r *Runtime) QueryEvents(f core.EventFilter) ([]protocol.Event, error) { return r.events.Query(f) }

// NodeStatuses 回傳所有節點狀態(離線顯示/重試依據)。
func (r *Runtime) NodeStatuses() []core.NodeStatus { return r.registry.List() }

// ---- 告警設定 ----

// WebhookConfigured 回報 Discord webhook URL 是否已設定(不回實值)。
func (r *Runtime) WebhookConfigured() bool { return r.discord.Configured() }

// SetWebhookURL 設定 Discord webhook URL(存入金鑰庫,不落 DB/log);空字串則清除。
func (r *Runtime) SetWebhookURL(url string) error {
	ref := protocol.SecretRef{Key: core.DiscordWebhookSecretKey}
	if url == "" {
		return r.secrets.Delete(ref)
	}
	return r.secrets.Set(ref, url)
}

// Thresholds 讀取實例資源門檻設定。
func (r *Runtime) Thresholds(uuid string) (core.ThresholdConfig, error) {
	return core.LoadThresholdConfig(r.store, uuid)
}

// SetThresholds 寫入實例資源門檻設定(ThresholdMonitor 於每筆取樣即時讀取,無需重新訂閱)。
func (r *Runtime) SetThresholds(uuid string, cfg core.ThresholdConfig) error {
	return core.SaveThresholdConfig(r.store, uuid, cfg)
}

// ---- 監控串流訂閱(供綁定層轉為 Wails 事件)----

// SubscribeLogs 訂閱一個實例的 log 串流;回傳 channel 與取消用 SubID。
func (r *Runtime) SubscribeLogs(uuid string) (<-chan core.LogLine, core.SubID) {
	return r.monitor.SubscribeLogs(uuid)
}

// UnsubscribeLogs 取消 log 訂閱。
func (r *Runtime) UnsubscribeLogs(uuid string, id core.SubID) { r.monitor.UnsubscribeLogs(uuid, id) }

// SubscribeStats 訂閱一個實例的資源取樣串流;回傳 channel 與取消用 SubID。
func (r *Runtime) SubscribeStats(uuid string) (<-chan protocol.ResourceStats, core.SubID) {
	return r.monitor.SubscribeStats(uuid)
}

// UnsubscribeStats 取消資源取樣訂閱。
func (r *Runtime) UnsubscribeStats(uuid string, id core.SubID) { r.monitor.UnsubscribeStats(uuid, id) }
