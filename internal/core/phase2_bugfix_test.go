package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// signalProber 恆回「不就緒」,並於每次探測呼叫 onProbe(供測試得知 awaitReady 已進入輪詢)。
type signalProber struct{ onProbe func() }

func (p *signalProber) Ready(context.Context, string, string) (bool, error) {
	if p.onProbe != nil {
		p.onProbe()
	}
	return false, nil
}

// fakeProberWithRunning 同時實作 ReadinessProber 與 runningChecker,供 B3 存活監控測試:Ready 回
// 就緒結果、Running 回「容器是否仍 running(代理可達)」,以驗「就緒失敗但仍 running=真卡死(計入)」
// 與「代理不可達=基礎設施瞬斷(不計入)」的區分。
type fakeProberWithRunning struct {
	ready    bool
	readyErr error
	running  bool
	runErr   error
}

func (f *fakeProberWithRunning) Ready(context.Context, string, string) (bool, error) {
	return f.ready, f.readyErr
}

func (f *fakeProberWithRunning) Running(context.Context, string, string) (bool, error) {
	return f.running, f.runErr
}

// TestOrchestrator_DelayedPlannedDieConsumedByOccurrenceTime_B6 驗證 B6:計畫停止的 die 以 ev.TsUTC
// (發生時刻)判 planned-stop token TTL,而非派工時刻。模擬事件迴圈被長持鎖操作 head-of-line 阻塞:
// 停止後把時鐘推進遠越 TTL,再遞送一個「發生於停止當下(TTL 內)」的 die。應 consume 為計畫停止、
// 不誤判崩潰(維持 Stopped);若改回派工時刻(此時已越 TTL)則會誤標 Crashed——此測試即回歸守衛。
func TestOrchestrator_DelayedPlannedDieConsumedByOccurrenceTime_B6(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{useFakeClock: true, grace: 30 * time.Second})
	rec := h.createInstance(t, "b6-1")
	h.startInstance(t, "b6-1")

	stopTime := h.clock.Now()
	if err := h.orch.Stop(context.Background(), "b6-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := h.state(t, "b6-1"); got != protocol.InstanceStateStopped {
		t.Fatalf("Stop 後應為 Stopped, 得 %s", got)
	}
	// 模擬 die 因事件迴圈被長持鎖操作卡住而延遲派工:時鐘推進遠越 TTL(2×grace=60s)。
	h.clock.Advance(5 * time.Minute)

	ec := 0
	h.orch.HandleRuntimeEvent(context.Background(), "local", protocol.RuntimeEvent{
		ID: rec.RuntimeID, Kind: protocol.RuntimeEventDie, ExitCode: &ec, TsUTC: stopTime,
	})

	if got := h.state(t, "b6-1"); got != protocol.InstanceStateStopped {
		t.Errorf("B6:發生於 TTL 內的計畫停止 die 應被 consume,state 應維持 Stopped, 得 %s", got)
	}
	if n := h.countEvents(t, protocol.EventInstanceCrashed); n != 0 {
		t.Errorf("B6:不應記 INSTANCE_CRASHED, 得 %d", n)
	}
	if h.crashCount() != 0 {
		t.Errorf("B6:不應觸發 crashHook, 得 %d", h.crashCount())
	}
}

// TestOrchestrator_StartupCrashDetectedNotSwallowed_B2a 驗證 B2a:啟動途中容器崩潰退出時,awaitReady
// 經 Status 輪詢快速偵測→標 Crashed、記 INSTANCE_CRASHED,而非空等就緒逾時、把崩潰吞成 Error。啟動崩潰
// 刻意不自動重啟(crashCount 應為 0,見 markStartupCrashed)。prober 恆不就緒使 awaitReady 持續輪詢;
// 第一次探測後把 mock 實例強制設為崩潰終態。
func TestOrchestrator_StartupCrashDetectedNotSwallowed_B2a(t *testing.T) {
	probed := make(chan struct{}, 1)
	prober := &signalProber{onProbe: func() {
		select {
		case probed <- struct{}{}:
		default:
		}
	}}
	h := newLifeHarness(t, lifeOpts{
		prober: prober, readyTimeout: 30 * time.Second, readyPoll: 5 * time.Millisecond,
	})
	rec := h.createInstance(t, "b2a-1")

	startErr := make(chan error, 1)
	go func() { startErr <- h.orch.Start(context.Background(), "b2a-1") }()

	<-probed // Start 已進入 awaitReady 並探測至少一次。
	if err := h.backend.ForceExit(rec.RuntimeID, 1); err != nil {
		t.Fatalf("ForceExit: %v", err)
	}

	select {
	case err := <-startErr:
		if err == nil {
			t.Fatal("B2a:啟動途中崩潰應回錯,得 nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B2a:啟動途中崩潰應快速失敗,未在 10s 內返回(疑空等就緒逾時)")
	}

	if got := h.state(t, "b2a-1"); got != protocol.InstanceStateCrashed {
		t.Errorf("B2a:啟動崩潰後 observed 應為 Crashed, 得 %s", got)
	}
	if n := h.countEvents(t, protocol.EventInstanceCrashed); n != 1 {
		t.Errorf("B2a:應記 INSTANCE_CRASHED 1 次, 得 %d", n)
	}
	if h.crashCount() != 0 {
		t.Errorf("B2a:啟動崩潰不應自動重啟(crashHook 應 0 次), 得 %d", h.crashCount())
	}
}

// TestHealthMonitor_B3_GenuineStuckCounts 驗證 B3:就緒探測失敗但容器仍 running(代理可達)=真卡死,
// 應計入門檻並於達門檻後復原。
func TestHealthMonitor_B3_GenuineStuckCounts(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	env.create("life", "b3g-1")
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg, CrashHook: env.recordCrash,
	})
	if err := orch.Start(context.Background(), "b3g-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 就緒探測失敗、但容器仍 running(代理可達)→ 真卡死,應計入。
	fp := &fakeProberWithRunning{ready: false, readyErr: errors.New("rcon 逾時"), running: true}
	mon := NewHealthMonitor(HealthMonitorConfig{Store: env.store, Prober: fp, Recoverer: orch, Threshold: 3})

	ctx := context.Background()
	mon.checkOnce(ctx, "local")
	mon.checkOnce(ctx, "local")
	if got := env.state("b3g-1"); got != protocol.InstanceStateRunning {
		t.Fatalf("未達門檻不應復原, 得 %s", got)
	}
	mon.checkOnce(ctx, "local") // 達門檻 3
	if got := env.state("b3g-1"); got != protocol.InstanceStateCrashed {
		t.Errorf("B3:真卡死達門檻應復原為 Crashed, 得 %s", got)
	}
	if env.crashCount() != 1 {
		t.Errorf("B3:真卡死應交棒 crashHook 1 次, 得 %d", env.crashCount())
	}
}

// TestHealthMonitor_B3_InfraBlipNotCounted 驗證 B3:就緒探測失敗且容器狀態查不到(代理/狀態端點不可達)
// =基礎設施瞬斷,不計入卡死門檻——健康的 Running 伺服器不因遠端瞬斷被誤判卡死並強停/自動重啟。
func TestHealthMonitor_B3_InfraBlipNotCounted(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	env.create("life", "b3i-1")
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg, CrashHook: env.recordCrash,
	})
	if err := orch.Start(context.Background(), "b3i-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 就緒探測失敗、且 Running 檢查也失敗(代理不可達)→ 基礎設施瞬斷,不計入。
	fp := &fakeProberWithRunning{
		ready: false, readyErr: errors.New("status http 瞬斷"),
		running: false, runErr: errors.New("節點不可達"),
	}
	mon := NewHealthMonitor(HealthMonitorConfig{Store: env.store, Prober: fp, Recoverer: orch, Threshold: 3})

	ctx := context.Background()
	for i := 0; i < 6; i++ { // 遠超門檻的探測次數
		mon.checkOnce(ctx, "local")
	}
	if got := env.state("b3i-1"); got != protocol.InstanceStateRunning {
		t.Errorf("B3:基礎設施瞬斷不應誤判卡死,state 應維持 Running, 得 %s", got)
	}
	if env.crashCount() != 0 {
		t.Errorf("B3:基礎設施瞬斷不應交棒 crashHook, 得 %d", env.crashCount())
	}
}
