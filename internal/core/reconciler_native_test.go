package core

// 對帳納入 native 的整合驗證(native-backend R13 / T11)。Reconciler(reconciler.go)刻意不改——本檔
// 驗證它經聚合 dispatchBackend.List 取得的 native refs(帶 "native:" 前綴 RuntimeID 與 gsm.* 標籤)
// 對帳正確,且 dispatch 對 docker 子後端 List 失敗軟降級時,native 對帳不受影響、不誤判離線。

import (
	"context"
	"errors"
	"testing"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// prefixedListBackend 包裝 MockBackend:List 時為每個 ref 冠上前綴以模擬 NativeBackend 的 RuntimeID
// 慣例("native:"+uuid);listErr 非 nil 時 List 直接回錯,模擬 docker 子後端 daemon 不可用(dispatch
// 據以軟降級)。其餘方法沿用 MockBackend。
type prefixedListBackend struct {
	*agent.MockBackend
	prefix  string
	listErr error
}

func (b *prefixedListBackend) List(ctx context.Context) ([]protocol.RuntimeRef, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	refs, err := b.MockBackend.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range refs {
		refs[i].ID = protocol.RuntimeID(b.prefix + string(refs[i].ID))
	}
	return refs, nil
}

// reconNativeEnv 是對帳 native 測試環境:dispatchBackend(native=前綴 MockBackend、docker=List 失敗
// MockBackend)經真 HTTP 代理暴露,搭配暫存 Store/EventLog/NodeRegistry/Journal 與 Reconciler。
type reconNativeEnv struct {
	t      *testing.T
	native *agent.MockBackend // 直接操作以佈置 native ref 狀態
	store  *Store
	events *EventLog
	recon  *Reconciler
	node   string
}

func newReconNativeEnv(t *testing.T) *reconNativeEnv {
	t.Helper()
	nativeMock := agent.NewMockBackend()
	native := &prefixedListBackend{MockBackend: nativeMock, prefix: "native:"}
	// docker 子後端 List 恆失敗:驗 dispatch 軟降級(native 對帳不受 Docker 不可用影響)。
	docker := &prefixedListBackend{MockBackend: agent.NewMockBackend(), listErr: errors.New("docker daemon 不可用")}
	disp, err := agent.NewDispatchBackend(native, docker)
	if err != nil {
		t.Fatalf("NewDispatchBackend: %v", err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	hs := newAgentServerWithBackend(t, disp)

	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	reg := NewNodeRegistry(events)
	reg.Register("local", NewNodeClient(hs.URL, agentTestToken, nil))
	journal, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	recon := NewReconciler(ReconcilerConfig{Store: store, Events: events, Registry: reg, Journal: journal})

	return &reconNativeEnv{
		t: t, native: nativeMock, store: store, events: events, recon: recon, node: "local",
	}
}

// createNativeRef 在 native 子後端建立一個帶 gsm.* 標籤的 ref,回傳其(未加前綴的)後端 RuntimeID。
func (e *reconNativeEnv) createNativeRef(uuid string) protocol.RuntimeID {
	e.t.Helper()
	rid, err := e.native.Create(context.Background(), protocol.InstanceSpec{
		UUID: uuid,
		Labels: map[string]string{
			labelUUID:      uuid,
			labelManagedBy: managedByValue,
			labelNode:      e.node,
		},
	})
	if err != nil {
		e.t.Fatalf("native.Create(%s): %v", uuid, err)
	}
	return rid
}

// upsertDB 直接寫一列 DB 實例(指定 observed 狀態)。
func (e *reconNativeEnv) upsertDB(uuid string, observed protocol.InstanceState) {
	e.t.Helper()
	if err := e.store.UpsertInstance(InstanceRecord{
		UUID:          uuid,
		TemplateID:    "native-t",
		Node:          e.node,
		RuntimeID:     protocol.RuntimeID("native:" + uuid),
		DesiredState:  protocol.InstanceStateRunning,
		ObservedState: observed,
	}); err != nil {
		e.t.Fatalf("UpsertInstance(%s): %v", uuid, err)
	}
}

func (e *reconNativeEnv) state(uuid string) protocol.InstanceState {
	e.t.Helper()
	rec, err := e.store.GetInstance(uuid)
	if err != nil {
		e.t.Fatalf("GetInstance(%s): %v", uuid, err)
	}
	return rec.ObservedState
}

func (e *reconNativeEnv) countEvents(code protocol.EventCode) int {
	return len(queryEvents(e.t, e.events, code))
}

// R13:DB 記 running 但 native 行程已死(List 回 exited)→ 修正為 stopped 並記 RECONCILE_MISMATCH。
// docker 子後端 List 失敗但 dispatch 軟降級,native 對帳照常(不誤判離線)。
func TestReconcileNative_DBRunningProcessDead(t *testing.T) {
	e := newReconNativeEnv(t)
	rid := e.createNativeRef("nat-dead")
	e.native.Start(context.Background(), rid)
	e.native.InjectDie(rid, 1) // native ref 現為 exited
	e.upsertDB("nat-dead", protocol.InstanceStateRunning)

	sum, err := e.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.NodeOffline {
		t.Fatal("docker 子後端失敗不應被判為節點離線(native 對帳照常)")
	}
	if got := e.state("nat-dead"); got != protocol.InstanceStateStopped {
		t.Fatalf("行程已死應修正 observed=Stopped, 得 %s", got)
	}
	if len(sum.Mismatched) != 1 || sum.Mismatched[0] != "nat-dead" {
		t.Fatalf("summary.Mismatched=%v 期望 [nat-dead]", sum.Mismatched)
	}
	if n := e.countEvents(protocol.EventReconcileMismatch); n != 1 {
		t.Fatalf("RECONCILE_MISMATCH=%d 期望 1", n)
	}
}

// R13:native 行程存活但 DB 記 stopped → 依既有規則以實際更新 observed=Running(與 Docker 對帳同策略)。
func TestReconcileNative_ProcessAliveDBStopped(t *testing.T) {
	e := newReconNativeEnv(t)
	rid := e.createNativeRef("nat-alive")
	e.native.Start(context.Background(), rid) // native ref running
	e.upsertDB("nat-alive", protocol.InstanceStateStopped)

	sum, err := e.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := e.state("nat-alive"); got != protocol.InstanceStateRunning {
		t.Fatalf("行程存活應以實際更新 observed=Running, 得 %s", got)
	}
	if len(sum.Mismatched) != 1 || sum.Mismatched[0] != "nat-alive" {
		t.Fatalf("summary.Mismatched=%v 期望 [nat-alive]", sum.Mismatched)
	}
}

// R13:docker 子後端不可用(List 失敗)時,dispatch 軟降級回 native refs,Reconciler 不判離線、不把
// 存活的 native 實例誤標為缺失。
func TestReconcileNative_DockerDownDoesNotMislabelNative(t *testing.T) {
	e := newReconNativeEnv(t)
	rid := e.createNativeRef("nat-ok")
	e.native.Start(context.Background(), rid)
	e.upsertDB("nat-ok", protocol.InstanceStateRunning) // DB 與實際一致

	sum, err := e.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.NodeOffline {
		t.Fatal("docker 不可用不應判節點離線")
	}
	if len(sum.Missing) != 0 {
		t.Fatalf("存活 native 實例不應被判缺失, Missing=%v", sum.Missing)
	}
	if got := e.state("nat-ok"); got != protocol.InstanceStateRunning {
		t.Fatalf("一致狀態不應被更動, 得 %s", got)
	}
}
