package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"servermonitor/internal/protocol"
)

// defaultRetentionN 是備份保留份數預設(R9:超過自動刪最舊)。
const defaultRetentionN = 7

// errTestCrash 是測試注入的備份中斷哨符(模擬「停機後、封存前」崩潰,留下殘留 op-journal)。
var errTestCrash = errors.New("core: 測試注入的備份中斷")

// BackupService 編排 R9 的備份/還原:停機一致快照、依原 desired 收斂、保留策略(刪最舊)、
// 備份清單、與中斷 crash-safe(操作 journal)。agent 的 Archive/Restore/Delete 機制已存在,
// 本服務負責 core 側的編排——
//   - **序列化**:所有操作經 Orchestrator 的 per-instance lock(RunLocked)+ 其 stopLocked/
//     startLocked,與 Start/Stop/Restart/崩潰復原共用同一把鎖,不互撞(R8);planned-stop 一律
//     經既有 token 機制,不繞過。
//   - **事件責任分工**:Backup 的 BACKUP_STARTED/COMPLETED 事件與主中繼 PutBackup 由已接線的
//     Scheduler.fireBackup(不改)負責——BackupService.Backup 即其注入的 backup callback,只做
//     編排 + 保留策略,並回傳中繼;唯保留策略刪除失敗記 BACKUP_FAILED(R9,Scheduler 不知情)。
//     Restore 無 Scheduler 對應,故自行記 RESTORE_STARTED/COMPLETED/FAILED。
//
// 併發安全性由 Orchestrator 的鎖與各元件自身保證。
type BackupService struct {
	store    *Store
	events   *EventLog
	registry *NodeRegistry
	orch     *Orchestrator
	opJrnl   *OpJournal
	retN     int
	now      func() time.Time

	// testCrashAfterStop 供測試注入:非 nil 且回 true 時,Backup 於「停機後、封存前」中止並回
	// errTestCrash,刻意不收斂、不清 op-journal,以驗證中斷復原(正式路徑恆為 nil)。
	testCrashAfterStop func() bool
	// testCrashAfterRestoreNode 供測試注入:非 nil 且回 true 時,Restore 於「nodeRestore 成功、
	// newID 已落 journal、DB Upsert 前」中止並回 errTestCrash,以驗證 #5 認養復原(正式路徑恆為 nil)。
	testCrashAfterRestoreNode func() bool
}

// BackupServiceConfig 是 BackupService 的建構參數。
type BackupServiceConfig struct {
	Store        *Store
	Events       *EventLog
	Registry     *NodeRegistry
	Orchestrator *Orchestrator
	OpJournal    *OpJournal
	RetentionN   int              // <=0 用預設 7
	Now          func() time.Time // 測試可注入
}

// NewBackupService 建立 BackupService。Store/Registry/Orchestrator/OpJournal 必須提供。
func NewBackupService(cfg BackupServiceConfig) *BackupService {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	retN := cfg.RetentionN
	if retN <= 0 {
		retN = defaultRetentionN
	}
	return &BackupService{
		store:    cfg.Store,
		events:   cfg.Events,
		registry: cfg.Registry,
		orch:     cfg.Orchestrator,
		opJrnl:   cfg.OpJournal,
		retN:     retN,
		now:      now,
	}
}

// Backup 對實例做停機一致快照並套用保留策略(R9),回傳備份中繼。簽名相容 Scheduler 的 backup
// callback(SchedulerConfig.Backup)。流程(於 per-instance lock 內):記原 desired(是否 Running)
// → 寫 op-journal → 若 Running 以既有 planned-stop 優雅停機(含 hooks.stop,不繞過 token)→
// NodeClient.Backup → 依原 desired 收斂(原 Running 重啟;原停止保持)→ 清 op-journal → 保留策略。
//
// 事件與主 PutBackup 由呼叫端(Scheduler.fireBackup)負責;本函式僅於保留策略刪除失敗時記
// BACKUP_FAILED(不中斷)。
func (bs *BackupService) Backup(ctx context.Context, uuid string) (protocol.BackupMeta, error) {
	var meta protocol.BackupMeta
	err := bs.orch.RunLocked(uuid, func() error {
		rec, gerr := bs.store.GetInstance(uuid)
		if gerr != nil {
			return gerr
		}
		// 停機判斷用 observed(當前是否在跑、是否需要停);收斂目標用 desired(操作前的意圖狀態),
		// 兩者分離:observed 因故障落後 desired 時(如 desired=Running/observed=Error),備份不誤停,
		// 收斂仍把實例拉回 desired(#3)。resumeToRunning 於 stopLocked 改寫 desired 前先擷取。
		wasRunning := rec.ObservedState == protocol.InstanceStateRunning
		resumeToRunning := rec.DesiredState == protocol.InstanceStateRunning
		node := rec.Node

		if jerr := bs.opJrnl.Begin(OpJournalEntry{
			UUID: uuid, Node: node, Kind: OpKindBackup, ResumeToRunning: resumeToRunning,
		}); jerr != nil {
			return jerr
		}

		if wasRunning {
			if serr := bs.orch.stopLocked(ctx, uuid); serr != nil {
				_ = bs.opJrnl.Complete(uuid)
				return fmt.Errorf("備份前停機失敗: %w", serr)
			}
		}

		// 測試注入:模擬「停機後、封存前」崩潰——不收斂、不清 journal,留殘留供復原測試。
		if bs.testCrashAfterStop != nil && bs.testCrashAfterStop() {
			return errTestCrash
		}

		m, berr := bs.nodeBackup(ctx, node, uuid)
		if berr != nil {
			// 封存失敗:仍收斂回原 desired,再清 journal;失敗事件由呼叫端記(BACKUP_FAILED)。
			bs.convergeAndRecord(ctx, uuid, resumeToRunning, protocol.EventBackupFailed)
			_ = bs.opJrnl.Complete(uuid)
			return berr
		}
		meta = m

		bs.convergeAndRecord(ctx, uuid, resumeToRunning, protocol.EventBackupFailed)
		_ = bs.opJrnl.Complete(uuid)

		bs.applyRetention(ctx, node, uuid)
		return nil
	})
	return meta, err
}

// Restore 以備份還原並依原 desired 收斂(R9)。流程(於 per-instance lock 內):記 RESTORE_STARTED
// → 寫 op-journal → 若 Running 以既有 planned-stop 停機 → NodeClient.Restore(回新 runtime ID)→
// 更新 InstanceRecord.RuntimeID → 依原 desired 收斂(原停止保持停止)→ RESTORE_COMPLETED。
// 任一步失敗記 RESTORE_FAILED 並盡力收斂回原 desired(不破壞原容器語意)。
func (bs *BackupService) Restore(ctx context.Context, uuid string, backupID protocol.BackupID) error {
	return bs.orch.RunLocked(uuid, func() error {
		rec, gerr := bs.store.GetInstance(uuid)
		if gerr != nil {
			return gerr
		}
		// 停機判斷用 observed、收斂目標用 desired(#3;resumeToRunning 於 stopLocked 改寫 desired 前擷取)。
		wasRunning := rec.ObservedState == protocol.InstanceStateRunning
		resumeToRunning := rec.DesiredState == protocol.InstanceStateRunning
		node := rec.Node

		bs.record(uuid, protocol.EventRestoreStarted, protocol.SeverityInfo,
			map[string]any{"backup_id": string(backupID)})

		if jerr := bs.opJrnl.Begin(OpJournalEntry{
			UUID: uuid, Node: node, Kind: OpKindRestore, BackupID: backupID, ResumeToRunning: resumeToRunning,
		}); jerr != nil {
			return jerr
		}

		if wasRunning {
			if serr := bs.orch.stopLocked(ctx, uuid); serr != nil {
				_ = bs.opJrnl.Complete(uuid)
				bs.record(uuid, protocol.EventRestoreFailed, protocol.SeverityError,
					map[string]any{"backup_id": string(backupID), "stage": "stop", "error": serr.Error()})
				return fmt.Errorf("還原前停機失敗: %w", serr)
			}
		}

		newID, rerr := bs.nodeRestore(ctx, node, uuid, backupID)
		if rerr != nil {
			// 還原失敗:原容器語意不破壞,收斂回原 desired。
			_ = bs.convergeToOrig(ctx, uuid, resumeToRunning)
			_ = bs.opJrnl.Complete(uuid)
			bs.record(uuid, protocol.EventRestoreFailed, protocol.SeverityError,
				map[string]any{"backup_id": string(backupID), "stage": "restore", "error": rerr.Error()})
			return rerr
		}

		// #5 crash-safe:先把新 RuntimeID 落 journal,再做 DB Upsert;中斷落在兩者之間時,
		// RecoverInterrupted 可據 journal 的 newID 認養新容器(否則 DB 仍指向已被切換的舊容器)。
		// 落 journal 為 best-effort:失敗不阻斷還原(資料已切換),僅損失該窗口的認養能力。
		_ = bs.opJrnl.SetNewRuntimeID(uuid, newID)

		// 測試注入:模擬「nodeRestore 成功、newID 已落 journal、DB Upsert 前」崩潰,驗 #5 認養復原。
		if bs.testCrashAfterRestoreNode != nil && bs.testCrashAfterRestoreNode() {
			return errTestCrash
		}

		// 更新 RuntimeID 為還原產生的新容器(重讀:stopLocked 已改寫 rec 的持久狀態)。
		rec, gerr = bs.store.GetInstance(uuid)
		if gerr != nil {
			_ = bs.opJrnl.Complete(uuid)
			return gerr
		}
		rec.RuntimeID = newID
		if uerr := bs.store.UpsertInstance(rec); uerr != nil {
			_ = bs.opJrnl.Complete(uuid)
			bs.record(uuid, protocol.EventRestoreFailed, protocol.SeverityError,
				map[string]any{"backup_id": string(backupID), "stage": "persist", "error": uerr.Error()})
			return uerr
		}

		// #4:收斂失敗**不**再記 RESTORE_FAILED(資料已還原、RuntimeID 已更新,還原本體成功);
		// 改把警告併入 RESTORE_COMPLETED 的 converge_warning 欄位,確保單一還原的事件流不會
		// 同時出現 FAILED 與 COMPLETED。
		details := map[string]any{"backup_id": string(backupID), "runtime_id": string(newID)}
		if cerr := bs.convergeToOrig(ctx, uuid, resumeToRunning); cerr != nil {
			details["converge_warning"] = cerr.Error()
		}
		_ = bs.opJrnl.Complete(uuid)
		bs.record(uuid, protocol.EventRestoreCompleted, protocol.SeverityInfo, details)
		return nil
	})
}

// ListBackups 回傳某實例備份清單,以 agent 為權威(NodeClient.ListBackups)並校正 store 中繼
// (補寫缺漏、刪除 agent 已無者);agent 不可達時退回 store 中繼供 GUI 顯示已知清單。
func (bs *BackupService) ListBackups(ctx context.Context, uuid string) ([]protocol.BackupMeta, error) {
	rec, err := bs.store.GetInstance(uuid)
	if err != nil {
		return nil, err
	}
	metas, lerr := bs.nodeListBackups(ctx, rec.Node, uuid)
	if lerr != nil {
		return bs.store.ListBackups(uuid) // 退回 store 中繼(agent 暫不可達)
	}
	agentIDs := make(map[protocol.BackupID]bool, len(metas))
	for _, m := range metas {
		agentIDs[m.BackupID] = true
		_ = bs.store.PutBackup(m)
	}
	// #6:agent 回**空清單**時不修剪 store 中繼——「真的一份備份都沒有」與「agent 暫時空手而回
	// (剛重啟/競態/檔案系統尚未就緒)」在此層無法區分,誤刪會讓 GUI 憑空少掉整批已知備份。備份全數
	// 合法歸零的情境極罕見,寧可留 stale 中繼(下次非空清單自會修剪);PutBackup 自癒路徑不受影響。
	if len(metas) > 0 {
		if stored, serr := bs.store.ListBackups(uuid); serr == nil {
			for _, sm := range stored {
				if !agentIDs[sm.BackupID] {
					_ = bs.store.DeleteBackup(sm.BackupID)
				}
			}
		}
	}
	return metas, nil
}

// RecoverInterrupted 對某節點殘留的操作 journal 做中斷復原(R9 crash-safe):依 journal 記錄的
// 原 desired 收斂(ResumeToRunning→重啟;否則保持停止),並記 BACKUP_FAILED/RESTORE_FAILED
// 提示,再清 journal。供啟動對帳掛鉤——**刻意不改 reconciler.go**;主迴圈(或 T15)應在對帳後
// 對每個節點呼叫本函式接線(agent 端 Restore 的 staging 回滾已自足,core 端補救的是「操作中斷
// 後 desired 收斂沒跑完」)。
func (bs *BackupService) RecoverInterrupted(ctx context.Context, node string) error {
	entries, err := bs.opJrnl.List()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Node != node {
			continue
		}
		e := e
		_ = bs.orch.RunLocked(e.UUID, func() error {
			rec, gerr := bs.store.GetInstance(e.UUID)
			if gerr != nil {
				_ = bs.opJrnl.Complete(e.UUID) // 實例已不存在:清殘留 journal。
				return nil
			}
			code := protocol.EventBackupFailed
			if e.Kind == OpKindRestore {
				code = protocol.EventRestoreFailed
			}

			// #5:還原中斷若 journal 已記新 RuntimeID 而 DB 未更新(崩潰於 nodeRestore 成功後、
			// Upsert 前),以 journal 的新 ID 認養新容器,再據此收斂;認養失敗留 journal 給下次重試。
			if e.Kind == OpKindRestore && e.NewRuntimeID != "" && rec.RuntimeID != e.NewRuntimeID {
				rec.RuntimeID = e.NewRuntimeID
				if uerr := bs.store.UpsertInstance(rec); uerr != nil {
					bs.record(e.UUID, code, protocol.SeverityWarning, map[string]any{
						"reason": "interrupted_adopt_failed", "kind": string(e.Kind), "error": uerr.Error(),
					})
					return nil // 不清 journal:下次啟動再試認養。
				}
				rec, gerr = bs.store.GetInstance(e.UUID)
				if gerr != nil {
					_ = bs.opJrnl.Complete(e.UUID)
					return nil
				}
			}

			// 依 journal 記錄的原 desired 收斂(ResumeToRunning 現存 desired,見 #3)。
			var convErr error
			if e.ResumeToRunning && rec.ObservedState != protocol.InstanceStateRunning {
				convErr = bs.orch.startLocked(ctx, e.UUID)
			} else if !e.ResumeToRunning && rec.ObservedState == protocol.InstanceStateRunning {
				convErr = bs.orch.stopLocked(ctx, e.UUID)
			}

			// #2:收斂失敗**不**清 journal(留給下次啟動重試),記含錯誤的警告事件;不再吞錯清除。
			if convErr != nil {
				bs.record(e.UUID, code, protocol.SeverityWarning, map[string]any{
					"reason":             "interrupted_recover_failed",
					"kind":               string(e.Kind),
					"resumed_to_running": e.ResumeToRunning,
					"error":              convErr.Error(),
				})
				return nil
			}

			bs.record(e.UUID, code, protocol.SeverityWarning, map[string]any{
				"reason":             "interrupted_recovered",
				"kind":               string(e.Kind),
				"resumed_to_running": e.ResumeToRunning,
			})
			_ = bs.opJrnl.Complete(e.UUID)
			return nil
		})
	}
	return nil
}

// ---- 內部輔助 ----

// convergeToOrig 依「操作前是否 Running」收斂:原 Running 且當前非 Running → 重新啟動;
// 原停止 → 保持停止(不動)。呼叫端須已持 per-instance lock。
func (bs *BackupService) convergeToOrig(ctx context.Context, uuid string, toRunning bool) error {
	if !toRunning {
		return nil
	}
	rec, err := bs.store.GetInstance(uuid)
	if err != nil {
		return err
	}
	if rec.ObservedState == protocol.InstanceStateRunning {
		return nil
	}
	return bs.orch.startLocked(ctx, uuid)
}

// convergeAndRecord 收斂回原 desired,收斂失敗以 failCode 記警告(不阻斷主流程)。
func (bs *BackupService) convergeAndRecord(ctx context.Context, uuid string, toRunning bool, failCode protocol.EventCode) {
	if cerr := bs.convergeToOrig(ctx, uuid, toRunning); cerr != nil {
		bs.record(uuid, failCode, protocol.SeverityWarning,
			map[string]any{"stage": "converge", "error": cerr.Error()})
	}
}

// applyRetention 於一次成功備份後套用保留策略(R9):agent 清單超過 N 份即刪最舊(len-N)份;
// 刪除失敗記 BACKUP_FAILED(warning)不中斷。以 agent 清單為權威,連動刪 store 中繼列。
func (bs *BackupService) applyRetention(ctx context.Context, node, uuid string) {
	metas, err := bs.nodeListBackups(ctx, node, uuid)
	if err != nil {
		bs.record(uuid, protocol.EventBackupFailed, protocol.SeverityWarning,
			map[string]any{"stage": "retention_list", "error": err.Error()})
		return
	}
	if len(metas) <= bs.retN {
		return
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].TsUTC.Before(metas[j].TsUTC) })
	excess := len(metas) - bs.retN
	for i := 0; i < excess; i++ {
		m := metas[i]
		if derr := bs.nodeDeleteBackup(ctx, node, uuid, m.BackupID); derr != nil {
			bs.record(uuid, protocol.EventBackupFailed, protocol.SeverityWarning,
				map[string]any{"stage": "retention_delete", "backup_id": string(m.BackupID), "error": derr.Error()})
			continue
		}
		_ = bs.store.DeleteBackup(m.BackupID)
	}
}

// record 追加一筆實例相關事件並回傳之(補 node/template 欄位)。
func (bs *BackupService) record(uuid string, code protocol.EventCode, sev protocol.Severity, details map[string]any) protocol.Event {
	ev := protocol.Event{Code: code, Severity: sev, InstanceUUID: strPtr(uuid)}
	if details != nil {
		ev.DetailsJSON = mustJSON(details)
	}
	if bs.store != nil {
		if rec, err := bs.store.GetInstance(uuid); err == nil {
			ev.Node = strPtr(rec.Node)
			ev.TemplateID = strPtr(rec.TemplateID)
		}
	}
	if bs.events != nil {
		_ = bs.events.Append(ev)
	}
	return ev
}

// ---- 節點呼叫包裝(經 registry,傳輸失敗自動標離線)----

func (bs *BackupService) nodeBackup(ctx context.Context, node, uuid string) (protocol.BackupMeta, error) {
	var meta protocol.BackupMeta
	err := bs.registry.Call(node, func(c *NodeClient) error {
		var e error
		meta, e = c.Backup(ctx, uuid)
		return e
	})
	return meta, err
}

func (bs *BackupService) nodeRestore(ctx context.Context, node, uuid string, backupID protocol.BackupID) (protocol.RuntimeID, error) {
	var rid protocol.RuntimeID
	err := bs.registry.Call(node, func(c *NodeClient) error {
		var e error
		rid, e = c.Restore(ctx, uuid, backupID)
		return e
	})
	return rid, err
}

func (bs *BackupService) nodeListBackups(ctx context.Context, node, uuid string) ([]protocol.BackupMeta, error) {
	var metas []protocol.BackupMeta
	err := bs.registry.Call(node, func(c *NodeClient) error {
		var e error
		metas, e = c.ListBackups(ctx, uuid)
		return e
	})
	return metas, err
}

func (bs *BackupService) nodeDeleteBackup(ctx context.Context, node, uuid string, backupID protocol.BackupID) error {
	return bs.registry.Call(node, func(c *NodeClient) error {
		return c.DeleteBackup(ctx, uuid, backupID)
	})
}
