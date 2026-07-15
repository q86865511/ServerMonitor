package core

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// ---- fake statsSource(供 Watch/Stop 接線測試,不需真實 MonitorHub/WS) ----

type fakeStatsSource struct {
	mu      sync.Mutex
	chans   map[string]chan protocol.ResourceStats
	subs    map[string]int
	closedM map[string]bool
	nextID  SubID
}

func newFakeStatsSource() *fakeStatsSource {
	return &fakeStatsSource{
		chans:   make(map[string]chan protocol.ResourceStats),
		subs:    make(map[string]int),
		closedM: make(map[string]bool),
	}
}

func (f *fakeStatsSource) SubscribeStats(uuid string) (<-chan protocol.ResourceStats, SubID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan protocol.ResourceStats, 8)
	f.chans[uuid] = ch
	f.subs[uuid]++
	f.nextID++
	return ch, f.nextID
}

func (f *fakeStatsSource) UnsubscribeStats(uuid string, _ SubID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.chans[uuid]; ok {
		delete(f.chans, uuid)
		f.closedM[uuid] = true
		close(ch)
	}
}

func (f *fakeStatsSource) push(uuid string, s protocol.ResourceStats) {
	f.mu.Lock()
	ch := f.chans[uuid]
	f.mu.Unlock()
	if ch != nil {
		ch <- s
	}
}

func (f *fakeStatsSource) subscribeCount(uuid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[uuid]
}

func (f *fakeStatsSource) closed(uuid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closedM[uuid]
}

// waitCond 輪詢等待條件成立(供非同步 goroutine 接線測試同步斷言)。
func waitCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("等待條件逾時(%s)", timeout)
	}
}

// ---- 持續窗口(60 秒)----

func TestThresholdMonitor_PersistWindow(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	tm := NewThresholdMonitor(ThresholdMonitorConfig{Store: store, Events: events, Alerts: sink, Now: clk.Now})

	uuid := "inst-1"
	if err := SaveThresholdConfig(store, uuid, ThresholdConfig{CPUPercent: 80}); err != nil {
		t.Fatalf("SaveThresholdConfig: %v", err)
	}

	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90}) // t0:超標起點
	clk.Advance(30 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90}) // t0+30s:未達 60s,不應觸發
	if sink.count() != 0 {
		t.Fatalf("超標 30 秒不應觸發, 得 %d", sink.count())
	}

	clk.Advance(30 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90}) // t0+60s:達持續窗口,應觸發
	if sink.count() != 1 {
		t.Fatalf("持續 60 秒應觸發, 得 %d", sink.count())
	}
	if n := len(queryEvents(t, events, EventResourceThreshold)); n != 1 {
		t.Errorf("RESOURCE_THRESHOLD = %d, 期望 1", n)
	}
	if n := len(queryEvents(t, events, protocol.EventAlertSent)); n != 1 {
		t.Errorf("ALERT_SENT = %d, 期望 1", n)
	}
}

// ---- 抖動:59 秒回落 → 重置計時、不觸發 ----

func TestThresholdMonitor_JitterResetsTimer(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	tm := NewThresholdMonitor(ThresholdMonitorConfig{Store: store, Events: events, Alerts: sink, Now: clk.Now})

	uuid := "inst-2"
	if err := SaveThresholdConfig(store, uuid, ThresholdConfig{CPUPercent: 80}); err != nil {
		t.Fatalf("SaveThresholdConfig: %v", err)
	}

	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90}) // t0 超標起點
	clk.Advance(59 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 70}) // t0+59s 回落到門檻以下(抖動)→ 重置
	clk.Advance(1 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90}) // 重新起算,僅過 1 秒
	if sink.count() != 0 {
		t.Fatalf("59 秒回落應重置計時、不應觸發, 得 %d", sink.count())
	}

	// 從重新起算點(上一筆 Process 已把 exceededSince 設為當下 clk)續滿 60 秒應可正常觸發
	// (驗證重置後仍可運作,非永久卡死)。
	clk.Advance(60 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90})
	if sink.count() != 1 {
		t.Fatalf("重置後續滿 60 秒應觸發, 得 %d", sink.count())
	}
}

// ---- 遲滯:降到門檻-10% 才解除,解除後才可能再觸發 ----

func TestThresholdMonitor_HysteresisRelease(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	tm := NewThresholdMonitor(ThresholdMonitorConfig{Store: store, Events: events, Alerts: sink, Now: clk.Now})

	uuid := "inst-3"
	if err := SaveThresholdConfig(store, uuid, ThresholdConfig{CPUPercent: 80}); err != nil { // 釋放線 = 72
		t.Fatalf("SaveThresholdConfig: %v", err)
	}

	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90})
	clk.Advance(60 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90}) // 觸發第 1 次
	if sink.count() != 1 {
		t.Fatalf("應觸發一次, 得 %d", sink.count())
	}

	clk.Advance(10 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 75}) // 高於釋放線 72:維持 armed,不解除
	clk.Advance(60 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 75}) // 持續超標但 armed 中:不重複觸發
	if sink.count() != 1 {
		t.Fatalf("armed 期間不應重複觸發, 得 %d", sink.count())
	}

	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 70}) // 降到 <=72:解除
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90}) // 解除後重新起算超標
	clk.Advance(60 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90}) // 60 秒後再度觸發
	if sink.count() != 2 {
		t.Fatalf("解除後應可再觸發, 得 %d", sink.count())
	}
}

// ---- 未設定門檻:不啟用 ----

func TestThresholdMonitor_DisabledWhenNoConfig(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	tm := NewThresholdMonitor(ThresholdMonitorConfig{Store: store, Events: events, Alerts: sink, Now: clk.Now})

	uuid := "inst-5" // 未 SaveThresholdConfig
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 99})
	clk.Advance(2 * time.Minute)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 99})
	if sink.count() != 0 {
		t.Errorf("未設定門檻不應觸發, 得 %d", sink.count())
	}
}

// ---- 記憶體門檻(佔 MemoryLimit 比例) ----

func TestThresholdMonitor_MemoryMetric(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	tm := NewThresholdMonitor(ThresholdMonitorConfig{Store: store, Events: events, Alerts: sink, Now: clk.Now})

	uuid := "inst-6"
	if err := SaveThresholdConfig(store, uuid, ThresholdConfig{MemoryPercent: 80}); err != nil {
		t.Fatalf("SaveThresholdConfig: %v", err)
	}

	stats := protocol.ResourceStats{MemoryBytes: 900, MemoryLimit: 1000} // 90%
	tm.Process(uuid, stats)
	clk.Advance(60 * time.Second)
	tm.Process(uuid, stats)
	if sink.count() != 1 {
		t.Fatalf("記憶體門檻應觸發, 得 %d", sink.count())
	}
	var details map[string]any
	if err := json.Unmarshal(sink.alerts[0].DetailsJSON, &details); err != nil {
		t.Fatalf("解析 details: %v", err)
	}
	if details["metric"] != "memory" {
		t.Errorf("metric 欄位 = %v, 期望 memory", details["metric"])
	}
}

// ---- 無管道(NopAlertSink):僅記錄不報錯 ----

func TestThresholdMonitor_NoSinkNoError(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	tm := NewThresholdMonitor(ThresholdMonitorConfig{Store: store, Events: events, Now: clk.Now}) // Alerts 未設,用 NopAlertSink

	uuid := "inst-4"
	if err := SaveThresholdConfig(store, uuid, ThresholdConfig{CPUPercent: 80}); err != nil {
		t.Fatalf("SaveThresholdConfig: %v", err)
	}
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90})
	clk.Advance(60 * time.Second)
	tm.Process(uuid, protocol.ResourceStats{CPUPercent: 90})

	if n := len(queryEvents(t, events, EventResourceThreshold)); n != 1 {
		t.Errorf("應記錄 RESOURCE_THRESHOLD, 得 %d", n)
	}
	if n := len(queryEvents(t, events, protocol.EventAlertSent)); n != 1 {
		t.Errorf("NopAlertSink 視為成功應記 ALERT_SENT, 得 %d", n)
	}
}

// ---- Watch/Stop 接線(fake statsSource,驗證 channel 消費 goroutine 確實運作) ----

func TestThresholdMonitor_WatchWiring(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	stats := newFakeStatsSource()
	tm := NewThresholdMonitor(ThresholdMonitorConfig{
		Store: store, Events: events, Alerts: sink, Now: clk.Now, Stats: stats,
	})

	uuid := "inst-7"
	if err := SaveThresholdConfig(store, uuid, ThresholdConfig{CPUPercent: 80}); err != nil {
		t.Fatalf("SaveThresholdConfig: %v", err)
	}

	// testHook 於每次 Process 結束時計數,供本測試同步「樣本已由消費 goroutine 處理完畢」,
	// 避免推入樣本與推進假時鐘之間出現資料競態(僅測試用,production 恆為 nil)。
	var processed int32
	tm.testHook = func(string) { atomic.AddInt32(&processed, 1) }

	tm.Watch(uuid)
	tm.Watch(uuid) // 冪等:第二次應為 no-op,不重複訂閱
	if n := stats.subscribeCount(uuid); n != 1 {
		t.Errorf("Watch 冪等應只訂閱 1 次, 得 %d", n)
	}

	stats.push(uuid, protocol.ResourceStats{CPUPercent: 90}) // 第一筆:設定超標起點
	waitCond(t, time.Second, func() bool { return atomic.LoadInt32(&processed) >= 1 })

	clk.Advance(60 * time.Second)
	stats.push(uuid, protocol.ResourceStats{CPUPercent: 90}) // 第二筆:持續 60 秒,應觸發
	waitCond(t, time.Second, func() bool { return atomic.LoadInt32(&processed) >= 2 })
	if sink.count() != 1 {
		t.Fatalf("經 Watch 接線持續 60 秒應觸發告警, 得 %d", sink.count())
	}

	tm.Stop(uuid)
	waitCond(t, time.Second, func() bool { return stats.closed(uuid) })
}
