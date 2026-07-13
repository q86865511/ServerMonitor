package core

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// testSpec 造一個帶 gsm.uuid 標籤的 spec,使代理的 resolve(以標籤解析)可用。
func testSpec(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:   uuid,
		Image:  "example/image:1.0",
		Labels: map[string]string{labelUUID: uuid},
	}
}

func TestNodeClient_CreateListInspectStatus(t *testing.T) {
	hs, _ := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	ctx := context.Background()
	const uuid = "11111111-2222-3333-4444-555555555555"

	resp, err := c.Create(ctx, testSpec(uuid))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.UUID != uuid || resp.RuntimeID == "" {
		t.Fatalf("Create resp = %+v", resp)
	}

	list, err := c.List(ctx)
	if err != nil || len(list) != 1 || list[0].UUID != uuid {
		t.Fatalf("List = %+v, err=%v", list, err)
	}

	info, err := c.Inspect(ctx, uuid)
	if err != nil || info.ID != resp.RuntimeID {
		t.Fatalf("Inspect = %+v, err=%v", info, err)
	}

	st, err := c.Status(ctx, uuid)
	if err != nil || st.ID != resp.RuntimeID {
		t.Fatalf("Status = %+v, err=%v", st, err)
	}

	h, err := c.Health(ctx)
	if err != nil || h.Status != "ok" {
		t.Fatalf("Health = %+v, err=%v", h, err)
	}
}

func TestNodeClient_ErrorMapping_NotFound(t *testing.T) {
	hs, _ := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	_, err := c.Status(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("期望 ErrNodeNotFound,得 %v", err)
	}
}

func TestNodeClient_ErrorMapping_Unauthorized(t *testing.T) {
	hs, _ := newAgentServer(t)
	c := NewNodeClient(hs.URL, "wrong-token", nil)
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrNodeUnauthorized) {
		t.Fatalf("期望 ErrNodeUnauthorized,得 %v", err)
	}
}

func TestNodeClient_ErrorMapping_PortConflict(t *testing.T) {
	// 以包裝後端讓 Create 回 agent.ErrPortConflict,驗證映射為 ErrNodePortConflict。
	backend := &portConflictBackend{MockBackend: agent.NewMockBackend()}
	hs := newAgentServerWithBackend(t, backend)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	_, err := c.Create(context.Background(), testSpec("x"))
	if !errors.Is(err, ErrNodePortConflict) {
		t.Fatalf("期望 ErrNodePortConflict,得 %v", err)
	}
}

func TestNodeClient_Unreachable(t *testing.T) {
	// 起後立刻關閉,使連線被拒。
	dead := httptest.NewServer(nil)
	url := dead.URL
	dead.Close()

	c := NewNodeClient(url, agentTestToken, nil)
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("期望 ErrNodeUnreachable,得 %v", err)
	}
}

func TestNodeClient_EventsWS(t *testing.T) {
	hs, backend := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	const uuid = "evt-uuid"

	rid, err := backend.Create(context.Background(), testSpec(uuid))
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	backend.InjectDie(rid, 7) // cursor=1,以 since=0 訂閱補送

	conn, err := c.EventsWS(context.Background(), protocol.Cursor("0"))
	if err != nil {
		t.Fatalf("EventsWS: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ev protocol.RuntimeEvent
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if ev.Kind != protocol.RuntimeEventDie || ev.ExitCode == nil || *ev.ExitCode != 7 {
		t.Fatalf("event = %+v", ev)
	}
}

// portConflictBackend 讓 Create 一律回 agent.ErrPortConflict(其餘委派 MockBackend)。
type portConflictBackend struct {
	*agent.MockBackend
}

func (b *portConflictBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	return "", agent.ErrPortConflict
}
