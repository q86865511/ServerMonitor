package app

import (
	"context"
	"encoding/json"
	"time"

	"servermonitor/internal/protocol"
)

// startupOpTimeout 是啟動對帳/中斷復原等一次性操作的逾時上限(避免離線時無限阻塞啟動)。
const startupOpTimeout = 30 * time.Second

// shutdownTimeout 是關閉代理 HTTP server 的等待上限。
const shutdownTimeout = 10 * time.Second

// shutdownDrainTimeout 是關閉時等待進行中綁定長操作收束的上限(逾時記事件續關,op-journal 兜底)。
const shutdownDrainTimeout = 10 * time.Second

// Start 啟動所有背景編排(冪等:重入為 no-op)。順序:
// 啟動對帳 → 中斷備份/還原復原 → 對 observed=Running 實例納入監控/門檻 →
// 事件迴圈 → 排程器 → 存活監控。離線時各步驟自行降級,不阻擋啟動。
func (r *Runtime) Start() {
	if r.started {
		return
	}
	r.started = true

	ctx, cancel := context.WithTimeout(r.rootCtx, startupOpTimeout)
	defer cancel()

	// 1) 啟動對帳(離線回 NodeOffline summary,不致命)。
	_, _ = r.recon.Reconcile(ctx, r.node)
	// 2) 中斷的備份/還原依原 desired 收斂(best-effort)。
	_ = r.backups.RecoverInterrupted(ctx, r.node)
	// 3) 對 observed=Running 實例納入監控聚合與資源門檻評估。
	if insts, err := r.store.ListInstances(); err == nil {
		for _, rec := range insts {
			if rec.Node == r.node && rec.ObservedState == protocol.InstanceStateRunning {
				r.startMonitoring(rec.UUID)
			}
		}
	}

	// 4) 背景迴圈(各自 context 以支援 Shutdown 有序收束)。以 loopWg 記帳,使 Shutdown 於 cancel
	//    後可 Wait 至三個 Run 迴圈確實返回(T15 雙審 #3)。
	r.eventCtx, r.eventCancel = context.WithCancel(r.rootCtx)
	r.schedCtx, r.schedCancel = context.WithCancel(r.rootCtx)
	r.healthCtx, r.healthCancel = context.WithCancel(r.rootCtx)

	r.loopWg.Add(3)
	go func() { defer r.loopWg.Done(); r.orch.RunEventLoop(r.eventCtx, r.node) }()
	go func() { defer r.loopWg.Done(); r.sched.Run(r.schedCtx) }()
	go func() { defer r.loopWg.Done(); r.health.Run(r.healthCtx, r.node) }()
}

// EventShutdownDrainTimeout 標記關閉時進行中的長操作未在時限內收束(補充事件碼,沿用
// EventInstanceRemoved/EventHookFailed 的「補充碼定義於使用處」慣例)。op-journal 兜底,故逾時
// 續關安全(下次啟動對帳/中斷復原會收斂半途操作)。
const EventShutdownDrainTimeout protocol.EventCode = "SHUTDOWN_DRAIN_TIMEOUT"

// Shutdown 優雅關閉(冪等)。依序:三個背景 Run 迴圈(取消+Wait)→ 排程動作 → 門檻監看 →
// 監控聚合 → 取消 root ctx → 等待進行中長操作收束 → 代理 HTTP → 後端 → Store → AppLock。
// 全程不停任何遊戲容器(NFR:核心關閉不影響執行中的遊戲)。
//
// T15 雙審 #3:先取消並 Wait 背景迴圈與進行中的綁定長操作(其 ctx 衍生自 rootCtx,rootCancel
// 即取消之),再關閉 Store,確保 Store 關閉後不再有背景寫入;長操作逾時則記事件續關(op-journal 兜底)。
func (r *Runtime) Shutdown() {
	r.shutdownOnce.Do(func() {
		// 1) 取消三個背景 Run 迴圈並 Wait 至其確實返回。
		if r.schedCancel != nil {
			r.schedCancel()
		}
		if r.healthCancel != nil {
			r.healthCancel()
		}
		if r.eventCancel != nil {
			r.eventCancel()
		}
		r.loopWg.Wait()
		// 2) 排程器已派出的動作 goroutine 收束。
		if r.sched != nil {
			r.sched.Wait()
		}
		// 3) 門檻監看:逐一 Stop(其 watcher 各持獨立 context)。
		r.watchedMu.Lock()
		for uuid := range r.watched {
			r.threshold.Stop(uuid)
		}
		r.watched = make(map[string]bool)
		r.watchedMu.Unlock()
		// 3b) 指標記錄器:先於監控聚合關閉(它訂閱 monitor stats 流),收束所有聚合/Prune goroutine,
		//     關閉後不再觸碰 Store。
		if r.metrics != nil {
			r.metrics.Close()
		}
		// 4) 監控聚合:關閉所有 stats/logs 串流。
		if r.monitor != nil {
			r.monitor.Close()
		}
		// 5) 取消 root context(取消進行中的綁定長操作、未決的自動重啟/告警派發)。
		if r.rootCancel != nil {
			r.rootCancel()
		}
		// 6) 等待進行中的長操作收束(逾時記事件續關;op-journal 兜底)。
		r.waitInflight(shutdownDrainTimeout)
		// 7) 代理 HTTP server + 後端(不觸碰容器)。
		r.agentMu.Lock()
		if r.agentHTTP != nil {
			sctx, scancel := context.WithTimeout(context.Background(), shutdownTimeout)
			_ = r.agentHTTP.Shutdown(sctx)
			scancel()
		}
		if r.agentSrv != nil {
			_ = r.agentSrv.Close()
		}
		if r.backend != nil {
			_ = closeBackend(r.backend)
		}
		r.agentMu.Unlock()
		// 8) Store。
		if r.store != nil {
			_ = r.store.Close()
		}
		// 9) 釋放單一實例鎖。
		if r.lock != nil {
			_ = r.lock.Release()
		}
	})
}

// trackOp 登記一個由綁定層觸發的進行中長操作:回傳衍生自 rootCtx 的 context(整體關閉時一併
// 取消)與 done 釋放函式(取消該 context 並自 in-flight 計數移除)。Shutdown 於關閉 Store 之前
// waitInflight 等待所有此類操作收束,避免關閉後仍有寫入落到已關閉的 Store(T15 雙審 #3)。
func (r *Runtime) trackOp() (context.Context, func()) {
	r.inflight.Add(1)
	ctx, cancel := context.WithCancel(r.rootCtx)
	return ctx, func() {
		cancel()
		r.inflight.Done()
	}
}

// waitInflight 等待所有進行中長操作收束,至多 timeout;逾時記 SHUTDOWN_DRAIN_TIMEOUT 事件後返回
// (由呼叫端續行關閉,op-journal 兜底半途操作)。
func (r *Runtime) waitInflight(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		if r.events != nil {
			_ = r.events.Append(protocol.Event{
				Code:     EventShutdownDrainTimeout,
				Severity: protocol.SeverityWarning,
				Node:     strPtrLifecycle(r.node),
			})
		}
	}
}

// strPtrLifecycle 回傳字串指標(事件 Node 欄位用;避免與 core 套件同名 helper 衝突)。
func strPtrLifecycle(s string) *string { return &s }

// startMonitoring 為一個執行中實例開啟監控聚合與資源門檻評估(順序:先 StartMonitoring
// 建立 stats 串流,再 Watch 訂閱之)。冪等。
func (r *Runtime) startMonitoring(uuid string) {
	r.monitor.StartMonitoring(uuid)
	r.threshold.Watch(uuid)
	r.metrics.Watch(uuid)
	r.watchedMu.Lock()
	r.watched[uuid] = true
	r.watchedMu.Unlock()
}

// stopMonitoring 停止一個實例的監控聚合與門檻評估。冪等。
func (r *Runtime) stopMonitoring(uuid string) {
	r.metrics.Stop(uuid)
	r.threshold.Stop(uuid)
	r.monitor.StopMonitoring(uuid)
	r.watchedMu.Lock()
	delete(r.watched, uuid)
	r.watchedMu.Unlock()
}

// ---- 生命週期操作(綁定層轉發至此,順帶維護監控掛接)----

// StartInstance 啟動實例並納入監控。ctx 衍生自 rootCtx 並計入 in-flight(關閉時取消/等待,
// T15 #3);傳入的 ctx 僅為呼叫慣例,實際使用受控的 op ctx。
func (r *Runtime) StartInstance(_ context.Context, uuid string) error {
	ctx, done := r.trackOp()
	defer done()
	if err := r.orch.Start(ctx, uuid); err != nil {
		return err
	}
	r.startMonitoring(uuid)
	return nil
}

// StopInstance 停止實例;停止成功後才解除監控(T15 #9:停止失敗→監控維持,避免對仍在執行的
// 實例失去可觀測性)。
func (r *Runtime) StopInstance(_ context.Context, uuid string) error {
	ctx, done := r.trackOp()
	defer done()
	if err := r.orch.Stop(ctx, uuid); err != nil {
		return err
	}
	r.stopMonitoring(uuid)
	return nil
}

// RestartInstance 重啟實例(監控維持;重啟前公告由 Scheduler 專責,手動重啟不自動公告)。
// 重啟後確保監控已啟用。
func (r *Runtime) RestartInstance(_ context.Context, uuid string) error {
	ctx, done := r.trackOp()
	defer done()
	if err := r.orch.Restart(ctx, uuid); err != nil {
		return err
	}
	r.startMonitoring(uuid)
	return nil
}

// RemoveInstance 移除實例:若在執行則先優雅停機,再回收資源(容器/埠/機密/DB);全部成功後才
// 解除監控(T15 #9:移除中途失敗→監控維持,使可重試且不失可觀測性)。
func (r *Runtime) RemoveInstance(_ context.Context, uuid string, purge bool) error {
	ctx, done := r.trackOp()
	defer done()
	rec, err := r.store.GetInstance(uuid)
	if err != nil {
		return err
	}
	if rec.ObservedState == protocol.InstanceStateRunning || rec.ObservedState == protocol.InstanceStateStarting {
		// 優雅停機(best-effort:停機失敗不阻擋移除,容器移除本身會強制清理)。
		_ = r.orch.Stop(ctx, uuid)
	}
	if err := r.instances.Remove(ctx, uuid, purge); err != nil {
		return err
	}
	r.stopMonitoring(uuid)
	return nil
}

// BackupNow 立即對實例做一次備份並記錄事件/中繼(手動觸發,語意對齊 Scheduler 的排程備份:
// BACKUP_STARTED → Backup → PutBackup + COMPLETED,失敗記 FAILED 並經 AlertSink 告警)。
// BackupService.Backup 本身刻意不記事件/不寫主中繼(那是呼叫端責任),故此處補齊。
func (r *Runtime) BackupNow(_ context.Context, uuid string) (protocol.BackupMeta, error) {
	ctx, done := r.trackOp()
	defer done()
	r.recordInstanceEvent(uuid, protocol.EventBackupStarted, protocol.SeverityInfo, nil)
	meta, err := r.backups.Backup(ctx, uuid)
	if err != nil {
		ev := r.recordInstanceEvent(uuid, protocol.EventBackupFailed, protocol.SeverityError,
			map[string]any{"error": err.Error()})
		if aerr := r.alerts.Alert(ctx, ev); aerr != nil {
			r.recordInstanceEvent(uuid, protocol.EventAlertFailed, protocol.SeverityWarning,
				map[string]any{"alert_code": string(protocol.EventBackupFailed), "error": aerr.Error()})
		} else {
			r.recordInstanceEvent(uuid, protocol.EventAlertSent, protocol.SeverityInfo,
				map[string]any{"alert_code": string(protocol.EventBackupFailed)})
		}
		return protocol.BackupMeta{}, err
	}
	if perr := r.store.PutBackup(meta); perr != nil {
		r.recordInstanceEvent(uuid, protocol.EventBackupCompleted, protocol.SeverityWarning,
			map[string]any{"backup_id": string(meta.BackupID), "meta_persist_error": perr.Error()})
		return meta, nil
	}
	r.recordInstanceEvent(uuid, protocol.EventBackupCompleted, protocol.SeverityInfo,
		map[string]any{"backup_id": string(meta.BackupID)})
	return meta, nil
}

// recordInstanceEvent 追加一筆實例相關事件並回傳(補 node/template 欄位),語意對齊 Scheduler.record。
func (r *Runtime) recordInstanceEvent(uuid string, code protocol.EventCode, sev protocol.Severity, details map[string]any) protocol.Event {
	ev := protocol.Event{Code: code, Severity: sev, InstanceUUID: &uuid}
	if details != nil {
		ev.DetailsJSON, _ = json.Marshal(details)
	}
	if rec, err := r.store.GetInstance(uuid); err == nil {
		node, tmpl := rec.Node, rec.TemplateID
		ev.Node, ev.TemplateID = &node, &tmpl
	}
	_ = r.events.Append(ev)
	return ev
}
