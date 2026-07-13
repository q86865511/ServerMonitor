package agent

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"servermonitor/internal/protocol"
)

// Events 回傳執行事件串流。since 語意見 RuntimeBackend.Events;實際緩衝/對帳由 eventHub
// 承擔,Docker events API 的即時串流由背景 eventPump 餵入 hub。
func (b *DockerBackend) Events(ctx context.Context, since protocol.Cursor) (EventStream, error) {
	return b.hub.subscribe(ctx, since)
}

// eventPump 是背景 goroutine:訂閱 Docker events API,把容器 die/start/health/oom 事件映射
// 後餵給 hub;連線中斷則退避重連,直到 backend Close。
func (b *DockerBackend) eventPump() {
	defer b.wg.Done()
	const baseBackoff = time.Second
	const maxBackoff = 30 * time.Second
	backoff := baseBackoff
	for {
		if b.ctx.Err() != nil {
			return
		}
		start := time.Now()
		b.streamEvents() // 阻塞至錯誤或 ctx 取消
		if time.Since(start) > 5*time.Second {
			backoff = baseBackoff // 穩定運行過即重置退避
		}
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// streamEvents 訂閱一次 Docker events 串流,把相關事件餵給 hub,直到錯誤或 ctx 取消返回。
func (b *DockerBackend) streamEvents() {
	f := filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
		filters.Arg("label", labelManagedBy+"="+managedByValue),
	)
	msgs, errs := b.cli.Events(b.ctx, events.ListOptions{Filters: f})
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-errs:
			return // 交由 eventPump 退避重連
		case msg := <-msgs:
			if ev, ok := mapDockerEvent(msg); ok {
				b.hub.emit(ev)
			}
		}
	}
}

// mapDockerEvent 把 Docker 容器事件映射為 RuntimeEvent;非關心的事件回 ok=false。
// die 帶 exitCode;health_status:* 帶健康狀態。planned-stop 與否不落在事件層(見 StopOpts)。
func mapDockerEvent(msg events.Message) (RuntimeEvent, bool) {
	if msg.Type != events.ContainerEventType {
		return RuntimeEvent{}, false
	}
	ev := RuntimeEvent{ID: protocol.RuntimeID(msg.Actor.ID), TsUTC: eventTime(msg)}
	action := string(msg.Action)
	switch {
	case msg.Action == events.ActionStart:
		ev.Kind = RuntimeEventStart
	case msg.Action == events.ActionDie:
		ev.Kind = RuntimeEventDie
		if v, ok := msg.Actor.Attributes["exitCode"]; ok {
			if code, err := strconv.Atoi(v); err == nil {
				ev.ExitCode = &code
			}
		}
	case msg.Action == events.ActionOOM:
		ev.Kind = RuntimeEventOOM
	case strings.HasPrefix(action, string(events.ActionHealthStatus)):
		ev.Kind = RuntimeEventHealth
		ev.Health = parseHealthAction(action)
	default:
		return RuntimeEvent{}, false
	}
	return ev, true
}

// eventTime 取事件時間(優先奈秒),皆缺時回現在。
func eventTime(msg events.Message) time.Time {
	if msg.TimeNano > 0 {
		return time.Unix(0, msg.TimeNano).UTC()
	}
	if msg.Time > 0 {
		return time.Unix(msg.Time, 0).UTC()
	}
	return time.Now().UTC()
}

// parseHealthAction 由 "health_status: healthy" 取出 "healthy";無冒號後綴則回空字串。
func parseHealthAction(action string) string {
	if i := strings.IndexByte(action, ':'); i >= 0 {
		return strings.TrimSpace(action[i+1:])
	}
	return ""
}
