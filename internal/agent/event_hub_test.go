package agent

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"

	"servermonitor/internal/protocol"
)

// 驗收:hub 即時發送、cursor 單調遞增(與 MockBackend.Events 一致)。
func TestEventHub_LiveMonotonic(t *testing.T) {
	h := newEventHub(0)
	defer h.close()

	s, err := h.subscribe(context.Background(), "")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer s.Close()

	c1 := h.emit(RuntimeEvent{ID: "a", Kind: RuntimeEventStart})
	c2 := h.emit(RuntimeEvent{ID: "a", Kind: RuntimeEventDie})

	ev1 := recvEvent(t, s, time.Second)
	ev2 := recvEvent(t, s, time.Second)
	if ev1.Cursor != c1 || ev2.Cursor != c2 {
		t.Fatalf("cursor mismatch: got %s,%s want %s,%s", ev1.Cursor, ev2.Cursor, c1, c2)
	}
	if !(ev1.Cursor < ev2.Cursor) {
		t.Fatalf("cursor not monotonic: %s !< %s", ev1.Cursor, ev2.Cursor)
	}
}

// 驗收(重連對帳):斷線期間注入的事件,以 since 游標重連後可補送。
func TestEventHub_ReconnectCursor(t *testing.T) {
	h := newEventHub(0)
	defer h.close()
	ctx := context.Background()

	s1, _ := h.subscribe(ctx, "")
	cA := h.emit(RuntimeEvent{ID: "x", Kind: RuntimeEventDie})
	evA := recvEvent(t, s1, time.Second)
	if evA.Cursor != cA {
		t.Fatalf("live cursor want %s, got %s", cA, evA.Cursor)
	}
	s1.Close()

	cB := h.emit(RuntimeEvent{ID: "x", Kind: RuntimeEventHealth, Health: "unhealthy"})

	s2, err := h.subscribe(ctx, cA) // 以最後游標重連
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
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
func TestEventHub_GapResync(t *testing.T) {
	h := newEventHub(1) // 逼出逐出
	defer h.close()

	c1 := h.emit(RuntimeEvent{ID: "x", Kind: RuntimeEventDie}) // history=[1]
	h.emit(RuntimeEvent{ID: "x", Kind: RuntimeEventDie})       // 逐出 1 → evictedUpTo=1
	h.emit(RuntimeEvent{ID: "x", Kind: RuntimeEventDie})       // 逐出 2 → evictedUpTo=2

	// since=c1(=1) < evictedUpTo(2):游標之後的部分事件已逐出 → gap。
	s, err := h.subscribe(context.Background(), c1)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer s.Close()

	ev := recvEvent(t, s, time.Second)
	if ev.Kind != RuntimeEventResync {
		t.Fatalf("want resync marker first, got %s", ev.Kind)
	}
}

// 驗收:ctx 取消收束串流,Err 反映原因。
func TestEventHub_ContextCancel(t *testing.T) {
	h := newEventHub(0)
	defer h.close()
	ctx, cancel := context.WithCancel(context.Background())

	s, _ := h.subscribe(ctx, "")
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

// 驗收:消費過慢的訂閱者被關閉,Err=ErrSlowConsumer;不阻塞 emit。
func TestEventHub_SlowConsumerDropped(t *testing.T) {
	h := newEventHub(0)
	defer h.close()

	s, _ := h.subscribe(context.Background(), "")
	defer s.Close()
	// 不讀 s.Events();灌爆訂閱者緩衝(defaultSubBuffer)。
	for i := 0; i < defaultSubBuffer+16; i++ {
		h.emit(RuntimeEvent{ID: "x", Kind: RuntimeEventDie})
	}
	// 串流應被關閉(out 關閉),Err 為 ErrSlowConsumer。
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-s.Events():
			if !ok {
				if err := s.Err(); err != ErrSlowConsumer {
					t.Fatalf("want ErrSlowConsumer, got %v", err)
				}
				return
			}
		case <-deadline:
			t.Fatal("slow consumer stream was not closed")
		}
	}
}

// 驗收:close 後 subscribe 回 ErrBackendClosed。
func TestEventHub_ClosedRejectsSubscribe(t *testing.T) {
	h := newEventHub(0)
	h.close()
	if _, err := h.subscribe(context.Background(), ""); err != ErrBackendClosed {
		t.Fatalf("want ErrBackendClosed after close, got %v", err)
	}
	// emit 於 close 後為 no-op(回空 cursor)。
	if c := h.emit(RuntimeEvent{ID: "x", Kind: RuntimeEventDie}); c != "" {
		t.Fatalf("emit after close should be no-op, got cursor %q", c)
	}
}

// 驗收:mapDockerEvent 對 die/start/health/oom 的映射(純型別,不需 Docker daemon)。
func TestMapDockerEvent(t *testing.T) {
	mkDie := events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionDie,
		Actor:  events.Actor{ID: "cid1", Attributes: map[string]string{"exitCode": "137"}},
	}
	ev, ok := mapDockerEvent(mkDie)
	if !ok || ev.Kind != RuntimeEventDie || ev.ID != protocol.RuntimeID("cid1") {
		t.Fatalf("die mapping: ok=%v ev=%+v", ok, ev)
	}
	if ev.ExitCode == nil || *ev.ExitCode != 137 {
		t.Fatalf("die exitCode want 137, got %v", ev.ExitCode)
	}

	mkStart := events.Message{Type: events.ContainerEventType, Action: events.ActionStart, Actor: events.Actor{ID: "cid1"}}
	if ev, ok := mapDockerEvent(mkStart); !ok || ev.Kind != RuntimeEventStart {
		t.Fatalf("start mapping: ok=%v ev=%+v", ok, ev)
	}

	mkOOM := events.Message{Type: events.ContainerEventType, Action: events.ActionOOM, Actor: events.Actor{ID: "cid1"}}
	if ev, ok := mapDockerEvent(mkOOM); !ok || ev.Kind != RuntimeEventOOM {
		t.Fatalf("oom mapping: ok=%v ev=%+v", ok, ev)
	}

	mkHealth := events.Message{Type: events.ContainerEventType, Action: "health_status: unhealthy", Actor: events.Actor{ID: "cid1"}}
	if ev, ok := mapDockerEvent(mkHealth); !ok || ev.Kind != RuntimeEventHealth || ev.Health != "unhealthy" {
		t.Fatalf("health mapping: ok=%v ev=%+v", ok, ev)
	}

	// 非容器事件與非關心動作應被忽略。
	if _, ok := mapDockerEvent(events.Message{Type: events.ImageEventType, Action: events.ActionDie}); ok {
		t.Fatal("non-container event should be ignored")
	}
	if _, ok := mapDockerEvent(events.Message{Type: events.ContainerEventType, Action: "attach", Actor: events.Actor{ID: "cid1"}}); ok {
		t.Fatal("uninteresting action should be ignored")
	}
}
