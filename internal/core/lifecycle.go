package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"servermonitor/internal/protocol"
)

// 生命週期編排的關鍵常數預設(R3;可經 OrchestratorConfig 覆寫,對齊 requirements「關鍵常數預設」)。
const (
	defaultReadyTimeout  = 60 * time.Second       // R3 啟動就緒逾時
	defaultStopGrace     = 30 * time.Second       // R3 優雅停機寬限期
	defaultReadyPoll     = 500 * time.Millisecond // 就緒輪詢間隔
	defaultReconnectBase = 500 * time.Millisecond // 事件流重連退避起始
	defaultReconnectMax  = 30 * time.Second       // 事件流重連退避上限
	defaultRecoverGrace  = 5 * time.Second        // 卡死復原強制停止的短寬限期(遊戲已卡死,不久候)
)

// ErrStartTimeout 表示啟動輪詢至就緒逾時(R3;實例標記 Error)。
var ErrStartTimeout = errors.New("core: 啟動就緒逾時")

// ReadinessProber 判定實例是否「就緒」(R3/R8)。範本未定義探針時,預設實作以「容器 running」
// 為就緒(見 runningProber)。健康探針(rest/rcon/tcp)屬 T11,屆時以自訂 prober 注入此縫。
type ReadinessProber interface {
	// Ready 回報實例是否就緒;探測失敗(如節點暫時不可達)回 (false, err),編排層據逾時決定放棄。
	Ready(ctx context.Context, node, uuid string) (bool, error)
}

// runningProber 是預設就緒探針:向代理查 runtime 狀態,running 即就緒。
type runningProber struct{ reg *NodeRegistry }

func (p runningProber) Ready(ctx context.Context, node, uuid string) (bool, error) {
	var st protocol.RuntimeStatus
	err := p.reg.Call(node, func(c *NodeClient) error {
		var e error
		st, e = c.Status(ctx, uuid)
		return e
	})
	if err != nil {
		return false, err
	}
	return st.Running, nil
}

// Orchestrator 實作每實例生命週期編排(R3/R8):Start/Stop/Restart 以 per-instance lock 序列化、
// planned-stop token 標記計畫停止、消費代理 die 事件判別死因(planned vs 崩潰)、並在事件流
// 斷線/resync 時觸發啟動對帳(Reconciler)。不含健康探針自動重啟與排程(T11);崩潰時經
// crashHook 交棒(預設 no-op)。
type Orchestrator struct {
	store    *Store
	events   *EventLog
	registry *NodeRegistry
	recon    *Reconciler
	prober   ReadinessProber
	tokens   *plannedStopTable
	now      func() time.Time

	readyTimeout time.Duration
	grace        time.Duration
	readyPoll    time.Duration

	// crashHook 於偵測到非計畫 die(崩潰)時呼叫,供 T11 接自動重啟;預設 nil(no-op)。
	crashHook func(uuid string, exitCode *int)

	// stopHook 於計畫停止「代理 Stop 之前」呼叫,執行範本 hooks.stop(讓伺服器優雅存檔退出,R3)。
	// best-effort:回錯誤僅記 HOOK_FAILED、不阻擋停止流程;預設 nil(no-op)。
	stopHook func(ctx context.Context, uuid string) error

	// progressHook 於啟動階段回報進度(stage: starting/awaiting-ready/ready),供 GUI 顯示於伺服器
	// 主控台與全域操作面板(階段 3)。非阻塞、可丟失(進度僅供顯示);預設 nil(no-op)。
	progressHook func(uuid string, p protocol.ProvisionProgress)

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-instance 序列化鎖
}

// OrchestratorConfig 是 Orchestrator 的建構參數。零值欄位採預設。
type OrchestratorConfig struct {
	Store        *Store
	Events       *EventLog
	Registry     *NodeRegistry
	Reconciler   *Reconciler      // resync/重連時觸發對帳;nil 則跳過對帳
	Prober       ReadinessProber  // nil 用預設 runningProber(容器 running=就緒)
	Now          func() time.Time // 測試可注入
	ReadyTimeout time.Duration    // <=0 用 60s
	Grace        time.Duration    // <=0 用 30s
	ReadyPoll    time.Duration    // <=0 用 500ms
	CrashHook    func(uuid string, exitCode *int)
	// StopHook 於計畫停止前 best-effort 執行 hooks.stop(見 Orchestrator.stopHook);nil 則不執行。
	// 通常以 CommandService.RunStopHook 注入。
	StopHook func(ctx context.Context, uuid string) error
	// ProgressHook 於啟動階段回報進度(見 Orchestrator.progressHook);nil 則不回報。通常由 app 層
	// 注入為「推入 Runtime 進度 channel → Wails EventsEmit provision:<uuid>」(階段 3)。
	ProgressHook func(uuid string, p protocol.ProvisionProgress)
}

// NewOrchestrator 建立 Orchestrator。
func NewOrchestrator(cfg OrchestratorConfig) *Orchestrator {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	prober := cfg.Prober
	if prober == nil {
		prober = runningProber{reg: cfg.Registry}
	}
	readyTimeout := cfg.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = defaultReadyTimeout
	}
	grace := cfg.Grace
	if grace <= 0 {
		grace = defaultStopGrace
	}
	poll := cfg.ReadyPoll
	if poll <= 0 {
		poll = defaultReadyPoll
	}
	return &Orchestrator{
		store:        cfg.Store,
		events:       cfg.Events,
		registry:     cfg.Registry,
		recon:        cfg.Reconciler,
		prober:       prober,
		tokens:       newPlannedStopTable(now),
		now:          now,
		readyTimeout: readyTimeout,
		grace:        grace,
		readyPoll:    poll,
		crashHook:    cfg.CrashHook,
		stopHook:     cfg.StopHook,
		progressHook: cfg.ProgressHook,
		locks:        make(map[string]*sync.Mutex),
	}
}

// emitProgress 於啟動階段 best-effort 回報一格進度(progressHook 為 nil 則 no-op)。
func (o *Orchestrator) emitProgress(uuid, stage string, percent float64, detail string) {
	if o.progressHook != nil {
		o.progressHook(uuid, protocol.ProvisionProgress{Stage: stage, Percent: percent, Detail: detail, InstanceUUID: uuid})
	}
}

// lockFor 取得(或建立)某實例的序列化鎖。不同實例各持一鎖,故可並行;同實例序列化。
func (o *Orchestrator) lockFor(uuid string) *sync.Mutex {
	o.mu.Lock()
	defer o.mu.Unlock()
	m, ok := o.locks[uuid]
	if !ok {
		m = &sync.Mutex{}
		o.locks[uuid] = m
	}
	return m
}

// ---- 生命週期編排(R3)----

// Start 啟動實例(R3):序列化 → 冪等檢查(已 Running 回報已在執行、不重建)→ 代理 Start →
// 輪詢就緒(逾時標 Error)→ observed/desired=Running、記 INSTANCE_STARTED。
func (o *Orchestrator) Start(ctx context.Context, uuid string) error {
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()
	return o.startLocked(ctx, uuid)
}

// AutoStart 是**自動重啟專用**的啟動入口(由 RestartPolicy.SetRestart 注入,取代直接注入 Start):
// 於 per-instance lock 內先確認 DesiredState 仍為 Running,才委給 startLocked。
//
// 守衛的必要性:planned-stop 判定只靠 token TTL 與 agent 蓋章的 ev.TsUTC(見 handleDie),不比對
// token 值(protocol.RuntimeEvent 刻意不攜帶 token)。遠端節點時鐘快於本機超過 TTL(2×grace)時,
// 使用者按「停止」產生的 die 會因 TsUTC 已越過 expiresAt 而被 consumeAt 判為過期 → 誤標 Crashed →
// 交棒 crashHook → 自動重啟把使用者刻意停掉的伺服器拉回 Running。此時 DesiredState 已由 stopLocked
// 收斂為 Stopped,故據此拒絕:誤判最多留下一則崩潰事件/告警,不會違背使用者的停止意圖。
// 手動 Start 與排程 Restart 不走本路徑(它們本就要把 DesiredState 改為 Running)。
//
// 回 nil(而非錯誤)使 RestartPolicy 不把「刻意不重啟」計為一次重試失敗、不續排退避。
func (o *Orchestrator) AutoStart(ctx context.Context, uuid string) error {
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()

	rec, err := o.store.GetInstance(uuid)
	if err != nil {
		return err
	}
	if rec.DesiredState == protocol.InstanceStateStopped {
		return nil
	}
	return o.startLocked(ctx, uuid)
}

// startLocked 是 Start 的實作(呼叫端須已持有 per-instance lock;供 Restart 內部重用)。
func (o *Orchestrator) startLocked(ctx context.Context, uuid string) error {
	rec, err := o.store.GetInstance(uuid)
	if err != nil {
		return err
	}
	// 冪等:已在執行則不重建容器,回報「已在執行」(不視為錯誤)。但仍需把 desired 收斂為
	// Running:外部手動啟動容器後,對帳把 observed 修正為 Running,此時 desired 可能仍停留在
	// Stopped(使用者先前按過停止);若不在此收斂,該實例之後崩潰時 AutoStart 會看到
	// DesiredState==Stopped 而放棄自動重啟(見 AutoStart 註解),形成「看似在跑、其實不會自動復原」
	// 的隱性缺口。未呼叫代理、未產生 planned-stop token,不算一次「操作」,故不遞增 OpGeneration
	// (該欄位僅追蹤實際發起的操作世代,供 planned-stop token 生成與診斷比對)。
	if rec.ObservedState == protocol.InstanceStateRunning {
		if rec.DesiredState != protocol.InstanceStateRunning {
			rec.DesiredState = protocol.InstanceStateRunning
			if err := o.store.UpsertInstance(rec); err != nil {
				return err
			}
		}
		return nil
	}

	rec.OpGeneration++
	rec.DesiredState = protocol.InstanceStateRunning
	if terr := o.transition(&rec, protocol.InstanceStateStarting); terr != nil {
		return terr
	}
	o.emitProgress(uuid, "starting", -1, "啟動中") // percent<0=不確定態(前端顯示流動動畫,無總量可估)

	if aerr := o.registry.Call(rec.Node, func(c *NodeClient) error {
		return c.Start(ctx, uuid)
	}); aerr != nil {
		o.emitProgress(uuid, "failed", -1, "啟動失敗") // 失敗終態:使主控台進度橫幅收束而非卡在啟動中
		_ = o.forceObserved(&rec, protocol.InstanceStateError)
		o.record(protocol.EventInstanceStartFailed, protocol.SeverityError, rec, map[string]any{
			"error": aerr.Error(),
		})
		return fmt.Errorf("代理啟動失敗: %w", aerr)
	}

	if rerr := o.awaitReady(ctx, rec.Node, uuid); rerr != nil {
		o.emitProgress(uuid, "failed", -1, "啟動失敗") // 逾時/崩潰/取消終態:主控台進度橫幅收束
		var se *startupExitError
		if errors.As(rerr, &se) {
			// B2a:啟動途中容器崩潰退出→標 Crashed、記 INSTANCE_CRASHED(不空等就緒逾時、不吞成 Error);
			// 不自動重啟(見 markStartupCrashed)。後續 HOL 阻塞的 die 事件抵達時見 observed==Crashed 去重。
			o.markStartupCrashed(&rec, se.code)
			return fmt.Errorf("啟動途中崩潰: %s: %w", uuid, rerr)
		}
		_ = o.forceObserved(&rec, protocol.InstanceStateError)
		// 就緒逾時或 ctx 取消(非崩潰,崩潰已由上面的 markStartupCrashed 記 INSTANCE_CRASHED):
		// 如實記錄啟動失敗,否則使用者按啟動失敗後事件頁完全無跡可查(稽查發現的主症狀)。
		o.record(protocol.EventInstanceStartFailed, protocol.SeverityError, rec, map[string]any{
			"error": rerr.Error(),
		})
		return rerr
	}
	o.emitProgress(uuid, "ready", 100, "已就緒")

	if terr := o.transition(&rec, protocol.InstanceStateRunning); terr != nil {
		return terr
	}
	o.record(protocol.EventInstanceStarted, protocol.SeverityInfo, rec, map[string]any{
		"runtime_id": string(rec.RuntimeID),
	})
	return nil
}

// awaitReady 輪詢就緒探針至就緒或逾時。逾時回 ErrStartTimeout;ctx 取消回 ctx.Err();
// 啟動途中容器已退出(崩潰)回 *startupExitError(B2a,快速失敗不空等)。
// 探針暫時性錯誤(如節點瞬斷)不立即放棄,持續輪詢至逾時。
func (o *Orchestrator) awaitReady(ctx context.Context, node, uuid string) error {
	pctx, cancel := context.WithTimeout(ctx, o.readyTimeout)
	defer cancel()
	ticker := time.NewTicker(o.readyPoll)
	defer ticker.Stop()
	attempt := 0
	for {
		attempt++
		o.emitProgress(uuid, "awaiting-ready", -1, fmt.Sprintf("等待就緒(第 %d 次探測)", attempt))
		ready, perr := o.prober.Ready(pctx, node, uuid)
		if perr == nil && ready {
			return nil
		}
		// B2a:偵測啟動途中容器已終態退出(崩潰)→快速失敗帶退出碼,不空等到就緒逾時(生產 10 分鐘)。
		// 就緒探針(tcp/rcon/rest)不查退出、docker/預設 kind 的 running 探針只回 Running 布林,故另查 Status。
		if exited, code := o.exitedDuringStartup(pctx, node, uuid); exited {
			return &startupExitError{code: code}
		}
		select {
		case <-pctx.Done():
			if errors.Is(pctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return fmt.Errorf("%w: %s(逾時 %s)", ErrStartTimeout, uuid, o.readyTimeout)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// startupExitError 表示啟動途中容器已終態退出(崩潰),供 awaitReady 快速失敗、startLocked 據以標
// Crashed 而非空等就緒逾時(B2a)。code 為退出碼(可為 nil)。
type startupExitError struct{ code *int }

func (e *startupExitError) Error() string {
	if e.code != nil {
		return fmt.Sprintf("啟動途中容器已退出(退出碼 %d)", *e.code)
	}
	return "啟動途中容器已退出"
}

// exitedDuringStartup 查 runtime 狀態判斷容器是否已終態退出(exited/dead,或已有 FinishedAt),
// 而非「尚未就緒」(created/starting)。查詢失敗(節點瞬斷等)一律回 false——不因暫時查不到而誤判退出。
func (o *Orchestrator) exitedDuringStartup(ctx context.Context, node, uuid string) (bool, *int) {
	var st protocol.RuntimeStatus
	err := o.registry.Call(node, func(c *NodeClient) error {
		var e error
		st, e = c.Status(ctx, uuid)
		return e
	})
	if err != nil {
		return false, nil
	}
	// 僅崩潰終態(exited/dead)判為啟動途中退出;不含 stopped(計畫停止,awaitReady 持鎖期間不會發生,
	// 但避免語意混淆)或 created/starting(尚未就緒,續輪詢)。docker/native 的崩潰皆映為 exited/dead。
	if st.State == protocol.RuntimeStateExited || st.State == protocol.RuntimeStateDead {
		return true, st.ExitCode
	}
	return false, nil
}

// Stop 計畫性停止實例(R3):序列化 → 產生 planned-stop token(遞增 operation generation、
// TTL=2×寬限期)→ 代理 Stop(帶 token 與寬限期)→ observed/desired=Stopped、記 INSTANCE_STOPPED。
// token 留待事件消費迴圈於對應 die 到達時 consume(未過期即判為 planned,不誤標崩潰)。
func (o *Orchestrator) Stop(ctx context.Context, uuid string) error {
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()
	return o.stopLocked(ctx, uuid)
}

// stopLocked 是 Stop 的實作(呼叫端須已持有 per-instance lock;供 Restart 內部重用)。
func (o *Orchestrator) stopLocked(ctx context.Context, uuid string) error {
	rec, err := o.store.GetInstance(uuid)
	if err != nil {
		return err
	}
	if rec.ObservedState == protocol.InstanceStateStopped {
		return nil // 冪等
	}

	rec.OpGeneration++
	rec.DesiredState = protocol.InstanceStateStopped
	token := newUUIDv4()
	// 先發 token 再轉 Stopping:token 涵蓋整個停止窗口,使代理 Stop 觸發的 die 必落在有效窗口內。
	o.tokens.issue(uuid, token, rec.OpGeneration, 2*o.grace)

	if terr := o.transition(&rec, protocol.InstanceStateStopping); terr != nil {
		o.tokens.clear(uuid)
		return terr
	}

	// R3:代理 Stop 之前 best-effort 執行 hooks.stop(讓伺服器優雅存檔退出)。失敗僅記事件,
	// 不阻擋後續強制停止(節點離線、無 hook、指令逾時皆不應卡住停止)。
	o.runStopHook(ctx, &rec)

	if aerr := o.registry.Call(rec.Node, func(c *NodeClient) error {
		return c.Stop(ctx, uuid, protocol.StopInstanceRequest{
			GraceSeconds:     int(o.grace / time.Second),
			PlannedStopToken: token,
		})
	}); aerr != nil {
		// 操作失敗即清除 token,避免 stale token 遮蔽下次真崩潰(R8)。
		o.tokens.clear(uuid)
		_ = o.forceObserved(&rec, protocol.InstanceStateError)
		// 如實記錄停止失敗,否則使用者按停止失敗後事件頁完全無跡可查(與啟動失敗同一缺口家族,
		// 見 startLocked 的 EventInstanceStartFailed)。
		o.record(protocol.EventInstanceStopFailed, protocol.SeverityError, rec, map[string]any{
			"error": aerr.Error(),
		})
		return fmt.Errorf("代理停止失敗: %w", aerr)
	}

	// 代理 Stop 為同步(回傳即已停);轉 Stopped 並記事件。對應 die 由事件迴圈 consume token。
	if terr := o.transition(&rec, protocol.InstanceStateStopped); terr != nil {
		return terr
	}
	o.record(protocol.EventInstanceStopped, protocol.SeverityInfo, rec, map[string]any{
		"planned_stop_generation": rec.OpGeneration,
	})
	return nil
}

// Restart 重建實例(R3):於 per-instance lock 內編排 Stop→Start,斷言仍是同一 runtime ID
// 回到 Running(代理無 /restart,避免繞過 lock/planned-stop)。記 INSTANCE_RESTARTED。
func (o *Orchestrator) Restart(ctx context.Context, uuid string) error {
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()

	before, err := o.store.GetInstance(uuid)
	if err != nil {
		return err
	}
	runtimeBefore := before.RuntimeID

	if serr := o.stopLocked(ctx, uuid); serr != nil {
		return fmt.Errorf("restart 停止階段失敗: %w", serr)
	}
	if serr := o.startLocked(ctx, uuid); serr != nil {
		return fmt.Errorf("restart 啟動階段失敗: %w", serr)
	}

	after, err := o.store.GetInstance(uuid)
	if err != nil {
		return err
	}
	if after.RuntimeID != runtimeBefore {
		return fmt.Errorf("core: restart 後 runtime ID 改變(%s → %s),違反同容器重建語意",
			runtimeBefore, after.RuntimeID)
	}
	if after.ObservedState != protocol.InstanceStateRunning {
		return fmt.Errorf("core: restart 後未回到 Running,實際為 %s", after.ObservedState)
	}
	o.record(protocol.EventInstanceRestarted, protocol.SeverityInfo, after, map[string]any{
		"runtime_id": string(after.RuntimeID),
	})
	return nil
}

// ---- 事件消費迴圈(R8/R13)----

// RunEventLoop 訂閱代理事件流並分派(R8/R13),阻塞至 ctx 取消。斷線→標節點離線
// (NODE_OFFLINE)並退避重連;每次(重)連成功後觸發一次對帳(補漏事件期間的狀態變化)。
// 測試可改為直接呼叫 HandleRuntimeEvent 餵事件(不經 WS),此處負責 WS 傳輸與重連。
func (o *Orchestrator) RunEventLoop(ctx context.Context, node string) {
	var since protocol.Cursor
	backoff := defaultReconnectBase
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := o.dialEvents(ctx, node, since)
		if err != nil {
			o.registry.MarkOffline(node, err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = defaultReconnectBase
		// (重)連成功:觸發對帳補齊離線期間的變化(Reconciler 成功呼叫 List 亦會把節點標回線上)。
		o.reconcile(ctx, node)

		readErr := o.readEvents(ctx, node, conn, &since)
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
		// 非 ctx 取消的斷線:標離線後退避重連。
		o.registry.MarkOffline(node, readErr)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// dialEvents 以最後游標建立事件 WS 連線。
func (o *Orchestrator) dialEvents(ctx context.Context, node string, since protocol.Cursor) (*websocket.Conn, error) {
	client, err := o.registry.Client(node)
	if err != nil {
		return nil, err
	}
	return client.EventsWS(ctx, since)
}

// readEvents 迴圈讀取事件並分派,更新 since 游標;讀取錯誤(斷線)回傳供上層退避重連。
// closeOnCancel 使 ctx 取消時立即關閉 conn 以中斷阻塞中的 ReadJSON——否則 ReadJSON 會阻塞至
// 對端送出訊息或連線斷開,導致 RunEventLoop 無法於 cancel 後及時返回(關閉時的 loopWg.Wait 會卡住,
// T15 雙審 #3)。
func (o *Orchestrator) readEvents(ctx context.Context, node string, conn *websocket.Conn, since *protocol.Cursor) error {
	stop := closeOnCancel(ctx, conn)
	defer stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ev protocol.RuntimeEvent
		if err := conn.ReadJSON(&ev); err != nil {
			return err
		}
		if ev.Cursor != "" {
			*since = ev.Cursor
		}
		o.HandleRuntimeEvent(ctx, node, ev)
	}
}

// HandleRuntimeEvent 分派單一執行事件(R8/R13)。公開以利測試直接餵事件,不必經 WS。
//   - die:比對 planned-stop token——有效 token→planned(已由 Stop 收斂,僅 consume);
//     無/過期→崩潰(標 Crashed、記 INSTANCE_CRASHED、觸發 crashHook)。
//   - start:外部啟動→更新 observed 為 Running(僅當先前為停止/崩潰/離線/錯誤等非啟動中狀態)。
//   - resync:串流偵測漏事件→觸發一次對帳。
//   - health/oom:探針與 OOM 處置屬 T11,此處忽略。
func (o *Orchestrator) HandleRuntimeEvent(ctx context.Context, node string, ev protocol.RuntimeEvent) {
	switch ev.Kind {
	case protocol.RuntimeEventDie:
		o.handleDie(ev)
	case protocol.RuntimeEventStart:
		o.handleExternalStart(ev)
	case protocol.RuntimeEventResync:
		o.reconcile(ctx, node)
	default:
		// health / oom 等:T11 處置。
	}
}

// handleDie 處理容器結束事件:於 per-instance lock 內比對 token 判別死因。
// TTL 判定基準用 ev.TsUTC(die 實際發生時刻,由 agent 蓋章),而非 o.now()(派工時刻):B6——
// 事件迴圈是單 goroutine 同步派工,被長持鎖操作(如備份全程持同一實例鎖數分鐘)HOL 阻塞時,後續
// 事件連「讀取時刻」都被延遲,唯 ev.TsUTC 反映真實發生時間。以派工時刻判 TTL 時,另一實例的計畫停機
// die 會因延遲派工使 token 過期而被誤判崩潰、自動重啟被刻意停止的伺服器(跨實例 head-of-line;近期
// 搶鎖前取樣的修法只解同實例)。前提為 localhost 同時鐘;遠端節點的時鐘偏差遠小於 TTL(2×grace)。
// TsUTC 缺失(理論上不會,die 事件皆蓋章)退回 o.now()。
func (o *Orchestrator) handleDie(ev protocol.RuntimeEvent) {
	occurredAt := ev.TsUTC
	if occurredAt.IsZero() {
		occurredAt = o.now()
	}
	o.handleDieAt(occurredAt, ev)
}

// handleDieAt 是 handleDie 的實作,arrivedAt 為 die 事件發生時刻(TTL 判定基準,見 handleDie)。
func (o *Orchestrator) handleDieAt(arrivedAt time.Time, ev protocol.RuntimeEvent) {
	uuid, ok := o.uuidForRuntime(ev.ID)
	if !ok {
		return // 非本核心管理的容器(或已移除)
	}
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()

	rec, err := o.store.GetInstance(uuid)
	if err != nil {
		return
	}
	// consumeAt:抵達時刻落在 TTL 內→planned(delete 並回 true);無 token/過期→false
	// (delete 過期者,不遮蔽後續)。以抵達時刻而非處理時刻判定,鎖等待時間不計入。
	if o.tokens.consumeAt(uuid, arrivedAt) {
		return // 計畫停止對應的 die:狀態已由 Stop 收斂為 Stopped,無需處理。
	}
	// 去重(第二道防線,配合 planned-stop token):已觀測為 Crashed/Error 表示此崩潰已被記錄
	// (例:RecoverStuck 已標 Crashed,或前一則 die 已處理)。此時「卡死後旋即真 die」等重複事件
	// 只會二次計數並多觸發一次 crashHook,故直接跳過,不重記 INSTANCE_CRASHED、不再交棒。
	if rec.ObservedState == protocol.InstanceStateCrashed || rec.ObservedState == protocol.InstanceStateError {
		return
	}
	// 非計畫崩潰:標 Crashed、記 INSTANCE_CRASHED、交棒 crashHook(T11 自動重啟)。
	if terr := o.transition(&rec, protocol.InstanceStateCrashed); terr != nil {
		// 來源狀態不允許轉 Crashed(如已 Stopped):以權威覆寫確保觀測到崩潰。
		_ = o.forceObserved(&rec, protocol.InstanceStateCrashed)
	}
	o.record(protocol.EventInstanceCrashed, protocol.SeverityError, rec, crashDetails(ev.ExitCode))
	if o.crashHook != nil {
		o.crashHook(uuid, ev.ExitCode)
	}
}

// markStartupCrashed 於啟動途中偵測到崩潰退出時,標 Crashed、記 INSTANCE_CRASHED(呼叫端須已持
// per-instance lock)。刻意**不**交棒 crashHook 自動重啟:啟動崩潰多為設定/環境問題,自動重啟通常徒勞
// (同設定會再崩),此維持修復前「啟動崩潰不自動重啟」的行為,僅把「卡 Starting 至就緒逾時再吞成 Error」
// 改為快速失敗+如實記 Crashed。另一關鍵:若此處交棒 crashHook 觸發非同步重啟,同一崩潰的 die 事件被跨
// 實例 HOL 阻塞超過重啟 backoff 才抵達時,實例已被重啟翻回 Running,handleDie 的 observed==Crashed 去重
// 失效→二次記錄/重啟一台剛恢復的伺服器;不交棒則 observed 穩定為 Crashed,延遲 die 可靠去重。runtime
// 崩潰(就緒後才死)的自動重啟不受影響,仍走 die 事件的正常 crashHook 路徑。
func (o *Orchestrator) markStartupCrashed(rec *InstanceRecord, exitCode *int) {
	if terr := o.transition(rec, protocol.InstanceStateCrashed); terr != nil {
		_ = o.forceObserved(rec, protocol.InstanceStateCrashed)
	}
	o.record(protocol.EventInstanceCrashed, protocol.SeverityError, *rec, crashDetails(exitCode))
}

// handleExternalStart 處理「非經本編排」的容器啟動:把停止/崩潰/離線/錯誤中的實例更新為 Running。
// 啟動中(Starting)或已 Running 的實例忽略之,避免與正在進行的 Start 編排相互干擾。
func (o *Orchestrator) handleExternalStart(ev protocol.RuntimeEvent) {
	uuid, ok := o.uuidForRuntime(ev.ID)
	if !ok {
		return
	}
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()

	rec, err := o.store.GetInstance(uuid)
	if err != nil {
		return
	}
	switch rec.ObservedState {
	case protocol.InstanceStateStopped, protocol.InstanceStateCrashed,
		protocol.InstanceStateOffline, protocol.InstanceStateError:
		_ = o.forceObserved(&rec, protocol.InstanceStateRunning)
		o.record(protocol.EventInstanceStarted, protocol.SeverityInfo, rec, map[string]any{
			"source": "external",
		})
	default:
		// Starting / Running / 忙碌狀態:交由既有編排收斂。
	}
}

// ---- T11 接線:存活探針復原 / 排程共用鎖 / 崩潰迴圈放棄 ----

// RecoverStuck 由存活探針判定「running 但卡死」時觸發(R8):於 per-instance lock 內,若實例仍
// Running(非操作進行中/已停)則:先「強制停止仍在執行的容器」(卡死的是 running 容器,直接標
// Crashed 後續 Start 對其為 docker no-op、探針續失敗;故須先停),再標 Crashed、記 INSTANCE_CRASHED、
// 交棒 crashHook——與 die 事件相同的復原路徑,故沿用 RestartPolicy 的自動重啟/上限判斷。持鎖確保
// 不與 Start/Stop/Restart/備份互撞;非 Running 即跳過(避免遮蔽正在進行的計畫操作)。
//
// 強制停止發放 planned-stop token:使停止產生的 die 被事件迴圈 consume、不二次進 crashHook;並跳過
// hooks.stop(遊戲已卡死,rcon stop hook 大概率逾時,直接以短寬限停)。代理停止失敗(節點離線等)→
// 記於 INSTANCE_CRASHED 詳情、仍標 Crashed 交 hook(重試時 Start 自身會失敗,走重試鏈)。
func (o *Orchestrator) RecoverStuck(uuid string) {
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()

	rec, err := o.store.GetInstance(uuid)
	if err != nil {
		return
	}
	if rec.ObservedState != protocol.InstanceStateRunning {
		return // 已非 Running:計畫操作進行中或已停止,不介入。
	}
	o.record(protocol.EventHealthProbeFailed, protocol.SeverityError, rec, map[string]any{
		"reason": "liveness_stuck",
	})
	details := map[string]any{"source": "liveness_probe"}
	if stopErr := o.forceStopForRecovery(&rec); stopErr != nil {
		details["force_stop_error"] = stopErr.Error()
	}
	if terr := o.transition(&rec, protocol.InstanceStateCrashed); terr != nil {
		_ = o.forceObserved(&rec, protocol.InstanceStateCrashed)
	}
	o.record(protocol.EventInstanceCrashed, protocol.SeverityError, rec, details)
	if o.crashHook != nil {
		o.crashHook(uuid, nil)
	}
}

// forceStopForRecovery 於卡死復原時強制停止仍在執行的容器(呼叫端須已持 per-instance lock)。
// 發放 planned-stop token(遞增 op generation、TTL=2×短寬限),使停止觸發的 die 落在有效窗口內被
// consume,不誤判崩潰、不二次進 crashHook;以短寬限直接呼叫代理 Stop、不執行 hooks.stop。回傳代理
// Stop 的錯誤:失敗(節點離線等)→清除 token(不會有對應 die,避免 stale token 遮蔽後續真崩潰)。
func (o *Orchestrator) forceStopForRecovery(rec *InstanceRecord) error {
	rec.OpGeneration++
	token := newUUIDv4()
	o.tokens.issue(rec.UUID, token, rec.OpGeneration, 2*defaultRecoverGrace)

	ctx, cancel := context.WithTimeout(context.Background(), 2*defaultRecoverGrace)
	defer cancel()
	aerr := o.registry.Call(rec.Node, func(c *NodeClient) error {
		return c.Stop(ctx, rec.UUID, protocol.StopInstanceRequest{
			GraceSeconds:     int(defaultRecoverGrace / time.Second),
			PlannedStopToken: token,
		})
	})
	if aerr != nil {
		o.tokens.clear(rec.UUID)
	}
	return aerr
}

// RunLocked 於某實例的 per-instance lock 內執行 fn(供 Scheduler 的排程備份與生命週期操作共用
// 同一把鎖,序列化不互撞,R8)。注意:fn 內不得再呼叫會取用同一實例鎖的 Orchestrator 方法
// (Start/Stop/Restart),否則自我死結。
func (o *Orchestrator) RunLocked(uuid string, fn func() error) error {
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

// markGiveup 於崩潰迴圈達上限(RESTART_GIVEUP)時把實例標 Error(狀態機 Crashed→Error 邊)。
// 由 RestartPolicy 於「放棄決策」時經 after 於 per-instance lock 之外派發呼叫:放棄可能源自崩潰
// (原持鎖)或重試失敗(原不持鎖)兩條路徑,兩者都在 after 內不持該實例鎖,故此處「自行取用
// per-instance lock」統一序列化這個 read-modify-write,與並行的 Start/Stop/Restart/handleDie 互斥。
// 切勿於已持該實例 lock 的路徑同步呼叫(sync.Mutex 非重入,會自我死結);一律經 after 派發即滿足。
func (o *Orchestrator) markGiveup(uuid string) {
	lock := o.lockFor(uuid)
	lock.Lock()
	defer lock.Unlock()

	rec, err := o.store.GetInstance(uuid)
	if err != nil {
		return
	}
	if rec.ObservedState == protocol.InstanceStateError {
		return
	}
	_ = o.forceObserved(&rec, protocol.InstanceStateError)
}

// MarkGiveup 是 markGiveup 的匯出包裝,供跨套件接線(如 app.Bootstrap 的
// RestartPolicy.SetRestart(orch.Start, orch.MarkGiveup))使用。契約與 markGiveup 相同:
// 只可經 RestartPolicy 的 after 於 per-instance lock 之外派發,切勿於持鎖路徑同步呼叫。
func (o *Orchestrator) MarkGiveup(uuid string) { o.markGiveup(uuid) }

// runStopHook best-effort 執行 hooks.stop:無 hook 或成功→無事;失敗→記 HOOK_FAILED 警告事件,
// 不回傳錯誤(不阻擋停止流程,R3)。
func (o *Orchestrator) runStopHook(ctx context.Context, rec *InstanceRecord) {
	if o.stopHook == nil {
		return
	}
	if err := o.stopHook(ctx, rec.UUID); err != nil {
		o.record(EventHookFailed, protocol.SeverityWarning, *rec, map[string]any{
			"hook":  "stop",
			"error": err.Error(),
		})
	}
}

// reconcile 觸發一次對帳(若已設 Reconciler)。
func (o *Orchestrator) reconcile(ctx context.Context, node string) {
	if o.recon == nil {
		return
	}
	_, _ = o.recon.Reconcile(ctx, node)
}

// uuidForRuntime 以 runtime ID 反查實例 UUID(規模小,直接掃描 DB 清單)。
func (o *Orchestrator) uuidForRuntime(rid protocol.RuntimeID) (string, bool) {
	if rid == "" {
		return "", false
	}
	insts, err := o.store.ListInstances()
	if err != nil {
		return "", false
	}
	for _, r := range insts {
		if r.RuntimeID == rid {
			return r.UUID, true
		}
	}
	return "", false
}

// ---- 狀態轉移與事件輔助 ----

// transition 經狀態機守衛把 rec 轉到 to 並持久化;非法轉移回 ErrIllegalTransition 且不改狀態。
func (o *Orchestrator) transition(rec *InstanceRecord, to protocol.InstanceState) error {
	if err := checkTransition(rec.ObservedState, to); err != nil {
		return err
	}
	rec.ObservedState = to
	return o.store.UpsertInstance(*rec)
}

// forceObserved 權威覆寫 observed(不經轉移守衛),供對帳/崩潰觀測等「以實際為準」的更新。
func (o *Orchestrator) forceObserved(rec *InstanceRecord, to protocol.InstanceState) error {
	rec.ObservedState = to
	return o.store.UpsertInstance(*rec)
}

// record 追加一筆實例相關事件(events 為 nil 時 no-op)。
func (o *Orchestrator) record(code protocol.EventCode, sev protocol.Severity, rec InstanceRecord, details map[string]any) {
	if o.events == nil {
		return
	}
	uuid, node, tmpl := rec.UUID, rec.Node, rec.TemplateID
	var dj json.RawMessage
	if details != nil {
		dj = mustJSON(details)
	}
	_ = o.events.Append(protocol.Event{
		Code:         code,
		Severity:     sev,
		InstanceUUID: &uuid,
		Node:         &node,
		TemplateID:   &tmpl,
		DetailsJSON:  dj,
	})
}

func crashDetails(exitCode *int) map[string]any {
	d := map[string]any{}
	if exitCode != nil {
		d["exit_code"] = *exitCode
	}
	return d
}

// ---- planned-stop token 表 ----

// plannedStop 是一筆計畫停止標記(記憶體;R8)。generation 供診斷對照 op_generation。
type plannedStop struct {
	token      string
	generation int64
	expiresAt  time.Time
}

// plannedStopTable 管理每實例的 planned-stop token:issue 遞增覆蓋、consumeAt 判有效並移除、
// clear 於操作失敗即時移除;過期 token 於 consumeAt/issue 時被丟棄,確保不遮蔽後續真崩潰。
type plannedStopTable struct {
	mu  sync.Mutex
	m   map[string]plannedStop
	now func() time.Time
}

func newPlannedStopTable(now func() time.Time) *plannedStopTable {
	return &plannedStopTable{m: make(map[string]plannedStop), now: now}
}

// issue 登記(或覆蓋)一筆有 TTL 的計畫停止標記。
func (t *plannedStopTable) issue(uuid, token string, generation int64, ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gcLocked()
	t.m[uuid] = plannedStop{token: token, generation: generation, expiresAt: t.now().Add(ttl)}
}

// consumeAt 取用某實例的標記:存在且 at(die 事件抵達核心的時刻)未過期回 true(planned),
// 並一律移除;不存在或已過期回 false(視為崩潰),過期者一併移除以免遮蔽後續。
// 以抵達時刻判定使 TTL 不受 per-instance lock 等待時間影響:計畫停機的 die 抵達於停止當下
// (必在 TTL 內),真崩潰的 die 抵達時刻必是當下,過期 token 照樣不遮蔽。
func (t *plannedStopTable) consumeAt(uuid string, at time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ps, ok := t.m[uuid]
	if !ok {
		return false
	}
	delete(t.m, uuid)
	return at.Before(ps.expiresAt)
}

// clear 移除某實例的標記(操作失敗時呼叫)。
func (t *plannedStopTable) clear(uuid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, uuid)
}

// gcLocked 清掉所有已過期標記(呼叫端須持 mu)。
func (t *plannedStopTable) gcLocked() {
	now := t.now()
	for k, ps := range t.m {
		if !now.Before(ps.expiresAt) {
			delete(t.m, k)
		}
	}
}

// ---- 重連退避輔助 ----

// sleepCtx 睡 d 或於 ctx 取消時提早返回;回 false 表示 ctx 已取消(應結束迴圈)。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > defaultReconnectMax {
		return defaultReconnectMax
	}
	return next
}
