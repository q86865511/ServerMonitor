package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"servermonitor/internal/app"
	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// App 是 Wails 綁定層(前端唯一的 Go 呼叫面)。它是 app.Runtime 之上的薄殼:
// 方法薄轉發到後端服務、把 core/protocol 型別轉為前端 JSON 友善的 DTO,並把 log/stats
// 串流經 Wails 事件(runtime.EventsEmit)推送給前端。所有後端組裝在 OnStartup 完成。
type App struct {
	ctx context.Context
	rt  *app.Runtime

	subMu     sync.Mutex
	logSubs   map[string]core.SubID
	statsSubs map[string]core.SubID
}

// NewApp 建立 App(後端在 OnStartup 才組裝)。
func NewApp() *App {
	return &App{
		logSubs:   make(map[string]core.SubID),
		statsSubs: make(map[string]core.SubID),
	}
}

// OnStartup 在 Wails 啟動時組裝後端。AppLock 衝突(R13)→顯示錯誤 dialog 後退出;
// 其餘致命錯誤同樣以 dialog 呈現後退出。Docker 不可用不致命(節點離線,GUI 照常)。
func (a *App) OnStartup(ctx context.Context) {
	a.ctx = ctx
	rt, err := app.Bootstrap(app.Options{})
	if err != nil {
		msg := err.Error()
		if errors.Is(err, app.ErrAppLocked) {
			msg = "另一個 ServerMonitor 實例正在執行中,本程式將關閉。"
		}
		_, _ = wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
			Type:    wailsruntime.ErrorDialog,
			Title:   "ServerMonitor 無法啟動",
			Message: msg,
		})
		wailsruntime.Quit(ctx)
		return
	}
	a.rt = rt
	a.rt.Start()
}

// OnShutdown 在 Wails 關閉時優雅收束後端(不停任何遊戲容器)。
func (a *App) OnShutdown(_ context.Context) {
	if a.rt != nil {
		a.rt.Shutdown()
	}
}

// bgCtx 回傳狀態變更操作用的 context(不隨個別前端呼叫取消;後端關閉時另有 root context 控制)。
func (a *App) bgCtx() context.Context { return context.Background() }

// ---- DTO ----

// TemplateDTO 是範本的前端視圖(供建立表單渲染)。
type TemplateDTO struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Runtime  string       `json:"runtime"`
	Variants []VariantDTO `json:"variants"`
	Params   []ParamDTO   `json:"params"`
	Secrets  []SecretDTO  `json:"secrets"`
	Ports    []PortDTO    `json:"ports"`
	Modpack  bool         `json:"modpack"` // 是否支援模組包(R11)
}

// VariantDTO 是變體視圖。
type VariantDTO struct {
	ID     string `json:"id"`
	Loader string `json:"loader"`
}

// ParamDTO 是參數視圖。
type ParamDTO struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Default  any    `json:"default"`
	Required bool   `json:"required"`
}

// SecretDTO 是機密欄位視圖(只含宣告,不含值)。
type SecretDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// PortDTO 是埠宣告視圖。
type PortDTO struct {
	Name      string `json:"name"`
	Container int    `json:"container"`
	HostPort  int    `json:"host_port"`
	Protocol  string `json:"protocol"`
	Required  bool   `json:"required"`
}

// InstanceDTO 是實例的前端視圖(含 desired/observed)。
type InstanceDTO struct {
	UUID          string `json:"uuid"`
	TemplateID    string `json:"template_id"`
	Variant       string `json:"variant"`
	Node          string `json:"node"`
	DesiredState  string `json:"desired_state"`
	ObservedState string `json:"observed_state"`
}

// SnapshotDTO 是聚合監控快照的前端視圖(R6)。
type SnapshotDTO struct {
	UUID          string                 `json:"uuid"`
	Monitored     bool                   `json:"monitored"`
	HasStats      bool                   `json:"has_stats"`
	Stats         protocol.ResourceStats `json:"stats"`
	PlayerCount   *int                   `json:"player_count"`
	Online        *bool                  `json:"online"`
	ObservedState string                 `json:"observed_state"`
}

// CreateInstanceRequest 是建立實例的輸入。
type CreateInstanceRequest struct {
	TemplateID string            `json:"template_id"`
	Variant    string            `json:"variant"`
	Params     map[string]string `json:"params"`
	Secrets    map[string]string `json:"secrets"`
	Node       string            `json:"node"`
	Modpack    *ModpackRequest   `json:"modpack"`
}

// ModpackRequest 是模組包來源輸入(R11)。
type ModpackRequest struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// ScheduleDTO 是排程的前端視圖。
type ScheduleDTO struct {
	ID           string `json:"id"`
	InstanceUUID string `json:"instance_uuid"`
	Kind         string `json:"kind"`
	At           string `json:"at"`
	Weekdays     []int  `json:"weekdays"`
	Enabled      bool   `json:"enabled"`
	LastFiredUTC string `json:"last_fired_utc"`
}

// UpsertScheduleRequest 是新增/更新排程的輸入(ID 空=新增)。
type UpsertScheduleRequest struct {
	ID           string `json:"id"`
	InstanceUUID string `json:"instance_uuid"`
	Kind         string `json:"kind"`
	At           string `json:"at"`
	Weekdays     []int  `json:"weekdays"`
	Enabled      bool   `json:"enabled"`
}

// AlertSettingsDTO 是告警設定的前端視圖(webhook 一律遮罩,只回是否已設定)。
type AlertSettingsDTO struct {
	WebhookConfigured bool    `json:"webhook_configured"`
	CPUPercent        float64 `json:"cpu_percent"`
	MemoryPercent     float64 `json:"memory_percent"`
}

// AlertSettingsRequest 是設定告警的輸入。UpdateWebhook=true 才依 WebhookURL 設定/清除
// (避免前端把遮罩後的空值誤寫回而清除既有 webhook)。
type AlertSettingsRequest struct {
	UpdateWebhook bool    `json:"update_webhook"`
	WebhookURL    string  `json:"webhook_url"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
}

// EventDTO 是事件的前端視圖(DetailsJSON 以字串承載,前端自行解析)。
type EventDTO struct {
	Code         string `json:"code"`
	TsUTC        string `json:"ts_utc"`
	Severity     string `json:"severity"`
	InstanceUUID string `json:"instance_uuid"`
	Node         string `json:"node"`
	TemplateID   string `json:"template_id"`
	Details      string `json:"details"`
}

// QueryEventsRequest 是事件查詢輸入(空欄位不參與過濾;時間為 Unix 秒,0=不限)。
type QueryEventsRequest struct {
	InstanceUUID string `json:"instance_uuid"`
	Code         string `json:"code"`
	SinceUnix    int64  `json:"since_unix"`
	UntilUnix    int64  `json:"until_unix"`
	Limit        int    `json:"limit"`
}

// NodeStatusDTO 是節點狀態的前端視圖。
type NodeStatusDTO struct {
	Node    string `json:"node"`
	Online  bool   `json:"online"`
	LastErr string `json:"last_err"`
}

// ---- 範本 / 建立 ----

// ListTemplates 回傳所有可建立的遊戲範本(R1)。
func (a *App) ListTemplates() []TemplateDTO {
	tmpls := a.rt.Templates()
	out := make([]TemplateDTO, 0, len(tmpls))
	for _, t := range tmpls {
		out = append(out, toTemplateDTO(t))
	}
	return out
}

// CreateInstance 一鍵建立實例(R2)。回傳新實例 UUID;失敗回可讀錯誤。
func (a *App) CreateInstance(req CreateInstanceRequest) (string, error) {
	opts := core.CreateOptions{
		TemplateID: req.TemplateID,
		Variant:    req.Variant,
		Params:     req.Params,
		Secrets:    req.Secrets,
		Node:       req.Node,
	}
	if req.Modpack != nil && (req.Modpack.Type != "" || req.Modpack.Ref != "") {
		opts.Modpack = &core.ModpackSource{Type: core.ModpackType(req.Modpack.Type), Ref: req.Modpack.Ref}
	}
	rec, err := a.rt.Create(a.bgCtx(), opts)
	if err != nil {
		return "", fmt.Errorf("建立實例失敗: %w", err)
	}
	return rec.UUID, nil
}

// ---- 生命週期 ----

// StartInstance 啟動實例(R3)。
func (a *App) StartInstance(uuid string) error { return a.rt.StartInstance(a.bgCtx(), uuid) }

// StopInstance 停止實例(R3)。
func (a *App) StopInstance(uuid string) error { return a.rt.StopInstance(a.bgCtx(), uuid) }

// RestartInstance 重啟實例(R3)。
func (a *App) RestartInstance(uuid string) error { return a.rt.RestartInstance(a.bgCtx(), uuid) }

// RemoveInstance 移除實例;purge=true 一併刪除 data/backups。
func (a *App) RemoveInstance(uuid string, purge bool) error {
	return a.rt.RemoveInstance(a.bgCtx(), uuid, purge)
}

// ListInstances 回傳所有實例(含 desired/observed)。
func (a *App) ListInstances() ([]InstanceDTO, error) {
	recs, err := a.rt.Instances()
	if err != nil {
		return nil, err
	}
	out := make([]InstanceDTO, 0, len(recs))
	for _, r := range recs {
		out = append(out, InstanceDTO{
			UUID: r.UUID, TemplateID: r.TemplateID, Variant: r.Variant, Node: r.Node,
			DesiredState: string(r.DesiredState), ObservedState: string(r.ObservedState),
		})
	}
	return out, nil
}

// ---- 監控 ----

// GetSnapshot 回傳一個實例的聚合監控快照(R6)。
func (a *App) GetSnapshot(uuid string) (SnapshotDTO, error) {
	snap, ok := a.rt.Snapshot(uuid)
	if !ok {
		return SnapshotDTO{UUID: uuid, Monitored: false}, nil
	}
	return SnapshotDTO{
		UUID: snap.UUID, Monitored: true, HasStats: snap.HasStats, Stats: snap.Stats,
		PlayerCount: snap.PlayerCount, Online: snap.Online, ObservedState: string(snap.ObservedState),
	}, nil
}

// SubscribeLogs 開始把某實例的 log 串流經事件 "logs:<uuid>" 推送給前端(冪等)。
func (a *App) SubscribeLogs(uuid string) error {
	a.subMu.Lock()
	if _, ok := a.logSubs[uuid]; ok {
		a.subMu.Unlock()
		return nil
	}
	ch, id := a.rt.SubscribeLogs(uuid)
	a.logSubs[uuid] = id
	a.subMu.Unlock()
	go func() {
		for ln := range ch {
			wailsruntime.EventsEmit(a.ctx, "logs:"+uuid, ln)
		}
	}()
	return nil
}

// UnsubscribeLogs 停止 log 推送。
func (a *App) UnsubscribeLogs(uuid string) error {
	a.subMu.Lock()
	id, ok := a.logSubs[uuid]
	if ok {
		delete(a.logSubs, uuid)
	}
	a.subMu.Unlock()
	if ok {
		a.rt.UnsubscribeLogs(uuid, id)
	}
	return nil
}

// SubscribeStats 開始把某實例的資源取樣經事件 "stats:<uuid>" 推送給前端(冪等)。
func (a *App) SubscribeStats(uuid string) error {
	a.subMu.Lock()
	if _, ok := a.statsSubs[uuid]; ok {
		a.subMu.Unlock()
		return nil
	}
	ch, id := a.rt.SubscribeStats(uuid)
	a.statsSubs[uuid] = id
	a.subMu.Unlock()
	go func() {
		for s := range ch {
			wailsruntime.EventsEmit(a.ctx, "stats:"+uuid, s)
		}
	}()
	return nil
}

// UnsubscribeStats 停止資源取樣推送。
func (a *App) UnsubscribeStats(uuid string) error {
	a.subMu.Lock()
	id, ok := a.statsSubs[uuid]
	if ok {
		delete(a.statsSubs, uuid)
	}
	a.subMu.Unlock()
	if ok {
		a.rt.UnsubscribeStats(uuid, id)
	}
	return nil
}

// ---- 指令 ----

// CommandCapabilityDTO 描述一個實例的指令能力(供主控台決定渲染方式)。Kind:
// "rcon"=自由指令輸入;"rest"=具名動作(Actions);"none"=無可用協定,停用並顯示 Reason。
type CommandCapabilityDTO struct {
	Kind    string             `json:"kind"`
	Reason  string             `json:"reason"`
	Actions []CommandActionDTO `json:"actions"`
}

// CommandActionDTO 是 rest 協定的一個具名動作視圖(供主控台動作下拉)。
type CommandActionDTO struct {
	ActionID string `json:"action_id"`
	Method   string `json:"method"`
	Path     string `json:"path"`
}

// GetCommandCapability 回傳實例的指令能力(R7)。無可用協定或無法解析(範本未載入等)→
// kind="none" 並帶 Reason,使主控台上前置停用而非等首次送出失敗才依字串比對停用。
func (a *App) GetCommandCapability(uuid string) (CommandCapabilityDTO, error) {
	cp, err := a.rt.CommandCapability(uuid)
	if err != nil {
		// 任何解析失敗(無協定/範本未載入/實例查無)一律降級為停用態,附原因供顯示。
		return CommandCapabilityDTO{Kind: "none", Reason: err.Error()}, nil
	}
	dto := CommandCapabilityDTO{Kind: cp.Kind}
	if cp.Kind == "rest" {
		for _, act := range cp.Actions {
			dto.Actions = append(dto.Actions, CommandActionDTO{ActionID: act.ActionID, Method: act.Method, Path: act.Path})
		}
	}
	return dto, nil
}

// SendCommand 送出一則遊戲指令(rcon 用 raw;rest 用 action_id + args)(R7)。
func (a *App) SendCommand(uuid string, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	return a.rt.SendCommand(a.bgCtx(), uuid, cmd)
}

// ---- 備份 ----

// ListBackups 回傳實例備份清單(R9)。
func (a *App) ListBackups(uuid string) ([]protocol.BackupMeta, error) {
	return a.rt.ListBackups(a.bgCtx(), uuid)
}

// BackupNow 立即備份實例(R9)。
func (a *App) BackupNow(uuid string) (protocol.BackupMeta, error) {
	return a.rt.BackupNow(a.bgCtx(), uuid)
}

// RestoreBackup 以指定備份還原實例(R9)。
func (a *App) RestoreBackup(uuid, backupID string) error {
	return a.rt.RestoreBackup(a.bgCtx(), uuid, protocol.BackupID(backupID))
}

// ---- 排程 ----

// ListSchedules 回傳實例的排程(uuid 空回全部)(R8)。
func (a *App) ListSchedules(uuid string) ([]ScheduleDTO, error) {
	recs, err := a.rt.Schedules(uuid)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduleDTO, 0, len(recs))
	for _, r := range recs {
		out = append(out, toScheduleDTO(r))
	}
	return out, nil
}

// UpsertSchedule 新增(ID 空)或更新排程,回傳排程 ID(R8)。
func (a *App) UpsertSchedule(req UpsertScheduleRequest) (string, error) {
	spec := core.ScheduleSpec{At: req.At, Weekdays: req.Weekdays}
	if req.ID == "" {
		return a.rt.CreateSchedule(req.InstanceUUID, req.Kind, spec, req.Enabled)
	}
	if err := a.rt.UpdateSchedule(req.ID, req.InstanceUUID, req.Kind, spec, req.Enabled); err != nil {
		return "", err
	}
	return req.ID, nil
}

// DeleteSchedule 刪除排程(R8)。
func (a *App) DeleteSchedule(id string) error { return a.rt.DeleteSchedule(id) }

// ---- 告警設定 ----

// GetAlertSettings 回傳告警設定(webhook 遮罩;門檻取自實例設定)(R10)。
func (a *App) GetAlertSettings(uuid string) (AlertSettingsDTO, error) {
	cfg, err := a.rt.Thresholds(uuid)
	if err != nil {
		return AlertSettingsDTO{}, err
	}
	return AlertSettingsDTO{
		WebhookConfigured: a.rt.WebhookConfigured(),
		CPUPercent:        cfg.CPUPercent,
		MemoryPercent:     cfg.MemoryPercent,
	}, nil
}

// SetAlertSettings 設定告警(門檻寫入實例設定;webhook 依 UpdateWebhook 決定是否變更)(R10)。
func (a *App) SetAlertSettings(uuid string, req AlertSettingsRequest) error {
	if req.UpdateWebhook {
		if err := a.rt.SetWebhookURL(req.WebhookURL); err != nil {
			return fmt.Errorf("設定 webhook 失敗: %w", err)
		}
	}
	return a.rt.SetThresholds(uuid, core.ThresholdConfig{CPUPercent: req.CPUPercent, MemoryPercent: req.MemoryPercent})
}

// ---- 事件 ----

// QueryEvents 依條件查詢事件(R14)。
func (a *App) QueryEvents(req QueryEventsRequest) ([]EventDTO, error) {
	f := core.EventFilter{Limit: req.Limit}
	if req.InstanceUUID != "" {
		u := req.InstanceUUID
		f.InstanceUUID = &u
	}
	if req.Code != "" {
		c := protocol.EventCode(req.Code)
		f.Code = &c
	}
	if req.SinceUnix > 0 {
		t := time.Unix(req.SinceUnix, 0).UTC()
		f.Since = &t
	}
	if req.UntilUnix > 0 {
		t := time.Unix(req.UntilUnix, 0).UTC()
		f.Until = &t
	}
	evs, err := a.rt.QueryEvents(f)
	if err != nil {
		return nil, err
	}
	out := make([]EventDTO, 0, len(evs))
	for _, e := range evs {
		out = append(out, toEventDTO(e))
	}
	return out, nil
}

// ---- 節點 ----

// NodeStatus 回傳所有節點狀態(離線顯示/重試依據)(R5)。
func (a *App) NodeStatus() []NodeStatusDTO {
	sts := a.rt.NodeStatuses()
	out := make([]NodeStatusDTO, 0, len(sts))
	for _, s := range sts {
		out = append(out, NodeStatusDTO{Node: s.Node, Online: s.Online, LastErr: s.LastErr})
	}
	return out
}

// RetryDocker 重試連線 Docker(節點離線時)(R5)。
func (a *App) RetryDocker() error { return a.rt.RetryDocker() }

// ---- DTO 轉換 ----

func toTemplateDTO(t *protocol.GameTemplate) TemplateDTO {
	dto := TemplateDTO{ID: t.ID, Name: t.Name, Runtime: t.Runtime, Modpack: t.Mods != nil}
	for _, v := range t.Variants {
		dto.Variants = append(dto.Variants, VariantDTO{ID: v.ID, Loader: v.Loader})
	}
	for _, p := range t.Params {
		dto.Params = append(dto.Params, ParamDTO{Key: p.Key, Label: p.Label, Type: p.Type, Default: p.Default, Required: p.Required})
	}
	for _, s := range t.Secrets {
		dto.Secrets = append(dto.Secrets, SecretDTO{Key: s.Key, Label: s.Label})
	}
	for _, p := range t.Ports {
		dto.Ports = append(dto.Ports, PortDTO{Name: p.Name, Container: p.Container, HostPort: p.HostPort, Protocol: p.Protocol, Required: p.Required})
	}
	return dto
}

func toScheduleDTO(r core.ScheduleRecord) ScheduleDTO {
	dto := ScheduleDTO{ID: r.ID, InstanceUUID: r.InstanceUUID, Kind: r.Kind, Enabled: r.Enabled}
	var spec core.ScheduleSpec
	if len(r.SpecJSON) > 0 {
		_ = json.Unmarshal(r.SpecJSON, &spec)
	}
	dto.At = spec.At
	dto.Weekdays = spec.Weekdays
	if r.LastFiredUTC != nil {
		dto.LastFiredUTC = r.LastFiredUTC.UTC().Format(time.RFC3339)
	}
	return dto
}

func toEventDTO(e protocol.Event) EventDTO {
	dto := EventDTO{
		Code:     string(e.Code),
		TsUTC:    e.TsUTC.UTC().Format(time.RFC3339Nano),
		Severity: string(e.Severity),
		Details:  string(e.DetailsJSON),
	}
	if e.InstanceUUID != nil {
		dto.InstanceUUID = *e.InstanceUUID
	}
	if e.Node != nil {
		dto.Node = *e.Node
	}
	if e.TemplateID != nil {
		dto.TemplateID = *e.TemplateID
	}
	return dto
}
