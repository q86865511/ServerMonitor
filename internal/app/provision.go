package app

import (
	"context"

	"github.com/gorilla/websocket"

	"servermonitor/internal/protocol"
)

// SubscribeProvision 開一條事件訂閱,篩出 native 建立期的供應進度(RuntimeEventProvision)並轉為
// channel(native-backend R12)。供綁定層於建立實例期間顯示供應進度;回傳的 stop 收束訂閱
// (取消 → 關閉 WS → goroutine 收束 → 關閉 channel)。節點離線/無法連線時回錯,呼叫端可略過
// 進度顯示而不影響建立。
//
// 以「獨立 events WS 訂閱」橋接 agent→GUI:agent 事件 hub 對多訂閱者廣播(見
// internal/agent/event_hub.go),故此訂閱與 Orchestrator 的生命週期事件迴圈並存、互不干擾,
// 且不需改動 core 的事件分派(HandleRuntimeEvent 目前忽略 provision 事件)。以空 cursor 訂閱
// (只收新事件),建立當下(訂閱之後)發生的供應進度即納入。進度為即時、可丟失(丟一格僅使
// 進度條較不連續),故不做斷線重連/resync。
func (r *Runtime) SubscribeProvision(ctx context.Context) (<-chan protocol.ProvisionProgress, func(), error) {
	client, err := r.registry.Client(r.node)
	if err != nil {
		return nil, nil, err
	}
	subCtx, cancel := context.WithCancel(ctx)
	conn, err := client.EventsWS(subCtx, "")
	if err != nil {
		cancel()
		return nil, nil, err
	}
	ch := make(chan protocol.ProvisionProgress, 32)
	go r.pumpProvision(subCtx, conn, ch)
	return ch, cancel, nil
}

// pumpProvision 讀取事件連線、篩出供應進度轉推至 ch,直到 ctx 取消或連線結束。ctx 取消時關閉
// conn 以中斷阻塞中的 ReadJSON(否則 goroutine 無法收束)。
func (r *Runtime) pumpProvision(ctx context.Context, conn *websocket.Conn, ch chan<- protocol.ProvisionProgress) {
	defer close(ch)
	defer conn.Close()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	for {
		var ev protocol.RuntimeEvent
		if err := conn.ReadJSON(&ev); err != nil {
			return
		}
		if ev.Kind != protocol.RuntimeEventProvision || ev.Progress == nil {
			continue
		}
		select {
		case ch <- *ev.Progress:
		case <-ctx.Done():
			return
		}
	}
}
