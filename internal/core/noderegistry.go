package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// ErrUnknownNode 表示查無已註冊的節點。
var ErrUnknownNode = errors.New("core: 未註冊的節點")

// NodeStatus 是一個節點的對外狀態快照(供 GUI 顯示;R5「代理無回應時標離線並顯示」)。
type NodeStatus struct {
	Node    string
	Online  bool
	LastErr string // 最近一次不可達的原因(online 時為空)
}

// nodeEntry 是登錄中的單一節點。
type nodeEntry struct {
	client  *NodeClient
	online  bool
	lastErr string
}

// NodeRegistry 管理節點集合與其線上狀態(R5)。NodeClient 呼叫回報「不可達」
// (ErrNodeUnreachable)時,把該節點標記離線並記一筆 NODE_OFFLINE 事件(僅於
// online→offline 轉換時記,避免重複轟炸);呼叫成功則標回線上。GUI 可經 Status/List
// 查詢,使單一節點離線不致整體當掉。併發安全。
type NodeRegistry struct {
	mu     sync.Mutex
	nodes  map[string]*nodeEntry
	events *EventLog
	now    func() time.Time
}

// NewNodeRegistry 建立節點登錄。events 可為 nil(則不記 NODE_OFFLINE,僅維護狀態)。
func NewNodeRegistry(events *EventLog) *NodeRegistry {
	return &NodeRegistry{
		nodes:  make(map[string]*nodeEntry),
		events: events,
		now:    time.Now,
	}
}

// Register 註冊(或覆蓋)一個節點的 NodeClient;初始視為線上。
func (r *NodeRegistry) Register(node string, client *NodeClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[node] = &nodeEntry{client: client, online: true}
}

// Client 取得某節點的 NodeClient;未註冊回 ErrUnknownNode。
func (r *NodeRegistry) Client(node string) (*NodeClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.nodes[node]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownNode, node)
	}
	return e.client, nil
}

// Call 對某節點的 NodeClient 執行一次呼叫,並依結果更新其線上狀態:
// fn 回 ErrNodeUnreachable → 標離線(必要時記 NODE_OFFLINE);否則標回線上。
// fn 的錯誤原樣回傳,呼叫端據以判別是傳輸失敗或 API 錯誤。
func (r *NodeRegistry) Call(node string, fn func(*NodeClient) error) error {
	client, err := r.Client(node)
	if err != nil {
		return err
	}
	cerr := fn(client)
	r.observe(node, cerr)
	return cerr
}

// observe 依一次呼叫的錯誤更新節點狀態。傳輸不可達→離線;其餘(含 API 錯誤與成功)→線上。
func (r *NodeRegistry) observe(node string, err error) {
	unreachable := errors.Is(err, ErrNodeUnreachable)

	r.mu.Lock()
	e, ok := r.nodes[node]
	if !ok {
		r.mu.Unlock()
		return
	}
	var transitionedOffline bool
	if unreachable {
		if e.online {
			transitionedOffline = true
		}
		e.online = false
		e.lastErr = err.Error()
	} else {
		e.online = true
		e.lastErr = ""
	}
	r.mu.Unlock()

	if transitionedOffline {
		r.recordOffline(node, err)
	}
}

// MarkOffline 明確把節點標離線(供對帳/健康檢查等外部判定)。
func (r *NodeRegistry) MarkOffline(node string, cause error) {
	r.mu.Lock()
	e, ok := r.nodes[node]
	if !ok {
		r.mu.Unlock()
		return
	}
	transitioned := e.online
	e.online = false
	if cause != nil {
		e.lastErr = cause.Error()
	}
	r.mu.Unlock()
	if transitioned {
		r.recordOffline(node, cause)
	}
}

// Status 回傳某節點狀態;第二回傳值表示節點是否存在。
func (r *NodeRegistry) Status(node string) (NodeStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.nodes[node]
	if !ok {
		return NodeStatus{}, false
	}
	return NodeStatus{Node: node, Online: e.online, LastErr: e.lastErr}, true
}

// List 回傳所有節點狀態,依節點名排序(穩定輸出)。
func (r *NodeRegistry) List() []NodeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NodeStatus, 0, len(r.nodes))
	for name, e := range r.nodes {
		out = append(out, NodeStatus{Node: name, Online: e.online, LastErr: e.lastErr})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// recordOffline 記一筆 NODE_OFFLINE 事件(R5/R14)。
func (r *NodeRegistry) recordOffline(node string, cause error) {
	if r.events == nil {
		return
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	details, _ := json.Marshal(map[string]string{"node": node, "error": msg})
	nodeCopy := node
	_ = r.events.Append(protocol.Event{
		Code:        protocol.EventNodeOffline,
		TsUTC:       r.now().UTC(),
		Severity:    protocol.SeverityWarning,
		Node:        &nodeCopy,
		DetailsJSON: details,
	})
}
