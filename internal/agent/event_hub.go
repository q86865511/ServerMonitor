package agent

import (
	"context"
	"strconv"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// eventHub 是執行事件的緩衝 + 扇出核心:指派單調遞增 cursor、保留有界歷史緩衝供
// reconnect 對帳、以每訂閱者非阻塞通道扇出,消費過慢者被關閉(ErrSlowConsumer)。
// 語意與 MockBackend 的 Events 一致(cursor=遞增 int64 字串;since 空=只收新事件;
// since 非空=補送游標後緩衝事件再轉即時;游標已逐出=先送 Resync)。
//
// DockerBackend 以此把 Docker events API 的即時串流轉為與 Mock 等價的可對帳串流;
// MockBackend 自帶等價實作(T3),兩者刻意各自持有以隔離、由本套件測試鎖定語意。
// 併發安全。
type eventHub struct {
	mu sync.Mutex

	evSeq  int64
	subSeq int

	history     []RuntimeEvent // 事件緩衝(cursor 遞增)
	historyMax  int
	evictedUpTo int64 // 已逐出的最大 cursor 數值;請求游標 < 此值即判定漏事件

	subs   map[int]*hubSub
	closed bool
}

// newEventHub 建立一個空的事件 hub;historyMax<=0 時採 defaultEventHistory。
func newEventHub(historyMax int) *eventHub {
	if historyMax <= 0 {
		historyMax = defaultEventHistory
	}
	return &eventHub{
		historyMax: historyMax,
		subs:       make(map[int]*hubSub),
	}
}

// emit 指派 cursor、寫入緩衝(必要時逐出最舊)並非阻塞扇出,回傳指派的 Cursor。
// closed 後為 no-op,回傳空 Cursor。
func (h *eventHub) emit(ev RuntimeEvent) protocol.Cursor {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ""
	}
	h.evSeq++
	cur := protocol.Cursor(strconv.FormatInt(h.evSeq, 10))
	ev.Cursor = cur
	if ev.TsUTC.IsZero() {
		ev.TsUTC = time.Now().UTC()
	}
	h.history = append(h.history, ev)
	if h.historyMax > 0 && len(h.history) > h.historyMax {
		drop := len(h.history) - h.historyMax
		if dn, ok := parseCursor(h.history[drop-1].Cursor); ok && dn > h.evictedUpTo {
			h.evictedUpTo = dn
		}
		h.history = append([]RuntimeEvent(nil), h.history[drop:]...)
	}
	for sid, sub := range h.subs {
		select {
		case sub.in <- ev:
		default:
			sub.stop(ErrSlowConsumer)
			delete(h.subs, sid)
		}
	}
	return cur
}

// subscribe 建立一個事件串流;since 語意見 RuntimeBackend.Events。
func (h *eventHub) subscribe(ctx context.Context, since protocol.Cursor) (EventStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrBackendClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sinceN, hasSince := parseCursor(since)
	var replay []RuntimeEvent
	gap := false
	if hasSince {
		if sinceN < h.evictedUpTo {
			gap = true // 請求游標之後的部分事件已被逐出 → 漏事件
		}
		for _, ev := range h.history {
			if cn, ok := parseCursor(ev.Cursor); ok && cn > sinceN {
				replay = append(replay, ev)
			}
		}
	}

	buf := len(replay) + defaultSubBuffer
	if gap {
		buf++
	}
	h.subSeq++
	sub := &hubSub{
		id:   h.subSeq,
		in:   make(chan RuntimeEvent, buf),
		quit: make(chan struct{}),
	}
	// 補送資料在註冊前預載進緩衝(容量已計入),不會阻塞;之後的即時事件由 emit 續推。
	if gap {
		sub.in <- RuntimeEvent{Kind: RuntimeEventResync, TsUTC: time.Now().UTC()}
	}
	for _, ev := range replay {
		sub.in <- ev
	}
	h.subs[sub.id] = sub
	return newHubEventStream(ctx, h, sub), nil
}

// close 關閉 hub 並終止所有事件串流。
func (h *eventHub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*hubSub, 0, len(h.subs))
	for _, s := range h.subs {
		subs = append(subs, s)
	}
	h.subs = make(map[int]*hubSub)
	h.mu.Unlock()
	for _, s := range subs {
		s.stop(nil)
	}
}

// unregister 從 subs 移除並終止一個訂閱者(供串流關閉時呼叫)。
func (h *eventHub) unregister(sub *hubSub) {
	h.mu.Lock()
	delete(h.subs, sub.id)
	h.mu.Unlock()
	sub.stop(nil)
}

// hubSub 是一個事件訂閱者:in 由 emit(持 h.mu)寫入,pump 由此讀出轉推給消費端。
type hubSub struct {
	id   int
	in   chan RuntimeEvent
	quit chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func (s *hubSub) stop(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.quit)
	})
}

func (s *hubSub) getErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// hubEventStream 以一條 pump goroutine 把 sub.in 轉推到 out,並在 ctx 取消 /
// Close() / quit 時收束、關閉 out 並自 hub 移除。
type hubEventStream struct {
	out chan RuntimeEvent
	sub *hubSub
	h   *eventHub
}

func newHubEventStream(ctx context.Context, h *eventHub, sub *hubSub) *hubEventStream {
	out := make(chan RuntimeEvent)
	st := &hubEventStream{out: out, sub: sub, h: h}
	go func() {
		defer close(out)
		defer h.unregister(sub)
		for {
			select {
			case <-ctx.Done():
				sub.stop(ctx.Err())
				return
			case <-sub.quit:
				return
			case ev := <-sub.in:
				select {
				case out <- ev:
				case <-ctx.Done():
					sub.stop(ctx.Err())
					return
				case <-sub.quit:
					return
				}
			}
		}
	}()
	return st
}

func (s *hubEventStream) Events() <-chan RuntimeEvent { return s.out }
func (s *hubEventStream) Err() error                  { return s.sub.getErr() }
func (s *hubEventStream) Close() error {
	s.h.unregister(s.sub)
	return nil
}
