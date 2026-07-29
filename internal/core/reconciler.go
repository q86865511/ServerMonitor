package core

import (
	"context"
	"errors"
	"time"

	"servermonitor/internal/protocol"
)

// Reconciler 執行啟動(與 resync)對帳(R13):比對 DB 與代理實際容器,區分「daemon 不可用」
// 與「容器清單為空」,處理孤兒/不一致/journal 未完成的建立,並產出 summary。
// 對 observed 的修正屬「以實際為準」的權威覆寫(不經狀態機轉移守衛)。
type Reconciler struct {
	store    *Store
	events   *EventLog
	registry *NodeRegistry
	journal  *Journal
	now      func() time.Time
}

// ReconcilerConfig 是 Reconciler 的建構參數。
type ReconcilerConfig struct {
	Store    *Store
	Events   *EventLog
	Registry *NodeRegistry
	Journal  *Journal
	Now      func() time.Time
}

// NewReconciler 建立 Reconciler。
func NewReconciler(cfg ReconcilerConfig) *Reconciler {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Reconciler{
		store:    cfg.Store,
		events:   cfg.Events,
		registry: cfg.Registry,
		journal:  cfg.Journal,
		now:      now,
	}
}

// ReconcileSummary 是一次對帳的結果摘要(R13:離線期至少產生 summary)。
type ReconcileSummary struct {
	Node           string   // 對帳的節點
	NodeOffline    bool     // 代理/daemon 不可達(此時不動容器、不誤刪)
	Orphans        []string // 有 gsm.uuid 容器但 DB 無(不自動刪,留 GUI 決策)
	Missing        []string // DB 有但容器不存在(observed 已修正)
	Mismatched     []string // 狀態不一致(observed 已以實際更新)
	JournalCleaned []string // 依 journal 清理的未完成建立
}

// Reconcile 對指定節點執行一次對帳(R13)。回傳 summary;List 傳輸失敗時回 NodeOffline summary
// (nil error——離線是常態結果而非對帳失敗)。
func (r *Reconciler) Reconcile(ctx context.Context, node string) (ReconcileSummary, error) {
	summary := ReconcileSummary{Node: node}

	dbInsts, err := r.instancesForNode(node)
	if err != nil {
		return summary, err
	}

	// 首次 List:區分 daemon 不可用(傳輸錯誤)vs 空清單。
	agentList, lerr := r.listAgent(ctx, node)
	if lerr != nil {
		if errors.Is(lerr, ErrNodeUnreachable) {
			// daemon/代理不可達:標實例離線、不誤刪不誤標;NODE_OFFLINE 已由 registry.Call 於轉換時記錄。
			r.markInstancesOffline(dbInsts)
			summary.NodeOffline = true
			return summary, nil
		}
		return summary, lerr
	}

	// journal 未完成的建立:先清理(可能移除孤兒容器),再做 list 對帳,避免與孤兒偵測重複處理。
	cleaned := r.cleanupJournal(ctx, node, dbInsts, &summary)

	// 清理後重新 List(反映已移除的容器)。
	agentList, lerr = r.listAgent(ctx, node)
	if lerr != nil {
		if errors.Is(lerr, ErrNodeUnreachable) {
			r.markInstancesOffline(dbInsts)
			summary.NodeOffline = true
			return summary, nil
		}
		return summary, lerr
	}

	dbByUUID := indexByUUID(dbInsts)
	agentByUUID := make(map[string]protocol.InstanceSummary)
	for _, ai := range agentList {
		if ai.UUID == "" {
			continue // 無 gsm.uuid:非本核心管理的容器,不介入。
		}
		agentByUUID[ai.UUID] = ai
	}

	// 孤兒:代理有 gsm.uuid 但 DB 無(且非本輪 journal 已清者)→ 記錄,不自動刪。
	for uuid, ai := range agentByUUID {
		if _, inDB := dbByUUID[uuid]; inDB {
			continue
		}
		if cleaned[uuid] {
			continue
		}
		r.recordReconcile(protocol.EventReconcileOrphan, protocol.SeverityWarning, uuid, node, map[string]any{
			"runtime_id":    string(ai.RuntimeID),
			"runtime_state": string(ai.State),
			"action":        "flagged", // 留待 GUI 決策(刪除/接管)
		})
		summary.Orphans = append(summary.Orphans, uuid)
	}

	// 缺失與不一致。
	for _, db := range dbInsts {
		ai, present := agentByUUID[db.UUID]
		if !present {
			// DB 有而容器不存在:若 DB 觀測仍認為有容器(Created/Starting/Running),修正為 Stopped。
			if expectsContainer(db.ObservedState) {
				r.forceObserved(db, protocol.InstanceStateStopped)
				r.recordReconcile(protocol.EventReconcileMismatch, protocol.SeverityWarning, db.UUID, node, map[string]any{
					"reason":        "container_missing",
					"was":           string(db.ObservedState),
					"now":           string(protocol.InstanceStateStopped),
					"desired_state": string(db.DesiredState),
				})
				summary.Missing = append(summary.Missing, db.UUID)
			}
			continue
		}
		// 狀態不一致:以實際 runtime 狀態更新 observed。
		want := reconcileObserved(ai.State)
		if db.ObservedState != want {
			r.forceObserved(db, want)
			r.recordReconcile(protocol.EventReconcileMismatch, protocol.SeverityWarning, db.UUID, node, map[string]any{
				"reason":        "state_mismatch",
				"was":           string(db.ObservedState),
				"now":           string(want),
				"runtime_state": string(ai.State),
			})
			summary.Mismatched = append(summary.Mismatched, db.UUID)
		}
	}

	return summary, nil
}

// cleanupJournal 處理 journal 中未完成的建立(R2 crash-safe + R13):
//   - DB 已有該 uuid(建立其實已完成、journal 殘留)→ 僅清 journal。
//   - DB 無該 uuid(建立未完成)→ 依既有回滾語意移除孤兒容器 + 釋放埠 + 清 journal,記 RECONCILE_ORPHAN。
//
// 回傳「本輪已清理的 uuid 集合」,供後續孤兒偵測排除(避免對同一容器重複處理)。
func (r *Reconciler) cleanupJournal(ctx context.Context, node string, dbInsts []InstanceRecord, summary *ReconcileSummary) map[string]bool {
	cleaned := make(map[string]bool)
	entries, err := r.journal.List()
	if err != nil {
		return cleaned
	}
	dbByUUID := indexByUUID(dbInsts)
	for _, e := range entries {
		if e.Node != node {
			continue
		}
		if _, inDB := dbByUUID[e.UUID]; inDB {
			_ = r.journal.Complete(e.UUID) // 建立已完成,清殘留 journal。
			continue
		}
		// 未完成建立:移除可能殘留的孤兒容器(冪等:查無亦視為成功)+ 釋放埠 + 清 journal。
		rerr := r.registry.Call(node, func(c *NodeClient) error {
			return c.Remove(ctx, e.UUID, true)
		})
		if rerr != nil && !errors.Is(rerr, ErrNodeNotFound) {
			// 移除失敗(非「查無」):保留 journal 供下次對帳重試,不清。
			continue
		}
		_ = r.store.ReleasePortsForInstance(e.UUID)
		_ = r.journal.Complete(e.UUID)
		cleaned[e.UUID] = true
		r.recordReconcile(protocol.EventReconcileOrphan, protocol.SeverityWarning, e.UUID, node, map[string]any{
			"action": "cleaned_incomplete_create",
		})
		summary.JournalCleaned = append(summary.JournalCleaned, e.UUID)
	}
	return cleaned
}

// instancesForNode 回傳屬於 node 的 DB 實例。
func (r *Reconciler) instancesForNode(node string) ([]InstanceRecord, error) {
	all, err := r.store.ListInstances()
	if err != nil {
		return nil, err
	}
	out := make([]InstanceRecord, 0, len(all))
	for _, rec := range all {
		if rec.Node == node {
			out = append(out, rec)
		}
	}
	return out, nil
}

// listAgent 經 registry 呼叫代理 List(registry.Call 於傳輸失敗時標節點離線並記 NODE_OFFLINE)。
func (r *Reconciler) listAgent(ctx context.Context, node string) ([]protocol.InstanceSummary, error) {
	var summaries []protocol.InstanceSummary
	err := r.registry.Call(node, func(c *NodeClient) error {
		var e error
		summaries, e = c.List(ctx)
		return e
	})
	return summaries, err
}

// markInstancesOffline 把一組實例的 observed 權威標記為 Offline(daemon 不可達,不誤刪)。
func (r *Reconciler) markInstancesOffline(insts []InstanceRecord) {
	for _, rec := range insts {
		if rec.ObservedState != protocol.InstanceStateOffline {
			r.forceObserved(rec, protocol.InstanceStateOffline)
		}
	}
}

// forceObserved 權威覆寫 observed 並持久化(對帳「以實際為準」)。
func (r *Reconciler) forceObserved(rec InstanceRecord, to protocol.InstanceState) {
	rec.ObservedState = to
	_ = r.store.UpsertInstance(rec)
}

// recordReconcile 記一筆對帳事件。
func (r *Reconciler) recordReconcile(code protocol.EventCode, sev protocol.Severity, uuid, node string, details map[string]any) {
	if r.events == nil {
		return
	}
	u, n := uuid, node
	_ = r.events.Append(protocol.Event{
		Code:         code,
		Severity:     sev,
		InstanceUUID: &u,
		Node:         &n,
		DetailsJSON:  mustJSON(details),
	})
}

// ---- 純函式輔助 ----

func indexByUUID(insts []InstanceRecord) map[string]InstanceRecord {
	m := make(map[string]InstanceRecord, len(insts))
	for _, rec := range insts {
		m[rec.UUID] = rec
	}
	return m
}

// expectsContainer 回報某 observed 狀態是否隱含「應有容器存在」(據以判斷容器缺失是否為不一致)。
//
// Error 刻意與 Created/Starting/Running/Stopping 同歸為「容器缺失即不一致」(缺陷修復):Error
// 本身不像 Crashed 承載明確的崩潰資訊,只表示「上次操作以不確定的失敗狀態收場」——啟動失敗、卡死
// 復原強制停止失敗、對帳放棄(RESTART_GIVEUP)皆可能落入此狀態,而容器當下是否還存在並不確定。
// 若容器已不存在,維持 Error 會使該實例卡死(GUI 只剩「移除實例」,無法重新啟動);既然容器確定
// 不存在,收斂為 Stopped 讓使用者能照正常路徑重新啟動。Crashed 則不放寬:它承載崩潰時的退出碼等
// 診斷資訊,使用者需要看到「崩潰過」而非被静默改寫為 Stopped,且崩潰迴圈自有 RestartPolicy 收斂。
func expectsContainer(s protocol.InstanceState) bool {
	switch s {
	case protocol.InstanceStateCreated, protocol.InstanceStateStarting,
		protocol.InstanceStateRunning, protocol.InstanceStateStopping,
		protocol.InstanceStateError:
		return true
	default:
		// Stopped/Crashed/Offline/BackingUp/Restoring:容器缺失不視為新的不一致。
		return false
	}
}

// reconcileObserved 把 runtime 層狀態映射為對帳修正用的 core 觀測狀態。
func reconcileObserved(s protocol.RuntimeState) protocol.InstanceState {
	switch s {
	case protocol.RuntimeStateRunning, protocol.RuntimeStatePaused:
		return protocol.InstanceStateRunning
	case protocol.RuntimeStateCreated:
		return protocol.InstanceStateCreated
	default:
		// stopped / exited / dead / unknown:視為已停止(無退出碼可判崩潰,保守取 Stopped)。
		return protocol.InstanceStateStopped
	}
}
