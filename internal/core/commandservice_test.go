package core

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// ---- 假 RCON server(core 端整合測試用;獨立於 agent 測試包,故在此重寫最小實作)----

type coreFakeRcon struct {
	ln       net.Listener
	password string

	mu    sync.Mutex
	execs []string
}

func startCoreFakeRcon(t *testing.T, password string) *coreFakeRcon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &coreFakeRcon{ln: ln, password: password}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *coreFakeRcon) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *coreFakeRcon) waitExec(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, e := range f.execs {
			if e == want {
				f.mu.Unlock()
				return
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	got := append([]string(nil), f.execs...)
	f.mu.Unlock()
	t.Fatalf("RCON 未在 %s 內收到 %q,實收 %v", timeout, want, got)
}

func (f *coreFakeRcon) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *coreFakeRcon) handle(conn net.Conn) {
	defer conn.Close()
	for {
		id, typ, body, err := coreReadRcon(conn)
		if err != nil {
			return
		}
		switch typ {
		case 3: // AUTH
			respID := id
			if body != f.password {
				respID = -1
			}
			_ = coreWriteRcon(conn, id, 0, "")
			_ = coreWriteRcon(conn, respID, 2, "")
		case 2: // EXECCOMMAND
			f.mu.Lock()
			f.execs = append(f.execs, body)
			f.mu.Unlock()
			_ = coreWriteRcon(conn, id, 0, "ok: "+body)
		case 0: // 哨兵請求(RconAdapter 緊接 EXECCOMMAND 送出的空 RESPONSE_VALUE)→ echo 同 id 標誌回應完結。
			_ = coreWriteRcon(conn, id, 0, "")
		}
	}
}

func coreWriteRcon(w io.Writer, id, typ int32, body string) error {
	size := int32(4 + 4 + len(body) + 2)
	buf := make([]byte, 0, 4+size)
	var hdr [12]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(size))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(id))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(typ))
	buf = append(buf, hdr[:]...)
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	_, err := w.Write(buf)
	return err
}

func coreReadRcon(r io.Reader) (id, typ int32, body string, err error) {
	var size int32
	if err = binary.Read(r, binary.LittleEndian, &size); err != nil {
		return
	}
	if size < 10 || size > 4096 {
		err = fmt.Errorf("bad size %d", size)
		return
	}
	payload := make([]byte, size)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	id = int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ = int32(binary.LittleEndian.Uint32(payload[4:8]))
	body = string(payload[8 : size-2])
	return
}

// ---- harness ----

type cmdHarness struct {
	svc    *InstanceService
	cmdSvc *CommandService
	orch   *Orchestrator
	store  *Store
	events *EventLog
	eng    *TemplateEngine
	reg    *NodeRegistry
}

func newCmdHarness(t *testing.T, templateName, templateBody string) *cmdHarness {
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
	writeTemplateFile(t, dir, templateName, templateBody)
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
	cmdSvc := NewCommandService(store, eng, secrets, reg)
	orch := NewOrchestrator(OrchestratorConfig{
		Store: store, Events: events, Registry: reg,
		StopHook: cmdSvc.RunStopHook,
	})
	return &cmdHarness{
		svc: svc, cmdSvc: cmdSvc, orch: orch,
		store: store, events: events, eng: eng, reg: reg,
	}
}

func (h *cmdHarness) create(t *testing.T, uuid, templateID string, secrets map[string]string) InstanceRecord {
	t.Helper()
	h.svc.newUUID = func() string { return uuid }
	rec, err := h.svc.Create(context.Background(), CreateOptions{TemplateID: templateID, Secrets: secrets})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return rec
}

// rconTemplate 造一個 rcon 範本(host_port 綁到假 RCON 埠),含 hooks.stop/announce。
func rconTemplate(port int) string {
	return fmt.Sprintf(`
schema_version = 1
id = "cmdrcon"
name = "Command RCON Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/cmd:1.0"
[[ports]]
name = "rcon"
container = 25575
host_port = %d
bind_ip = "127.0.0.1"
protocol = "tcp"
[[secrets]]
key = "RCON_PASSWORD"
label = "RCON 密碼"
[[command_protocols]]
protocol_id = "mc-rcon"
kind = "rcon"
host_port_ref = "rcon"
password_ref = "RCON_PASSWORD"
[hooks]
stop = { protocol_id = "mc-rcon", command = "stop" }
announce = { protocol_id = "mc-rcon", command = "say {msg}" }
`, port)
}

// ---- 假 REST server(core 端整合測試用,驗證雙審修正 #6:hook args/message_key/username)----

type restCapture struct {
	mu       sync.Mutex
	path     string
	body     map[string]string
	authUser string
	authPass string
}

func (c *restCapture) snapshot() (path string, body map[string]string, user, pass string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.body, c.authUser, c.authPass
}

// startFakeRest 起一個 httptest REST server,擷取路徑/body/basic auth,供斷言 hook args 與
// username 是否正確送達(全程經 core CommandService → 真代理 → PalworldRestAdapter,無 mock)。
func startFakeRest(t *testing.T) (*httptest.Server, *restCapture) {
	t.Helper()
	cap := &restCapture{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		cap.mu.Lock()
		cap.path = r.URL.Path
		cap.body = body
		cap.authUser, cap.authPass = u, p
		cap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hs.Close)
	return hs, cap
}

// restPort 取 httptest server 綁定的實際埠(供寫入範本 host_port)。
func restPort(t *testing.T, hs *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(hs.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	return port
}

// restTemplate 造一個 rest 範本(host_port 綁到假 REST 埠),含 hooks.stop(shutdown,帶靜態
// waittime/message args,對齊 Palworld spike)與 hooks.announce(以 message_key 對映動態訊息)。
// auth=basic 且未宣告 username → core 應回退 "admin"。
func restTemplate(port int) string {
	return fmt.Sprintf(`
schema_version = 1
id = "cmdrest"
name = "Command REST Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/rest:1.0"
[[ports]]
name = "rest"
container = 8212
host_port = %d
bind_ip = "127.0.0.1"
protocol = "tcp"
[[secrets]]
key = "ADMIN_PASSWORD"
label = "Admin 密碼"
[[command_protocols]]
protocol_id = "test-rest"
kind = "rest"
host_port_ref = "rest"
password_ref = "ADMIN_PASSWORD"
auth = "basic"
actions = [
  { action_id = "shutdown", method = "POST", path = "/shutdown" },
  { action_id = "announce", method = "POST", path = "/announce" },
]
[hooks]
stop = { protocol_id = "test-rest", action_id = "shutdown", args = { waittime = "30", message = "Server is shutting down" } }
announce = { protocol_id = "test-rest", action_id = "announce", message_key = "message" }
`, port)
}

// restTemplateWithUsername 同 restTemplate,但 command_protocols 顯式宣告 username,
// 驗證 core 優先採用範本值而非硬編回退。
func restTemplateWithUsername(port int, username string) string {
	return fmt.Sprintf(`
schema_version = 1
id = "cmdrestuser"
name = "Command REST Username Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/rest:1.0"
[[ports]]
name = "rest"
container = 8212
host_port = %d
bind_ip = "127.0.0.1"
protocol = "tcp"
[[secrets]]
key = "ADMIN_PASSWORD"
label = "Admin 密碼"
[[command_protocols]]
protocol_id = "test-rest"
kind = "rest"
host_port_ref = "rest"
password_ref = "ADMIN_PASSWORD"
auth = "basic"
username = %q
actions = [
  { action_id = "shutdown", method = "POST", path = "/shutdown" },
]
`, port, username)
}

// ---- 純單元:協定選擇 ----

func TestActiveProtocol_SkipsLegacy(t *testing.T) {
	tmpl := &protocol.GameTemplate{
		ID: "x",
		CommandProtocols: []protocol.CommandProtocol{
			{ProtocolID: "legacy-rcon", Kind: "rcon", Legacy: true},
			{ProtocolID: "rest-main", Kind: "rest"},
		},
	}
	cp, err := activeProtocol(tmpl)
	if err != nil {
		t.Fatalf("activeProtocol: %v", err)
	}
	if cp.ProtocolID != "rest-main" {
		t.Fatalf("應選非 legacy 協定,得 %q", cp.ProtocolID)
	}
}

func TestActiveProtocol_AllLegacyOrEmpty(t *testing.T) {
	empty := &protocol.GameTemplate{ID: "e"}
	if _, err := activeProtocol(empty); !errors.Is(err, ErrCommandNotEnabled) {
		t.Fatalf("空協定應回 ErrCommandNotEnabled,得 %v", err)
	}
	allLegacy := &protocol.GameTemplate{
		ID:               "l",
		CommandProtocols: []protocol.CommandProtocol{{ProtocolID: "p", Kind: "rcon", Legacy: true}},
	}
	if _, err := activeProtocol(allLegacy); !errors.Is(err, ErrCommandNotEnabled) {
		t.Fatalf("全 legacy 應回 ErrCommandNotEnabled,得 %v", err)
	}
}

func TestProtocolByID_LegacyRejected(t *testing.T) {
	tmpl := &protocol.GameTemplate{
		ID:               "x",
		CommandProtocols: []protocol.CommandProtocol{{ProtocolID: "legacy", Kind: "rcon", Legacy: true}},
	}
	if _, err := protocolByID(tmpl, "legacy"); !errors.Is(err, ErrCommandNotEnabled) {
		t.Fatalf("legacy 協定不可被 hook 選中,得 %v", err)
	}
	if _, err := protocolByID(tmpl, "nope"); err == nil {
		t.Fatalf("不存在協定應回錯誤")
	}
}

func TestResolveCommandPort_Errors(t *testing.T) {
	tmpl := &protocol.GameTemplate{
		Ports: []protocol.PortSpec{
			{Name: "dyn", HostPort: 0, BindIP: "127.0.0.1"},
			{Name: "ok", HostPort: 25575, BindIP: "0.0.0.0"},
		},
	}
	if _, _, err := resolveCommandPort(tmpl, "dyn"); err == nil {
		t.Fatalf("動態埠應回錯誤")
	}
	if _, _, err := resolveCommandPort(tmpl, "missing"); err == nil {
		t.Fatalf("缺對應埠應回錯誤")
	}
	host, port, err := resolveCommandPort(tmpl, "ok")
	if err != nil {
		t.Fatalf("resolveCommandPort: %v", err)
	}
	if host != "127.0.0.1" || port != 25575 {
		t.Fatalf("wildcard bind_ip 應正規化為 loopback,得 %s:%d", host, port)
	}
}

// ---- 整合:Send / Announce 經真代理 + 假 RCON ----

func TestCommandService_Send_RconEcho(t *testing.T) {
	f := startCoreFakeRcon(t, "secret")
	h := newCmdHarness(t, "cmdrcon.toml", rconTemplate(f.port()))
	h.create(t, "u-send", "cmdrcon", map[string]string{"RCON_PASSWORD": "secret"})

	res, err := h.cmdSvc.Send(context.Background(), "u-send", protocol.GameCommand{Raw: "list"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !res.Success {
		t.Fatalf("期望 success,得 %+v", res)
	}
	f.waitExec(t, "list", time.Second)
}

func TestCommandService_Announce_Substitutes(t *testing.T) {
	f := startCoreFakeRcon(t, "secret")
	h := newCmdHarness(t, "cmdrcon.toml", rconTemplate(f.port()))
	h.create(t, "u-ann", "cmdrcon", map[string]string{"RCON_PASSWORD": "secret"})

	if _, err := h.cmdSvc.Announce(context.Background(), "u-ann", "restart soon"); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	f.waitExec(t, "say restart soon", time.Second)
}

func TestCommandService_NotEnabled(t *testing.T) {
	noProto := `
schema_version = 1
id = "noproto"
name = "No Protocol"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/np:1.0"
`
	h := newCmdHarness(t, "noproto.toml", noProto)
	h.create(t, "u-np", "noproto", nil)

	_, err := h.cmdSvc.Send(context.Background(), "u-np", protocol.GameCommand{Raw: "list"})
	if !errors.Is(err, ErrCommandNotEnabled) {
		t.Fatalf("無協定應回 ErrCommandNotEnabled,得 %v", err)
	}
}

func TestCommandService_LegacyOnlyNotEnabled(t *testing.T) {
	legacyOnly := `
schema_version = 1
id = "legonly"
name = "Legacy Only"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/lo:1.0"
[[secrets]]
key = "ADMIN_PASSWORD"
label = "pw"
[[command_protocols]]
protocol_id = "pal-rcon"
kind = "rcon"
legacy = true
host_port_ref = "rcon"
password_ref = "ADMIN_PASSWORD"
[[ports]]
name = "rcon"
container = 25575
host_port = 25599
bind_ip = "127.0.0.1"
protocol = "tcp"
`
	h := newCmdHarness(t, "legonly.toml", legacyOnly)
	h.create(t, "u-lo", "legonly", map[string]string{"ADMIN_PASSWORD": "x"})

	_, err := h.cmdSvc.Send(context.Background(), "u-lo", protocol.GameCommand{Raw: "list"})
	if !errors.Is(err, ErrCommandNotEnabled) {
		t.Fatalf("全 legacy 應回 ErrCommandNotEnabled,得 %v", err)
	}
}

// ---- 整合:Orchestrator.Stop 接 hooks.stop ----

func TestOrchestrator_StopHook_Delivered(t *testing.T) {
	f := startCoreFakeRcon(t, "secret")
	h := newCmdHarness(t, "cmdrcon.toml", rconTemplate(f.port()))
	h.create(t, "u-stop", "cmdrcon", map[string]string{"RCON_PASSWORD": "secret"})

	ctx := context.Background()
	if err := h.orch.Start(ctx, "u-stop"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.orch.Stop(ctx, "u-stop"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 範本 hooks.stop = "stop":假 RCON 應收到。
	f.waitExec(t, "stop", time.Second)

	rec, _ := h.store.GetInstance("u-stop")
	if rec.ObservedState != protocol.InstanceStateStopped {
		t.Fatalf("Stop 後應為 Stopped,得 %s", rec.ObservedState)
	}
}

func TestOrchestrator_StopHook_FailureDoesNotBlock(t *testing.T) {
	// 取一個空閒埠後關閉,使 hooks.stop 的 RCON 連線被拒(hook 失敗)。
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	h := newCmdHarness(t, "cmdrcon.toml", rconTemplate(deadPort))
	h.create(t, "u-stopfail", "cmdrcon", map[string]string{"RCON_PASSWORD": "secret"})

	ctx := context.Background()
	if err := h.orch.Start(ctx, "u-stopfail"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// hook 失敗不應阻擋停止。
	if err := h.orch.Stop(ctx, "u-stopfail"); err != nil {
		t.Fatalf("hook 失敗不應使 Stop 失敗: %v", err)
	}
	rec, _ := h.store.GetInstance("u-stopfail")
	if rec.ObservedState != protocol.InstanceStateStopped {
		t.Fatalf("Stop 後應為 Stopped,得 %s", rec.ObservedState)
	}
	if n := len(queryEvents(t, h.events, EventHookFailed)); n == 0 {
		t.Fatalf("hook 失敗應記 HOOK_FAILED 事件")
	}
}

// ---- 純單元:buildHookCommand(雙審修正 #6:hook args + message_key)----

func TestBuildHookCommand_Rest_StaticArgsAndDefaultMessageKey(t *testing.T) {
	cp := protocol.CommandProtocol{ProtocolID: "p", Kind: "rest"}
	hook := &protocol.Hook{ActionID: "shutdown", Args: map[string]string{"waittime": "30"}}

	cmd := buildHookCommand(cp, hook, "bye")
	if cmd.ActionID != "shutdown" {
		t.Fatalf("ActionID = %q, 期望 shutdown", cmd.ActionID)
	}
	if cmd.Args["waittime"] != "30" || cmd.Args["message"] != "bye" {
		t.Fatalf("Args = %+v, 期望 waittime=30 且未設 message_key 時預設欄位名 message=bye", cmd.Args)
	}
}

func TestBuildHookCommand_Rest_CustomMessageKey(t *testing.T) {
	cp := protocol.CommandProtocol{ProtocolID: "p", Kind: "rest"}
	hook := &protocol.Hook{ActionID: "announce", MessageKey: "msg"}

	cmd := buildHookCommand(cp, hook, "hi")
	if cmd.Args["msg"] != "hi" {
		t.Fatalf("Args = %+v, 期望以自訂 message_key=msg 寫入 hi", cmd.Args)
	}
	if _, ok := cmd.Args["message"]; ok {
		t.Fatalf("不應同時寫入預設欄位名 message,得 %+v", cmd.Args)
	}
}

func TestBuildHookCommand_Rest_NoArgsNoMsg(t *testing.T) {
	cp := protocol.CommandProtocol{ProtocolID: "p", Kind: "rest"}
	hook := &protocol.Hook{ActionID: "save"}

	cmd := buildHookCommand(cp, hook, "")
	if cmd.Args != nil {
		t.Fatalf("無 args 且無訊息時 Args 應為 nil,得 %+v", cmd.Args)
	}
}

// ---- 純單元:buildTarget 的 rest username(雙審修正 #6)----

func TestBuildTarget_Rest_UsernameFallbackToAdmin(t *testing.T) {
	svc := &CommandService{}
	tmpl := &protocol.GameTemplate{
		Ports: []protocol.PortSpec{{Name: "rest", HostPort: 8212, BindIP: "127.0.0.1"}},
	}
	cp := protocol.CommandProtocol{ProtocolID: "p", Kind: "rest", Auth: "basic", HostPortRef: "rest"}

	target, err := svc.buildTarget(InstanceRecord{UUID: "u"}, tmpl, cp)
	if err != nil {
		t.Fatalf("buildTarget: %v", err)
	}
	if target.Username != "admin" {
		t.Fatalf("Username = %q, 未宣告 username 時期望回退 admin", target.Username)
	}
}

func TestBuildTarget_Rest_UsernameFromProtocol(t *testing.T) {
	svc := &CommandService{}
	tmpl := &protocol.GameTemplate{
		Ports: []protocol.PortSpec{{Name: "rest", HostPort: 8212, BindIP: "127.0.0.1"}},
	}
	cp := protocol.CommandProtocol{ProtocolID: "p", Kind: "rest", Auth: "basic", HostPortRef: "rest", Username: "customadmin"}

	target, err := svc.buildTarget(InstanceRecord{UUID: "u"}, tmpl, cp)
	if err != nil {
		t.Fatalf("buildTarget: %v", err)
	}
	if target.Username != "customadmin" {
		t.Fatalf("Username = %q, 期望取自範本 customadmin", target.Username)
	}
}

// ---- 整合:CommandService 經真代理送 REST(雙審修正 #6)----

func TestCommandService_StopHook_Rest_SendsStaticArgs(t *testing.T) {
	hs, cap := startFakeRest(t)
	h := newCmdHarness(t, "cmdrest.toml", restTemplate(restPort(t, hs)))
	h.create(t, "u-reststop", "cmdrest", map[string]string{"ADMIN_PASSWORD": "pw"})

	if err := h.cmdSvc.RunStopHook(context.Background(), "u-reststop"); err != nil {
		t.Fatalf("RunStopHook: %v", err)
	}

	path, body, user, _ := cap.snapshot()
	if path != "/shutdown" {
		t.Fatalf("路徑 = %q, 期望 /shutdown", path)
	}
	if body["waittime"] != "30" || body["message"] != "Server is shutting down" {
		t.Fatalf("shutdown body = %+v, 期望靜態 waittime=30 message=Server is shutting down", body)
	}
	if user != "admin" {
		t.Fatalf("basic auth 帳號 = %q, 未宣告 username 時期望回退 admin", user)
	}
}

func TestCommandService_Announce_Rest_UsesMessageKey(t *testing.T) {
	hs, cap := startFakeRest(t)
	h := newCmdHarness(t, "cmdrest.toml", restTemplate(restPort(t, hs)))
	h.create(t, "u-restann", "cmdrest", map[string]string{"ADMIN_PASSWORD": "pw"})

	if _, err := h.cmdSvc.Announce(context.Background(), "u-restann", "restart soon"); err != nil {
		t.Fatalf("Announce: %v", err)
	}

	path, body, _, _ := cap.snapshot()
	if path != "/announce" {
		t.Fatalf("路徑 = %q, 期望 /announce", path)
	}
	if body["message"] != "restart soon" {
		t.Fatalf("announce body = %+v, 期望以 message_key 寫入 message=restart soon", body)
	}
}

func TestCommandService_Send_Rest_UsesExplicitUsername(t *testing.T) {
	hs, cap := startFakeRest(t)
	h := newCmdHarness(t, "cmdrestuser.toml", restTemplateWithUsername(restPort(t, hs), "customadmin"))
	h.create(t, "u-restuser", "cmdrestuser", map[string]string{"ADMIN_PASSWORD": "pw"})

	if _, err := h.cmdSvc.Send(context.Background(), "u-restuser", protocol.GameCommand{ActionID: "shutdown"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	_, _, user, _ := cap.snapshot()
	if user != "customadmin" {
		t.Fatalf("basic auth 帳號 = %q, 期望取自範本 username=customadmin", user)
	}
}
