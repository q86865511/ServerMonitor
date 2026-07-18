package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// memKeyring 是 core.Keyring 的記憶體替身,使節點管理測試不觸碰真實 OS 金鑰庫。
type memKeyring struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemKeyring() *memKeyring { return &memKeyring{m: map[string]string{}} }

func (k *memKeyring) k(s, u string) string { return s + "\x00" + u }

func (k *memKeyring) Set(s, u, p string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[k.k(s, u)] = p
	return nil
}

func (k *memKeyring) Get(s, u string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[k.k(s, u)]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (k *memKeyring) Delete(s, u string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	kk := k.k(s, u)
	if _, ok := k.m[kk]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.m, kk)
	return nil
}

// fakeAgentServer 起一個假 agent 的 TLS server(對 /v1/health 驗 token),回傳 server 與葉憑證指紋。
func fakeAgentServer(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(protocol.APIError{Code: protocol.ErrUnauthorized, Message: "bad token"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.HealthResponse{Status: "ok", Version: protocol.AgentAPIVersion})
	})
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	return ts, core.CertFingerprint(ts.Certificate().Raw)
}

// bootRuntimeWithSecrets 以記憶體金鑰庫啟一個 Runtime(供節點管理測試),回傳 rt 與金鑰庫。
func bootRuntimeWithSecrets(t *testing.T) (*Runtime, *memKeyring) {
	t.Helper()
	opts, _, _ := bootOpts(t)
	kr := newMemKeyring()
	opts.Secrets = core.NewSecretStoreWithKeyring("test", kr)
	rt, err := Bootstrap(opts)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(rt.Shutdown)
	return rt, kr
}

func TestProbeNode_OK(t *testing.T) {
	rt, _ := bootRuntimeWithSecrets(t)
	const token = "good-token"
	ts, fp := fakeAgentServer(t, token)

	res := rt.ProbeNode(ts.URL, token, false)
	if !res.OK {
		t.Fatalf("撥測應成功: %+v", res)
	}
	if res.Fingerprint != fp {
		t.Errorf("指紋不符: got=%s want=%s", res.Fingerprint, fp)
	}
	if res.Version != protocol.AgentAPIVersion {
		t.Errorf("version 非預期: %q", res.Version)
	}
}

func TestProbeNode_WrongToken(t *testing.T) {
	rt, _ := bootRuntimeWithSecrets(t)
	ts, fp := fakeAgentServer(t, "correct")

	res := rt.ProbeNode(ts.URL, "wrong", false)
	if res.OK {
		t.Fatal("token 錯誤不應成功")
	}
	if res.Fingerprint != fp {
		t.Errorf("即使 token 錯,仍應回指紋供確認: got=%s want=%s", res.Fingerprint, fp)
	}
	if res.Err == "" {
		t.Error("應帶繁中錯誤訊息")
	}
}

func TestAddNode_PersistsAndRegisters(t *testing.T) {
	rt, kr := bootRuntimeWithSecrets(t)
	const token = "tok-1"
	ts, fp := fakeAgentServer(t, token)

	if err := rt.AddNode("vps1", ts.URL, token, fp, false); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// config 持久化:重新讀 config 應含該節點,且 token 不落 config。
	cfg, _, err := core.LoadAppConfig(rt.configPath)
	if err != nil {
		t.Fatalf("LoadAppConfig: %v", err)
	}
	found := false
	for _, n := range cfg.Nodes {
		if n.Name == "vps1" {
			found = true
			if n.TLSFingerprint != fp {
				t.Errorf("config 指紋不符: %s", n.TLSFingerprint)
			}
		}
	}
	if !found {
		t.Fatal("config 未持久化節點")
	}

	// token 進金鑰庫。
	if v, gerr := kr.Get("test", core.NodeTokenRef("vps1").Key); gerr != nil || v != token {
		t.Errorf("token 未入金鑰庫: v=%q err=%v", v, gerr)
	}

	// registry 註冊且上線(AddNode 內即時探測)。
	nodes := rt.ListNodes()
	var vps *NodeInfo
	for i := range nodes {
		if nodes[i].Name == "vps1" {
			vps = &nodes[i]
		}
	}
	if vps == nil {
		t.Fatal("ListNodes 未含 vps1")
	}
	if !vps.Online {
		t.Errorf("節點應上線: %+v", *vps)
	}
	if !vps.Removable {
		t.Error("遠端節點應可移除")
	}
}

func TestAddNode_DuplicateAndReserved(t *testing.T) {
	rt, _ := bootRuntimeWithSecrets(t)
	ts, fp := fakeAgentServer(t, "t")

	if err := rt.AddNode("vps1", ts.URL, "t", fp, false); err != nil {
		t.Fatalf("首次 AddNode: %v", err)
	}
	if err := rt.AddNode("vps1", ts.URL, "t", fp, false); err == nil {
		t.Error("重複名稱應被拒")
	}
	if err := rt.AddNode("local", ts.URL, "t", fp, false); err == nil {
		t.Error("保留名稱 local 應被拒")
	}
	if err := rt.AddNode("bad name!", ts.URL, "t", fp, false); err == nil {
		t.Error("非法名稱應被拒")
	}
}

func TestRemoveNode_Success(t *testing.T) {
	rt, kr := bootRuntimeWithSecrets(t)
	ts, fp := fakeAgentServer(t, "t")
	if err := rt.AddNode("vps1", ts.URL, "t", fp, false); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	if err := rt.RemoveNode("vps1"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	// config 已無、金鑰庫已清、registry 已反註冊。
	cfg, _, _ := core.LoadAppConfig(rt.configPath)
	for _, n := range cfg.Nodes {
		if n.Name == "vps1" {
			t.Error("config 仍殘留節點")
		}
	}
	if _, gerr := kr.Get("test", core.NodeTokenRef("vps1").Key); gerr == nil {
		t.Error("金鑰庫 token 未清除")
	}
	for _, n := range rt.ListNodes() {
		if n.Name == "vps1" {
			t.Error("registry 仍含節點")
		}
	}
}

func TestRemoveNode_BlockedByInstance(t *testing.T) {
	rt, _ := bootRuntimeWithSecrets(t)
	ts, fp := fakeAgentServer(t, "t")
	if err := rt.AddNode("vps1", ts.URL, "t", fp, false); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	// 在該節點掛一個實例記錄。
	if err := rt.store.UpsertInstance(core.InstanceRecord{
		UUID: "inst-1", TemplateID: "testgame", Node: "vps1",
		DesiredState: protocol.InstanceStateStopped, ObservedState: protocol.InstanceStateStopped,
	}); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}
	if err := rt.RemoveNode("vps1"); err == nil {
		t.Error("節點掛有實例時應拒絕移除")
	}
}

func TestRemoveNode_LocalRejected(t *testing.T) {
	rt, _ := bootRuntimeWithSecrets(t)
	if err := rt.RemoveNode("local"); err == nil {
		t.Error("local 不可移除")
	}
}
