package core

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"servermonitor/internal/protocol"
)

func TestNodeRegistry_OfflineOnUnreachable(t *testing.T) {
	log, _ := newTestEventLog(t)
	reg := NewNodeRegistry(log)

	// 指向已關閉的 server，使呼叫不可達。
	dead := httptest.NewServer(nil)
	url := dead.URL
	dead.Close()
	reg.Register("local", NewNodeClient(url, agentTestToken, nil))

	// 初始線上。
	if st, _ := reg.Status("local"); !st.Online {
		t.Fatalf("初始應線上")
	}

	err := reg.Call("local", func(c *NodeClient) error {
		_, e := c.Health(context.Background())
		return e
	})
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("期望不可達錯誤,得 %v", err)
	}

	st, ok := reg.Status("local")
	if !ok || st.Online {
		t.Fatalf("不可達後應標離線,得 %+v", st)
	}
	if st.LastErr == "" {
		t.Fatalf("離線應記錄 LastErr")
	}

	// NODE_OFFLINE 事件記一筆。
	evs := queryEvents(t, log, protocol.EventNodeOffline)
	if len(evs) != 1 {
		t.Fatalf("NODE_OFFLINE 事件數 = %d, 期望 1", len(evs))
	}

	// 再次不可達不應重複記事件(僅 online→offline 轉換時記)。
	_ = reg.Call("local", func(c *NodeClient) error {
		_, e := c.Health(context.Background())
		return e
	})
	if evs := queryEvents(t, log, protocol.EventNodeOffline); len(evs) != 1 {
		t.Fatalf("重複不可達不應多記事件,得 %d", len(evs))
	}
}

func TestNodeRegistry_RecoversOnline(t *testing.T) {
	log, _ := newTestEventLog(t)
	reg := NewNodeRegistry(log)
	hs, _ := newAgentServer(t)
	reg.Register("local", NewNodeClient(hs.URL, agentTestToken, nil))

	// 先人工標離線,再成功呼叫應標回線上。
	reg.MarkOffline("local", errors.New("test"))
	if st, _ := reg.Status("local"); st.Online {
		t.Fatalf("應為離線")
	}
	err := reg.Call("local", func(c *NodeClient) error {
		_, e := c.Health(context.Background())
		return e
	})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if st, _ := reg.Status("local"); !st.Online {
		t.Fatalf("成功呼叫後應標回線上")
	}
}

func TestNodeRegistry_UnknownNode(t *testing.T) {
	reg := NewNodeRegistry(nil)
	if _, err := reg.Client("nope"); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("期望 ErrUnknownNode,得 %v", err)
	}
}
