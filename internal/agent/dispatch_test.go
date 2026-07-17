package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// fakeBackend 是可觀測路由目標、可注入錯誤、可主動 emit 事件的 RuntimeBackend 測試替身。
type fakeBackend struct {
	name string
	hub  *eventHub

	mu       sync.Mutex
	lastOp   string
	lastID   protocol.RuntimeID
	createID protocol.RuntimeID

	createErr error
	listRefs  []protocol.RuntimeRef
	listErr   error
}

func newFakeBackend(name string) *fakeBackend {
	return &fakeBackend{name: name, hub: newEventHub(0)}
}

func (f *fakeBackend) record(op string, id protocol.RuntimeID) {
	f.mu.Lock()
	f.lastOp, f.lastID = op, id
	f.mu.Unlock()
}

func (f *fakeBackend) op() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastOp
}

func (f *fakeBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	f.record("Create", "")
	return f.createID, f.createErr
}
func (f *fakeBackend) Start(ctx context.Context, id protocol.RuntimeID) error {
	f.record("Start", id)
	return nil
}
func (f *fakeBackend) Stop(ctx context.Context, id protocol.RuntimeID, opts StopOpts) error {
	f.record("Stop", id)
	return nil
}
func (f *fakeBackend) Status(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeStatus, error) {
	f.record("Status", id)
	return protocol.RuntimeStatus{ID: id}, nil
}
func (f *fakeBackend) List(ctx context.Context) ([]protocol.RuntimeRef, error) {
	f.record("List", "")
	return f.listRefs, f.listErr
}
func (f *fakeBackend) Inspect(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeInfo, error) {
	f.record("Inspect", id)
	return protocol.RuntimeInfo{ID: id}, nil
}
func (f *fakeBackend) Remove(ctx context.Context, id protocol.RuntimeID, opts RemoveOpts) error {
	f.record("Remove", id)
	return nil
}
func (f *fakeBackend) Logs(ctx context.Context, id protocol.RuntimeID, opts LogOpts) (LogStream, error) {
	f.record("Logs", id)
	return nil, nil
}
func (f *fakeBackend) ExecProcess(ctx context.Context, id protocol.RuntimeID, cmd ExecCmd) (ExecResult, error) {
	f.record("ExecProcess", id)
	return ExecResult{}, nil
}
func (f *fakeBackend) Stats(ctx context.Context, id protocol.RuntimeID) (protocol.ResourceStats, error) {
	f.record("Stats", id)
	return protocol.ResourceStats{}, nil
}
func (f *fakeBackend) Archive(ctx context.Context, id protocol.RuntimeID) (protocol.BackupID, error) {
	f.record("Archive", id)
	return "", nil
}
func (f *fakeBackend) Restore(ctx context.Context, id protocol.RuntimeID, b protocol.BackupID) (protocol.RuntimeID, error) {
	f.record("Restore", id)
	return id, nil
}
func (f *fakeBackend) Events(ctx context.Context, since protocol.Cursor) (EventStream, error) {
	return f.hub.subscribe(ctx, since)
}
func (f *fakeBackend) Close() error {
	f.hub.close()
	return nil
}
func (f *fakeBackend) emit(ev RuntimeEvent) { f.hub.emit(ev) }

// TestDispatchCreateRoutesByRuntime 驗證 Create 依 spec.Runtime 路由,未知值回 ERR_BAD_REQUEST。
func TestDispatchCreateRoutesByRuntime(t *testing.T) {
	native := newFakeBackend("native")
	native.createID = "native:abc"
	docker := newFakeBackend("docker")
	docker.createID = "c123"
	d, err := NewDispatchBackend(native, docker)
	if err != nil {
		t.Fatalf("NewDispatchBackend: %v", err)
	}
	defer d.Close()
	ctx := context.Background()

	// 空 runtime → docker。
	if id, err := d.Create(ctx, protocol.InstanceSpec{}); err != nil || id != "c123" {
		t.Fatalf("空 runtime 應路由 docker,得 id=%q err=%v", id, err)
	}
	if docker.op() != "Create" {
		t.Fatalf("docker 應收到 Create,得 %q", docker.op())
	}
	// "docker" → docker。
	if id, _ := d.Create(ctx, protocol.InstanceSpec{Runtime: "docker"}); id != "c123" {
		t.Fatalf("docker runtime 路由錯誤,得 %q", id)
	}
	// "native" → native。
	if id, err := d.Create(ctx, protocol.InstanceSpec{Runtime: "native"}); err != nil || id != "native:abc" {
		t.Fatalf("native runtime 應路由 native,得 id=%q err=%v", id, err)
	}
	if native.op() != "Create" {
		t.Fatalf("native 應收到 Create,得 %q", native.op())
	}
	// 未知 runtime → ERR_BAD_REQUEST。
	_, err = d.Create(ctx, protocol.InstanceSpec{Runtime: "podman"})
	var apiErr *protocol.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != protocol.ErrBadRequest {
		t.Fatalf("未知 runtime 應回 ERR_BAD_REQUEST,得 %v", err)
	}
}

// TestDispatchRoutesByPrefix 驗證生命週期方法依 RuntimeID 前綴路由。
func TestDispatchRoutesByPrefix(t *testing.T) {
	native := newFakeBackend("native")
	docker := newFakeBackend("docker")
	d, _ := NewDispatchBackend(native, docker)
	defer d.Close()
	ctx := context.Background()

	_ = d.Start(ctx, "native:abc")
	if native.op() != "Start" || docker.op() == "Start" {
		t.Fatalf("native: 前綴應路由 native,native.op=%q docker.op=%q", native.op(), docker.op())
	}
	_ = d.Stop(ctx, "c123", StopOpts{})
	if docker.op() != "Stop" {
		t.Fatalf("裸 ID 應路由 docker,得 docker.op=%q", docker.op())
	}
	if _, err := d.Status(ctx, "native:xyz"); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if native.op() != "Status" {
		t.Fatalf("native Status 路由錯誤,得 %q", native.op())
	}
}

// TestDispatchUnavailableRuntime 驗證目標子後端為 nil 時回明確的 ErrRuntimeUnavailable。
func TestDispatchUnavailableRuntime(t *testing.T) {
	native := newFakeBackend("native")
	d, _ := NewDispatchBackend(native, nil) // docker=nil
	defer d.Close()
	ctx := context.Background()

	if _, err := d.Create(ctx, protocol.InstanceSpec{Runtime: "docker"}); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("docker 不可用時 Create 應回 ErrRuntimeUnavailable,得 %v", err)
	}
	if err := d.Start(ctx, "c123"); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("docker 不可用時裸 ID Start 應回 ErrRuntimeUnavailable,得 %v", err)
	}

	// 對稱:native=nil 時請求 native。
	docker := newFakeBackend("docker")
	d2, _ := NewDispatchBackend(nil, docker)
	defer d2.Close()
	if _, err := d2.Create(ctx, protocol.InstanceSpec{Runtime: "native"}); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("native 不可用時 Create 應回 ErrRuntimeUnavailable,得 %v", err)
	}
	if err := d2.Start(ctx, "native:abc"); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("native 不可用時 native: 前綴 Start 應回 ErrRuntimeUnavailable,得 %v", err)
	}
}

// TestDispatchListSoftDegrade 驗證 List 對單一子後端錯誤軟降級,全數失敗才回錯。
func TestDispatchListSoftDegrade(t *testing.T) {
	native := newFakeBackend("native")
	native.listRefs = []protocol.RuntimeRef{{ID: "native:n1"}}
	docker := newFakeBackend("docker")
	docker.listErr = errors.New("docker daemon 不可用")
	d, _ := NewDispatchBackend(native, docker)
	defer d.Close()
	ctx := context.Background()

	// docker 錯誤 → 軟降級:回 native refs,不整體失敗。
	refs, err := d.List(ctx)
	if err != nil {
		t.Fatalf("docker 錯誤應軟降級不回錯,得 %v", err)
	}
	if len(refs) != 1 || refs[0].ID != "native:n1" {
		t.Fatalf("軟降級應只回 native refs,得 %+v", refs)
	}

	// 全數失敗 → 回錯(避免空清單被誤判為全部消失)。
	native.listErr = errors.New("native 掃描失敗")
	if _, err := d.List(ctx); err == nil {
		t.Fatal("所有子後端失敗時 List 應回錯")
	}
}

// TestDispatchEventsAggregateWithPrefix 驗證兩子後端事件皆聚合、ID 前綴一致。
func TestDispatchEventsAggregateWithPrefix(t *testing.T) {
	native := newFakeBackend("native")
	docker := newFakeBackend("docker")
	d, _ := NewDispatchBackend(native, docker)
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := d.Events(ctx, "")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer stream.Close()

	nativeEv := RuntimeEvent{ID: "native:abc", Kind: RuntimeEventStart}
	dockerEv := RuntimeEvent{ID: "c123", Kind: RuntimeEventDie}
	got := collectUntil(t, stream, native, docker, nativeEv, dockerEv, map[protocol.RuntimeID]bool{
		"native:abc": true, "c123": true,
	})
	if !got["native:abc"] || !got["c123"] {
		t.Fatalf("應聚合兩子後端事件且前綴一致,得 %v", got)
	}
}

// TestDispatchSwapDockerReconnectsEvents 驗證 SwapDocker 後新 docker 的事件能接上,DockerAvailable 反映。
func TestDispatchSwapDockerReconnectsEvents(t *testing.T) {
	native := newFakeBackend("native")
	docker1 := newFakeBackend("docker1")
	d, _ := NewDispatchBackend(native, docker1)
	defer d.Close()

	if !d.DockerAvailable() {
		t.Fatal("初始 docker 非 nil,DockerAvailable 應為 true")
	}

	docker2 := newFakeBackend("docker2")
	d.SwapDocker(docker2)
	if !d.DockerAvailable() {
		t.Fatal("SwapDocker 後 DockerAvailable 應為 true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := d.Events(ctx, "")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer stream.Close()

	// 新 docker2 的事件應經重建的 pump 接上聚合 hub。
	newEv := RuntimeEvent{ID: "c999", Kind: RuntimeEventStart}
	got := collectUntil(t, stream, native, docker2, newEv, newEv, map[protocol.RuntimeID]bool{"c999": true})
	if !got["c999"] {
		t.Fatal("SwapDocker 後新 docker 事件未接上聚合串流")
	}

	// swap 為 nil → DockerAvailable false。
	d.SwapDocker(nil)
	if d.DockerAvailable() {
		t.Fatal("SwapDocker(nil) 後 DockerAvailable 應為 false")
	}
}

// TestNewDispatchBackendRequiresOne 驗證兩子後端皆 nil 時建構失敗。
func TestNewDispatchBackendRequiresOne(t *testing.T) {
	if _, err := NewDispatchBackend(nil, nil); err == nil {
		t.Fatal("兩子後端皆 nil 應建構失敗")
	}
}

// TestDispatchSetCurseForgeKey 驗證熱設定金鑰經 dispatch→native→adapter→provisioner 全鏈路生效
// (native-backend R14):以真 NativeBackend+provisionAdapter(無內嵌 key)組 dispatch,初始停用;
// SetCurseForgeKey 後 CurseForgeEnabled 翻真;清除(空字串,內嵌為空)後翻回停用。
func TestDispatchSetCurseForgeKey(t *testing.T) {
	adapter := NewProvisionAdapter(t.TempDir()) // 無內嵌/覆蓋 key
	native, err := NewNativeBackend(NativeOptions{
		DataRoot:   t.TempDir(),
		BackupRoot: t.TempDir(),
		CacheRoot:  t.TempDir(),
		Node:       "local",
		Prov:       adapter,
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	d, err := NewDispatchBackend(native, nil)
	if err != nil {
		t.Fatalf("NewDispatchBackend: %v", err)
	}
	defer d.Close()

	if d.CurseForgeEnabled() {
		t.Fatal("初始無 key 應停用")
	}
	d.SetCurseForgeKey("user-fake-key")
	if !d.CurseForgeEnabled() {
		t.Fatal("熱設定金鑰後 CurseForgeEnabled 應為 true(全鏈路生效)")
	}
	d.SetCurseForgeKey("")
	if d.CurseForgeEnabled() {
		t.Fatal("清除金鑰後(內嵌為空)應翻回停用")
	}
}

// collectUntil 反覆在指定 fake 上 emit(規避 pump 訂閱競態),收集聚合串流上出現的 ID 直到集齊
// wanted 或逾時。emitOn 指定要 emit evA/evB 的兩個 fake(可為同一個)。
func collectUntil(t *testing.T, stream EventStream, fa, fb *fakeBackend, evA, evB RuntimeEvent, wanted map[protocol.RuntimeID]bool) map[protocol.RuntimeID]bool {
	t.Helper()
	got := make(map[protocol.RuntimeID]bool)
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	fa.emit(evA)
	fb.emit(evB)
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				return got
			}
			if _, want := wanted[ev.ID]; want {
				got[ev.ID] = true
			}
			done := true
			for id := range wanted {
				if !got[id] {
					done = false
					break
				}
			}
			if done {
				return got
			}
		case <-tick.C:
			fa.emit(evA)
			fb.emit(evB)
		case <-deadline:
			return got
		}
	}
}
