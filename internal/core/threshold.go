package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// 資源門檻評估的關鍵常數預設(R10「關鍵常數預設」)。
const (
	defaultThresholdPersist    = 60 * time.Second // 持續超標窗口:須連續超標達此時長才觸發
	defaultThresholdHysteresis = 0.10             // 遲滯比例:降到門檻 × (1-此比例) 才解除
)

// thresholdSettingPrefix 是 DB settings 表中資源門檻設定鍵的前綴(見 store_records.go
// GetSetting/SetSetting;既有機制,本檔不新增 schema)。
const thresholdSettingPrefix = "alert.threshold."

// thresholdSettingKey 回傳實例 uuid 對應的設定鍵。
func thresholdSettingKey(uuid string) string {
	return thresholdSettingPrefix + uuid
}

// ThresholdConfig 是使用者為一實例設定的資源告警門檻,以 JSON 存於 Store settings
// (鍵見 thresholdSettingKey)。個別欄位 <=0 視為該項門檻未啟用。
type ThresholdConfig struct {
	CPUPercent    float64 `json:"cpu_percent,omitempty"`    // CPU 使用率門檻(%);<=0 不啟用
	MemoryPercent float64 `json:"memory_percent,omitempty"` // 記憶體使用率門檻(佔 MemoryLimit 的 %);<=0 不啟用
}

// LoadThresholdConfig 從 Store settings 讀取一實例的資源門檻設定;未設定回零值(兩項門檻皆不啟用)。
func LoadThresholdConfig(store *Store, uuid string) (ThresholdConfig, error) {
	var cfg ThresholdConfig
	if store == nil {
		return cfg, nil
	}
	raw, ok, err := store.GetSetting(thresholdSettingKey(uuid))
	if err != nil {
		return cfg, fmt.Errorf("讀取資源門檻設定失敗: %w", err)
	}
	if !ok || raw == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("解析資源門檻設定失敗: %w", err)
	}
	return cfg, nil
}

// SaveThresholdConfig 把一實例的資源門檻設定寫入 Store settings。
func SaveThresholdConfig(store *Store, uuid string, cfg ThresholdConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化資源門檻設定失敗: %w", err)
	}
	if err := store.SetSetting(thresholdSettingKey(uuid), string(raw)); err != nil {
		return fmt.Errorf("寫入資源門檻設定失敗: %w", err)
	}
	return nil
}

// statsSource 是資源取樣的消費端介面(*MonitorHub 天然滿足,見本檔尾的靜態斷言);
// 測試以 fake channel 注入,不需真實 WS/Docker。
type statsSource interface {
	SubscribeStats(uuid string) (<-chan protocol.ResourceStats, SubID)
	UnsubscribeStats(uuid string, id SubID)
}

// metricState 是單一 (instance, metric) 的持續超標狀態機狀態。
type metricState struct {
	exceededSince time.Time // 零值 = 目前未處於「超標中」;否則為本次連續超標起點
	armed         bool      // 是否已觸發且尚未依遲滯解除(解除前不重複觸發同一次超標)
}

// ThresholdMonitorConfig 是 ThresholdMonitor 的建構參數。
type ThresholdMonitorConfig struct {
	Store  *Store    // 讀門檻設定(GetSetting)與實例 node/template(enrich);nil 則門檻恆不啟用
	Events *EventLog // 記 RESOURCE_THRESHOLD / ALERT_SENT / ALERT_FAILED;nil 則略過記錄
	Alerts AlertSink // nil 用 NopAlertSink
	Stats  statsSource

	Now        func() time.Time
	Persist    time.Duration // 持續超標窗口;<=0 用 60 秒
	Hysteresis float64       // 遲滯比例;<=0 用 0.10(10%)
}

// ThresholdMonitor 實作 R10 的資源門檻評估:對受監控實例的 CPU/記憶體取樣,以「持續窗口
// 60 秒 + 遲滯(門檻-10% 解除)」的邊緣觸發狀態機決定何時經 AlertSink 告警——避免每筆取樣
// (通常數秒一筆)都轟炸告警。同一 dedup key 的 cooldown 去重則由 AlertDispatcher(呼叫端)
// 負責,本檔不重複實作。
type ThresholdMonitor struct {
	store  *Store
	events *EventLog
	alerts AlertSink
	stats  statsSource

	now        func() time.Time
	persist    time.Duration
	hysteresis float64

	mu       sync.Mutex
	watchers map[string]watcherHandle      // uuid → 正在運行的訂閱(供 Stop)
	states   map[string]*instanceThreshold // uuid → 每實例的 cpu/mem 狀態

	// testHook 於每次 Process 結束時呼叫(uuid);production 恆為 nil。僅供同套件測試在
	// Watch/Stop 的非同步 channel 消費路徑上同步斷言(避免「已推入樣本」與「已處理樣本」
	// 之間的資料競態),不影響任何正式行為。
	testHook func(uuid string)
}

type watcherHandle struct {
	subID  SubID
	cancel context.CancelFunc
}

type instanceThreshold struct {
	cpu metricState
	mem metricState
}

// NewThresholdMonitor 建立資源門檻評估器。
func NewThresholdMonitor(cfg ThresholdMonitorConfig) *ThresholdMonitor {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	persist := cfg.Persist
	if persist <= 0 {
		persist = defaultThresholdPersist
	}
	hysteresis := cfg.Hysteresis
	if hysteresis <= 0 {
		hysteresis = defaultThresholdHysteresis
	}
	alerts := cfg.Alerts
	if alerts == nil {
		alerts = NopAlertSink{}
	}
	return &ThresholdMonitor{
		store:      cfg.Store,
		events:     cfg.Events,
		alerts:     alerts,
		stats:      cfg.Stats,
		now:        now,
		persist:    persist,
		hysteresis: hysteresis,
		watchers:   make(map[string]watcherHandle),
		states:     make(map[string]*instanceThreshold),
	}
}

// Watch 開始監看一個實例的資源取樣(冪等:已在監看則 no-op)。由生命週期編排在實例轉為
// Running 且已 StartMonitoring 之後呼叫;停止監看見 Stop。
func (tm *ThresholdMonitor) Watch(uuid string) {
	if tm.stats == nil {
		return
	}
	tm.mu.Lock()
	if _, ok := tm.watchers[uuid]; ok {
		tm.mu.Unlock()
		return
	}
	ch, id := tm.stats.SubscribeStats(uuid)
	ctx, cancel := context.WithCancel(context.Background())
	tm.watchers[uuid] = watcherHandle{subID: id, cancel: cancel}
	if _, ok := tm.states[uuid]; !ok {
		tm.states[uuid] = &instanceThreshold{}
	}
	tm.mu.Unlock()

	go tm.run(ctx, uuid, ch)
}

// Stop 停止監看一個實例並清除其狀態機記憶(冪等)。
func (tm *ThresholdMonitor) Stop(uuid string) {
	tm.mu.Lock()
	h, ok := tm.watchers[uuid]
	if ok {
		delete(tm.watchers, uuid)
	}
	delete(tm.states, uuid)
	tm.mu.Unlock()
	if ok {
		h.cancel()
		tm.stats.UnsubscribeStats(uuid, h.subID)
	}
}

// run 消費一個實例的 stats channel,逐筆餵給 Process,直到 channel 關閉或 ctx 取消。
func (tm *ThresholdMonitor) run(ctx context.Context, uuid string, ch <-chan protocol.ResourceStats) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-ch:
			if !ok {
				return
			}
			tm.Process(uuid, s)
		}
	}
}

// Process 對一筆資源取樣套用持續窗口 + 遲滯狀態機,判斷是否需要觸發告警。為主要可測試入口:
// 呼叫端(生產為 run 的 channel 消費迴圈;測試可直接呼叫)以注入的 Now() 驅動時間前進。
func (tm *ThresholdMonitor) Process(uuid string, s protocol.ResourceStats) {
	if tm.testHook != nil {
		defer tm.testHook(uuid)
	}

	cfg, err := LoadThresholdConfig(tm.store, uuid)
	if err != nil || (cfg.CPUPercent <= 0 && cfg.MemoryPercent <= 0) {
		return // 讀取失敗或兩項門檻皆未啟用:不評估
	}

	tm.mu.Lock()
	st, ok := tm.states[uuid]
	if !ok {
		st = &instanceThreshold{}
		tm.states[uuid] = st
	}
	tm.mu.Unlock()

	now := tm.now()

	if cfg.CPUPercent > 0 {
		tm.evaluate(uuid, "cpu", s.CPUPercent, cfg.CPUPercent, &st.cpu, now)
	}
	if cfg.MemoryPercent > 0 && s.MemoryLimit > 0 {
		memPct := float64(s.MemoryBytes) / float64(s.MemoryLimit) * 100
		tm.evaluate(uuid, "memory", memPct, cfg.MemoryPercent, &st.mem, now)
	}
}

// evaluate 是單一 (instance, metric) 的邊緣觸發狀態機:
//   - 未 armed 且超過門檻:記錄/延續超標起點;連續超標達 persist → armed + 觸發告警。
//   - 未 armed 且未超過門檻(含抖動短暫超標又回落):重置超標起點,不觸發。
//   - 已 armed:降到門檻 × (1-hysteresis) 才解除 armed(遲滯),解除後才可能因下一輪
//     連續超標再度觸發;armed 期間維持超標不重複觸發(避免每筆取樣重送——去重本身另由
//     AlertDispatcher 的 cooldown 負責,此處僅負責「何時算一次新的超標」)。
func (tm *ThresholdMonitor) evaluate(uuid, metric string, value, threshold float64, st *metricState, now time.Time) {
	if st.armed {
		releaseAt := threshold * (1 - tm.hysteresis)
		if value <= releaseAt {
			st.armed = false
			st.exceededSince = time.Time{}
		}
		return
	}
	if value <= threshold {
		st.exceededSince = time.Time{} // 抖動/回落:重置連續計時
		return
	}
	if st.exceededSince.IsZero() {
		st.exceededSince = now
	}
	if now.Sub(st.exceededSince) >= tm.persist {
		st.armed = true
		tm.fire(uuid, metric, value, threshold, now)
	}
}

// fire 記 RESOURCE_THRESHOLD 事件並經 AlertSink 告警,依結果記 ALERT_SENT/ALERT_FAILED——
// 沿用 Scheduler.fireBackup / RestartPolicy.dispatchAlert 的既有慣例(呼叫端自行記錄結果)。
func (tm *ThresholdMonitor) fire(uuid, metric string, value, threshold float64, now time.Time) {
	details := map[string]any{
		"metric":            metric,
		"value_percent":     value,
		"threshold_percent": threshold,
	}
	ev := protocol.Event{
		Code:         EventResourceThreshold,
		Severity:     protocol.SeverityWarning,
		InstanceUUID: strPtr(uuid),
		TsUTC:        now,
		DetailsJSON:  mustJSON(details),
	}
	tm.enrich(&ev, uuid)
	if tm.events != nil {
		_ = tm.events.Append(ev)
	}

	ctx, cancel := context.WithTimeout(context.Background(), alertDispatchTimeout)
	defer cancel()
	if err := tm.alerts.Alert(ctx, ev); err != nil {
		tm.recordAlertResult(ev, protocol.EventAlertFailed, protocol.SeverityWarning, err.Error())
		return
	}
	tm.recordAlertResult(ev, protocol.EventAlertSent, protocol.SeverityInfo, "")
}

// recordAlertResult 記一筆 ALERT_SENT / ALERT_FAILED 事件(結構同 RestartPolicy 的同名方法)。
func (tm *ThresholdMonitor) recordAlertResult(src protocol.Event, code protocol.EventCode, sev protocol.Severity, errMsg string) {
	if tm.events == nil {
		return
	}
	details := map[string]any{"alert_code": string(src.Code)}
	if errMsg != "" {
		details["error"] = errMsg
	}
	ev := protocol.Event{
		Code:         code,
		Severity:     sev,
		InstanceUUID: src.InstanceUUID,
		Node:         src.Node,
		TemplateID:   src.TemplateID,
		DetailsJSON:  mustJSON(details),
	}
	_ = tm.events.Append(ev)
}

// enrich 以實例記錄補上 node/template 欄位(best-effort;查不到則留空)。
func (tm *ThresholdMonitor) enrich(ev *protocol.Event, uuid string) {
	if tm.store == nil {
		return
	}
	if rec, err := tm.store.GetInstance(uuid); err == nil {
		ev.Node = strPtr(rec.Node)
		ev.TemplateID = strPtr(rec.TemplateID)
	}
}

// 確保 *MonitorHub 滿足 statsSource(單機直接注入,無需配接層)。
var _ statsSource = (*MonitorHub)(nil)
