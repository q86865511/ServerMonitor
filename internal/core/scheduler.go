package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// 排程種類。
const (
	ScheduleKindRestart = "restart"
	ScheduleKindBackup  = "backup"
)

// defaultSchedulerTick 是排程輪詢週期。以 <60s 週期輪詢並用「分鐘去重」確保每個到點分鐘至多
// 觸發一次;GUI 關閉期間不輪詢 → 錯過的到點不補跑(rev.3 R8 定案:僅記錄,不 backfill)。
const defaultSchedulerTick = 30 * time.Second

// ScheduleSpec 是排程的觸發規則(存 schedules.spec_json)。時間一律以 UTC 判定,規避時區/DST。
// 首版支援「每日/每週固定時刻」:At 為 "HH:MM"(24 小時、UTC);Weekdays 為 0=週日..6=週六
// 的集合,空集合表示每天。此格式刻意簡明且無 DST 歧義(不引入 cron 依賴)。
type ScheduleSpec struct {
	At       string `json:"at"`                 // "HH:MM" 24h UTC
	Weekdays []int  `json:"weekdays,omitempty"` // 0=Sun..6=Sat;空=每天
}

// parseScheduleSpec 解析並驗證 spec_json。
func parseScheduleSpec(raw json.RawMessage) (ScheduleSpec, int, int, error) {
	var s ScheduleSpec
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, 0, 0, fmt.Errorf("解析排程 spec 失敗: %w", err)
		}
	}
	h, m, err := parseHHMM(s.At)
	if err != nil {
		return s, 0, 0, err
	}
	for _, wd := range s.Weekdays {
		if wd < 0 || wd > 6 {
			return s, 0, 0, fmt.Errorf("排程 weekday 超出範圍(0..6): %d", wd)
		}
	}
	return s, h, m, nil
}

// parseHHMM 解析 "HH:MM" 為時、分;格式錯誤回明確錯誤。
func parseHHMM(at string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(at), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("排程時刻須為 HH:MM(UTC),得 %q", at)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("排程時刻小時無效: %q", at)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("排程時刻分鐘無效: %q", at)
	}
	return h, m, nil
}

// matches 判斷 now(以 UTC 檢視)是否落在 spec 的到點分鐘。
func (s ScheduleSpec) matches(now time.Time, h, m int) bool {
	u := now.UTC()
	if u.Hour() != h || u.Minute() != m {
		return false
	}
	if len(s.Weekdays) == 0 {
		return true
	}
	wd := int(u.Weekday()) // Sunday=0
	for _, d := range s.Weekdays {
		if d == wd {
			return true
		}
	}
	return false
}

// SchedulerConfig 是 Scheduler 的建構參數。動作函式由 app 層以 Orchestrator/CommandService/
// NodeRegistry 組裝注入,使排程邏輯可獨立測試:
//   - Restart:通常為 Orchestrator.Restart(內部持 per-instance lock,不與備份/還原互撞)。
//   - Announce:重啟前的最佳努力公告(通常包 CommandService.Announce 帶固定訊息);nil 則略過。
//   - Backup:通常為「Orchestrator.RunLocked 內呼叫 NodeClient.Backup」的組合(共用 per-instance
//     lock);回傳 BackupMeta 供記錄。
type SchedulerConfig struct {
	Store  *Store
	Events *EventLog
	Alerts AlertSink // nil 用 NopAlertSink
	Now    func() time.Time
	Tick   time.Duration // <=0 用 30s

	Restart  func(ctx context.Context, uuid string) error
	Announce func(ctx context.Context, uuid string) error
	Backup   func(ctx context.Context, uuid string) (protocol.BackupMeta, error)
}

// Scheduler 依 schedules 表以 UTC 判定並觸發排程重啟/備份(R8)。提供排程 CRUD 包裝。
// GUI 關閉後重開不補跑錯過的到點(僅在 app 執行時輪詢當下時刻)。
type Scheduler struct {
	store    *Store
	events   *EventLog
	alerts   AlertSink
	now      func() time.Time
	tick     time.Duration
	restart  func(ctx context.Context, uuid string) error
	announce func(ctx context.Context, uuid string) error
	backup   func(ctx context.Context, uuid string) (protocol.BackupMeta, error)

	mu        sync.Mutex
	lastFired map[string]time.Time // scheduleID → 已觸發的到點分鐘(去重,防同分鐘重複)
	wg        sync.WaitGroup       // 追蹤 Tick 派出、尚未完成的動作 goroutine(#5;供 Wait 同步)
}

// NewScheduler 建立 Scheduler。
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	tick := cfg.Tick
	if tick <= 0 {
		tick = defaultSchedulerTick
	}
	alerts := cfg.Alerts
	if alerts == nil {
		alerts = NopAlertSink{}
	}
	return &Scheduler{
		store:     cfg.Store,
		events:    cfg.Events,
		alerts:    alerts,
		now:       now,
		tick:      tick,
		restart:   cfg.Restart,
		announce:  cfg.Announce,
		backup:    cfg.Backup,
		lastFired: make(map[string]time.Time),
	}
}

// Run 以 tick 週期評估到期排程,阻塞至 ctx 取消。進入輪詢迴圈前先評估一次當下時刻(#4):
// 若不等待首個 ticker 週期,開機時機落在某排程到點分鐘的後段就可能被錯過整個週期。
func (s *Scheduler) Run(ctx context.Context) {
	s.Tick(ctx, s.now())
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx, s.now())
		}
	}
}

// Wait 阻塞至所有由 Tick 派出、尚未完成的動作 goroutine 結束。正式執行路徑不需呼叫
// (Scheduler 停止不必等動作完成,動作自身以 per-instance lock 序列化);供測試同步斷言用。
func (s *Scheduler) Wait() {
	s.wg.Wait()
}

// Tick 以 now 評估所有啟用排程,對到點且該分鐘尚未觸發者「認領」後以獨立 goroutine 執行
// 對應動作(#5):認領(claim)本身同步完成,故回傳值仍準確反映本次到點分鐘觸發了哪些排程
// (供測試斷言);但動作(尤其備份)可能耗時數分鐘,同步執行會阻塞 ticker 導致其他排程到點
// 分鐘流逝而錯過,故動作派給 goroutine 非同步執行。公開以利測試以 fake clock 逐次驅動。
func (s *Scheduler) Tick(ctx context.Context, now time.Time) []string {
	recs, err := s.store.ListSchedules()
	if err != nil {
		return nil
	}
	minute := now.UTC().Truncate(time.Minute)
	var fired []string
	for _, rec := range recs {
		if !rec.Enabled {
			continue
		}
		spec, h, m, perr := parseScheduleSpec(rec.SpecJSON)
		if perr != nil {
			continue // 無效排程略過(建立時已驗證;此處防禦性)
		}
		if !spec.matches(now, h, m) {
			continue
		}
		if !s.claim(rec, minute) {
			continue // 本到點分鐘已觸發(記憶體或 DB 已記錄——後者涵蓋同分鐘 app 重啟,#6)
		}
		fired = append(fired, rec.ID)
		s.wg.Add(1)
		go func(rec ScheduleRecord) {
			defer s.wg.Done()
			s.fire(ctx, rec)
		}(rec)
	}
	return fired
}

// claim 標記 rec 於 minute 已觸發;若記憶體已標同一分鐘回 false。記憶體無記錄時再查
// rec.LastFiredUTC(來自本次 ListSchedules 讀到的 DB 值)——若相等,代表另一個 Scheduler
// 實例(同分鐘內 app 重啟後新建)已觸發過,同樣回 false 並補記憶體快取,避免重觸發(#6)。
// 成功認領後同步寫回 DB,供之後的實例讀到。
func (s *Scheduler) claim(rec ScheduleRecord, minute time.Time) bool {
	s.mu.Lock()
	last, hasMem := s.lastFired[rec.ID]
	if hasMem && last.Equal(minute) {
		s.mu.Unlock()
		return false
	}
	if !hasMem && rec.LastFiredUTC != nil && rec.LastFiredUTC.Equal(minute) {
		s.lastFired[rec.ID] = minute
		s.mu.Unlock()
		return false
	}
	s.lastFired[rec.ID] = minute
	s.mu.Unlock()
	if s.store != nil {
		_ = s.store.MarkScheduleFired(rec.ID, minute)
	}
	return true
}

// fire 依 kind 執行排程動作。
func (s *Scheduler) fire(ctx context.Context, rec ScheduleRecord) {
	switch rec.Kind {
	case ScheduleKindRestart:
		s.fireRestart(ctx, rec.InstanceUUID)
	case ScheduleKindBackup:
		s.fireBackup(ctx, rec.InstanceUUID)
	}
}

// fireRestart 排程重啟:先最佳努力公告(範本有 hooks.announce 才發、失敗不阻擋)→ 重啟。
func (s *Scheduler) fireRestart(ctx context.Context, uuid string) {
	if s.announce != nil {
		_ = s.announce(ctx, uuid) // best-effort:無 hook / 失敗皆不阻擋重啟
	}
	if s.restart != nil {
		_ = s.restart(ctx, uuid)
	}
}

// fireBackup 排程備份:記 BACKUP_STARTED → 觸發備份 → COMPLETED / FAILED(失敗經 AlertSink)。
// 備份的 planned-stop 一致性語意由 R9/T12 完整化;本任務僅觸發並記錄。
func (s *Scheduler) fireBackup(ctx context.Context, uuid string) {
	s.record(uuid, protocol.EventBackupStarted, protocol.SeverityInfo, nil)
	if s.backup == nil {
		return
	}
	meta, err := s.backup(ctx, uuid)
	if err != nil {
		ev := s.record(uuid, protocol.EventBackupFailed, protocol.SeverityError,
			map[string]any{"error": err.Error()})
		if aerr := s.alerts.Alert(ctx, ev); aerr != nil {
			s.record(uuid, protocol.EventAlertFailed, protocol.SeverityWarning,
				map[string]any{"alert_code": string(protocol.EventBackupFailed), "error": aerr.Error()})
		} else {
			s.record(uuid, protocol.EventAlertSent, protocol.SeverityInfo,
				map[string]any{"alert_code": string(protocol.EventBackupFailed)})
		}
		return
	}
	if err := s.store.PutBackup(meta); err != nil {
		// 中繼寫入失敗不影響備份本體(agent 擁有備份根);記為警告。
		s.record(uuid, protocol.EventBackupCompleted, protocol.SeverityWarning,
			map[string]any{"backup_id": string(meta.BackupID), "meta_persist_error": err.Error()})
		return
	}
	s.record(uuid, protocol.EventBackupCompleted, protocol.SeverityInfo,
		map[string]any{"backup_id": string(meta.BackupID)})
}

// record 追加一筆實例相關事件並回傳之(補 node/template 欄位)。
func (s *Scheduler) record(uuid string, code protocol.EventCode, sev protocol.Severity, details map[string]any) protocol.Event {
	ev := protocol.Event{
		Code:         code,
		Severity:     sev,
		InstanceUUID: strPtr(uuid),
	}
	if details != nil {
		ev.DetailsJSON = mustJSON(details)
	}
	if s.store != nil {
		if rec, err := s.store.GetInstance(uuid); err == nil {
			ev.Node = strPtr(rec.Node)
			ev.TemplateID = strPtr(rec.TemplateID)
		}
	}
	if s.events != nil {
		_ = s.events.Append(ev)
	}
	return ev
}

// ---- 排程 CRUD 包裝(存 schedules 表)----

// CreateSchedule 新增一筆排程(自動產生 ID);驗證 kind 與 spec 合法。回傳新 ID。
func (s *Scheduler) CreateSchedule(uuid, kind string, spec ScheduleSpec, enabled bool) (string, error) {
	if err := validateKind(kind); err != nil {
		return "", err
	}
	raw := mustJSON(spec)
	if _, _, _, err := parseScheduleSpec(raw); err != nil {
		return "", err
	}
	id := newUUIDv4()
	rec := ScheduleRecord{ID: id, InstanceUUID: uuid, Kind: kind, SpecJSON: raw, Enabled: enabled}
	if err := s.store.UpsertSchedule(rec); err != nil {
		return "", err
	}
	return id, nil
}

// UpdateSchedule 覆寫一筆既有排程(以 id 為鍵);驗證 kind 與 spec。
func (s *Scheduler) UpdateSchedule(id, uuid, kind string, spec ScheduleSpec, enabled bool) error {
	if id == "" {
		return fmt.Errorf("排程 id 不可為空")
	}
	if err := validateKind(kind); err != nil {
		return err
	}
	raw := mustJSON(spec)
	if _, _, _, err := parseScheduleSpec(raw); err != nil {
		return err
	}
	rec := ScheduleRecord{ID: id, InstanceUUID: uuid, Kind: kind, SpecJSON: raw, Enabled: enabled}
	return s.store.UpsertSchedule(rec)
}

// DeleteSchedule 刪除一筆排程並清除其去重狀態。
func (s *Scheduler) DeleteSchedule(id string) error {
	if err := s.store.DeleteSchedule(id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.lastFired, id)
	s.mu.Unlock()
	return nil
}

// ListSchedules 回傳某實例的排程(uuid 為空則回全部),依 ID 排序。
func (s *Scheduler) ListSchedules(uuid string) ([]ScheduleRecord, error) {
	all, err := s.store.ListSchedules()
	if err != nil {
		return nil, err
	}
	if uuid == "" {
		return all, nil
	}
	out := make([]ScheduleRecord, 0, len(all))
	for _, r := range all {
		if r.InstanceUUID == uuid {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// validateKind 驗證排程種類。
func validateKind(kind string) error {
	switch kind {
	case ScheduleKindRestart, ScheduleKindBackup:
		return nil
	default:
		return fmt.Errorf("未知排程種類 %q(須為 restart|backup)", kind)
	}
}
