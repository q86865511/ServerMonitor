package core

import (
	"context"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// 崩潰復原的關鍵常數預設(R8「關鍵常數預設」)。
const (
	defaultRestartWindow   = 5 * time.Minute // 崩潰迴圈計數窗口
	defaultRestartMax      = 3               // 窗口內崩潰次數達此上限→放棄自動重試
	defaultRestartBackoff  = 5 * time.Second // 重試退避起始
	defaultRestartMaxDelay = 60 * time.Second
)

// RestartPolicyConfig 是 RestartPolicy 的建構參數。
type RestartPolicyConfig struct {
	Store  *Store
	Events *EventLog
	Alerts AlertSink // nil 用 NopAlertSink
	Now    func() time.Time

	// Window 崩潰計數的滑動窗口長度;<=0 用 5 分鐘。窗口內崩潰次數達 MaxAttempts→放棄。
	// 「成功穩定運行後重置計數」由滑動窗口自然達成:穩定運行(無崩潰)超過窗口長度後,舊的
	// 崩潰時間戳老化滑出窗口,計數即歸零。
	Window time.Duration
	// MaxAttempts 窗口內達此崩潰次數→停止自動重試 + RESTART_GIVEUP 告警;<=0 用 3。
	MaxAttempts int
	// Backoff 由「本窗口內第 n 次崩潰」計算重試前的退避;nil 用指數退避(5s 起,上限 60s)。
	Backoff func(attempt int) time.Duration

	// BaseCtx 重試呼叫的基底 context(app 關閉時取消,停止未決重試);nil 用 context.Background。
	BaseCtx context.Context
	// after 排程一個延後執行(退避重試/告警分派);nil 用 time.AfterFunc。測試可注入同步版本。
	after func(d time.Duration, f func())
}

// RestartPolicy 接 Orchestrator 的 CrashHook,實作 R8 自動重啟/崩潰復原:
//   - 非計畫崩潰(handleDie/RecoverStuck 判定,計畫停止已由 planned-stop token 濾除)→ 退避後重試 Start。
//   - 重試本身失敗(如代理暫離線、就緒逾時)→ 視為一次新嘗試入帳,遞增退避續排,不斷鏈。
//   - 滑動窗口內嘗試次數達上限 → 停止自動重試、記 RESTART_GIVEUP、經 AlertSink 告警、標實例 Error。
//
// OnCrash 於 Orchestrator 的 per-instance lock 內被呼叫(crashHook 契約),但窗口記帳只靠 rp.mu 保護,
// 與「重試失敗於 lock 外續排」的路徑共用同一 rp.mu,故兩者可安全併發。所有會取用 per-instance lock 的
// 副作用(重試 Start、放棄時的 markGiveup、告警分派)一律排到 lock 之外的延後執行(after):既避免與
// 持鎖路徑死結,也使 giveup 的狀態寫入不論從「崩潰(持鎖)」或「重試失敗(不持鎖)」哪條路徑進入,都在
// lock 之外統一由 markGiveup 自行取鎖序列化。
type RestartPolicy struct {
	store   *Store
	events  *EventLog
	alerts  AlertSink
	now     func() time.Time
	window  time.Duration
	maxAtt  int
	backoff func(attempt int) time.Duration
	baseCtx context.Context
	after   func(d time.Duration, f func())

	// restart 執行一次重啟(通常為 Orchestrator.Start);由 SetRestart 於建構後注入(避免與
	// Orchestrator 的建構循環相依)。onGiveup 於放棄時標實例 Error(通常為 Orchestrator.markGiveup;
	// 該實作自行取用 per-instance lock,故僅可經 after 於 lock 外派發,切勿於持鎖路徑同步呼叫)。
	mu       sync.Mutex
	restart  func(ctx context.Context, uuid string) error
	onGiveup func(uuid string)
	windows  map[string][]time.Time // 每實例窗口內的崩潰時間戳
}

// NewRestartPolicy 建立 RestartPolicy。建構後須以 SetRestart 注入重啟函式方能自動重試。
func NewRestartPolicy(cfg RestartPolicyConfig) *RestartPolicy {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	window := cfg.Window
	if window <= 0 {
		window = defaultRestartWindow
	}
	maxAtt := cfg.MaxAttempts
	if maxAtt <= 0 {
		maxAtt = defaultRestartMax
	}
	backoff := cfg.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	alerts := cfg.Alerts
	if alerts == nil {
		alerts = NopAlertSink{}
	}
	baseCtx := cfg.BaseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	after := cfg.after
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	return &RestartPolicy{
		store:   cfg.Store,
		events:  cfg.Events,
		alerts:  alerts,
		now:     now,
		window:  window,
		maxAtt:  maxAtt,
		backoff: backoff,
		baseCtx: baseCtx,
		after:   after,
		windows: make(map[string][]time.Time),
	}
}

// SetRestart 注入重啟與放棄回呼(通常 restart=Orchestrator.Start、onGiveup=Orchestrator.markGiveup)。
// 於建構 Orchestrator 之後、開始消費事件之前呼叫。
func (rp *RestartPolicy) SetRestart(restart func(ctx context.Context, uuid string) error, onGiveup func(uuid string)) {
	rp.mu.Lock()
	rp.restart = restart
	rp.onGiveup = onGiveup
	rp.mu.Unlock()
}

// OnCrash 是 Orchestrator CrashHook 的實作(於 per-instance lock 內被呼叫)。記一次崩潰嘗試,
// 判斷是否達上限:未達→排程退避重試;達上限→放棄並告警。實作委給 recordAttempt(重試失敗時
// 亦由該函式再度入帳,故兩處共用同一記帳/退避/放棄邏輯)。
func (rp *RestartPolicy) OnCrash(uuid string, exitCode *int) {
	rp.recordAttempt(uuid, exitCode)
}

// recordAttempt 把一次「重啟嘗試」(崩潰或重試失敗皆算)記入滑動窗口,並依窗口內累計次數決定:
//   - 未達上限 → 排程退避重試(delay 隨累計次數遞增);重試失敗會再度呼叫本函式續排,不斷鏈。
//   - 達上限   → 記 RESTART_GIVEUP,並經 after 於 per-instance lock 之外派發「標 Error + 告警」。
//
// 窗口記帳僅靠 rp.mu 保護,故可從持有 per-instance lock 的崩潰路徑、或不持鎖的重試失敗路徑安全併發。
// 一切會取用 per-instance lock 的副作用(重試 Start、markGiveup)都排到 after,避免與持鎖路徑死結。
func (rp *RestartPolicy) recordAttempt(uuid string, exitCode *int) {
	now := rp.now()
	rp.mu.Lock()
	w := pruneBefore(rp.windows[uuid], now.Add(-rp.window))
	w = append(w, now)
	rp.windows[uuid] = w
	count := len(w)
	restart := rp.restart
	onGiveup := rp.onGiveup
	rp.mu.Unlock()

	if count >= rp.maxAtt {
		// 達上限:先記 RESTART_GIVEUP(事件寫入自帶鎖,可於任何路徑同步進行),再經 after 於
		// per-instance lock 之外派發「標 Error + 告警」——使本路徑不論持鎖與否都不重入該實例鎖。
		ev := rp.recordGiveup(uuid, exitCode, count)
		rp.after(0, func() {
			if rp.baseCtx.Err() != nil {
				return // app 關閉:取消未決的放棄處置。
			}
			if onGiveup != nil {
				onGiveup(uuid)
			}
			rp.dispatchAlert(ev)
		})
		return
	}
	if restart == nil {
		return // 尚未注入重啟函式:不自動重試(退化為手動)。
	}
	delay := rp.backoff(count)
	rp.after(delay, func() {
		if rp.baseCtx.Err() != nil {
			return // app 關閉:取消未決重試。
		}
		ctx, cancel := context.WithCancel(rp.baseCtx)
		defer cancel()
		if err := restart(ctx, uuid); err != nil {
			// 重試失敗(代理暫離線/就緒逾時等,不會有 die 事件)→ 視為一次新嘗試入帳,
			// 遞增退避續排;窗口內累計達上限時由本函式轉入放棄路徑。
			rp.recordAttempt(uuid, exitCode)
		}
	})
}

// recordGiveup 記一筆 RESTART_GIVEUP 事件並回傳該事件(供告警分派)。
func (rp *RestartPolicy) recordGiveup(uuid string, exitCode *int, count int) protocol.Event {
	details := map[string]any{
		"crashes_in_window": count,
		"window_seconds":    int(rp.window / time.Second),
	}
	if exitCode != nil {
		details["exit_code"] = *exitCode
	}
	ev := protocol.Event{
		Code:         protocol.EventRestartGiveup,
		Severity:     protocol.SeverityError,
		InstanceUUID: strPtr(uuid),
		DetailsJSON:  mustJSON(details),
	}
	rp.enrich(&ev, uuid)
	if rp.events != nil {
		_ = rp.events.Append(ev)
	}
	return ev
}

// dispatchAlert 把一則事件送往 AlertSink;失敗記 ALERT_FAILED、成功記 ALERT_SENT。
// 於 after 排程的執行緒中執行(不在 per-instance lock 上)。
func (rp *RestartPolicy) dispatchAlert(ev protocol.Event) {
	ctx, cancel := context.WithTimeout(rp.baseCtx, defaultProbeTimeout)
	defer cancel()
	if err := rp.alerts.Alert(ctx, ev); err != nil {
		rp.recordAlertResult(ev, protocol.EventAlertFailed, protocol.SeverityWarning, err.Error())
		return
	}
	rp.recordAlertResult(ev, protocol.EventAlertSent, protocol.SeverityInfo, "")
}

// recordAlertResult 記一筆 ALERT_SENT / ALERT_FAILED 事件。
func (rp *RestartPolicy) recordAlertResult(src protocol.Event, code protocol.EventCode, sev protocol.Severity, errMsg string) {
	if rp.events == nil {
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
	_ = rp.events.Append(ev)
}

// enrich 以實例記錄補上 node/template 欄位(best-effort;查不到則留空)。
func (rp *RestartPolicy) enrich(ev *protocol.Event, uuid string) {
	if rp.store == nil {
		return
	}
	if rec, err := rp.store.GetInstance(uuid); err == nil {
		ev.Node = strPtr(rec.Node)
		ev.TemplateID = strPtr(rec.TemplateID)
	}
}

// defaultBackoff 是預設指數退避:5s、10s、...,上限 60s。attempt 為本窗口內第幾次崩潰(≥1)。
func defaultBackoff(attempt int) time.Duration {
	d := defaultRestartBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= defaultRestartMaxDelay {
			return defaultRestartMaxDelay
		}
	}
	return d
}

// pruneBefore 回傳 ts 中不早於 cutoff 的元素(就地重用底層陣列)。
func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

func strPtr(s string) *string { return &s }
