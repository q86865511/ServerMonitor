package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// t15Backend 是可配置的測試後端:Create 注入 gsm.uuid 標籤(同 labelInjectBackend),並可
// 選擇性讓 Start 阻塞至 ctx 取消(blockStart)或讓 Stop 回錯(stopErr)。
type t15Backend struct {
	*agent.MockBackend
	blockStart   bool
	startEntered chan struct{}
	startOnce    sync.Once
	stopErr      error
}

func newT15Backend() *t15Backend {
	return &t15Backend{MockBackend: agent.NewMockBackend(), startEntered: make(chan struct{})}
}

func (b *t15Backend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels["gsm.uuid"] = spec.UUID
	return b.MockBackend.Create(ctx, spec)
}

func (b *t15Backend) Start(ctx context.Context, id protocol.RuntimeID) error {
	if b.blockStart {
		b.startOnce.Do(func() { close(b.startEntered) })
		<-ctx.Done()
		return ctx.Err()
	}
	return b.MockBackend.Start(ctx, id)
}

func (b *t15Backend) Stop(ctx context.Context, id protocol.RuntimeID, opts agent.StopOpts) error {
	if b.stopErr != nil {
		return b.stopErr
	}
	return b.MockBackend.Stop(ctx, id, opts)
}

// t15Opts 準備一個獨立 dataRoot(含最小範本)與注入指定 factory 的 Options。
func t15Opts(t *testing.T, factory BackendFactory) Options {
	t.Helper()
	dataRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataRoot, "templates"), 0o755); err != nil {
		t.Fatalf("建立範本目錄: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "templates", "testgame.toml"), []byte(minimalTemplate), 0o644); err != nil {
		t.Fatalf("寫入範本: %v", err)
	}
	return Options{
		DataRoot:           dataRoot,
		BuiltinTemplateDir: t.TempDir(),
		BackendFactory:     factory,
	}
}

// TestOfflineBootstrapRetryDockerStats 驗證 T15 雙審 #1:離線啟動(佔位 client)後經 RetryDocker
// 恢復,啟動實例並訂閱 stats 不 nil-panic 且能收到資料(證明 MonitorHub 經 registry dialer 取當前
// client,一次解決 (a) 離線 nil dialer 與 (b) RetryDocker 後仍用舊連線)。以 -race 執行以捕捉競態。
func TestOfflineBootstrapRetryDockerStats(t *testing.T) {
	var attempts int32
	var backend *t15Backend
	factory := func(_ agent.BackendOptions) (agent.RuntimeBackend, error) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			return nil, errors.New("docker 不可用(模擬離線啟動)")
		}
		backend = newT15Backend()
		return backend, nil
	}
	rt, err := Bootstrap(t15Opts(t, factory))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer rt.Shutdown()

	// 離線啟動:節點應離線。
	if st, ok := rt.registry.Status(rt.node); !ok || st.Online {
		t.Fatalf("離線啟動後節點應離線: %+v", st)
	}

	// RetryDocker 恢復連線(沿用注入的 factory)。
	if err := rt.RetryDocker(); err != nil {
		t.Fatalf("RetryDocker: %v", err)
	}
	if st, ok := rt.registry.Status(rt.node); !ok || !st.Online {
		t.Fatalf("RetryDocker 後節點應線上: %+v", st)
	}

	// 先建立(此時尚未啟動事件迴圈,避免對帳與建立中 journal 競態),再啟動背景迴圈。
	ctx := context.Background()
	rec, err := rt.Create(ctx, core.CreateOptions{TemplateID: "testgame"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rt.Start()
	if err := rt.StartInstance(ctx, rec.UUID); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}

	// 訂閱 stats:經 registry dialer 撥到 RetryDocker 建立的新 client,應收到資料且不 panic。
	ch, id := rt.SubscribeStats(rec.UUID)
	defer rt.UnsubscribeStats(rec.UUID, id)
	select {
	case <-ch:
		// 收到一筆取樣即證明串流經新 client 打通。
	case <-time.After(8 * time.Second):
		t.Fatal("未在時限內收到 stats(registry dialer 未打通新連線?)")
	}
}

// TestShutdownDrainsInflightOp 驗證 T15 雙審 #3:關閉時進行中的長操作(ctx 衍生自 rootCtx)被
// 取消,Shutdown 於時限內完成(不因 in-flight 操作卡住),且該操作返回。
func TestShutdownDrainsInflightOp(t *testing.T) {
	backend := newT15Backend()
	backend.blockStart = true
	factory := func(_ agent.BackendOptions) (agent.RuntimeBackend, error) {
		return backend, nil
	}
	rt, err := Bootstrap(t15Opts(t, factory))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// 先建立(事件迴圈未起,避免對帳競態),再啟動背景迴圈。
	ctx := context.Background()
	rec, err := rt.Create(ctx, core.CreateOptions{TemplateID: "testgame"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rt.Start()

	// 於背景啟動實例:backend.Start 會阻塞至 op ctx 取消(模擬慢操作)。
	startErr := make(chan error, 1)
	go func() { startErr <- rt.StartInstance(ctx, rec.UUID) }()

	// 等 Start 真正進入阻塞後再關閉。
	select {
	case <-backend.startEntered:
	case err := <-startErr:
		t.Fatalf("StartInstance 於進入 backend.Start 前即返回: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("StartInstance 未進入 backend.Start")
	}

	// Shutdown 應取消 in-flight 操作並於時限內完成(遠低於 shutdownDrainTimeout 10s——op ctx 被
	// rootCancel 取消後 StartInstance 立即返回)。
	shutStart := time.Now()
	done := make(chan struct{})
	go func() { rt.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("Shutdown 未在時限內完成(已 %s)", time.Since(shutStart))
	}

	// 被取消的 StartInstance 應返回(帶錯誤)。
	select {
	case err := <-startErr:
		if err == nil {
			t.Fatal("關閉取消下 StartInstance 應回錯,得 nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartInstance 未返回")
	}
}

// TestStopFailureKeepsMonitoring 驗證 T15 雙審 #9:StopInstance 失敗時監控維持(不先解除監控),
// 避免對仍在執行的實例失去可觀測性。
func TestStopFailureKeepsMonitoring(t *testing.T) {
	backend := newT15Backend()
	factory := func(_ agent.BackendOptions) (agent.RuntimeBackend, error) {
		return backend, nil
	}
	rt, err := Bootstrap(t15Opts(t, factory))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer rt.Shutdown()

	// 先建立(事件迴圈未起,避免對帳競態),再啟動背景迴圈。
	ctx := context.Background()
	rec, err := rt.Create(ctx, core.CreateOptions{TemplateID: "testgame"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rt.Start()
	if err := rt.StartInstance(ctx, rec.UUID); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	// 啟動後應在監控中。
	if _, ok := rt.monitor.Snapshot(rec.UUID); !ok {
		t.Fatal("啟動後實例應在監控中")
	}

	// 注入停止失敗。
	backend.stopErr = errors.New("模擬代理停止失敗")
	if err := rt.StopInstance(ctx, rec.UUID); err == nil {
		t.Fatal("StopInstance 應因後端停止失敗而回錯")
	}

	// 停止失敗→監控仍在(watched 與 monitor 皆維持)。
	if _, ok := rt.monitor.Snapshot(rec.UUID); !ok {
		t.Fatal("停止失敗後監控應維持(monitor),但已被解除")
	}
	rt.watchedMu.Lock()
	watched := rt.watched[rec.UUID]
	rt.watchedMu.Unlock()
	if !watched {
		t.Fatal("停止失敗後門檻監看應維持(watched),但已被解除")
	}
}
