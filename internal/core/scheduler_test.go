package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// schedTestStore 建一個帶 EventLog 的暫存 Store(排程測試不需代理)。
func schedTestStore(t *testing.T) (*Store, *EventLog) {
	t.Helper()
	store, _ := newTempStore(t)
	return store, NewEventLog(store, nil, EventLogOptions{})
}

func countCode(t *testing.T, events *EventLog, code protocol.EventCode) int {
	t.Helper()
	return len(queryEvents(t, events, code))
}

// ---- CRUD ----

func TestScheduler_CRUD(t *testing.T) {
	store, events := schedTestStore(t)
	s := NewScheduler(SchedulerConfig{Store: store, Events: events})

	id, err := s.CreateSchedule("inst-1", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, true)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	got, err := s.ListSchedules("inst-1")
	if err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("ListSchedules 應回 1 筆含 %s, 得 %+v err=%v", id, got, err)
	}

	if err := s.UpdateSchedule(id, "inst-1", ScheduleKindBackup, ScheduleSpec{At: "05:00"}, false); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	got, _ = s.ListSchedules("inst-1")
	if got[0].Kind != ScheduleKindBackup || got[0].Enabled {
		t.Errorf("更新後應為 backup/disabled, 得 kind=%s enabled=%v", got[0].Kind, got[0].Enabled)
	}

	if err := s.DeleteSchedule(id); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	got, _ = s.ListSchedules("inst-1")
	if len(got) != 0 {
		t.Errorf("刪除後應為空, 得 %d 筆", len(got))
	}
}

func TestScheduler_CreateRejectsInvalid(t *testing.T) {
	store, events := schedTestStore(t)
	s := NewScheduler(SchedulerConfig{Store: store, Events: events})

	if _, err := s.CreateSchedule("i", "reboot", ScheduleSpec{At: "04:30"}, true); err == nil {
		t.Error("未知 kind 應被拒")
	}
	if _, err := s.CreateSchedule("i", ScheduleKindRestart, ScheduleSpec{At: "25:00"}, true); err == nil {
		t.Error("非法時刻應被拒")
	}
	if _, err := s.CreateSchedule("i", ScheduleKindRestart, ScheduleSpec{At: "04:30", Weekdays: []int{7}}, true); err == nil {
		t.Error("非法 weekday 應被拒")
	}
}

// ---- 排程重啟:UTC 觸發、先 announce、分鐘去重 ----

func TestScheduler_RestartFiresWithAnnounceUTC(t *testing.T) {
	store, events := schedTestStore(t)
	var mu sync.Mutex
	var order []string
	s := NewScheduler(SchedulerConfig{
		Store: store, Events: events,
		Announce: func(_ context.Context, uuid string) error {
			mu.Lock()
			order = append(order, "announce:"+uuid)
			mu.Unlock()
			return nil
		},
		Restart: func(_ context.Context, uuid string) error {
			mu.Lock()
			order = append(order, "restart:"+uuid)
			mu.Unlock()
			return nil
		},
	})
	if _, err := s.CreateSchedule("inst-r", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC)
	fired := s.Tick(context.Background(), at)
	if len(fired) != 1 {
		t.Fatalf("到點應觸發 1 筆, 得 %d", len(fired))
	}
	s.Wait() // 動作已改為 goroutine 非同步派出(#5),斷言前需等待完成
	mu.Lock()
	gotOrder := append([]string(nil), order...)
	mu.Unlock()
	if len(gotOrder) != 2 || gotOrder[0] != "announce:inst-r" || gotOrder[1] != "restart:inst-r" {
		t.Errorf("應先 announce 再 restart, 得 %v", gotOrder)
	}

	// 同一到點分鐘再 Tick:去重,不重複觸發。
	if f := s.Tick(context.Background(), at.Add(20*time.Second)); len(f) != 0 {
		t.Errorf("同分鐘重複 Tick 不應再觸發, 得 %v", f)
	}
	// 非到點分鐘:不觸發。
	if f := s.Tick(context.Background(), at.Add(time.Minute)); len(f) != 0 {
		t.Errorf("非到點分鐘不應觸發, 得 %v", f)
	}
}

// UTC 判定:本地 06:30 (+02:00) == 04:30 UTC → 應觸發(規避時區/DST)。
func TestScheduler_UTCAcrossTimezone(t *testing.T) {
	store, events := schedTestStore(t)
	var restarts int
	s := NewScheduler(SchedulerConfig{
		Store: store, Events: events,
		Restart: func(context.Context, string) error { restarts++; return nil },
	})
	if _, err := s.CreateSchedule("inst-tz", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	local := time.Date(2026, 1, 5, 6, 30, 0, 0, time.FixedZone("EET", 2*3600))
	if f := s.Tick(context.Background(), local); len(f) != 1 {
		t.Fatalf("本地 06:30(+02:00)=04:30 UTC 應觸發, 得 %v", f)
	}
	s.Wait() // 動作已改為 goroutine 非同步派出(#5),斷言前需等待完成
	if restarts != 1 {
		t.Errorf("restarts = %d, 期望 1", restarts)
	}
}

// 停用的排程不觸發;weekday 不符不觸發。
func TestScheduler_DisabledAndWeekdaySkipped(t *testing.T) {
	store, events := schedTestStore(t)
	var restarts int
	s := NewScheduler(SchedulerConfig{
		Store: store, Events: events,
		Restart: func(context.Context, string) error { restarts++; return nil },
	})
	// 停用。
	if _, err := s.CreateSchedule("d", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, false); err != nil {
		t.Fatal(err)
	}
	// 只在週日(0)觸發,但測試時間為週一。
	if _, err := s.CreateSchedule("w", ScheduleKindRestart, ScheduleSpec{At: "04:30", Weekdays: []int{0}}, true); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC) // 週一
	if f := s.Tick(context.Background(), at); len(f) != 0 {
		t.Errorf("停用/weekday 不符不應觸發, 得 %v", f)
	}
	if restarts != 0 {
		t.Errorf("restarts = %d, 期望 0", restarts)
	}
}

// ---- 排程備份:成功記 STARTED/COMPLETED;失敗記 FAILED + 告警 ----

func TestScheduler_BackupSuccess(t *testing.T) {
	store, events := schedTestStore(t)
	s := NewScheduler(SchedulerConfig{
		Store: store, Events: events,
		Backup: func(_ context.Context, uuid string) (protocol.BackupMeta, error) {
			return protocol.BackupMeta{BackupID: "bk-1", InstanceUUID: uuid, TsUTC: time.Now().UTC()}, nil
		},
	})
	if _, err := s.CreateSchedule("inst-b", ScheduleKindBackup, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC)
	s.Tick(context.Background(), at)
	s.Wait() // 動作已改為 goroutine 非同步派出(#5),斷言前需等待完成

	if n := countCode(t, events, protocol.EventBackupStarted); n != 1 {
		t.Errorf("BACKUP_STARTED = %d, 期望 1", n)
	}
	if n := countCode(t, events, protocol.EventBackupCompleted); n != 1 {
		t.Errorf("BACKUP_COMPLETED = %d, 期望 1", n)
	}
	// 備份中繼落地。
	metas, err := store.ListBackups("inst-b")
	if err != nil || len(metas) != 1 {
		t.Errorf("備份中繼應落地 1 筆, 得 %+v err=%v", metas, err)
	}
}

// TestOrchestrator_RunLockedSharesInstanceLock 驗證排程備份(RunLocked)與生命週期操作(Start)
// 共用同一把 per-instance lock:RunLocked 持鎖期間,對同實例的 Start 會被阻擋直到釋放。
func TestOrchestrator_RunLockedSharesInstanceLock(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	orch := NewOrchestrator(OrchestratorConfig{Store: env.store, Events: env.events, Registry: env.reg})
	env.create("life", "lk-1")

	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = orch.RunLocked("lk-1", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	done := make(chan error, 1)
	go func() { done <- orch.Start(context.Background(), "lk-1") }()
	select {
	case <-done:
		t.Fatal("RunLocked 持鎖期間 Start 不應完成(未共用鎖)")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("釋放後 Start 應成功: %v", err)
	}
	if got := env.state("lk-1"); got != protocol.InstanceStateRunning {
		t.Errorf("observed = %s, 期望 Running", got)
	}
}

func TestScheduler_BackupFailureAlerts(t *testing.T) {
	store, events := schedTestStore(t)
	sink := &fakeAlertSink{}
	s := NewScheduler(SchedulerConfig{
		Store: store, Events: events, Alerts: sink,
		Backup: func(context.Context, string) (protocol.BackupMeta, error) {
			return protocol.BackupMeta{}, errors.New("disk full")
		},
	})
	if _, err := s.CreateSchedule("inst-f", ScheduleKindBackup, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC)
	s.Tick(context.Background(), at)
	s.Wait() // 動作已改為 goroutine 非同步派出(#5),斷言前需等待完成

	if n := countCode(t, events, protocol.EventBackupFailed); n != 1 {
		t.Errorf("BACKUP_FAILED = %d, 期望 1", n)
	}
	if sink.count() != 1 {
		t.Errorf("備份失敗應發告警 1 次, 得 %d", sink.count())
	}
	if n := countCode(t, events, protocol.EventAlertSent); n != 1 {
		t.Errorf("ALERT_SENT = %d, 期望 1", n)
	}
}

// ---- 雙審修正 #4:Run 進入輪詢迴圈前先評估一次當下時刻 ----

// TestScheduler_RunTicksImmediatelyOnStart 驗證 Run 不等待首個 ticker 週期就先評估一次:
// tick 週期設得很長(遠大於 ctx 存活時間),若 Run 只在 ticker.C 觸發, ctx 會先取消而
// restarts 仍為 0;若 Run 有前置 Tick,則開機當下若剛好到點就應立即觸發。
func TestScheduler_RunTicksImmediatelyOnStart(t *testing.T) {
	store, events := schedTestStore(t)
	var mu sync.Mutex
	var restarts int
	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC)
	s := NewScheduler(SchedulerConfig{
		Store: store, Events: events,
		Now:  func() time.Time { return at }, // 固定在到點分鐘
		Tick: time.Hour,                      // 遠大於下面的 ctx 存活時間,證明不是靠 ticker 觸發
		Restart: func(context.Context, string) error {
			mu.Lock()
			restarts++
			mu.Unlock()
			return nil
		},
	})
	if _, err := s.CreateSchedule("inst-run", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	s.Wait() // Run 內派出的動作是非同步 goroutine,回傳後需等待完成才能斷言

	mu.Lock()
	got := restarts
	mu.Unlock()
	if got != 1 {
		t.Errorf("Run 應於進迴圈前先評估一次當下時刻並觸發, restarts = %d, 期望 1", got)
	}
}

// ---- 雙審修正 #5:Tick 對到點動作 goroutine 化,不阻塞其他排程 ----

// TestScheduler_TickDoesNotBlockOnSlowAction 驗證同一次 Tick 中,一筆排程的慢動作不會阻塞
// Tick 本身回傳、也不阻擋另一筆排程的動作執行。
func TestScheduler_TickDoesNotBlockOnSlowAction(t *testing.T) {
	store, events := schedTestStore(t)
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	var fastFired int32
	s := NewScheduler(SchedulerConfig{
		Store: store, Events: events,
		Restart: func(_ context.Context, uuid string) error {
			if uuid == "slow" {
				started <- struct{}{}
				<-block // 模擬耗時動作(如備份),故意阻塞至測試釋放
				return nil
			}
			atomic.AddInt32(&fastFired, 1)
			return nil
		},
	})
	if _, err := s.CreateSchedule("slow", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule(slow): %v", err)
	}
	if _, err := s.CreateSchedule("fast", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule(fast): %v", err)
	}
	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC)

	tickDone := make(chan []string, 1)
	go func() { tickDone <- s.Tick(context.Background(), at) }()

	select {
	case fired := <-tickDone:
		if len(fired) != 2 {
			t.Fatalf("應認領 2 筆到點排程, 得 %v", fired)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tick 不應被慢動作阻塞,逾時未回傳")
	}

	select {
	case <-started:
		// 慢動作確實已開始執行(證明非同步派出,而非在 Tick 內同步等待)。
	case <-time.After(2 * time.Second):
		t.Fatal("慢動作應已開始執行")
	}
	close(block)
	s.Wait() // 等待所有 goroutine 結束,避免殘留到下一個測試(#5 的洩漏防範)

	if got := atomic.LoadInt32(&fastFired); got != 1 {
		t.Errorf("快動作應已觸發 1 次(不受慢動作阻塞), 得 %d", got)
	}
}

// ---- 雙審修正 #6:同分鐘 app 重啟不重觸發(last_fired_utc 持久化去重) ----

// TestScheduler_CrossRestartDedupSameMinute 驗證:同一個 store 上新建的 Scheduler 實例
// (模擬 app 同分鐘重啟,記憶體 lastFired 清空)在同一到點分鐘不會重觸發;跨分鐘後可再觸發。
func TestScheduler_CrossRestartDedupSameMinute(t *testing.T) {
	store, events := schedTestStore(t)
	var restarts int32
	newSched := func() *Scheduler {
		return NewScheduler(SchedulerConfig{
			Store: store, Events: events,
			Restart: func(context.Context, string) error {
				atomic.AddInt32(&restarts, 1)
				return nil
			},
		})
	}
	s1 := newSched()
	if _, err := s1.CreateSchedule("inst-x", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, true); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC)
	if f := s1.Tick(context.Background(), at); len(f) != 1 {
		t.Fatalf("首次應觸發 1 筆, 得 %v", f)
	}
	s1.Wait()

	// 模擬 app 同分鐘重啟:全新 Scheduler 實例(記憶體 lastFired 為空),同一個 store。
	s2 := newSched()
	if f := s2.Tick(context.Background(), at.Add(20*time.Second)); len(f) != 0 {
		t.Errorf("同分鐘重啟不應重觸發, 得 %v", f)
	}

	// 跨到點分鐘應可再觸發(證明去重只鎖同一分鐘,不是永久停用)。
	next := at.Add(24 * time.Hour)
	if f := s2.Tick(context.Background(), next); len(f) != 1 {
		t.Errorf("跨到點分鐘應再觸發, 得 %v", f)
	}
	s2.Wait()

	if got := atomic.LoadInt32(&restarts); got != 2 {
		t.Errorf("restarts = %d, 期望 2(首次 + 跨分鐘再觸發,同分鐘重啟不計)", got)
	}

	// DB 應已持久化 last_fired_utc(供第三個實例讀回驗證去重狀態非僅記憶體)。
	recs, err := store.ListSchedules()
	if err != nil || len(recs) != 1 || recs[0].LastFiredUTC == nil {
		t.Fatalf("last_fired_utc 應已持久化, 得 %+v err=%v", recs, err)
	}
	if !recs[0].LastFiredUTC.Equal(next.UTC().Truncate(time.Minute)) {
		t.Errorf("last_fired_utc = %v, 期望 %v", recs[0].LastFiredUTC, next.UTC().Truncate(time.Minute))
	}
}

// TestScheduler_UpsertSchedulePreservesLastFired 驗證 UpsertSchedule(經 UpdateSchedule 呼叫,
// 不帶 LastFiredUTC)不會抹掉既有的 last_fired_utc——否則編輯排程會意外重啟同分鐘去重狀態。
func TestScheduler_UpsertSchedulePreservesLastFired(t *testing.T) {
	store, events := schedTestStore(t)
	s := NewScheduler(SchedulerConfig{
		Store: store, Events: events,
		Restart: func(context.Context, string) error { return nil },
	})
	id, err := s.CreateSchedule("inst-y", ScheduleKindRestart, ScheduleSpec{At: "04:30"}, true)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	at := time.Date(2026, 1, 5, 4, 30, 0, 0, time.UTC)
	if f := s.Tick(context.Background(), at); len(f) != 1 {
		t.Fatalf("首次應觸發, 得 %v", f)
	}
	s.Wait()

	if err := s.UpdateSchedule(id, "inst-y", ScheduleKindRestart, ScheduleSpec{At: "05:00"}, true); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	recs, err := store.ListSchedules()
	if err != nil || len(recs) != 1 || recs[0].LastFiredUTC == nil {
		t.Fatalf("UpdateSchedule 後 last_fired_utc 不應被抹掉, 得 %+v err=%v", recs, err)
	}
}
