package core

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// ---- 測試環境(MockBackend + httptest 代理,免 Docker)----

type backupEnv struct {
	t       *testing.T
	backend agent.RuntimeBackend
	mock    *agent.MockBackend // 供直接斷言 agent 端備份狀態
	store   *Store
	events  *EventLog
	reg     *NodeRegistry
	orch    *Orchestrator
	svc     *InstanceService
	bs      *BackupService
	opj     *OpJournal
	node    string
}

// newBackupEnv 建立備份測試環境。backend 為 server 後端(可為包裝可注入失敗的後端);
// mock 為底層 MockBackend(供斷言);retN 為保留份數。
func newBackupEnv(t *testing.T, backend agent.RuntimeBackend, mock *agent.MockBackend, retN int) *backupEnv {
	t.Helper()
	hs := newAgentServerWithBackend(t, backend)

	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
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
	recon := NewReconciler(ReconcilerConfig{Store: store, Events: events, Registry: reg, Journal: journal})
	orch := NewOrchestrator(OrchestratorConfig{Store: store, Events: events, Registry: reg, Reconciler: recon})
	opj, err := NewOpJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewOpJournal: %v", err)
	}
	bs := NewBackupService(BackupServiceConfig{
		Store: store, Events: events, Registry: reg, Orchestrator: orch, OpJournal: opj, RetentionN: retN,
	})
	return &backupEnv{
		t: t, backend: backend, mock: mock, store: store, events: events,
		reg: reg, orch: orch, svc: svc, bs: bs, opj: opj, node: "local",
	}
}

func newMockBackupEnv(t *testing.T, retN int) *backupEnv {
	backend := agent.NewMockBackend()
	return newBackupEnv(t, backend, backend, retN)
}

func (e *backupEnv) create(uuid string) InstanceRecord {
	e.t.Helper()
	e.svc.newUUID = func() string { return uuid }
	rec, err := e.svc.Create(context.Background(), CreateOptions{TemplateID: "life"})
	if err != nil {
		e.t.Fatalf("Create(%s): %v", uuid, err)
	}
	return rec
}

func (e *backupEnv) start(uuid string) {
	e.t.Helper()
	if err := e.orch.Start(context.Background(), uuid); err != nil {
		e.t.Fatalf("Start(%s): %v", uuid, err)
	}
}

func (e *backupEnv) stop(uuid string) {
	e.t.Helper()
	if err := e.orch.Stop(context.Background(), uuid); err != nil {
		e.t.Fatalf("Stop(%s): %v", uuid, err)
	}
}

func (e *backupEnv) state(uuid string) protocol.InstanceState {
	e.t.Helper()
	rec, err := e.store.GetInstance(uuid)
	if err != nil {
		e.t.Fatalf("GetInstance(%s): %v", uuid, err)
	}
	return rec.ObservedState
}

func (e *backupEnv) runtimeID(uuid string) protocol.RuntimeID {
	e.t.Helper()
	rec, err := e.store.GetInstance(uuid)
	if err != nil {
		e.t.Fatalf("GetInstance(%s): %v", uuid, err)
	}
	return rec.RuntimeID
}

func (e *backupEnv) countEvents(code protocol.EventCode) int {
	return len(queryEvents(e.t, e.events, code))
}

// setStates 直接改寫 store 中某實例的 observed/desired,供測「observed 落後 desired」情境。
func (e *backupEnv) setStates(uuid string, observed, desired protocol.InstanceState) {
	e.t.Helper()
	rec, err := e.store.GetInstance(uuid)
	if err != nil {
		e.t.Fatalf("GetInstance(%s): %v", uuid, err)
	}
	rec.ObservedState = observed
	rec.DesiredState = desired
	if err := e.store.UpsertInstance(rec); err != nil {
		e.t.Fatalf("UpsertInstance(%s): %v", uuid, err)
	}
}

// eventHasDetail 回報某 code 的事件中是否有一筆 details 含指定鍵、且(keyVal 非空時)值等於 keyVal。
func (e *backupEnv) eventHasDetail(code protocol.EventCode, key, keyVal string) bool {
	for _, ev := range queryEvents(e.t, e.events, code) {
		if len(ev.DetailsJSON) == 0 {
			continue
		}
		var d map[string]any
		if json.Unmarshal(ev.DetailsJSON, &d) != nil {
			continue
		}
		v, ok := d[key]
		if !ok {
			continue
		}
		if keyVal == "" {
			return true
		}
		if s, _ := v.(string); s == keyVal {
			return true
		}
	}
	return false
}

// toggleStartFailBackend 包裝 MockBackend,fail 為真時 Start 失敗(注入收斂/啟動失敗,可即時切換)。
type toggleStartFailBackend struct {
	*agent.MockBackend
	fail atomic.Bool
}

func (b *toggleStartFailBackend) Start(ctx context.Context, id protocol.RuntimeID) error {
	if b.fail.Load() {
		return errors.New("injected start failure")
	}
	return b.MockBackend.Start(ctx, id)
}

func (e *backupEnv) agentBackups(uuid string) []protocol.BackupMeta {
	e.t.Helper()
	metas, err := e.mock.ListBackups(context.Background(), uuid)
	if err != nil {
		e.t.Fatalf("mock.ListBackups: %v", err)
	}
	return metas
}

// ---- Backup:停機一致 + 依原 desired 收斂 ----

// 於 Running 實例備份:planned-stop(INSTANCE_STOPPED)→ 封存 → 自動回 Running(desired 收斂)。
func TestBackupService_RunningInstance_PlannedStopThenResume(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("bk-run")
	e.start("bk-run")
	if e.state("bk-run") != protocol.InstanceStateRunning {
		t.Fatalf("前置: 應為 Running")
	}
	stoppedBefore := e.countEvents(protocol.EventInstanceStopped)

	meta, err := e.bs.Backup(context.Background(), "bk-run")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if meta.Checksum == "" {
		t.Fatalf("備份中繼 checksum 應非空, 得 %+v", meta)
	}
	if meta.BackupID == "" {
		t.Fatalf("備份中繼 BackupID 應非空")
	}
	// 有發生 planned-stop(INSTANCE_STOPPED 增加)。
	if got := e.countEvents(protocol.EventInstanceStopped); got <= stoppedBefore {
		t.Fatalf("備份應觸發 planned-stop, INSTANCE_STOPPED %d→%d", stoppedBefore, got)
	}
	// desired 收斂:回到 Running。
	if e.state("bk-run") != protocol.InstanceStateRunning {
		t.Fatalf("備份後應回 Running, 得 %s", e.state("bk-run"))
	}
	// 清單可列。
	if metas := e.agentBackups("bk-run"); len(metas) != 1 {
		t.Fatalf("agent 備份清單應有 1 份, 得 %d", len(metas))
	}
}

// 於 Stopped 實例備份:不重啟,保持 Stopped。
func TestBackupService_StoppedInstance_StaysStopped(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("bk-stop")
	e.start("bk-stop")
	e.stop("bk-stop")
	if e.state("bk-stop") != protocol.InstanceStateStopped {
		t.Fatalf("前置: 應為 Stopped, 得 %s", e.state("bk-stop"))
	}
	stoppedBefore := e.countEvents(protocol.EventInstanceStopped)
	startedBefore := e.countEvents(protocol.EventInstanceStarted)

	meta, err := e.bs.Backup(context.Background(), "bk-stop")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if meta.Checksum == "" {
		t.Fatalf("備份中繼 checksum 應非空")
	}
	// 未再停、未再啟(不動)。
	if got := e.countEvents(protocol.EventInstanceStopped); got != stoppedBefore {
		t.Fatalf("Stopped 實例備份不應再 planned-stop, %d→%d", stoppedBefore, got)
	}
	if got := e.countEvents(protocol.EventInstanceStarted); got != startedBefore {
		t.Fatalf("Stopped 實例備份不應重啟, %d→%d", startedBefore, got)
	}
	if e.state("bk-stop") != protocol.InstanceStateStopped {
		t.Fatalf("備份後應保持 Stopped, 得 %s", e.state("bk-stop"))
	}
}

// ---- 保留策略:超過 N 刪最舊 ----

func TestBackupService_Retention_DeletesOldest(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("bk-ret")
	e.start("bk-ret")

	var firstID protocol.BackupID
	for i := 0; i < 8; i++ {
		meta, err := e.bs.Backup(context.Background(), "bk-ret")
		if err != nil {
			t.Fatalf("Backup #%d: %v", i, err)
		}
		if i == 0 {
			firstID = meta.BackupID
		}
	}
	metas := e.agentBackups("bk-ret")
	if len(metas) != 7 {
		t.Fatalf("保留策略後 agent 應有 7 份, 得 %d", len(metas))
	}
	// 最舊(第一份)已被刪除。
	for _, m := range metas {
		if m.BackupID == firstID {
			t.Fatalf("最舊備份 %s 應被刪除, 仍在清單中", firstID)
		}
	}
}

// deleteFailBackend 包裝 MockBackend,使 DeleteBackup 恆失敗(注入保留策略刪除失敗)。
type deleteFailBackend struct {
	*agent.MockBackend
}

func (d *deleteFailBackend) DeleteBackup(_ context.Context, _ string, _ protocol.BackupID) error {
	return errors.New("injected delete failure")
}

// 保留策略刪除失敗:不中斷主流程,記 BACKUP_FAILED(warning)。
func TestBackupService_RetentionDeleteFailure_NonFatal(t *testing.T) {
	mock := agent.NewMockBackend()
	backend := &deleteFailBackend{MockBackend: mock}
	e := newBackupEnv(t, backend, mock, 7)
	e.create("bk-df")
	e.start("bk-df")

	failedBefore := e.countEvents(protocol.EventBackupFailed)
	for i := 0; i < 8; i++ {
		if _, err := e.bs.Backup(context.Background(), "bk-df"); err != nil {
			t.Fatalf("Backup #%d 不應因刪除失敗而中斷: %v", i, err)
		}
	}
	// 刪除失敗但備份本體都在(未刪成功 → 8 份)。
	if got := len(e.agentBackups("bk-df")); got != 8 {
		t.Fatalf("刪除失敗時備份不應消失, 得 %d 份", got)
	}
	// 記了 BACKUP_FAILED(保留策略刪除失敗)。
	if got := e.countEvents(protocol.EventBackupFailed); got <= failedBefore {
		t.Fatalf("刪除失敗應記 BACKUP_FAILED, %d→%d", failedBefore, got)
	}
}

// ---- 還原:新 runtime ID 落 DB + 依原 desired 收斂 ----

func TestBackupService_Restore_RunningConverge(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("rs-run")
	e.start("rs-run")
	meta, err := e.bs.Backup(context.Background(), "rs-run")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	oldRuntime := e.runtimeID("rs-run")

	if err := e.bs.Restore(context.Background(), "rs-run", meta.BackupID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// 新 runtime ID 落 DB。
	if newRuntime := e.runtimeID("rs-run"); newRuntime == oldRuntime || newRuntime == "" {
		t.Fatalf("還原後 RuntimeID 應更新為新值, old=%s new=%s", oldRuntime, newRuntime)
	}
	// 依原 desired 收斂:回 Running。
	if e.state("rs-run") != protocol.InstanceStateRunning {
		t.Fatalf("還原後應回 Running, 得 %s", e.state("rs-run"))
	}
	if e.countEvents(protocol.EventRestoreStarted) < 1 || e.countEvents(protocol.EventRestoreCompleted) < 1 {
		t.Fatalf("應記 RESTORE_STARTED/COMPLETED")
	}
}

func TestBackupService_Restore_StoppedConverge(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("rs-stop")
	e.start("rs-stop")
	meta, err := e.bs.Backup(context.Background(), "rs-stop")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	e.stop("rs-stop")
	oldRuntime := e.runtimeID("rs-stop")
	startedBefore := e.countEvents(protocol.EventInstanceStarted)

	if err := e.bs.Restore(context.Background(), "rs-stop", meta.BackupID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if newRuntime := e.runtimeID("rs-stop"); newRuntime == oldRuntime || newRuntime == "" {
		t.Fatalf("還原後 RuntimeID 應更新, old=%s new=%s", oldRuntime, newRuntime)
	}
	// 原停止則保持停止,不重啟。
	if e.state("rs-stop") != protocol.InstanceStateStopped {
		t.Fatalf("還原後應保持 Stopped, 得 %s", e.state("rs-stop"))
	}
	if got := e.countEvents(protocol.EventInstanceStarted); got != startedBefore {
		t.Fatalf("停止實例還原不應重啟, %d→%d", startedBefore, got)
	}
	if e.countEvents(protocol.EventRestoreCompleted) < 1 {
		t.Fatalf("應記 RESTORE_COMPLETED")
	}
}

// 還原失敗(未知 backupID):記 RESTORE_FAILED,原容器語意不被破壞。
func TestBackupService_RestoreFailure_RecordsFailed(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("rs-fail")
	e.start("rs-fail")
	e.stop("rs-fail")
	oldRuntime := e.runtimeID("rs-fail")

	err := e.bs.Restore(context.Background(), "rs-fail", protocol.BackupID("no-such-backup"))
	if err == nil {
		t.Fatalf("未知 backupID 還原應失敗")
	}
	if e.countEvents(protocol.EventRestoreFailed) < 1 {
		t.Fatalf("應記 RESTORE_FAILED")
	}
	// RuntimeID 未變、狀態未變(原語意不破壞)。
	if e.runtimeID("rs-fail") != oldRuntime {
		t.Fatalf("還原失敗不應更動 RuntimeID")
	}
	if e.state("rs-fail") != protocol.InstanceStateStopped {
		t.Fatalf("還原失敗應保持 Stopped, 得 %s", e.state("rs-fail"))
	}
}

// ---- 中斷 crash-safe:op-journal 殘留 → 補救函式收斂 desired + 記事件 ----

func TestBackupService_InterruptedBackup_Recovers(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("bk-crash")
	e.start("bk-crash")

	// 注入:停機後、封存前崩潰。
	e.bs.testCrashAfterStop = func() bool { return true }
	_, err := e.bs.Backup(context.Background(), "bk-crash")
	if !errors.Is(err, errTestCrash) {
		t.Fatalf("應回注入的中斷錯誤, 得 %v", err)
	}
	e.bs.testCrashAfterStop = nil

	// 中斷後:op-journal 殘留、實例停在 Stopped(planned-stop 已跑、收斂沒跑)。
	entries, _ := e.opj.List()
	if len(entries) != 1 || entries[0].UUID != "bk-crash" || !entries[0].ResumeToRunning {
		t.Fatalf("應殘留一筆 ResumeToRunning 的 op-journal, 得 %+v", entries)
	}
	if e.state("bk-crash") != protocol.InstanceStateStopped {
		t.Fatalf("中斷時應停在 Stopped, 得 %s", e.state("bk-crash"))
	}
	failedBefore := e.countEvents(protocol.EventBackupFailed)

	// 補救:依原 desired 收斂(回 Running)+ 記事件 + 清 journal。
	if err := e.bs.RecoverInterrupted(context.Background(), e.node); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if e.state("bk-crash") != protocol.InstanceStateRunning {
		t.Fatalf("補救後應收斂回 Running, 得 %s", e.state("bk-crash"))
	}
	if got := e.countEvents(protocol.EventBackupFailed); got <= failedBefore {
		t.Fatalf("補救應記 BACKUP_FAILED 提示, %d→%d", failedBefore, got)
	}
	if entries, _ := e.opj.List(); len(entries) != 0 {
		t.Fatalf("補救後 op-journal 應清空, 得 %d", len(entries))
	}
}

// ---- 與 Scheduler 接線:BackupService.Backup 作為 backup callback(簽名相容 + 全事件鏈)----

func TestBackupService_AsSchedulerCallback(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("sc-bk")
	e.start("sc-bk")

	sched := NewScheduler(SchedulerConfig{
		Store: e.store, Events: e.events,
		Backup: e.bs.Backup, // 簽名相容:func(ctx, uuid) (BackupMeta, error)
	})
	if _, err := sched.CreateSchedule("sc-bk", ScheduleKindBackup, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC)
	sched.Tick(context.Background(), at)
	sched.Wait()

	// Scheduler 記 BACKUP_COMPLETED + PutBackup;BackupService 完成停機/封存/收斂。
	if n := e.countEvents(protocol.EventBackupCompleted); n != 1 {
		t.Fatalf("BACKUP_COMPLETED = %d, 期望 1", n)
	}
	stored, err := e.store.ListBackups("sc-bk")
	if err != nil {
		t.Fatalf("store.ListBackups: %v", err)
	}
	if len(stored) != 1 || stored[0].Checksum == "" {
		t.Fatalf("Scheduler 應 PutBackup 一份含 checksum 的中繼, 得 %+v", stored)
	}
	if e.state("sc-bk") != protocol.InstanceStateRunning {
		t.Fatalf("排程備份後應回 Running, 得 %s", e.state("sc-bk"))
	}
}

// ---- ListBackups:agent 權威 + 校正 store ----

func TestBackupService_ListBackups_AgentAuthoritative(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("ls-bk")
	e.start("ls-bk")
	m1, _ := e.bs.Backup(context.Background(), "ls-bk")

	// 於 store 塞一筆 agent 沒有的陳舊中繼,應被 ListBackups 校正刪除。
	if err := e.store.PutBackup(protocol.BackupMeta{
		BackupID: "stale-bk", InstanceUUID: "ls-bk", TsUTC: time.Now().UTC(), Checksum: "x",
	}); err != nil {
		t.Fatalf("PutBackup stale: %v", err)
	}

	metas, err := e.bs.ListBackups(context.Background(), "ls-bk")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(metas) != 1 || metas[0].BackupID != m1.BackupID {
		t.Fatalf("應以 agent 為權威回 1 份, 得 %+v", metas)
	}
	// store 中的陳舊中繼已被校正刪除。
	stored, _ := e.store.ListBackups("ls-bk")
	for _, s := range stored {
		if s.BackupID == "stale-bk" {
			t.Fatalf("陳舊中繼 stale-bk 應被校正刪除")
		}
	}
}

// ---- #3 收斂依 desired 非 observed ----

// desired=Running、observed 非 Running(Error):備份不停機,但備份後收斂拉回 Running。
func TestBackupService_Converge_DesiredRunningObservedError(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("cv-err")
	e.start("cv-err") // agent Running
	// 人為讓 core observed 落後 desired:desired 仍 Running,observed=Error。
	e.setStates("cv-err", protocol.InstanceStateError, protocol.InstanceStateRunning)
	stoppedBefore := e.countEvents(protocol.EventInstanceStopped)

	if _, err := e.bs.Backup(context.Background(), "cv-err"); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// observed 非 Running → 不觸發 planned-stop。
	if got := e.countEvents(protocol.EventInstanceStopped); got != stoppedBefore {
		t.Fatalf("observed 非 Running 不應停機, INSTANCE_STOPPED %d→%d", stoppedBefore, got)
	}
	// 收斂目標為 desired=Running → 拉回 Running。
	if e.state("cv-err") != protocol.InstanceStateRunning {
		t.Fatalf("備份後應收斂回 Running, 得 %s", e.state("cv-err"))
	}
}

// desired=Stopped、observed=Running:停機備份後保持停止(不因 observed 而重啟)。
func TestBackupService_Converge_DesiredStoppedObservedRunning(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("cv-stp")
	e.start("cv-stp") // agent+core Running
	// desired 改 Stopped,observed 保持 Running。
	e.setStates("cv-stp", protocol.InstanceStateRunning, protocol.InstanceStateStopped)
	stoppedBefore := e.countEvents(protocol.EventInstanceStopped)
	startedBefore := e.countEvents(protocol.EventInstanceStarted)

	if _, err := e.bs.Backup(context.Background(), "cv-stp"); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// observed=Running → 有停機。
	if got := e.countEvents(protocol.EventInstanceStopped); got <= stoppedBefore {
		t.Fatalf("observed=Running 應停機備份, INSTANCE_STOPPED %d→%d", stoppedBefore, got)
	}
	// desired=Stopped → 不重啟,保持 Stopped。
	if got := e.countEvents(protocol.EventInstanceStarted); got != startedBefore {
		t.Fatalf("desired=Stopped 不應重啟, INSTANCE_STARTED %d→%d", startedBefore, got)
	}
	if e.state("cv-stp") != protocol.InstanceStateStopped {
		t.Fatalf("備份後應保持 Stopped, 得 %s", e.state("cv-stp"))
	}
}

// ---- #4 RESTORE_FAILED 與 COMPLETED 不並存 ----

// 還原本體成功但收斂失敗:事件流無 RESTORE_FAILED,COMPLETED 帶 converge_warning。
func TestBackupService_RestoreConvergeFailure_NoFailedEvent(t *testing.T) {
	mock := agent.NewMockBackend()
	backend := &toggleStartFailBackend{MockBackend: mock}
	e := newBackupEnv(t, backend, mock, 7)
	e.create("rs-cf")
	e.start("rs-cf")
	meta, err := e.bs.Backup(context.Background(), "rs-cf")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	failedBefore := e.countEvents(protocol.EventRestoreFailed)

	// 還原時 desired=Running → 會嘗試 converge 重啟;令 Start 失敗使收斂失敗。
	backend.fail.Store(true)
	if err := e.bs.Restore(context.Background(), "rs-cf", meta.BackupID); err != nil {
		t.Fatalf("收斂失敗不應使 Restore 回錯: %v", err)
	}
	// 事件流:無新增 RESTORE_FAILED。
	if got := e.countEvents(protocol.EventRestoreFailed); got != failedBefore {
		t.Fatalf("收斂失敗不應記 RESTORE_FAILED, %d→%d", failedBefore, got)
	}
	// RESTORE_COMPLETED 存在且帶 converge_warning。
	if e.countEvents(protocol.EventRestoreCompleted) < 1 {
		t.Fatalf("應記 RESTORE_COMPLETED")
	}
	if !e.eventHasDetail(protocol.EventRestoreCompleted, "converge_warning", "") {
		t.Fatalf("RESTORE_COMPLETED 應含 converge_warning 欄位")
	}
}

// ---- #5 還原中斷:journal 記 newID → 認養新容器 ----

// 注入「nodeRestore 成功、newID 已落 journal、DB Upsert 前」崩潰:RecoverInterrupted 據 journal
// 的 newID 更新 DB(認養新容器)並依 desired 收斂。
func TestBackupService_InterruptedRestore_AdoptsNewRuntimeID(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("rs-adopt")
	e.start("rs-adopt")
	meta, err := e.bs.Backup(context.Background(), "rs-adopt")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	oldRuntime := e.runtimeID("rs-adopt")

	// 注入崩潰。
	e.bs.testCrashAfterRestoreNode = func() bool { return true }
	if err := e.bs.Restore(context.Background(), "rs-adopt", meta.BackupID); !errors.Is(err, errTestCrash) {
		t.Fatalf("應回注入的中斷錯誤, 得 %v", err)
	}
	e.bs.testCrashAfterRestoreNode = nil

	// 崩潰後:journal 殘留且記了 newID;DB RuntimeID 仍為舊值(Upsert 未跑)。
	entries, _ := e.opj.List()
	if len(entries) != 1 || entries[0].Kind != OpKindRestore || entries[0].NewRuntimeID == "" {
		t.Fatalf("應殘留一筆帶 NewRuntimeID 的還原 journal, 得 %+v", entries)
	}
	newRuntime := entries[0].NewRuntimeID
	if newRuntime == oldRuntime {
		t.Fatalf("journal newID 不應等於舊 RuntimeID")
	}
	if e.runtimeID("rs-adopt") != oldRuntime {
		t.Fatalf("崩潰於 Upsert 前, DB RuntimeID 應仍為舊值 %s, 得 %s", oldRuntime, e.runtimeID("rs-adopt"))
	}

	// 補救:認養新容器 + 收斂 + 清 journal。
	if err := e.bs.RecoverInterrupted(context.Background(), e.node); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if e.runtimeID("rs-adopt") != newRuntime {
		t.Fatalf("補救後 DB RuntimeID 應認養為 journal 的 newID %s, 得 %s", newRuntime, e.runtimeID("rs-adopt"))
	}
	// 原 desired=Running(還原前 observed/desired 皆 Running)→ 收斂回 Running。
	if e.state("rs-adopt") != protocol.InstanceStateRunning {
		t.Fatalf("補救後應依 desired 收斂回 Running, 得 %s", e.state("rs-adopt"))
	}
	if rem, _ := e.opj.List(); len(rem) != 0 {
		t.Fatalf("補救後 op-journal 應清空, 得 %d", len(rem))
	}
}

// ---- #2 RecoverInterrupted 收斂失敗不吞錯清 journal ----

// 收斂 start 失敗:journal 保留、事件含 error;修復後第二次呼叫收斂成功並清 journal。
func TestBackupService_RecoverInterrupted_StartFailure_KeepsJournal(t *testing.T) {
	mock := agent.NewMockBackend()
	backend := &toggleStartFailBackend{MockBackend: mock}
	e := newBackupEnv(t, backend, mock, 7)
	e.create("rec-sf")
	e.start("rec-sf")

	// 用「停機後、封存前」崩潰造出殘留的備份 journal(ResumeToRunning=true,實例停在 Stopped)。
	e.bs.testCrashAfterStop = func() bool { return true }
	if _, err := e.bs.Backup(context.Background(), "rec-sf"); !errors.Is(err, errTestCrash) {
		t.Fatalf("應回注入的中斷錯誤, 得 %v", err)
	}
	e.bs.testCrashAfterStop = nil
	if e.state("rec-sf") != protocol.InstanceStateStopped {
		t.Fatalf("中斷時應停在 Stopped, 得 %s", e.state("rec-sf"))
	}

	// 第一次補救:令 Start 失敗 → 收斂失敗。
	backend.fail.Store(true)
	failedBefore := e.countEvents(protocol.EventBackupFailed)
	if err := e.bs.RecoverInterrupted(context.Background(), e.node); err != nil {
		t.Fatalf("RecoverInterrupted(1): %v", err)
	}
	// journal 保留(未清)。
	if entries, _ := e.opj.List(); len(entries) != 1 {
		t.Fatalf("收斂失敗應保留 journal 供重試, 得 %d 筆", len(entries))
	}
	// 記了含 error 的警告事件。
	if got := e.countEvents(protocol.EventBackupFailed); got <= failedBefore {
		t.Fatalf("收斂失敗應記 BACKUP_FAILED, %d→%d", failedBefore, got)
	}
	if !e.eventHasDetail(protocol.EventBackupFailed, "reason", "interrupted_recover_failed") {
		t.Fatalf("失敗事件應標 reason=interrupted_recover_failed")
	}
	if !e.eventHasDetail(protocol.EventBackupFailed, "error", "") {
		t.Fatalf("失敗事件應含 error 欄位")
	}

	// 第二次補救:Start 恢復正常 → 收斂成功 + 清 journal。
	backend.fail.Store(false)
	if err := e.bs.RecoverInterrupted(context.Background(), e.node); err != nil {
		t.Fatalf("RecoverInterrupted(2): %v", err)
	}
	if e.state("rec-sf") != protocol.InstanceStateRunning {
		t.Fatalf("修復後應收斂回 Running, 得 %s", e.state("rec-sf"))
	}
	if entries, _ := e.opj.List(); len(entries) != 0 {
		t.Fatalf("收斂成功後 journal 應清空, 得 %d", len(entries))
	}
}

// ---- #6 空清單不誤清中繼 ----

// agent 回空清單 → 保留 store 中繼;agent 回非空清單 → 刪掉 agent 已無的多餘中繼。
func TestBackupService_ListBackups_EmptyAgent_KeepsStore(t *testing.T) {
	e := newMockBackupEnv(t, 7)
	e.create("ls-empty")
	e.start("ls-empty")

	// store 塞兩筆 agent 沒有的中繼;agent 端無任何備份(空清單)。
	for _, bid := range []protocol.BackupID{"orphan-1", "orphan-2"} {
		if err := e.store.PutBackup(protocol.BackupMeta{
			BackupID: bid, InstanceUUID: "ls-empty", TsUTC: time.Now().UTC(), Checksum: "x",
		}); err != nil {
			t.Fatalf("PutBackup(%s): %v", bid, err)
		}
	}

	metas, err := e.bs.ListBackups(context.Background(), "ls-empty")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("agent 應回空清單, 得 %d", len(metas))
	}
	// 中繼未被誤清(agent 空清單時跳過修剪)。
	if stored, _ := e.store.ListBackups("ls-empty"); len(stored) != 2 {
		t.Fatalf("agent 空清單時 store 中繼應保留, 得 %d 筆", len(stored))
	}

	// 對照:agent 有 1 份(非空)時,store 中多餘的 orphan 應被修剪。
	m1, err := e.bs.Backup(context.Background(), "ls-empty")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := e.bs.ListBackups(context.Background(), "ls-empty"); err != nil {
		t.Fatalf("ListBackups(2): %v", err)
	}
	stored, _ := e.store.ListBackups("ls-empty")
	if len(stored) != 1 || stored[0].BackupID != m1.BackupID {
		t.Fatalf("agent 非空時應修剪多餘中繼, 僅留 %s, 得 %+v", m1.BackupID, stored)
	}
}
