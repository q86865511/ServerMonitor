package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// t11Env 是 T11(探針/重啟策略/排程)測試共用的環境:MockBackend + httptest 代理(免 Docker)、
// 暫存 Store/EventLog、單節點 NodeRegistry、可載入自訂範本的 TemplateEngine 與 InstanceService。
// 有別於 lifecycle_test.go 的 lifeHarness(綁定 life 範本、預設 crashHook 記錄器),本環境讓各測試
// 自行注入範本、prober 與 crashHook,以覆蓋 tcp/rcon health 與 RestartPolicy 接線。
type t11Env struct {
	t       *testing.T
	backend *agent.MockBackend
	store   *Store
	events  *EventLog
	reg     *NodeRegistry
	secrets *SecretStore
	eng     *TemplateEngine
	svc     *InstanceService
	node    string
	clock   *fakeClock

	crashMu sync.Mutex
	crashed []crashRec
}

// newT11Env 建環境;tmpls 為 templateID→TOML 內容(至少一份)。useFakeClock 供 orch/policy 注入。
func newT11Env(t *testing.T, tmpls map[string]string, useFakeClock bool) *t11Env {
	t.Helper()
	backend := agent.NewMockBackend()
	hs := newAgentServerWithBackend(t, backend)

	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	reg := NewNodeRegistry(events)
	reg.Register("local", NewNodeClient(hs.URL, agentTestToken, nil))

	eng := NewTemplateEngine(nil, events)
	dir := t.TempDir()
	for id, body := range tmpls {
		writeTemplateFile(t, dir, id+".toml", body)
	}
	if _, err := eng.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	journal, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	svc := NewInstanceService(InstanceServiceConfig{
		Store: store, Secrets: secrets, Events: events, Engine: eng,
		Registry: reg, Journal: journal,
	})

	env := &t11Env{
		t: t, backend: backend, store: store, events: events, reg: reg,
		secrets: secrets, eng: eng, svc: svc, node: "local",
	}
	if useFakeClock {
		env.clock = newFakeClock(time.Unix(1700000000, 0).UTC())
	}
	return env
}

// recordCrash 是可作為 crashHook 的記錄器(供斷言崩潰交棒)。
func (e *t11Env) recordCrash(uuid string, exitCode *int) {
	e.crashMu.Lock()
	e.crashed = append(e.crashed, crashRec{uuid: uuid, exitCode: exitCode})
	e.crashMu.Unlock()
}

func (e *t11Env) crashCount() int {
	e.crashMu.Lock()
	defer e.crashMu.Unlock()
	return len(e.crashed)
}

// create 以 svc 建一個指定 UUID 的實例(不啟動)。
func (e *t11Env) create(templateID, uuid string) InstanceRecord {
	e.t.Helper()
	e.svc.newUUID = func() string { return uuid }
	rec, err := e.svc.Create(context.Background(), CreateOptions{TemplateID: templateID})
	if err != nil {
		e.t.Fatalf("Create(%s): %v", templateID, err)
	}
	return rec
}

// state 取實例 observed 狀態。
func (e *t11Env) state(uuid string) protocol.InstanceState {
	rec, err := e.store.GetInstance(uuid)
	if err != nil {
		e.t.Fatalf("GetInstance(%s): %v", uuid, err)
	}
	return rec.ObservedState
}

// countEvents 依 code 計數。
func (e *t11Env) countEvents(code protocol.EventCode) int {
	return len(queryEvents(e.t, e.events, code))
}

// waitState 輪詢等待狀態(供非同步重啟/復原路徑)。
func (e *t11Env) waitState(uuid string, want protocol.InstanceState, timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.state(uuid) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("等待 %s 至 %s 逾時,實際 %s", uuid, want, e.state(uuid))
}
