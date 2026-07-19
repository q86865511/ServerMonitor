package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
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

	// tray 系統匣圖示、singleInst 單一實例喚醒通道(皆為 Windows 專屬,非 Windows 為 no-op 存根)。
	// singleInst 於 main 建立並注入,tray 於 OnStartup 建立。
	tray       *trayController
	singleInst *singleInstance

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

// OnStartup 在 Wails 啟動時組裝後端。AppLock 衝突(R13)→Windows 上喚醒既有實例視窗後靜默
// 退出(單一實例喚醒體驗),喚醒不可用(非 Windows/事件失效)則維持原錯誤 dialog;
// 其餘致命錯誤同樣以 dialog 呈現後退出。Docker 不可用不致命(節點離線,GUI 照常)。
func (a *App) OnStartup(ctx context.Context) {
	a.ctx = ctx
	// ReadyTimeout:遊戲伺服器冷啟(itzg 前置下載/JVM 起動/世界生成、native 下載 JRE+伺服器檔)
	// 是分鐘級(E2E 對 Minecraft/Palworld 分別給 8~15 分鐘),預設 60s 會把健康的首次啟動判成
	// 逾時並標 Error。GUI 放寬到 10 分鐘;就緒探針一成功即返回,不會讓正常啟動多等。
	rt, err := app.Bootstrap(app.Options{ReadyTimeout: 10 * time.Minute})
	if err != nil {
		// AppLock 衝突(R13):本實例為後啟者。喚醒成功才靜默退出——此為防禦性後備,正常
		// 時序下第二實例已於 main 的 newSingleInstance 提早偵測退出(見 singleinstance_windows.go);
		// 喚醒失敗(非 Windows 為 no-op、或事件失效)則落到下方錯誤對話框,不讓使用者毫無提示。
		if errors.Is(err, app.ErrAppLocked) && a.singleInst.wake() {
			wailsruntime.Quit(ctx)
			return
		}
		// 其餘致命錯誤:以對話框呈現後退出。
		_, _ = wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
			Type:    wailsruntime.ErrorDialog,
			Title:   "ServerMonitor 無法啟動",
			Message: err.Error(),
		})
		wailsruntime.Quit(ctx)
		return
	}
	a.rt = rt
	a.rt.Start()

	// 階段 3:drain 核心啟動階段進度(starting/awaiting-ready/ready),以 provision:<uuid> 事件推送給
	// 前端(伺服器主控台顯示啟動進度、全域操作面板彙整)。a.ctx 取消(關閉)時 goroutine 收束。
	go func() {
		ch := a.rt.StartupProgress()
		for {
			select {
			case p, ok := <-ch:
				if !ok {
					return
				}
				if p.InstanceUUID != "" {
					wailsruntime.EventsEmit(a.ctx, "provision:"+p.InstanceUUID, p)
				}
			case <-a.ctx.Done():
				return
			}
		}
	}()

	// 系統匣圖示與單一實例喚醒監聽(僅 Windows 有實作,其他平台為 no-op 存根)。
	a.tray = newTray()
	a.tray.start(ctx, a.showWindow, a.quitApp)
	a.singleInst.watch(a.showWindow)
}

// OnShutdown 在 Wails 關閉時優雅收束後端(不停任何遊戲容器),並移除系統匣圖示、釋放單一實例句柄。
func (a *App) OnShutdown(_ context.Context) {
	if a.tray != nil {
		a.tray.stop()
	}
	a.singleInst.close()
	if a.rt != nil {
		a.rt.Shutdown()
	}
}

// showWindow 顯示並還原主視窗(縮匣後由系統匣「開啟主視窗」/左鍵單擊,或第二實例喚醒觸發)。
func (a *App) showWindow() {
	wailsruntime.WindowShow(a.ctx)
	wailsruntime.WindowUnminimise(a.ctx)
}

// quitApp 結束整個應用(系統匣「結束」)。經 Wails 觸發既有 OnShutdown 收束後端與系統匣。
func (a *App) quitApp() {
	wailsruntime.Quit(a.ctx)
}

// bgCtx 回傳狀態變更操作用的 context(不隨個別前端呼叫取消;後端關閉時另有 root context 控制)。
func (a *App) bgCtx() context.Context { return context.Background() }

// ---- DTO ----

// TemplateDTO 是範本的前端視圖(供建立表單渲染)。
type TemplateDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Runtime 是建立表單應預選的「預設 runtime」(native-backend R12)。對齊 core.resolveRuntime
	// 的預設分派(Windows 且範本支援 native → native;否則支援 docker → docker),使表單預選與
	// 空 Runtime 建立實際落到的後端一致。
	Runtime string `json:"runtime"`
	// Runtimes 是範本宣告的可用 runtime 能力清單(由 [docker]/[native] 區段存在推導;native-backend
	// R3/R12)。順序固定 docker、native。供表單渲染 runtime 選項與置灰判斷。
	Runtimes []string     `json:"runtimes"`
	Variants []VariantDTO `json:"variants"`
	Params   []ParamDTO   `json:"params"`
	Secrets  []SecretDTO  `json:"secrets"`
	Ports    []PortDTO    `json:"ports"`
	Modpack  bool         `json:"modpack"`  // 是否支援模組包(R11)
	HasIcon  bool         `json:"has_icon"` // 範本是否有可服務的 icon(R14);true 時前端載入 /tpl-icons/{id},否則用佔位圖
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
	Key      string `json:"key"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
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
	UUID       string `json:"uuid"`
	TemplateID string `json:"template_id"`
	Variant    string `json:"variant"`
	// Name 是使用者指定的顯示名稱(R10);空字串時前端以「範本名 #uuid8」fallback。
	Name          string `json:"name"`
	Node          string `json:"node"`
	DesiredState  string `json:"desired_state"`
	ObservedState string `json:"observed_state"`
	// Runtime 是此實例的執行後端標記("native" | "docker";native-backend R12),由 RuntimeID
	// 前綴推導(見 runtimeFromID),供清單/卡片顯示 runtime badge。
	Runtime string `json:"runtime"`
	// Ports 是此實例已分配的連接埠(R12);name 用於區分遊戲埠與管理埠等角色(既有資料 name 為空)。
	Ports []InstancePortDTO `json:"ports"`
}

// InstancePortDTO 是實例已分配連接埠的前端視圖(R12)。Name 取自範本 PortSpec.Name(舊資料為空);
// 卡片位址欄依 name 對應範本第一個 required 埠決定顯示哪一項。
type InstancePortDTO struct {
	Name     string `json:"name"`
	BindIP   string `json:"bind_ip"`
	Protocol string `json:"protocol"`
	HostPort int    `json:"host_port"`
}

// SnapshotDTO 是聚合監控快照的前端視圖(R6)。
type SnapshotDTO struct {
	UUID          string                 `json:"uuid"`
	Monitored     bool                   `json:"monitored"`
	HasStats      bool                   `json:"has_stats"`
	Stats         protocol.ResourceStats `json:"stats"`
	PlayerCount   *int                   `json:"player_count"`
	Online        *bool                  `json:"online"`
	StartedAt     string                 `json:"started_at"` // 運行中實例啟動時刻(RFC3339;R11);停止/未知為空字串,前端顯示「—」
	ObservedState string                 `json:"observed_state"`
}

// CreateInstanceRequest 是建立實例的輸入。
type CreateInstanceRequest struct {
	TemplateID string            `json:"template_id"`
	Variant    string            `json:"variant"`
	Name       string            `json:"name"` // 使用者指定的顯示名稱(R10;可空)
	Params     map[string]string `json:"params"`
	Secrets    map[string]string `json:"secrets"`
	Node       string            `json:"node"`
	Modpack    *ModpackRequest   `json:"modpack"`
	// Runtime 為使用者所選執行後端("docker" | "native";空=依平台/範本能力預設,見
	// core.resolveRuntime)。native-backend R2/R12。
	Runtime string `json:"runtime"`
	// MemoryMB / CPUPercent 為 native 資源上限(Job Objects;native-backend R9),0=不限。
	// 僅 runtime=native 有意義;轉填 core CreateOptions.Resources(兩者皆 0 時為 nil)。
	MemoryMB   int `json:"memory_mb"`
	CPUPercent int `json:"cpu_percent"`
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
	Node   string `json:"node"`
	Online bool   `json:"online"`
	// DockerAvailable 表示本節點 Docker 能力是否就緒(節點在線與 Docker 能力分離;native-backend
	// R12/R13)。native 恆在使節點可在線但 Docker 仍不可用;供表單 docker 選項置灰判斷。
	// 僅本機節點反映實際值;遠端節點一律 false(能力未知,GUI 據 IsLocal 顯「未知」而非「無」)。
	DockerAvailable bool `json:"docker_available"`
	// IsLocal 為本機 in-process 節點;前端據此判定本機節點,不硬編 "local" 名稱。
	IsLocal bool   `json:"is_local"`
	LastErr string `json:"last_err"`
}

// ---- 範本 / 建立 ----

// ListTemplates 回傳所有可建立的遊戲範本(R1)。
func (a *App) ListTemplates() []TemplateDTO {
	tmpls := a.rt.Templates()
	out := make([]TemplateDTO, 0, len(tmpls))
	for _, t := range tmpls {
		dto := toTemplateDTO(t)
		_, dto.HasIcon = a.rt.TemplateIconPath(t.ID) // R14:engine 查得可服務 icon 才 true
		out = append(out, dto)
	}
	return out
}

// CreateInstance 一鍵建立實例(R2)。回傳新實例 UUID;失敗回可讀錯誤。
//
// 建立為同步阻塞:native 後端於 Create 內供應 JRE/伺服器檔案/SteamCMD。期間訂閱 native 供應
// 進度並轉推 Wails "provision" 事件供建立精靈顯示進度(native-backend R12)。訂閱為 best-effort:
// 失敗(節點離線等)不阻斷建立,僅無進度顯示。因建立精靈一次僅建一個實例,採全域(非以 uuid
// 索引)事件名——建立當下前端尚未取得 uuid。
func (a *App) CreateInstance(req CreateInstanceRequest) (string, error) {
	if ch, stop, err := a.rt.SubscribeProvision(a.bgCtx()); err == nil {
		defer stop()
		go func() {
			for p := range ch {
				// 全域 provision(建立精靈)+ per-uuid(伺服器主控台/全域操作面板);建立進度由後端
				// emitter 蓋 InstanceUUID(階段 3)。
				wailsruntime.EventsEmit(a.ctx, "provision", p)
				if p.InstanceUUID != "" {
					wailsruntime.EventsEmit(a.ctx, "provision:"+p.InstanceUUID, p)
				}
			}
		}()
	}
	rec, err := a.rt.Create(a.bgCtx(), req.toCreateOptions())
	if err != nil {
		return "", fmt.Errorf("建立實例失敗: %w", err)
	}
	return rec.UUID, nil
}

// toCreateOptions 把建立請求轉為 core.CreateOptions(含 native-backend R2 runtime 與 R9 資源上限)。
// 抽為獨立方法以利單元測轉填正確性,不需啟動整個後端。
func (req CreateInstanceRequest) toCreateOptions() core.CreateOptions {
	opts := core.CreateOptions{
		TemplateID: req.TemplateID,
		Variant:    req.Variant,
		Name:       req.Name,
		Params:     req.Params,
		Secrets:    req.Secrets,
		Node:       req.Node,
		Runtime:    req.Runtime,
	}
	if req.Modpack != nil && (req.Modpack.Type != "" || req.Modpack.Ref != "") {
		opts.Modpack = &core.ModpackSource{Type: core.ModpackType(req.Modpack.Type), Ref: req.Modpack.Ref}
	}
	if req.MemoryMB != 0 || req.CPUPercent != 0 {
		opts.Resources = &protocol.ResourceLimits{MemoryMB: req.MemoryMB, CPUPercent: req.CPUPercent}
	}
	return opts
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

// ListInstances 回傳所有實例(含 desired/observed 與已分配連接埠)。埠以單次查詢分組帶入,
// 避免對每個實例各查一次的 N+1(R12)。
func (a *App) ListInstances() ([]InstanceDTO, error) {
	recs, err := a.rt.Instances()
	if err != nil {
		return nil, err
	}
	portsByInst, err := a.rt.PortsByInstance()
	if err != nil {
		return nil, err
	}
	out := make([]InstanceDTO, 0, len(recs))
	for _, r := range recs {
		out = append(out, InstanceDTO{
			UUID: r.UUID, TemplateID: r.TemplateID, Variant: r.Variant, Name: r.Name, Node: r.Node,
			DesiredState: string(r.DesiredState), ObservedState: string(r.ObservedState),
			Runtime: runtimeFromID(r.RuntimeID),
			Ports:   toInstancePortDTOs(portsByInst[r.UUID]),
		})
	}
	return out, nil
}

// toInstancePortDTOs 把 core 埠預留轉為前端視圖(R12)。
func toInstancePortDTOs(ports []core.PortReservation) []InstancePortDTO {
	out := make([]InstancePortDTO, 0, len(ports))
	for _, p := range ports {
		out = append(out, InstancePortDTO{
			Name: p.Name, BindIP: p.BindIP, Protocol: p.Protocol, HostPort: p.HostPort,
		})
	}
	return out
}

// ---- 監控 ----

// GetSnapshot 回傳一個實例的聚合監控快照(R6)。
func (a *App) GetSnapshot(uuid string) (SnapshotDTO, error) {
	snap, ok := a.rt.Snapshot(uuid)
	if !ok {
		return SnapshotDTO{UUID: uuid, Monitored: false}, nil
	}
	startedAt := ""
	if snap.StartedAt != nil {
		startedAt = snap.StartedAt.UTC().Format(time.RFC3339)
	}
	return SnapshotDTO{
		UUID: snap.UUID, Monitored: true, HasStats: snap.HasStats, Stats: snap.Stats,
		PlayerCount: snap.PlayerCount, Online: snap.Online, StartedAt: startedAt,
		ObservedState: string(snap.ObservedState),
	}, nil
}

// MetricPointDTO 是聚合指標時序的前端視圖(R13)。TsUTC 為 15s bucket 起點(RFC3339,UTC);
// MemoryBytes/MemoryLimit/PlayerCount 為 nil 時表該點該欄不可採集/不適用,趨勢圖畫缺口不補 0。
type MetricPointDTO struct {
	TsUTC       string  `json:"ts_utc"`
	CPUPercent  float64 `json:"cpu_percent"`
	MemoryBytes *int64  `json:"memory_bytes"`
	MemoryLimit *int64  `json:"memory_limit"`
	PlayerCount *int    `json:"player_count"`
}

// QueryMetrics 回傳某實例自 sinceUnix(Unix 秒;含)起的聚合指標時序,按時間升冪(R13)。
// 供詳細頁趨勢圖回填歷史。
func (a *App) QueryMetrics(uuid string, sinceUnix int64) ([]MetricPointDTO, error) {
	pts, err := a.rt.QueryMetrics(uuid, time.Unix(sinceUnix, 0).UTC())
	if err != nil {
		return nil, err
	}
	return toMetricPointDTOs(pts), nil
}

// QueryMetricsSummary 回傳自 sinceUnix(Unix 秒;含)起、各 15s bucket 的全體平均(R13)。
// 供總覽頁全體 CPU/RAM 歷史趨勢。
func (a *App) QueryMetricsSummary(sinceUnix int64) ([]MetricPointDTO, error) {
	pts, err := a.rt.QueryMetricsSummary(time.Unix(sinceUnix, 0).UTC())
	if err != nil {
		return nil, err
	}
	return toMetricPointDTOs(pts), nil
}

// toMetricPointDTOs 把 core 指標點轉為前端視圖(NULL 欄位以 nil 指標透傳)。
func toMetricPointDTOs(pts []core.MetricPoint) []MetricPointDTO {
	out := make([]MetricPointDTO, 0, len(pts))
	for _, p := range pts {
		out = append(out, MetricPointDTO{
			TsUTC:       p.TsUTC.UTC().Format(time.RFC3339),
			CPUPercent:  p.CPUPercent,
			MemoryBytes: p.MemoryBytes,
			MemoryLimit: p.MemoryLimit,
			PlayerCount: p.PlayerCount,
		})
	}
	return out
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
		// B10:channel 關閉(核心 StopMonitoring)後清除 map 項,使停止再啟動同實例後 SubscribeLogs 能
		// 重新訂閱(否則冪等守衛因殘留項永久 no-op、即時 log 靜默)。僅當仍是本訂閱時清除(避免清掉新訂閱)。
		a.subMu.Lock()
		if cur, ok := a.logSubs[uuid]; ok && cur == id {
			delete(a.logSubs, uuid)
		}
		a.subMu.Unlock()
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
		// B10:同 SubscribeLogs——channel 關閉後清除 map 項,使停止再啟動同實例後可重新訂閱 stats。
		a.subMu.Lock()
		if cur, ok := a.statsSubs[uuid]; ok && cur == id {
			delete(a.statsSubs, uuid)
		}
		a.subMu.Unlock()
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

// DeleteBackup 刪除實例的一份備份(GUI 手動刪除;R9)。二次確認由前端負責,後端照令執行:
// 經節點代理刪 agent 端備份、清 store 中繼並記 BACKUP_DELETED 事件。
func (a *App) DeleteBackup(uuid, backupID string) error {
	return a.rt.DeleteBackup(a.bgCtx(), uuid, protocol.BackupID(backupID))
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

// NodeStatus 回傳所有節點狀態(離線顯示/重試依據)(R5)。DockerAvailable 為節點層 Docker 能力
// (與在線分離;native-backend R12/R13):單機下對本機節點填 a.rt.DockerAvailable()。
func (a *App) NodeStatus() []NodeStatusDTO {
	sts := a.rt.NodeStatuses()
	localNode := a.rt.Node()
	dockerOK := a.rt.DockerAvailable()
	out := make([]NodeStatusDTO, 0, len(sts))
	for _, s := range sts {
		isLocal := s.Node == localNode || s.Node == "local"
		// 遠端節點 Docker 能力未知(/health 未回報),不複製本機值以免誤導;GUI 據 IsLocal 顯「未知」。
		docker := false
		if isLocal {
			docker = dockerOK
		}
		out = append(out, NodeStatusDTO{Node: s.Node, Online: s.Online, DockerAvailable: docker, IsLocal: isLocal, LastErr: s.LastErr})
	}
	return out
}

// DockerAvailable 回報本節點 Docker 能力是否就緒(native-backend R12)。native 恆在使節點可在線但
// Docker 仍不可用;供建立表單決定是否置灰 docker 選項。
func (a *App) DockerAvailable() bool { return a.rt.DockerAvailable() }

// CurseForgeEnabled 回報本建置是否啟用 CurseForge 模組包(native-backend R14:建置內嵌或設定覆蓋了
// API key)。供建立表單決定是否顯示 native CurseForge 模組包來源選項(未啟用時隱藏/置灰並提示)。
func (a *App) CurseForgeEnabled() bool { return a.rt.CurseForgeEnabled() }

// SetCurseForgeAPIKey 設定(空字串=清除)使用者覆蓋的 CurseForge API 金鑰(native-backend R14:GUI
// 設定)。實值只落 OS 金鑰庫,永不明文落 config.json;設定後熱生效(免重啟),CurseForgeEnabled 與
// 建立精靈的 CurseForge 選項置灰即時反映。失敗回可讀錯誤。
func (a *App) SetCurseForgeAPIKey(key string) error {
	if err := a.rt.SetCurseForgeOverrideKey(key); err != nil {
		return fmt.Errorf("設定 CurseForge 金鑰失敗: %w", err)
	}
	return nil
}

// CurseForgeKeyConfigured 回報使用者是否已於金鑰庫設定「覆蓋」的 CurseForge API 金鑰(native-backend
// R14)。只回有無、不回明文。有別於 CurseForgeEnabled(後者尚含建置內嵌 key):供 GUI 區分三態——
// 使用者已設定(可清除)、僅內建金鑰啟用、皆無(停用)。
func (a *App) CurseForgeKeyConfigured() bool { return a.rt.CurseForgeOverrideKeySet() }

// RetryDocker 重試連線 Docker(節點離線時)(R5)。
func (a *App) RetryDocker() error { return a.rt.RetryDocker() }

// ---- 遠端節點管理(R5 多節點)----

// ProbeNodeResultDTO 是撥測(尚未新增)一個遠端節點的結果視圖。撥測成功時 OK=true、Version 帶回;
// https 撥測即使 token 未驗也會回 Fingerprint 供 GUI 顯示讓使用者確認(TOFU)。Error 非空=失敗(繁中)。
type ProbeNodeResultDTO struct {
	OK          bool   `json:"ok"`
	Fingerprint string `json:"fingerprint"`
	Version     string `json:"version"`
	Error       string `json:"error"`
}

// NodeInfoDTO 是節點清單的前端視圖(R5 多節點)。DockerAvailable 對本機節點反映實際 Docker 能力;
// 遠端節點目前一律 false(需額外查詢代理,留待後續)。Removable=false 表示不可移除("local")。
type NodeInfoDTO struct {
	Name            string `json:"name"`
	BaseURL         string `json:"base_url"`
	Online          bool   `json:"online"`
	DockerAvailable bool   `json:"docker_available"`
	Fingerprint     string `json:"fingerprint"`
	InsecureHTTP    bool   `json:"insecure_http"`
	LastErr         string `json:"last_err"`
	Removable       bool   `json:"removable"`
	IsLocal         bool   `json:"is_local"`
}

// AddNodeRequest 是新增遠端節點的輸入。Token 為 agent 啟動時印出的持久 bearer token;Fingerprint
// 為使用者於撥測後確認的 TLS 指紋(空=對端為正式憑證、走系統 CA);InsecureHTTP 允許明文(僅內網)。
type AddNodeRequest struct {
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	Token        string `json:"token"`
	Fingerprint  string `json:"fingerprint"`
	InsecureHTTP bool   `json:"insecure_http"`
}

// ProbeNode 撥測一個遠端節點(TOFU:回傳 TLS 指紋供使用者確認)。不持久化;失敗以 result.Error
// 繁中回報(不拋 error)。
func (a *App) ProbeNode(baseURL, token string, insecureHTTP bool) ProbeNodeResultDTO {
	res := a.rt.ProbeNode(baseURL, token, insecureHTTP)
	return ProbeNodeResultDTO{OK: res.OK, Fingerprint: res.Fingerprint, Version: res.Version, Error: res.Err}
}

// AddNode 新增並持久化一個遠端節點(token 入金鑰庫、設定入 config、即時註冊上線)。失敗回繁中錯誤。
func (a *App) AddNode(req AddNodeRequest) error {
	return a.rt.AddNode(req.Name, req.BaseURL, req.Token, req.Fingerprint, req.InsecureHTTP)
}

// RemoveNode 移除一個遠端節點(反註冊、清 config 與金鑰庫);掛有實例時拒絕。"local" 不可移除。
func (a *App) RemoveNode(name string) error { return a.rt.RemoveNode(name) }

// ListNodes 回傳所有節點的摘要(name/baseURL/online/dockerAvailable/fingerprint 等;R5 多節點)。
func (a *App) ListNodes() []NodeInfoDTO {
	infos := a.rt.ListNodes()
	out := make([]NodeInfoDTO, 0, len(infos))
	for _, n := range infos {
		out = append(out, NodeInfoDTO{
			Name: n.Name, BaseURL: n.BaseURL, Online: n.Online, DockerAvailable: n.DockerAvailable,
			Fingerprint: n.Fingerprint, InsecureHTTP: n.InsecureHTTP, LastErr: n.LastErr,
			Removable: n.Removable, IsLocal: n.IsLocal,
		})
	}
	return out
}

// ---- Docker 資源管理(階段 4)----

// ImageDTO 是 Docker 映像的前端視圖。Created 為 RFC3339;Containers 為使用該映像的容器數
// (docker 未計算時為 -1,前端顯「—」)。
type ImageDTO struct {
	ID         string   `json:"id"`
	Tags       []string `json:"tags"`
	SizeBytes  int64    `json:"size_bytes"`
	Created    string   `json:"created"`
	Containers int      `json:"containers"`
}

// ContainerDTO 是 Docker 容器的前端視圖。Name 取 Names[0] 去前導 "/";GSM 表本工具建立
// (gsm.managed-by 標籤);UUID 取 gsm.uuid 標籤(孤兒/非本工具容器為空)。
type ContainerDTO struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	State string `json:"state"`
	GSM   bool   `json:"gsm"`
	UUID  string `json:"uuid"`
}

// DiskUsageDTO 是實例宿主磁碟用量的前端視圖(資料/備份根位元組)。
type DiskUsageDTO struct {
	DataBytes   int64 `json:"data_bytes"`
	BackupBytes int64 `json:"backup_bytes"`
}

// PruneResultDTO 是清除懸掛映像的結果視圖(回收量 + 被刪 ID)。
type PruneResultDTO struct {
	ReclaimedBytes int64    `json:"reclaimed_bytes"`
	Deleted        []string `json:"deleted"`
}

// gsm.* 標籤鍵(對映 internal/agent docker.go 的 labelManagedBy/labelUUID/managedByValue;
// 於綁定層以字面值辨識容器歸屬,不引入對 agent 內部常數的相依)。
const (
	labelGSMManagedBy = "gsm.managed-by"
	labelGSMUUID      = "gsm.uuid"
	gsmManagedByValue = "servermonitor"
)

// ListImages 列出本機節點的 Docker 映像(階段 4)。native-only 節點回不支援錯誤。
func (a *App) ListImages() ([]ImageDTO, error) {
	imgs, err := a.rt.ListImages(a.bgCtx())
	if err != nil {
		return nil, err
	}
	out := make([]ImageDTO, 0, len(imgs))
	for _, im := range imgs {
		out = append(out, toImageDTO(im))
	}
	return out, nil
}

// RemoveImage 刪除一份映像(精確 id;force 對映 docker `-f`)。使用中映像的錯誤由後端原樣透傳。
func (a *App) RemoveImage(id string, force bool) error {
	return a.rt.RemoveImage(a.bgCtx(), id, force)
}

// PruneImages 清除本機節點的懸掛映像,回傳回收量與被刪 ID。
func (a *App) PruneImages() (PruneResultDTO, error) {
	res, err := a.rt.PruneImages(a.bgCtx())
	if err != nil {
		return PruneResultDTO{}, err
	}
	deleted := res.Deleted
	if deleted == nil {
		deleted = []string{}
	}
	return PruneResultDTO{ReclaimedBytes: res.ReclaimedBytes, Deleted: deleted}, nil
}

// ListContainers 列出本機節點所有 Docker 容器(含孤兒/非本工具建立)。
func (a *App) ListContainers() ([]ContainerDTO, error) {
	cs, err := a.rt.ListContainers(a.bgCtx())
	if err != nil {
		return nil, err
	}
	out := make([]ContainerDTO, 0, len(cs))
	for _, c := range cs {
		out = append(out, toContainerDTO(c))
	}
	return out, nil
}

// RemoveContainer 刪除一個容器(精確 id;force 亦刪執行中)。
func (a *App) RemoveContainer(id string, force bool) error {
	return a.rt.RemoveContainer(a.bgCtx(), id, force)
}

// InstanceDiskUsage 查詢某實例的宿主磁碟用量(資料/備份根)。
func (a *App) InstanceDiskUsage(uuid string) (DiskUsageDTO, error) {
	du, err := a.rt.InstanceDiskUsage(a.bgCtx(), uuid)
	if err != nil {
		return DiskUsageDTO{}, err
	}
	return DiskUsageDTO{DataBytes: du.DataBytes, BackupBytes: du.BackupBytes}, nil
}

// toImageDTO 轉換映像摘要;Tags 正規化為非 nil 切片(前端型別為 string[])。
func toImageDTO(im protocol.ImageSummary) ImageDTO {
	tags := im.Tags
	if tags == nil {
		tags = []string{}
	}
	return ImageDTO{
		ID: im.ID, Tags: tags, SizeBytes: im.SizeBytes,
		Created: im.CreatedUTC.Format(time.RFC3339), Containers: im.Containers,
	}
}

// toContainerDTO 轉換容器摘要(名稱去前導 "/",辨識 gsm.* 標籤)。
func toContainerDTO(c protocol.ContainerSummary) ContainerDTO {
	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	return ContainerDTO{
		ID: c.ID, Name: name, Image: c.Image, State: c.State,
		GSM:  c.Labels[labelGSMManagedBy] == gsmManagedByValue,
		UUID: c.Labels[labelGSMUUID],
	}
}

// ---- DTO 轉換 ----

// defaultRuntime 回傳建立表單應預選的預設 runtime,對齊 core.resolveRuntime 的預設分派
// (native-backend R2/R12):Windows 且範本支援 native → native;否則支援 docker → docker;
// 僅支援 native 的非 Windows 邊角回 native;皆非(理論上不可達,範本驗證保證至少宣告其一)
// 退回範本宣告的 runtime。goos 以參數注入以利單元測。
func defaultRuntime(t *protocol.GameTemplate, goos string) string {
	if goos == "windows" && t.SupportsNative() {
		return "native"
	}
	if t.SupportsDocker() {
		return "docker"
	}
	if t.SupportsNative() {
		return "native"
	}
	return t.Runtime
}

// runtimeFromID 由 RuntimeID 前綴推導實例的執行後端標記(native-backend R12):帶 "native:" 前綴
// → "native";其餘(裸容器 ID,既有 docker 實例零遷移)→ "docker"。
func runtimeFromID(id protocol.RuntimeID) string {
	if strings.HasPrefix(string(id), "native:") {
		return "native"
	}
	return "docker"
}

// templateRuntimes 回傳範本在本平台可選的執行後端清單,順序固定 docker、native(#13:native 僅
// Windows 可用,非 Windows 不列出——對齊 core.resolveRuntime 的平台約束,避免 Linux GUI 顯示無法
// 建立的 native 選項)。goos 以參數注入以利單元測。
func templateRuntimes(t *protocol.GameTemplate, goos string) []string {
	var rts []string
	if t.SupportsDocker() {
		rts = append(rts, "docker")
	}
	if goos == "windows" && t.SupportsNative() {
		rts = append(rts, "native")
	}
	return rts
}

func toTemplateDTO(t *protocol.GameTemplate) TemplateDTO {
	dto := TemplateDTO{ID: t.ID, Name: t.Name, Runtime: defaultRuntime(t, runtime.GOOS), Modpack: t.Mods != nil}
	dto.Runtimes = templateRuntimes(t, runtime.GOOS)
	for _, v := range t.Variants {
		dto.Variants = append(dto.Variants, VariantDTO{ID: v.ID, Loader: v.Loader})
	}
	for _, p := range t.Params {
		dto.Params = append(dto.Params, ParamDTO{Key: p.Key, Label: p.Label, Type: p.Type, Default: p.Default, Required: p.Required})
	}
	for _, s := range t.Secrets {
		dto.Secrets = append(dto.Secrets, SecretDTO{Key: s.Key, Label: s.Label, Required: s.Required})
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
