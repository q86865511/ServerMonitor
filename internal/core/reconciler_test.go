package core

import (
	"context"
	"net/http"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// createOrphanContainer 直接在後端建一個帶 gsm.uuid 標籤、但 DB 無對應紀錄的容器(孤兒)。
func (h *lifeHarness) createOrphanContainer(t *testing.T, uuid string) protocol.RuntimeID {
	t.Helper()
	rid, err := h.backend.Create(context.Background(), protocol.InstanceSpec{
		UUID: uuid,
		Labels: map[string]string{
			labelUUID:      uuid,
			labelManagedBy: managedByValue,
			labelNode:      "local",
		},
	})
	if err != nil {
		t.Fatalf("建立孤兒容器: %v", err)
	}
	return rid
}

func (h *lifeHarness) hasContainer(t *testing.T, uuid string) bool {
	t.Helper()
	refs, err := h.backend.List(context.Background())
	if err != nil {
		t.Fatalf("backend.List: %v", err)
	}
	for _, r := range refs {
		if r.Labels[labelUUID] == uuid {
			return true
		}
	}
	return false
}

// R13:DB 有而容器不存在 → 標記(observed 修正、記 RECONCILE_MISMATCH),不誤刪、不離線。
func TestReconciler_DBHasContainerMissing(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	rec := h.createInstance(t, "miss-1") // observed=Created,後端有 Created 容器
	if err := h.backend.Remove(context.Background(), rec.RuntimeID, agent.RemoveOpts{}); err != nil {
		t.Fatalf("移除容器: %v", err)
	}

	sum, err := h.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.NodeOffline {
		t.Fatal("空清單不應被判為離線")
	}
	if got := h.state(t, "miss-1"); got != protocol.InstanceStateStopped {
		t.Errorf("容器缺失後 observed = %s, 期望 Stopped", got)
	}
	if n := h.countEvents(t, protocol.EventReconcileMismatch); n != 1 {
		t.Errorf("RECONCILE_MISMATCH = %d, 期望 1", n)
	}
	if _, gerr := h.store.GetInstance("miss-1"); gerr != nil {
		t.Errorf("實例不應被刪除: %v", gerr)
	}
	if len(sum.Missing) != 1 || sum.Missing[0] != "miss-1" {
		t.Errorf("summary.Missing = %v, 期望 [miss-1]", sum.Missing)
	}
}

// R13(缺陷修復):容器不存在且 observed 為 Error(如啟動失敗、或卡死放棄後容器又被外部刪除)時,
// 對帳應收斂為 Stopped 並記 RECONCILE_MISMATCH——先前 expectsContainer 排除 Error,使這種實例
// 永遠卡在 Error(GUI 只剩「移除實例」可用)。
func TestReconciler_ErrorStateContainerMissingResolves(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	rec := h.createInstance(t, "err-1")

	// 竄改 observed 為 Error(模擬啟動失敗或卡死復原放棄後的狀態),並移除底層容器。
	cur, err := h.store.GetInstance("err-1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	cur.ObservedState = protocol.InstanceStateError
	if err := h.store.UpsertInstance(cur); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}
	if err := h.backend.Remove(context.Background(), rec.RuntimeID, agent.RemoveOpts{}); err != nil {
		t.Fatalf("移除容器: %v", err)
	}

	sum, err := h.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := h.state(t, "err-1"); got != protocol.InstanceStateStopped {
		t.Errorf("Error 狀態容器缺失後應收斂為 Stopped, 得 %s", got)
	}
	if len(sum.Missing) != 1 || sum.Missing[0] != "err-1" {
		t.Errorf("summary.Missing = %v, 期望 [err-1]", sum.Missing)
	}
	if n := h.countEvents(t, protocol.EventReconcileMismatch); n != 1 {
		t.Errorf("RECONCILE_MISMATCH = %d, 期望 1", n)
	}
}

// R13:容器有 gsm.uuid 而 DB 無 → 孤兒(記 RECONCILE_ORPHAN,不自動刪)。
func TestReconciler_ContainerOrphanFlagged(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	h.createOrphanContainer(t, "orphan-x")

	sum, err := h.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(sum.Orphans) != 1 || sum.Orphans[0] != "orphan-x" {
		t.Errorf("summary.Orphans = %v, 期望 [orphan-x]", sum.Orphans)
	}
	if n := h.countEvents(t, protocol.EventReconcileOrphan); n != 1 {
		t.Errorf("RECONCILE_ORPHAN = %d, 期望 1", n)
	}
	if !h.hasContainer(t, "orphan-x") {
		t.Errorf("孤兒容器不應被自動刪除")
	}
	if _, gerr := h.store.GetInstance("orphan-x"); gerr == nil {
		t.Errorf("不應為孤兒自動建立 DB 紀錄")
	}
}

// R13:狀態不一致 → 以實際更新 observed 並記 RECONCILE_MISMATCH。
func TestReconciler_StateMismatchCorrected(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	h.createInstance(t, "mm-1")
	h.startInstance(t, "mm-1") // 後端 Running,observed Running

	// 竄改 DB observed 為 Stopped,模擬離線期間漏掉的狀態變化。
	rec, _ := h.store.GetInstance("mm-1")
	rec.ObservedState = protocol.InstanceStateStopped
	if err := h.store.UpsertInstance(rec); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}

	sum, err := h.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := h.state(t, "mm-1"); got != protocol.InstanceStateRunning {
		t.Errorf("應以實際 Running 更新 observed, 得 %s", got)
	}
	if len(sum.Mismatched) != 1 || sum.Mismatched[0] != "mm-1" {
		t.Errorf("summary.Mismatched = %v, 期望 [mm-1]", sum.Mismatched)
	}
	if n := h.countEvents(t, protocol.EventReconcileMismatch); n != 1 {
		t.Errorf("RECONCILE_MISMATCH = %d, 期望 1", n)
	}
}

// R13:List 傳輸失敗(daemon 不可達)≠ 空清單 → 標離線、實例 observed=Offline,不誤刪不誤標。
func TestReconciler_ListFailureIsOfflineNotMislabel(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	h.createInstance(t, "off-1")

	// 換成不可達 client:List 連線被拒 → ErrNodeUnreachable。
	h.reg.Register("local", NewNodeClient("http://127.0.0.1:1", agentTestToken, &http.Client{Timeout: time.Second}))

	sum, err := h.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !sum.NodeOffline {
		t.Fatal("daemon 不可達應標 NodeOffline")
	}
	if got := h.state(t, "off-1"); got != protocol.InstanceStateOffline {
		t.Errorf("離線後 observed = %s, 期望 Offline", got)
	}
	if _, gerr := h.store.GetInstance("off-1"); gerr != nil {
		t.Errorf("離線不應刪除實例: %v", gerr)
	}
	if n := h.countEvents(t, protocol.EventNodeOffline); n < 1 {
		t.Errorf("NODE_OFFLINE = %d, 期望 >=1", n)
	}
}

// R13/R2:journal 有未完成的建立 → 依 journal 清理(移除孤兒容器 + 釋放埠 + 清 journal)。
func TestReconciler_JournalIncompleteCreateCleaned(t *testing.T) {
	h := newLifeHarness(t, lifeOpts{})
	// 孤兒容器 + 埠預留 + journal 紀錄,但無 DB 完成紀錄(模擬「建容器後、寫 DB 前」崩潰)。
	h.createOrphanContainer(t, "inc-1")
	if err := h.store.ReservePort(PortReservation{
		BindIP: "0.0.0.0", Protocol: "tcp", HostPort: 34567, InstanceUUID: "inc-1",
	}); err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	if err := h.journal.Begin(JournalEntry{
		UUID: "inc-1", Node: "local", TemplateID: "life", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("journal.Begin: %v", err)
	}

	sum, err := h.recon.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if entries, _ := h.journal.List(); len(entries) != 0 {
		t.Errorf("journal 應清空, 得 %+v", entries)
	}
	if h.hasContainer(t, "inc-1") {
		t.Errorf("未完成建立的孤兒容器應被清除")
	}
	ports, _ := h.store.ListPortReservations()
	for _, p := range ports {
		if p.InstanceUUID == "inc-1" {
			t.Errorf("未完成建立的埠預留應被釋放, 仍有 %+v", p)
		}
	}
	if len(sum.JournalCleaned) != 1 || sum.JournalCleaned[0] != "inc-1" {
		t.Errorf("summary.JournalCleaned = %v, 期望 [inc-1]", sum.JournalCleaned)
	}
	if n := h.countEvents(t, protocol.EventReconcileOrphan); n < 1 {
		t.Errorf("RECONCILE_ORPHAN = %d, 期望 >=1", n)
	}
}
