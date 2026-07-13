package agent

import (
	"context"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

func recvEvent(t *testing.T, s EventStream, d time.Duration) RuntimeEvent {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatal("event stream closed unexpectedly")
		}
		return ev
	case <-time.After(d):
		t.Fatal("timed out waiting for event")
		return RuntimeEvent{}
	}
}

func sampleSpec(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:       uuid,
		TemplateID: "minecraft",
		Image:      "itzg/minecraft-server:latest",
		Labels:     map[string]string{"gsm.uuid": uuid},
	}
}

// 驗收:MockBackend 跑通 Create→Start→Status→Stop。
func TestMockBackend_Lifecycle(t *testing.T) {
	m := NewMockBackend()
	defer m.Close()
	ctx := context.Background()

	id, err := m.Create(ctx, sampleSpec("u1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty RuntimeID")
	}

	st, err := m.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status after Create: %v", err)
	}
	if st.State != protocol.RuntimeStateCreated || st.Running {
		t.Fatalf("after Create want created/not-running, got %s running=%v", st.State, st.Running)
	}

	if err := m.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, _ = m.Status(ctx, id)
	if st.State != protocol.RuntimeStateRunning || !st.Running {
		t.Fatalf("after Start want running, got %s running=%v", st.State, st.Running)
	}
	if st.StartedAt == nil {
		t.Fatal("after Start StartedAt should be set")
	}

	if err := m.Stop(ctx, id, StopOpts{Grace: 30 * time.Second}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, _ = m.Status(ctx, id)
	if st.State != protocol.RuntimeStateStopped || st.Running {
		t.Fatalf("after Stop want stopped, got %s running=%v", st.State, st.Running)
	}
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("after Stop want exit 0, got %v", st.ExitCode)
	}
}

// 驗收:List / Inspect / Remove。
func TestMockBackend_ListInspectRemove(t *testing.T) {
	m := NewMockBackend()
	defer m.Close()
	ctx := context.Background()

	id1, _ := m.Create(ctx, sampleSpec("u1"))
	id2, _ := m.Create(ctx, sampleSpec("u2"))

	refs, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("List want 2, got %d", len(refs))
	}

	info, err := m.Inspect(ctx, id1)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.ID != id1 || info.Image != "itzg/minecraft-server:latest" {
		t.Fatalf("Inspect mismatch: %+v", info)
	}
	if info.Labels["gsm.uuid"] != "u1" {
		t.Fatalf("Inspect labels mismatch: %v", info.Labels)
	}

	if err := m.Remove(ctx, id2, RemoveOpts{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	refs, _ = m.List(ctx)
	if len(refs) != 1 || refs[0].ID != id1 {
		t.Fatalf("after Remove want only %s, got %+v", id1, refs)
	}

	if err := m.Remove(ctx, id2, RemoveOpts{}); err != ErrNotFound {
		t.Fatalf("Remove missing want ErrNotFound, got %v", err)
	}
	if _, err := m.Status(ctx, id2); err != ErrNotFound {
		t.Fatalf("Status missing want ErrNotFound, got %v", err)
	}
}

// 驗收:Archive→Restore 回傳新 RuntimeID。
func TestMockBackend_ArchiveRestore(t *testing.T) {
	m := NewMockBackend()
	defer m.Close()
	ctx := context.Background()

	id, _ := m.Create(ctx, sampleSpec("u1"))
	bid, err := m.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if bid == "" {
		t.Fatal("Archive returned empty BackupID")
	}

	newID, err := m.Restore(ctx, id, bid)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if newID == "" || newID == id {
		t.Fatalf("Restore want a new RuntimeID != %s, got %q", id, newID)
	}
	// 新實例可被查詢,且承載備份時的 spec(同 UUID)。
	info, err := m.Inspect(ctx, newID)
	if err != nil {
		t.Fatalf("Inspect restored: %v", err)
	}
	if info.Labels["gsm.uuid"] != "u1" {
		t.Fatalf("restored instance lost spec: %v", info.Labels)
	}

	if _, err := m.Restore(ctx, id, "no-such-backup"); err != ErrNotFound {
		t.Fatalf("Restore missing backup want ErrNotFound, got %v", err)
	}
}

// 驗收:Logs 回可控假資料並正常關閉。
func TestMockBackend_Logs(t *testing.T) {
	m := NewMockBackend()
	defer m.Close()
	ctx := context.Background()

	id, _ := m.Create(ctx, sampleSpec("u1"))
	stream, err := m.Logs(ctx, id, LogOpts{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer stream.Close()

	var got int
	for range stream.Lines() {
		got++
	}
	if got == 0 {
		t.Fatal("Logs produced no lines")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Logs Err after normal close: %v", err)
	}

	if _, err := m.Logs(ctx, "missing", LogOpts{}); err != ErrNotFound {
		t.Fatalf("Logs missing want ErrNotFound, got %v", err)
	}
}

// 驗收(崩潰路徑):Events 注入一個 die 事件後可被讀到。
func TestMockBackend_EventsInjectDie(t *testing.T) {
	m := NewMockBackend()
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	id, _ := m.Create(ctx, sampleSpec("u1"))
	if err := m.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream, err := m.Events(ctx, "") // 只收訂閱後的即時事件
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer stream.Close()

	cur := m.InjectDie(id, 137)

	ev := recvEvent(t, stream, time.Second)
	if ev.Kind != RuntimeEventDie {
		t.Fatalf("want die, got %s", ev.Kind)
	}
	if ev.ID != id {
		t.Fatalf("want id %s, got %s", id, ev.ID)
	}
	if ev.ExitCode == nil || *ev.ExitCode != 137 {
		t.Fatalf("want exit 137, got %v", ev.ExitCode)
	}
	if ev.Cursor != cur {
		t.Fatalf("want cursor %s, got %s", cur, ev.Cursor)
	}
	// 實例狀態亦轉為 exited。
	if st, _ := m.Status(ctx, id); st.State != protocol.RuntimeStateExited {
		t.Fatalf("after die want exited, got %s", st.State)
	}
}

// 驗收(重連對帳):斷線期間注入的事件,以 since 游標重連後可補送。
func TestMockBackend_EventsReconnectCursor(t *testing.T) {
	m := NewMockBackend()
	defer m.Close()
	ctx := context.Background()

	id, _ := m.Create(ctx, sampleSpec("u1"))

	s1, _ := m.Events(ctx, "")
	cA := m.InjectDie(id, 1)
	evA := recvEvent(t, s1, time.Second)
	if evA.Cursor != cA {
		t.Fatalf("live cursor want %s, got %s", cA, evA.Cursor)
	}
	s1.Close()

	// 斷線期間發生的事件。
	cB := m.InjectHealth(id, "unhealthy")

	s2, err := m.Events(ctx, cA) // 以最後游標重連
	if err != nil {
		t.Fatalf("reconnect Events: %v", err)
	}
	defer s2.Close()
	evB := recvEvent(t, s2, time.Second)
	if evB.Cursor != cB {
		t.Fatalf("want replayed cursor %s, got %s", cB, evB.Cursor)
	}
	if evB.Kind != RuntimeEventHealth || evB.Health != "unhealthy" {
		t.Fatalf("want replayed health event, got %+v", evB)
	}
}

// 驗收(漏事件對帳):請求的游標已被逐出時,重連先收到 resync 標記。
func TestMockBackend_EventsGapResync(t *testing.T) {
	m := NewMockBackend()
	m.historyMax = 1 // 逼出逐出
	defer m.Close()

	id := protocol.RuntimeID("x")
	c1 := m.InjectDie(id, 1) // history=[1]
	m.InjectDie(id, 2)       // 逐出 1 → evictedUpTo=1
	m.InjectDie(id, 3)       // 逐出 2 → evictedUpTo=2

	// since=c1(=1) < evictedUpTo(2):游標之後的部分事件已逐出 → gap。
	s, err := m.Events(context.Background(), c1)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer s.Close()

	ev := recvEvent(t, s, time.Second)
	if ev.Kind != RuntimeEventResync {
		t.Fatalf("want resync marker first, got %s", ev.Kind)
	}
}

// Events 因 ctx 取消而收束,Err 反映原因。
func TestMockBackend_EventsContextCancel(t *testing.T) {
	m := NewMockBackend()
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())

	s, _ := m.Events(ctx, "")
	cancel()

	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("expected stream to close on ctx cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not close after ctx cancel")
	}
}

// Stats / ExecProcess 回可控假資料。
func TestMockBackend_StatsExec(t *testing.T) {
	m := NewMockBackend()
	defer m.Close()
	ctx := context.Background()

	id, _ := m.Create(ctx, sampleSpec("u1"))

	stats, err := m.Stats(ctx, id)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.CPUPercent == 0 && stats.MemoryBytes == 0 {
		t.Fatal("Stats returned all-zero sample")
	}

	res, err := m.ExecProcess(ctx, id, ExecCmd{Cmd: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("ExecProcess: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout == "" {
		t.Fatalf("ExecProcess unexpected result: %+v", res)
	}
}

func TestNopCommandAdapter(t *testing.T) {
	var a GameCommandAdapter = NopCommandAdapter{}
	res, err := a.Send(context.Background(), CommandTarget{ProtocolID: "rcon", Kind: "rcon"}, protocol.GameCommand{Raw: "list"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Success {
		t.Fatalf("nop adapter should not report success, got %+v", res)
	}
}
