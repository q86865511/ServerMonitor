package core

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

const agentTestToken = "core-test-token"

// builtinTemplatesDir 是 repo 根 templates/ 目錄相對於本套件(internal/core)的路徑;
// `go test` 以套件目錄為 cwd,故此相對路徑於測試時解析到內建範本。
const builtinTemplatesDir = "../../templates"

// newAgentServer 起一個以 MockBackend 為後端的 T5 代理 server(真 HTTP,免 Docker),
// 回傳 httptest server 與底層 MockBackend(供注入事件/斷言容器狀態)。
func newAgentServer(t *testing.T) (*httptest.Server, *agent.MockBackend) {
	t.Helper()
	backend := agent.NewMockBackend()
	return newAgentServerWithBackend(t, backend), backend
}

// newAgentServerWithBackend 允許注入自訂後端(如包裝可注入錯誤的後端)。
func newAgentServerWithBackend(t *testing.T, backend agent.RuntimeBackend) *httptest.Server {
	t.Helper()
	s, err := agent.NewServer(agent.Config{Backend: backend, Token: agentTestToken})
	if err != nil {
		t.Fatalf("agent.NewServer: %v", err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		hs.Close()
		_ = s.Close()
	})
	return hs
}

// newTestEventLog 建立以暫存 Store 為後端的 EventLog(供斷言事件寫入)。
func newTestEventLog(t *testing.T) (*EventLog, *Store) {
	t.Helper()
	st, _ := newTempStore(t)
	return NewEventLog(st, nil, EventLogOptions{}), st
}

// writeTemplateFile 在 dir 寫一份範本檔。
func writeTemplateFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("寫入範本 %s: %v", name, err)
	}
}

// queryEvents 依 code 查詢事件(供斷言 TEMPLATE_LOAD_FAILED / NODE_OFFLINE 等)。
func queryEvents(t *testing.T, log *EventLog, code protocol.EventCode) []protocol.Event {
	t.Helper()
	evs, err := log.Query(EventFilter{Code: &code})
	if err != nil {
		t.Fatalf("Query %s: %v", code, err)
	}
	return evs
}
