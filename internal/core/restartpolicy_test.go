package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// fakeAlertSink 是可捕捉告警的 AlertSink 替身(供 RestartPolicy / Scheduler 測試共用)。
type fakeAlertSink struct {
	mu     sync.Mutex
	alerts []protocol.Event
	err    error
}

func (f *fakeAlertSink) Alert(_ context.Context, ev protocol.Event) error {
	f.mu.Lock()
	f.alerts = append(f.alerts, ev)
	err := f.err
	f.mu.Unlock()
	return err
}

func (f *fakeAlertSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.alerts)
}

// syncAfter 是同步版 after(立即執行 f),供無真實 orch 的單元測試(不會與 per-instance lock 死結)。
func syncAfter(_ time.Duration, f func()) { f() }

// newUnitPolicy 建一個注入 fake 時鐘/after/restart 的 RestartPolicy(單元測試用)。
func newUnitPolicy(t *testing.T, restart func(context.Context, string) error, onGiveup func(string)) (*RestartPolicy, *fakeAlertSink, *EventLog, *fakeClock) {
	t.Helper()
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	rp := NewRestartPolicy(RestartPolicyConfig{
		Store: store, Events: events, Alerts: sink, Now: clk.Now,
		Window: 5 * time.Minute, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return 0 },
		after:   syncAfter,
	})
	rp.SetRestart(restart, onGiveup)
	return rp, sink, events, clk
}

// ---- 崩潰迴圈:窗口內達上限 → 放棄 + 告警 ----

func TestRestartPolicy_GiveupAtLimit(t *testing.T) {
	var restarts int
	var giveups int
	rp, sink, events, clk := newUnitPolicy(t,
		func(context.Context, string) error { restarts++; return nil },
		func(string) { giveups++ },
	)

	ec := 1
	for i := 0; i < 3; i++ {
		rp.OnCrash("gu-1", &ec)
		clk.Advance(time.Second) // 仍在同一 5 分鐘窗口內
	}

	if restarts != 2 {
		t.Errorf("前兩次崩潰應各重試一次, restarts = %d 期望 2", restarts)
	}
	if giveups != 1 {
		t.Errorf("第三次應放棄(標 Error)一次, giveups = %d 期望 1", giveups)
	}
	if n := len(queryEvents(t, events, protocol.EventRestartGiveup)); n != 1 {
		t.Errorf("RESTART_GIVEUP = %d, 期望 1", n)
	}
	if sink.count() != 1 {
		t.Errorf("放棄應發告警 1 次, 得 %d", sink.count())
	}
	if n := len(queryEvents(t, events, protocol.EventAlertSent)); n != 1 {
		t.Errorf("ALERT_SENT = %d, 期望 1", n)
	}
}

// ---- 窗口外崩潰:舊時間戳老化滑出 → 計數重置,不放棄 ----

func TestRestartPolicy_WindowResets(t *testing.T) {
	var restarts int
	var giveups int
	rp, _, events, clk := newUnitPolicy(t,
		func(context.Context, string) error { restarts++; return nil },
		func(string) { giveups++ },
	)

	ec := 1
	rp.OnCrash("wr-1", &ec) // count 1 → restart
	clk.Advance(time.Second)
	rp.OnCrash("wr-1", &ec) // count 2 → restart

	clk.Advance(6 * time.Minute) // 超過窗口:前兩次老化

	rp.OnCrash("wr-1", &ec) // 修剪後 count 1 → restart
	clk.Advance(time.Second)
	rp.OnCrash("wr-1", &ec) // count 2 → restart

	if restarts != 4 {
		t.Errorf("窗口重置後應共重試 4 次, 得 %d", restarts)
	}
	if giveups != 0 {
		t.Errorf("未在單一窗口達上限, 不應放棄, giveups = %d", giveups)
	}
	if n := len(queryEvents(t, events, protocol.EventRestartGiveup)); n != 0 {
		t.Errorf("RESTART_GIVEUP = %d, 期望 0", n)
	}
}

// ---- 告警發送失敗 → 記 ALERT_FAILED ----

func TestRestartPolicy_AlertFailureRecorded(t *testing.T) {
	rp, sink, events, clk := newUnitPolicy(t,
		func(context.Context, string) error { return nil },
		func(string) {},
	)
	sink.err = errors.New("webhook 429")

	ec := 1
	for i := 0; i < 3; i++ {
		rp.OnCrash("af-1", &ec)
		clk.Advance(time.Second)
	}
	if n := len(queryEvents(t, events, protocol.EventAlertFailed)); n != 1 {
		t.Errorf("ALERT_FAILED = %d, 期望 1", n)
	}
	if n := len(queryEvents(t, events, protocol.EventAlertSent)); n != 0 {
		t.Errorf("ALERT_SENT = %d, 期望 0", n)
	}
}

// ---- 重試失敗不斷鏈:失敗即入帳續排,直到成功或達上限(#2)----

func TestRestartPolicy_RetryFailureChainsUntilSuccess(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	var attempts, giveups int
	rp := NewRestartPolicy(RestartPolicyConfig{
		Store: store, Events: events, Alerts: sink, Now: clk.Now,
		Window: 5 * time.Minute, MaxAttempts: 5,
		Backoff: func(int) time.Duration { return 0 },
		after:   syncAfter,
	})
	rp.SetRestart(func(context.Context, string) error {
		attempts++
		if attempts < 3 {
			return errors.New("agent 暫離線") // 前兩次失敗
		}
		return nil // 第三次成功 → 回 Running,不再排下一次
	}, func(string) { giveups++ })

	ec := 1
	rp.OnCrash("rf-1", &ec)

	if attempts != 3 {
		t.Errorf("重試應續排至第三次成功, attempts = %d 期望 3", attempts)
	}
	if giveups != 0 {
		t.Errorf("成功前未達上限, 不應放棄, giveups = %d", giveups)
	}
	if n := len(queryEvents(t, events, protocol.EventRestartGiveup)); n != 0 {
		t.Errorf("RESTART_GIVEUP = %d, 期望 0", n)
	}
}

func TestRestartPolicy_RetryFailureReachesGiveup(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	sink := &fakeAlertSink{}
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	var attempts, giveups int
	rp := NewRestartPolicy(RestartPolicyConfig{
		Store: store, Events: events, Alerts: sink, Now: clk.Now,
		Window: 5 * time.Minute, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return 0 },
		after:   syncAfter,
	})
	rp.SetRestart(func(context.Context, string) error {
		attempts++
		return errors.New("持續失敗") // 每次重試都失敗
	}, func(string) { giveups++ })

	// 崩潰(count1)→重試1失敗(count2)→重試2失敗(count3≥上限)→放棄(不再有重試3)。
	rp.OnCrash("rg-1", intPtr(1))

	if attempts != 2 {
		t.Errorf("達上限前應重試 2 次, attempts = %d 期望 2", attempts)
	}
	if giveups != 1 {
		t.Errorf("窗口內連續失敗達上限應放棄一次, giveups = %d", giveups)
	}
	if n := len(queryEvents(t, events, protocol.EventRestartGiveup)); n != 1 {
		t.Errorf("RESTART_GIVEUP = %d, 期望 1", n)
	}
	if sink.count() != 1 {
		t.Errorf("放棄應發告警 1 次, 得 %d", sink.count())
	}
	if n := len(queryEvents(t, events, protocol.EventAlertSent)); n != 1 {
		t.Errorf("ALERT_SENT = %d, 期望 1", n)
	}
}

// ---- app 關閉:baseCtx 取消後,未決的退避重試不執行(#8)----

func TestRestartPolicy_ShutdownCancelsPendingRestart(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	ctx, cancel := context.WithCancel(context.Background())
	var attempts int
	var pending []func() // 捕捉延後函式,模擬「排程後、執行前 app 關閉」。
	rp := NewRestartPolicy(RestartPolicyConfig{
		Store: store, Events: events, Now: clk.Now,
		Window: 5 * time.Minute, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return time.Second },
		BaseCtx: ctx,
		after:   func(_ time.Duration, f func()) { pending = append(pending, f) },
	})
	rp.SetRestart(func(context.Context, string) error { attempts++; return nil }, func(string) {})

	rp.OnCrash("sd-1", intPtr(1)) // 排程一個退避重試(存入 pending,尚未執行)
	cancel()                      // app 關閉
	for _, f := range pending {   // 觸發未決重試
		f()
	}

	if attempts != 0 {
		t.Errorf("baseCtx 取消後未決重試不應執行, attempts = %d 期望 0", attempts)
	}
}

// ---- 端到端接線:非計畫崩潰 → 自動重啟回 Running;計畫停止不觸發 ----

func TestRestartPolicy_AutoRestartOnUnplannedDie(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	policy := NewRestartPolicy(RestartPolicyConfig{
		Store: env.store, Events: env.events,
		Backoff: func(int) time.Duration { return 10 * time.Millisecond },
	})
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg,
		CrashHook: policy.OnCrash,
	})
	policy.SetRestart(orch.Start, orch.markGiveup)

	env.create("life", "ar-1")
	if err := orch.Start(context.Background(), "ar-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec, _ := env.store.GetInstance("ar-1")

	ec := 1
	orch.HandleRuntimeEvent(context.Background(), "local", protocol.RuntimeEvent{
		ID: rec.RuntimeID, Kind: protocol.RuntimeEventDie, ExitCode: &ec, TsUTC: time.Now(),
	})

	// 崩潰 → 退避後自動重啟回 Running。
	env.waitState("ar-1", protocol.InstanceStateRunning, 3*time.Second)
	if n := env.countEvents(protocol.EventInstanceCrashed); n != 1 {
		t.Errorf("INSTANCE_CRASHED = %d, 期望 1", n)
	}
	if n := env.countEvents(protocol.EventInstanceStarted); n < 2 {
		t.Errorf("INSTANCE_STARTED = %d, 期望 >=2(初次 + 自動重啟)", n)
	}
}

func TestRestartPolicy_PlannedStopNoRestart(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	var restarts int
	policy := NewRestartPolicy(RestartPolicyConfig{Store: env.store, Events: env.events})
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg, CrashHook: policy.OnCrash,
	})
	policy.SetRestart(func(ctx context.Context, uuid string) error {
		restarts++
		return orch.Start(ctx, uuid)
	}, orch.markGiveup)

	env.create("life", "ps-1")
	if err := orch.Start(context.Background(), "ps-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec, _ := env.store.GetInstance("ps-1")

	if err := orch.Stop(context.Background(), "ps-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 計畫停止對應的 die 到達(token 仍有效)→ 不得觸發自動重啟。
	orch.HandleRuntimeEvent(context.Background(), "local", protocol.RuntimeEvent{
		ID: rec.RuntimeID, Kind: protocol.RuntimeEventDie, ExitCode: intPtr(0), TsUTC: time.Now(),
	})
	time.Sleep(80 * time.Millisecond)

	if got := env.state("ps-1"); got != protocol.InstanceStateStopped {
		t.Errorf("計畫停止後 observed = %s, 期望維持 Stopped", got)
	}
	if restarts != 0 {
		t.Errorf("計畫停止不應觸發自動重啟, restarts = %d", restarts)
	}
}

// TestRestartPolicy_ClockSkewMisjudgedStopNotRestarted 驗證 DesiredState 守衛(Orchestrator.AutoStart):
// 遠端節點時鐘快於核心超過 planned-stop token TTL(2×grace,預設 60s)時,使用者刻意停止產生的 die 會
// 因 agent 蓋章的 TsUTC 已越過 expiresAt 而被 consumeAt 判為過期 → 誤標 Crashed → 交棒 crashHook。
// 此時 DesiredState 已由 stopLocked 收斂為 Stopped,自動重啟必須放行不啟動:誤判最多留下一則崩潰事件,
// 不得把使用者停掉的伺服器拉回 Running。若把注入改回 orch.Start,本測試會紅——即回歸守衛。
func TestRestartPolicy_ClockSkewMisjudgedStopNotRestarted(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	policy := NewRestartPolicy(RestartPolicyConfig{
		Store: env.store, Events: env.events,
		Backoff: func(int) time.Duration { return 10 * time.Millisecond },
	})
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg, CrashHook: policy.OnCrash,
	})
	policy.SetRestart(orch.AutoStart, orch.markGiveup) // 與 app.Bootstrap 的接線一致

	env.create("life", "cs-1")
	if err := orch.Start(context.Background(), "cs-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec, _ := env.store.GetInstance("cs-1")

	if err := orch.Stop(context.Background(), "cs-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 節點時鐘快 90s(> TTL 60s):die 的 TsUTC 越過 expiresAt → token 判過期 → 誤判為崩潰。
	orch.HandleRuntimeEvent(context.Background(), "local", protocol.RuntimeEvent{
		ID: rec.RuntimeID, Kind: protocol.RuntimeEventDie, ExitCode: intPtr(0),
		TsUTC: time.Now().Add(90 * time.Second),
	})
	if n := env.countEvents(protocol.EventInstanceCrashed); n != 1 {
		t.Fatalf("前提不成立:時鐘偏移應使計畫停機 die 被誤判為崩潰, INSTANCE_CRASHED = %d 期望 1", n)
	}

	time.Sleep(200 * time.Millisecond) // 遠大於 10ms 退避:若守衛失效,重啟早已完成
	if got := env.state("cs-1"); got == protocol.InstanceStateRunning {
		t.Error("使用者刻意停止的實例不得被誤判崩潰的自動重啟拉回 Running")
	}
	after, _ := env.store.GetInstance("cs-1")
	if after.DesiredState != protocol.InstanceStateStopped {
		t.Errorf("desired = %s, 期望維持 Stopped", after.DesiredState)
	}
}

// TestRestartPolicy_AutoStartRestartsGenuineCrash 驗證 DesiredState 守衛不誤傷真崩潰:desired 仍為
// Running 的實例崩潰後,AutoStart 照常委給 startLocked 重啟回 Running(守衛只擋 desired=Stopped)。
func TestRestartPolicy_AutoStartRestartsGenuineCrash(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	policy := NewRestartPolicy(RestartPolicyConfig{
		Store: env.store, Events: env.events,
		Backoff: func(int) time.Duration { return 10 * time.Millisecond },
	})
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg, CrashHook: policy.OnCrash,
	})
	policy.SetRestart(orch.AutoStart, orch.markGiveup)

	env.create("life", "gc-1")
	if err := orch.Start(context.Background(), "gc-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec, _ := env.store.GetInstance("gc-1")

	orch.HandleRuntimeEvent(context.Background(), "local", protocol.RuntimeEvent{
		ID: rec.RuntimeID, Kind: protocol.RuntimeEventDie, ExitCode: intPtr(1), TsUTC: time.Now(),
	})
	env.waitState("gc-1", protocol.InstanceStateRunning, 3*time.Second)
}

func intPtr(n int) *int { return &n }
