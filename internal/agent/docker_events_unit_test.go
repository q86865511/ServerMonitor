package agent

// 免 Docker 的單元測試,鎖定雙審修正 C:events 斷線補收 + Resync 對帳。
// 以假 dockerAPI 注入 Events 串流,直接驅動 streamEvents(不經 eventPump 退避,精準可測)。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
)

// fakeEventsCli 只實作 dockerAPI 的 Events/Close;其餘方法由內嵌介面(nil)承接,
// 本測試不觸及故不會被呼叫。每次 Events 呼叫記錄 Since 並換上新通道供推事件/錯誤。
type fakeEventsCli struct {
	dockerAPI
	mu     sync.Mutex
	sinces []string
	msgCh  chan events.Message
	errCh  chan error
	calls  chan struct{}
}

func newFakeEventsCli() *fakeEventsCli {
	return &fakeEventsCli{calls: make(chan struct{}, 8)}
}

func (f *fakeEventsCli) Events(_ context.Context, opts events.ListOptions) (<-chan events.Message, <-chan error) {
	f.mu.Lock()
	f.sinces = append(f.sinces, opts.Since)
	msgCh := make(chan events.Message, 8)
	errCh := make(chan error, 1)
	f.msgCh = msgCh
	f.errCh = errCh
	f.mu.Unlock()
	f.calls <- struct{}{}
	return msgCh, errCh
}

func (f *fakeEventsCli) Close() error { return nil }

func (f *fakeEventsCli) lastSince() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sinces) == 0 {
		return ""
	}
	return f.sinces[len(f.sinces)-1]
}

func (f *fakeEventsCli) pushMsg(m events.Message) {
	f.mu.Lock()
	ch := f.msgCh
	f.mu.Unlock()
	ch <- m
}

func (f *fakeEventsCli) pushErr(err error) {
	f.mu.Lock()
	ch := f.errCh
	f.mu.Unlock()
	ch <- err
}

func TestStreamEvents_ReconnectSinceAndResync(t *testing.T) {
	fake := newFakeEventsCli()
	b := &DockerBackend{cli: fake, hub: newEventHub(0)}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	defer b.cancel()

	sub, err := b.hub.subscribe(b.ctx, "")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	// 首連:streamEvents(zero) → Events 不帶 Since。
	done := make(chan time.Time, 1)
	go func() { done <- b.streamEvents(time.Time{}) }()
	<-fake.calls
	if got := fake.lastSince(); got != "" {
		t.Fatalf("首連 Since 應為空,got %q", got)
	}

	// 推一個帶時間戳的 die 事件,訂閱者應即時收到。
	evTime := time.Now().UTC().Truncate(time.Second)
	fake.pushMsg(events.Message{
		Type:     events.ContainerEventType,
		Action:   events.ActionDie,
		Actor:    events.Actor{ID: "c1", Attributes: map[string]string{"exitCode": "1"}},
		TimeNano: evTime.UnixNano(),
	})
	if ev := recvEvent(t, sub, time.Second); ev.Kind != RuntimeEventDie {
		t.Fatalf("want die, got %s", ev.Kind)
	}

	// 觸發斷線:streamEvents 應 emit Resync 並回傳最後事件時間。
	fake.pushErr(errors.New("connection reset"))
	if ev := recvEvent(t, sub, time.Second); ev.Kind != RuntimeEventResync {
		t.Fatalf("斷線後訂閱者應收到 Resync,got %s", ev.Kind)
	}
	last := <-done
	if last.IsZero() || !last.Equal(evTime) {
		t.Fatalf("last event time=%v want %v", last, evTime)
	}

	// 重連:以 last 為 Since 補收斷線窗口。
	go func() { done <- b.streamEvents(last) }()
	<-fake.calls
	wantSince := strconv.FormatInt(last.UnixNano(), 10)
	if got := fake.lastSince(); got != wantSince {
		t.Fatalf("重連 Since=%q want %q", got, wantSince)
	}

	// ctx 取消收束,不再 emit Resync。
	b.cancel()
	<-done
	select {
	case ev, ok := <-sub.Events():
		if ok && ev.Kind == RuntimeEventResync {
			t.Fatal("ctx 取消不應 emit Resync")
		}
	case <-time.After(200 * time.Millisecond):
	}
}
