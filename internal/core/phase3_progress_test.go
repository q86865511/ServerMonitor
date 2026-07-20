package core

import (
	"context"
	"sync"
	"testing"

	"servermonitor/internal/protocol"
)

// TestOrchestrator_StartupProgressEmitted_Phase3 驗證階段 3:啟動時 Orchestrator 經 ProgressHook 回報
// starting / awaiting-ready / ready 階段,且每格帶正確 InstanceUUID(供 GUI 以 provision:<uuid> 分派)。
func TestOrchestrator_StartupProgressEmitted_Phase3(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	env.create("life", "p3-1")

	var mu sync.Mutex
	var stages []string
	badUUID := ""
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg,
		ProgressHook: func(uuid string, p protocol.ProvisionProgress) {
			mu.Lock()
			stages = append(stages, p.Stage)
			if p.InstanceUUID != "p3-1" {
				badUUID = p.InstanceUUID
			}
			mu.Unlock()
		},
	})
	if err := orch.Start(context.Background(), "p3-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if badUUID != "" {
		t.Errorf("進度事件 InstanceUUID 應為 p3-1,實得 %q", badUUID)
	}
	has := map[string]bool{}
	for _, s := range stages {
		has[s] = true
	}
	if !has["starting"] || !has["awaiting-ready"] || !has["ready"] {
		t.Errorf("啟動進度階段應含 starting/awaiting-ready/ready,實得 %v", stages)
	}
}

// TestIsWSErrorFrame_Phase3B4 驗證 B4:APIError 錯誤 frame 被辨識、正常 stats/log frame 不被誤判
// (據以在 pumpStats/pumpLogs 跳過錯誤 frame,不 fanout 假零資料/空白行)。
func TestIsWSErrorFrame_Phase3B4(t *testing.T) {
	if !isWSErrorFrame([]byte(`{"code":"ERR_INTERNAL","message":"boom"}`)) {
		t.Error("APIError frame(含非空 code)應被判為錯誤 frame")
	}
	if isWSErrorFrame([]byte(`{"ts_utc":"2024-01-01T00:00:00Z","cpu_percent":1.5,"memory_bytes":100}`)) {
		t.Error("ResourceStats frame 不應被判為錯誤 frame")
	}
	if isWSErrorFrame([]byte(`{"ts_utc":"2024-01-01T00:00:00Z","stream":"stdout","line":"hi"}`)) {
		t.Error("LogLine frame 不應被判為錯誤 frame")
	}
	if isWSErrorFrame([]byte(`{"code":"","message":""}`)) {
		t.Error("空 code 不應被判為錯誤 frame(避免誤傷)")
	}
}
