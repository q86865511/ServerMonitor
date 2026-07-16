package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// minimalTemplate 是測試用的最小合法範本(runtime=docker、無 params/secrets/ports),
// 讓 InstanceService.Create 在 MockBackend 下無需遊戲語意即可跑通。
const minimalTemplate = `schema_version = 1
id = "testgame"
name = "Test Game"
runtime = "docker"
data_dirs = ["/data"]

[docker]
image = "busybox:latest"
`

// labelInjectBackend 在 Create 時補上 gsm.uuid 標籤——DockerBackend 於 Create 內自行注入
// 該標籤(agent Server 以此把路徑 uuid 解析回 RuntimeID),MockBackend 則仰賴呼叫端提供;
// 本包裝讓 MockBackend 在完整 core→agent 鏈路下也可被以 uuid 定址,忠實模擬生產後端。
type labelInjectBackend struct{ *agent.MockBackend }

func (b labelInjectBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels["gsm.uuid"] = spec.UUID
	return b.MockBackend.Create(ctx, spec)
}

// fakeChannel 是記錄告警 payload 的 AlertChannel 測試替身。
type fakeChannel struct{ ch chan core.AlertPayload }

func newFakeChannel() *fakeChannel { return &fakeChannel{ch: make(chan core.AlertPayload, 32)} }

func (f *fakeChannel) Send(_ context.Context, p core.AlertPayload) error {
	f.ch <- p
	return nil
}

// bootOpts 準備一個獨立 dataRoot(含最小範本)與注入 MockBackend 的 Options。
// 回傳 Options、指向所建 MockBackend 的指標位址、與 dataRoot。
func bootOpts(t *testing.T) (Options, **agent.MockBackend, string) {
	t.Helper()
	dataRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataRoot, "templates"), 0o755); err != nil {
		t.Fatalf("建立範本目錄: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "templates", "testgame.toml"), []byte(minimalTemplate), 0o644); err != nil {
		t.Fatalf("寫入範本: %v", err)
	}
	builtinEmpty := t.TempDir()

	var mb *agent.MockBackend
	holder := &mb
	factory := func(_ agent.BackendOptions) (agent.RuntimeBackend, error) {
		*holder = agent.NewMockBackend()
		return labelInjectBackend{*holder}, nil
	}
	return Options{
		DataRoot:           dataRoot,
		BuiltinTemplateDir: builtinEmpty,
		BackendFactory:     factory,
	}, holder, dataRoot
}

func TestBootstrapAssembly(t *testing.T) {
	opts, _, _ := bootOpts(t)
	rt, err := Bootstrap(opts)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer rt.Shutdown()

	// journal 兩目錄必相異(op-journal 設計要求)。
	if subDirJournalCreate == subDirJournalOps {
		t.Fatal("journal create/ops 子路徑不應相同")
	}
	if _, err := os.Stat(filepath.Join(rt.dataRoot, subDirJournalCreate)); err != nil {
		t.Fatalf("create journal 目錄不存在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rt.dataRoot, subDirJournalOps)); err != nil {
		t.Fatalf("ops journal 目錄不存在: %v", err)
	}

	// agent listener 必為 loopback(R5)。
	tcp, ok := rt.agentLn.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		t.Fatalf("agent 未綁定 loopback: %v", rt.agentLn.Addr())
	}

	// 節點應線上(mock 後端可用)。
	st, ok := rt.registry.Status(rt.node)
	if !ok || !st.Online {
		t.Fatalf("節點應線上: %+v", st)
	}

	// 關鍵服務皆已組裝。
	if rt.orch == nil || rt.backups == nil || rt.sched == nil || rt.monitor == nil ||
		rt.threshold == nil || rt.health == nil || rt.instances == nil || rt.commands == nil {
		t.Fatal("服務組裝不完整")
	}
}

// TestBootstrapDispatchRetryDocker 驗證 dispatch 頂層(native-backend R13):native 恆在使節點在線,
// docker=nil 時 DockerAvailable=false;RetryDocker 經 DockerFactory 熱替換 docker 子後端(不重建
// 整個 agent),DockerAvailable 翻為 true。
func TestBootstrapDispatchRetryDocker(t *testing.T) {
	dataRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataRoot, "templates"), 0o755); err != nil {
		t.Fatalf("建立範本目錄: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "templates", "testgame.toml"), []byte(minimalTemplate), 0o644); err != nil {
		t.Fatalf("寫入範本: %v", err)
	}
	// 頂層為 dispatch(native=mock、docker=nil);DockerFactory 供 RetryDocker 熱替換用。
	dispatchFactory := func(_ agent.BackendOptions) (agent.RuntimeBackend, error) {
		return agent.NewDispatchBackend(agent.NewMockBackend(), nil)
	}
	dockerFactory := func(_ agent.BackendOptions) (agent.RuntimeBackend, error) {
		return agent.NewMockBackend(), nil
	}
	rt, err := Bootstrap(Options{
		DataRoot:           dataRoot,
		BuiltinTemplateDir: t.TempDir(),
		BackendFactory:     dispatchFactory,
		DockerFactory:      dockerFactory,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer rt.Shutdown()

	// native 恆在 → 節點在線,即使 docker 不可用。
	if st, ok := rt.registry.Status(rt.node); !ok || !st.Online {
		t.Fatalf("dispatch 頂層節點應在線(native 恆在): %+v", st)
	}
	if rt.DockerAvailable() {
		t.Fatal("初始 docker=nil,DockerAvailable 應為 false")
	}
	if err := rt.RetryDocker(); err != nil {
		t.Fatalf("RetryDocker: %v", err)
	}
	if !rt.DockerAvailable() {
		t.Fatal("RetryDocker 熱替換後 DockerAvailable 應為 true")
	}
}

func TestBootstrapAppLockConflict(t *testing.T) {
	opts, _, _ := bootOpts(t)
	rt1, err := Bootstrap(opts)
	if err != nil {
		t.Fatalf("首次 Bootstrap: %v", err)
	}
	defer rt1.Shutdown()

	// 同一 dataRoot 二次啟動應被單一實例鎖拒絕。
	opts2 := opts
	opts2.BackendFactory = func(_ agent.BackendOptions) (agent.RuntimeBackend, error) {
		return agent.NewMockBackend(), nil
	}
	_, err = Bootstrap(opts2)
	if err == nil {
		t.Fatal("二次啟動應回 ErrAppLocked")
	}
	if err != ErrAppLocked && err != core.ErrAppLocked {
		t.Fatalf("預期 ErrAppLocked,得: %v", err)
	}
}

func TestStartupReconcileAndRecover(t *testing.T) {
	opts, _, _ := bootOpts(t)
	rt, err := Bootstrap(opts)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer rt.Shutdown()

	// 寫入一筆指向不存在實例的殘留 op-journal:RecoverInterrupted 應將其清除(認定實例已不存在)。
	if err := rt.opJrnl.Begin(core.OpJournalEntry{UUID: "ghost", Node: rt.node, Kind: core.OpKindBackup}); err != nil {
		t.Fatalf("寫入 op-journal: %v", err)
	}
	rt.Start()

	// 對帳與復原在 Start 內同步執行完;殘留 journal 應已被清除。
	entries, err := rt.opJrnl.List()
	if err != nil {
		t.Fatalf("列舉 op-journal: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("RecoverInterrupted 應清除殘留 journal,仍有 %d 筆", len(entries))
	}
}

func TestCrashHookDispatchesAlertAndRestart(t *testing.T) {
	opts, mbHolder, _ := bootOpts(t)
	fake := newFakeChannel()
	opts.AlertChannel = fake
	rt, err := Bootstrap(opts)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer rt.Shutdown()

	ctx := context.Background()
	// 先建立(此時尚未啟動事件迴圈,避免對帳與建立中 journal 競態),再啟動背景迴圈。
	uuid, err := rt.Create(ctx, core.CreateOptions{TemplateID: "testgame"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rt.Start()
	if err := rt.StartInstance(ctx, uuid.UUID); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	rec, err := rt.store.GetInstance(uuid.UUID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if rec.ObservedState != protocol.InstanceStateRunning {
		t.Fatalf("實例應為 Running,得 %s", rec.ObservedState)
	}

	mb := *mbHolder
	if mb == nil {
		t.Fatal("MockBackend 未建立")
	}

	// 反覆注入 die,直到崩潰事件被事件迴圈消費並經 crashHook 派發告警(避免與 WS 訂閱建立競態;
	// 一旦觀測轉 Crashed,後續 die 會被 handleDie 去重,不重複告警)。
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	mb.InjectDie(rec.RuntimeID, 1)
	for {
		select {
		case p := <-fake.ch:
			if p.Code != protocol.EventInstanceCrashed {
				continue
			}
			if p.InstanceUUID != uuid.UUID {
				t.Fatalf("告警 InstanceUUID 不符:%s", p.InstanceUUID)
			}
			if p.Node == "" || p.TemplateID == "" {
				t.Fatalf("告警未 enrich node/template: %+v", p)
			}
			return // 成功:crashHook 執行了 OnCrash 後的告警派發(證明整條 crashHook 已接線)
		case <-tick.C:
			mb.InjectDie(rec.RuntimeID, 1)
		case <-deadline:
			t.Fatal("逾時未收到 INSTANCE_CRASHED 告警")
		}
	}
}
