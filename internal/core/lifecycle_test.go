package core

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// lifeTestTemplate 是最小 docker 範本:無埠、無必填參數、無機密,使多實例可並存
// (無埠預留即無衝突),且建立只需 TemplateID。生命週期/對帳測試以此為底。
const lifeTestTemplate = `
schema_version = 1
id = "life"
name = "Life Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/life:1.0"
`

// ---- 測試替身 ----

// neverReadyProber 恆回「未就緒」,用以驅動啟動就緒逾時。
type neverReadyProber struct{}

func (neverReadyProber) Ready(context.Context, string, string) (bool, error) { return false, nil }

// fakeClock 是可手動推進的時鐘,供 planned-stop token TTL 測試。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---- harness ----

type crashRec struct {
	uuid     string
	exitCode *int
}

type lifeOpts struct {
	prober       ReadinessProber
	readyTimeout time.Duration
	readyPoll    time.Duration
	grace        time.Duration
	useFakeClock bool
}

type lifeHarness struct {
	orch    *Orchestrator
	recon   *Reconciler
	svc     *InstanceService
	backend *agent.MockBackend
	store   *Store
	events  *EventLog
	reg     *NodeRegistry
	journal *Journal
	eng     *TemplateEngine
	node    string
	clock   *fakeClock

	crashMu sync.Mutex
	crashed []crashRec
}

func newLifeHarness(t *testing.T, opts lifeOpts) *lifeHarness {
	t.Helper()
	backend := agent.NewMockBackend()
	hs := newAgentServerWithBackend(t, backend)

	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	kr := newMemoryKeyring()
	secrets := NewSecretStoreWithKeyring("test", kr)
	reg := NewNodeRegistry(events)
	reg.Register("local", NewNodeClient(hs.URL, agentTestToken, nil))
	eng := NewTemplateEngine(nil, events)
	dir := t.TempDir()
	writeTemplateFile(t, dir, "life.toml", lifeTestTemplate)
	if _, err := eng.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	journal, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	svc := NewInstanceService(InstanceServiceConfig{
		Store: store, Secrets: secrets, Events: events, Engine: eng,
		Registry: reg, Journal: journal,
	})

	h := &lifeHarness{
		svc: svc, backend: backend, store: store, events: events,
		reg: reg, journal: journal, eng: eng, node: "local",
	}

	nowFn := time.Now
	if opts.useFakeClock {
		h.clock = newFakeClock(time.Unix(1700000000, 0).UTC())
		nowFn = h.clock.Now
	}

	recon := NewReconciler(ReconcilerConfig{Store: store, Events: events, Registry: reg, Journal: journal})
	orch := NewOrchestrator(OrchestratorConfig{
		Store: store, Events: events, Registry: reg, Reconciler: recon,
		Prober: opts.prober, Now: nowFn,
		ReadyTimeout: opts.readyTimeout, Grace: opts.grace, ReadyPoll: opts.readyPoll,
		CrashHook: func(uuid string, exitCode *int) {
			h.crashMu.Lock()
			h.crashed = append(h.crashed, crashRec{uuid: uuid, exitCode: exitCode})
			h.crashMu.Unlock()
		},
	})
	h.orch = orch
	h.recon = recon
	return h
}

func (h *lifeHarness) createInstance(t *testing.T, uuid string) InstanceRecord {
	t.Helper()
	if uuid != "" {
		h.svc.newUUID = func() string { return uuid }
	}
	rec, err := h.svc.Create(context.Background(), CreateOptions{TemplateID: "life"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return rec
}

func (h *lifeHarness) startInstance(t *testing.T, uuid string) {
	t.Helper()
	if err := h.orch.Start(context.Background(), uuid); err != nil {
		t.Fatalf("Start(%s): %v", uuid, err)
	}
}

func (h *lifeHarness) state(t *testing.T, uuid string) protocol.InstanceState {
	t.Helper()
	rec, err := h.store.GetInstance(uuid)
	if err != nil {
		t.Fatalf("GetInstance(%s): %v", uuid, err)
	}
	return rec.ObservedState
}

func (h *lifeHarness) countEvents(t *testing.T, code protocol.EventCode) int {
	t.Helper()
	return len(queryEvents(t, h.events, code))
}

func (h *lifeHarness) waitForState(t *testing.T, uuid string, want protocol.InstanceState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.state(t, uuid) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 至 %s 逾時,實際 %s", uuid, want, h.state(t, uuid))
}

func (h *lifeHarness) crashCount() int {
	h.crashMu.Lock()
	defer h.crashMu.Unlock()
	return len(h.crashed)
}

// ---- Start / Stop / Restart(R3)----

func TestOrchestrator_StartReady(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	h.createInstance(t, "sr-1")
	if err := h.orch.Start(context.Background(), "sr-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := h.state(t, "sr-1"); got != protocol.InstanceStateRunning {
		t.Errorf("observed = %s, 期望 Running", got)
	}
	rec, _ := h.store.GetInstance("sr-1")
	if rec.DesiredState != protocol.InstanceStateRunning {
		t.Errorf("desired = %s, 期望 Running", rec.DesiredState)
	}
	if n := h.countEvents(t, protocol.EventInstanceStarted); n != 1 {
		t.Errorf("INSTANCE_STARTED = %d, 期望 1", n)
	}
}

func TestOrchestrator_StartIdempotent(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	h.createInstance(t, "id-1")
	h.startInstance(t, "id-1")
	if err := h.orch.Start(context.Background(), "id-1"); err != nil {
		t.Fatalf("第二次 Start 應冪等, 得 %v", err)
	}
	if got := h.state(t, "id-1"); got != protocol.InstanceStateRunning {
		t.Errorf("observed = %s, 期望 Running", got)
	}
	if n := h.countEvents(t, protocol.EventInstanceStarted); n != 1 {
		t.Errorf("INSTANCE_STARTED = %d, 期望 1(冪等不重複)", n)
	}
}

func TestOrchestrator_StartReadyTimeout(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{
		prober: neverReadyProber{}, readyTimeout: 120 * time.Millisecond, readyPoll: 20 * time.Millisecond,
	})
	h.createInstance(t, "to-1")
	err := h.orch.Start(context.Background(), "to-1")
	if !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("期望 ErrStartTimeout, 得 %v", err)
	}
	if got := h.state(t, "to-1"); got != protocol.InstanceStateError {
		t.Errorf("就緒逾時後 observed = %s, 期望 Error", got)
	}
}

func TestOrchestrator_RestartSameRuntimeID(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	rec := h.createInstance(t, "rs-1")
	h.startInstance(t, "rs-1")

	if err := h.orch.Restart(context.Background(), "rs-1"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	after, _ := h.store.GetInstance("rs-1")
	if after.RuntimeID != rec.RuntimeID {
		t.Errorf("restart 後 runtime ID 改變: %s != %s", after.RuntimeID, rec.RuntimeID)
	}
	if after.ObservedState != protocol.InstanceStateRunning {
		t.Errorf("restart 後 observed = %s, 期望 Running", after.ObservedState)
	}
	if n := h.countEvents(t, protocol.EventInstanceRestarted); n != 1 {
		t.Errorf("INSTANCE_RESTARTED = %d, 期望 1", n)
	}
}

// ---- planned-stop token 與死因判別(R8)----

func TestOrchestrator_PlannedStopNotCrashed(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	rec := h.createInstance(t, "ps-1")
	h.startInstance(t, "ps-1")

	if err := h.orch.Stop(context.Background(), "ps-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := h.state(t, "ps-1"); got != protocol.InstanceStateStopped {
		t.Fatalf("Stop 後 observed = %s, 期望 Stopped", got)
	}
	// 計畫停止對應的 die 到達(token 仍有效)→ 不得標 Crashed。
	ec := 0
	h.orch.HandleRuntimeEvent(context.Background(), "local", protocol.RuntimeEvent{
		ID: rec.RuntimeID, Kind: protocol.RuntimeEventDie, ExitCode: &ec, TsUTC: time.Now(),
	})
	if got := h.state(t, "ps-1"); got != protocol.InstanceStateStopped {
		t.Errorf("planned die 後 observed = %s, 期望維持 Stopped", got)
	}
	if n := h.countEvents(t, protocol.EventInstanceCrashed); n != 0 {
		t.Errorf("planned die 不應記 INSTANCE_CRASHED, 得 %d", n)
	}
	if n := h.countEvents(t, protocol.EventInstanceStopped); n != 1 {
		t.Errorf("INSTANCE_STOPPED = %d, 期望 1", n)
	}
	if h.crashCount() != 0 {
		t.Errorf("planned die 不應觸發 crashHook, 得 %d", h.crashCount())
	}
}

func TestOrchestrator_UnplannedDieCrashed(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	rec := h.createInstance(t, "ud-1")
	h.startInstance(t, "ud-1")

	ec := 1
	h.orch.HandleRuntimeEvent(context.Background(), "local", protocol.RuntimeEvent{
		ID: rec.RuntimeID, Kind: protocol.RuntimeEventDie, ExitCode: &ec, TsUTC: time.Now(),
	})
	if got := h.state(t, "ud-1"); got != protocol.InstanceStateCrashed {
		t.Errorf("非計畫 die 後 observed = %s, 期望 Crashed", got)
	}
	if n := h.countEvents(t, protocol.EventInstanceCrashed); n != 1 {
		t.Errorf("INSTANCE_CRASHED = %d, 期望 1", n)
	}
	if h.crashCount() != 1 {
		t.Fatalf("crashHook 應被觸發 1 次, 得 %d", h.crashCount())
	}
	if h.crashed[0].uuid != "ud-1" || h.crashed[0].exitCode == nil || *h.crashed[0].exitCode != 1 {
		t.Errorf("crashHook 參數 = %+v, 期望 uuid=ud-1 exit=1", h.crashed[0])
	}
}

func TestOrchestrator_ExpiredTokenDoesNotMaskCrash(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{useFakeClock: true})
	rec := h.createInstance(t, "et-1")
	h.startInstance(t, "et-1")

	// 發一個 planned-stop token 後推進時鐘越過 TTL:過期 token 不得遮蔽後續真崩潰。
	h.orch.tokens.issue("et-1", "tok", 1, 60*time.Second)
	h.clock.Advance(61 * time.Second)

	ec := 1
	h.orch.HandleRuntimeEvent(context.Background(), "local", protocol.RuntimeEvent{
		ID: rec.RuntimeID, Kind: protocol.RuntimeEventDie, ExitCode: &ec, TsUTC: time.Now(),
	})
	if got := h.state(t, "et-1"); got != protocol.InstanceStateCrashed {
		t.Errorf("過期 token 後的 die 應標 Crashed, 得 %s", got)
	}
	if n := h.countEvents(t, protocol.EventInstanceCrashed); n != 1 {
		t.Errorf("INSTANCE_CRASHED = %d, 期望 1", n)
	}
}

// ---- 併發序列化(R3)----

func TestOrchestrator_ConcurrentSameInstanceSerialized(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	h.createInstance(t, "conc-a")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.orch.Start(context.Background(), "conc-a")
		}()
	}
	wg.Wait()

	if got := h.state(t, "conc-a"); got != protocol.InstanceStateRunning {
		t.Fatalf("observed = %s, 期望 Running", got)
	}
	// 序列化 + 冪等:僅一筆 INSTANCE_STARTED。
	if n := h.countEvents(t, protocol.EventInstanceStarted); n != 1 {
		t.Errorf("INSTANCE_STARTED = %d, 期望 1(序列化 + 冪等)", n)
	}
}

func TestOrchestrator_DifferentInstancesParallel(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	h.createInstance(t, "par-a")
	h.createInstance(t, "par-b")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = h.orch.Start(context.Background(), "par-a") }()
	go func() { defer wg.Done(); _ = h.orch.Start(context.Background(), "par-b") }()
	wg.Wait()

	if got := h.state(t, "par-a"); got != protocol.InstanceStateRunning {
		t.Errorf("par-a observed = %s, 期望 Running", got)
	}
	if got := h.state(t, "par-b"); got != protocol.InstanceStateRunning {
		t.Errorf("par-b observed = %s, 期望 Running", got)
	}
}

// ---- 事件迴圈端到端(R8/R13):WS 崩潰偵測與斷線離線 ----

func TestOrchestrator_EventLoopDetectsCrash(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	rec := h.createInstance(t, "ws-crash")
	h.startInstance(t, "ws-crash")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.orch.RunEventLoop(ctx, "local")
	// 給事件流訂閱建立的時間(MockBackend 以 since="" 只轉發訂閱後的新事件)。
	time.Sleep(300 * time.Millisecond)

	ec := 137
	h.backend.InjectDie(rec.RuntimeID, ec)

	h.waitForState(t, "ws-crash", protocol.InstanceStateCrashed, 3*time.Second)
	if n := h.countEvents(t, protocol.EventInstanceCrashed); n < 1 {
		t.Errorf("INSTANCE_CRASHED = %d, 期望 >=1", n)
	}
	if h.crashCount() < 1 {
		t.Errorf("crashHook 應被觸發, 得 %d", h.crashCount())
	}
}

func TestOrchestrator_EventLoopMarksOfflineOnDisconnect(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	reg := NewNodeRegistry(events)
	// 指向不可達的節點:WS 撥接失敗 → 標離線 → NODE_OFFLINE。
	reg.Register("local", NewNodeClient("http://127.0.0.1:1", "tok", &http.Client{Timeout: time.Second}))
	orch := NewOrchestrator(OrchestratorConfig{Store: store, Events: events, Registry: reg})

	ctx, cancel := context.WithCancel(context.Background())
	go orch.RunEventLoop(ctx, "local")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(queryEvents(t, events, protocol.EventNodeOffline)) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	if n := len(queryEvents(t, events, protocol.EventNodeOffline)); n < 1 {
		t.Fatalf("斷線應記至少一筆 NODE_OFFLINE, 得 %d", n)
	}
}
