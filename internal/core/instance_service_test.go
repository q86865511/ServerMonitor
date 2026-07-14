package core

import (
	"context"
	"errors"
	"sync"
	"testing"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

const svcTestTemplate = `
schema_version = 1
id = "svc"
name = "Svc Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/svc:1.0"
image_digest = "sha256:abc"
[[variants]]
id = "paper"
[variants.env]
TYPE = "PAPER"
[[ports]]
name = "game"
container = 25565
host_port = 25565
bind_ip = "0.0.0.0"
protocol = "tcp"
required = true
[[ports]]
name = "rcon"
container = 25575
host_port = 25575
bind_ip = "127.0.0.1"
protocol = "tcp"
[[params]]
key = "MEMORY"
type = "string"
default = "2G"
[[params]]
key = "EULA"
type = "bool"
required = true
[[secrets]]
key = "RCON_PASSWORD"
[[command_protocols]]
protocol_id = "svc-rcon"
kind = "rcon"
host_port_ref = "rcon"
password_ref = "RCON_PASSWORD"
[health]
kind = "tcp"
port_ref = "game"
`

// captureBackend 包 MockBackend:記錄最後一次 Create 的 spec、可注入 Create 錯誤,
// 供斷言 env/label 注入與 failure injection。
type captureBackend struct {
	*agent.MockBackend
	mu          sync.Mutex
	lastSpec    protocol.InstanceSpec
	createErr   error
	createCount int
}

func (b *captureBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	b.mu.Lock()
	b.lastSpec = spec
	b.createCount++
	ce := b.createErr
	b.mu.Unlock()
	if ce != nil {
		return "", ce
	}
	return b.MockBackend.Create(ctx, spec)
}

func (b *captureBackend) spec() protocol.InstanceSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSpec
}

type svcHarness struct {
	svc     *InstanceService
	backend *captureBackend
	store   *Store
	events  *EventLog
	keyring *memoryKeyring
	reg     *NodeRegistry
	eng     *TemplateEngine
	dir     string // 範本目錄
}

func newSvcHarness(t *testing.T) *svcHarness {
	t.Helper()
	backend := &captureBackend{MockBackend: agent.NewMockBackend()}
	hs := newAgentServerWithBackend(t, backend)

	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	kr := newMemoryKeyring()
	secrets := NewSecretStoreWithKeyring("test", kr)
	reg := NewNodeRegistry(events)
	reg.Register("local", NewNodeClient(hs.URL, agentTestToken, nil))
	eng := NewTemplateEngine(nil, events)
	dir := t.TempDir()
	writeTemplateFile(t, dir, "svc.toml", svcTestTemplate)
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
	return &svcHarness{svc: svc, backend: backend, store: store, events: events, keyring: kr, reg: reg, eng: eng, dir: dir}
}

func (h *svcHarness) validCreate() CreateOptions {
	return CreateOptions{
		TemplateID: "svc",
		Variant:    "paper",
		Params:     map[string]string{"EULA": "true"},
		Secrets:    map[string]string{"RCON_PASSWORD": "s3cr3t"},
	}
}

func TestInstanceService_CreateSuccess(t *testing.T) {
	h := newSvcHarness(t)
	rec, err := h.svc.Create(context.Background(), h.validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// DB 完成紀錄:狀態 Created、runtime ID 已記。
	got, err := h.store.GetInstance(rec.UUID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.ObservedState != protocol.InstanceStateCreated {
		t.Errorf("ObservedState = %q, 期望 Created", got.ObservedState)
	}
	if got.RuntimeID == "" || got.TemplateID != "svc" {
		t.Errorf("紀錄 = %+v", got)
	}

	// 埠預留(game + rcon = 2)。
	ports, _ := h.store.ListPortReservations()
	if len(ports) != 2 {
		t.Fatalf("埠預留數 = %d, 期望 2", len(ports))
	}

	// 機密入金鑰庫(UUID 命名空間),DB 不含明文。
	val, err := h.keyring.Get("test", instanceSecretKey(rec.UUID, "RCON_PASSWORD"))
	if err != nil || val != "s3cr3t" {
		t.Errorf("金鑰庫機密 = %q, err=%v", val, err)
	}
	if contains := string(got.ParamsJSON); contains == "" || contains == "{}" {
		t.Errorf("params_json 應含非機密參數, 得 %s", contains)
	}
	if got.ParamsJSON != nil && containsSecret(string(got.ParamsJSON)) {
		t.Errorf("params_json 不應含機密值: %s", got.ParamsJSON)
	}

	// spec 注入:env(變體 + 參數 + 機密明文)與 gsm.* 標籤、映像鎖。
	spec := h.backend.spec()
	if spec.Env["EULA"] != "true" || spec.Env["MEMORY"] != "2G" || spec.Env["TYPE"] != "PAPER" {
		t.Errorf("env = %+v", spec.Env)
	}
	if spec.Env["RCON_PASSWORD"] != "s3cr3t" {
		t.Errorf("機密應以明文注入 runtime env(R12 例外),得 %q", spec.Env["RCON_PASSWORD"])
	}
	if spec.Labels[labelUUID] != rec.UUID || spec.Labels[labelManagedBy] != managedByValue ||
		spec.Labels[labelNode] != "local" || spec.Labels[labelSchema] != "1" {
		t.Errorf("labels = %+v", spec.Labels)
	}
	if spec.Image != "example/svc:1.0@sha256:abc" {
		t.Errorf("Image = %q, 期望鎖 digest", spec.Image)
	}

	// journal 已清、INSTANCE_CREATED 已記。
	if entries, _ := h.svc.journal.List(); len(entries) != 0 {
		t.Errorf("journal 應已清空, 得 %+v", entries)
	}
	if evs := queryEvents(t, h.events, protocol.EventInstanceCreated); len(evs) != 1 {
		t.Errorf("INSTANCE_CREATED 事件數 = %d, 期望 1", len(evs))
	}
}

func TestInstanceService_MissingParams(t *testing.T) {
	h := newSvcHarness(t)
	opts := h.validCreate()
	opts.Params = map[string]string{} // 缺 EULA

	_, err := h.svc.Create(context.Background(), opts)
	if !errors.Is(err, ErrMissingParams) {
		t.Fatalf("期望 ErrMissingParams,得 %v", err)
	}
	var pe *ParamError
	if !errors.As(err, &pe) || len(pe.Missing) != 1 || pe.Missing[0] != "EULA" {
		t.Fatalf("應指出缺項 EULA,得 %v", err)
	}
	h.assertNoSideEffects(t)
}

func TestInstanceService_EULANotAccepted(t *testing.T) {
	h := newSvcHarness(t)
	opts := h.validCreate()
	opts.Params = map[string]string{"EULA": "false"} // 明確不接受

	_, err := h.svc.Create(context.Background(), opts)
	if !errors.Is(err, ErrEULANotAccepted) {
		t.Fatalf("期望 ErrEULANotAccepted,得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestInstanceService_MissingRequiredSecret:E 修正——svc 範本的 svc-rcon 協定(非 legacy)
// 引用 RCON_PASSWORD,未提供時建立前應被擋、指明鍵名、不留副作用。
func TestInstanceService_MissingRequiredSecret(t *testing.T) {
	h := newSvcHarness(t)
	opts := h.validCreate()
	opts.Secrets = map[string]string{} // 缺 RCON_PASSWORD

	_, err := h.svc.Create(context.Background(), opts)
	if !errors.Is(err, ErrMissingSecrets) {
		t.Fatalf("期望 ErrMissingSecrets,得 %v", err)
	}
	var pe *ParamError
	if !errors.As(err, &pe) || len(pe.MissingSecrets) != 1 || pe.MissingSecrets[0] != "RCON_PASSWORD" {
		t.Fatalf("應指出缺項 RCON_PASSWORD,得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// legacySecretTemplate 有兩個指令協定:非 legacy 的 rest(引用 REST_PASSWORD)與
// legacy=true 的 rcon(引用 RCON_PASSWORD)。用以驗證「legacy 協定引用的機密不計入必填」。
const legacySecretTemplate = `
schema_version = 1
id = "legacysec"
name = "LegacySecret Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/legacysec:1.0"
[[ports]]
name = "game"
container = 25565
host_port = 25568
bind_ip = "0.0.0.0"
protocol = "tcp"
required = true
[[ports]]
name = "rcon"
container = 25575
host_port = 25577
bind_ip = "127.0.0.1"
protocol = "tcp"
[[secrets]]
key = "REST_PASSWORD"
[[secrets]]
key = "RCON_PASSWORD"
[[command_protocols]]
protocol_id = "legacysec-rest"
kind = "rest"
password_ref = "REST_PASSWORD"
auth = "basic"
[[command_protocols]]
protocol_id = "legacysec-rcon"
kind = "rcon"
legacy = true
host_port_ref = "rcon"
password_ref = "RCON_PASSWORD"
`

// TestInstanceService_LegacyProtocolSecretNotRequired:legacy=true 的協定首版不啟用,
// 其引用的機密(RCON_PASSWORD)缺失不應阻擋建立;非 legacy 的 REST_PASSWORD 仍須提供。
func TestInstanceService_LegacyProtocolSecretNotRequired(t *testing.T) {
	h := newSvcHarness(t)
	writeTemplateFile(t, h.dir, "legacysec.toml", legacySecretTemplate)
	if _, err := h.eng.LoadDir(h.dir); err != nil {
		t.Fatalf("載入 legacysec 範本: %v", err)
	}

	rec, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "legacysec",
		Secrets:    map[string]string{"REST_PASSWORD": "x"}, // 不給 legacy RCON 的 RCON_PASSWORD
	})
	if err != nil {
		t.Fatalf("Create: 不應因 legacy 協定缺機密而擋,得 %v", err)
	}
	if rec.UUID == "" {
		t.Errorf("應成功建立, rec = %+v", rec)
	}
}

// reqDefaultTemplate 有一個 required=true 且 default=true 的 bool 參數,用以驗證
// I 修正:必填參數不得因範本 default 而被悄悄滿足。
const reqDefaultTemplate = `
schema_version = 1
id = "reqdefault"
name = "ReqDefault Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/reqdefault:1.0"
[[ports]]
name = "game"
container = 25565
host_port = 25569
bind_ip = "0.0.0.0"
protocol = "tcp"
required = true
[[params]]
key = "EULA"
type = "bool"
required = true
default = true
`

// TestInstanceService_RequiredParamNotMaskedByDefault:I 修正——EULA 為必填 bool 且範本
// default=true;使用者未提供時仍應被擋(不得靠 default 悄悄通過),明確提供 true 才通過。
func TestInstanceService_RequiredParamNotMaskedByDefault(t *testing.T) {
	h := newSvcHarness(t)
	writeTemplateFile(t, h.dir, "reqdefault.toml", reqDefaultTemplate)
	if _, err := h.eng.LoadDir(h.dir); err != nil {
		t.Fatalf("載入 reqdefault 範本: %v", err)
	}

	_, err := h.svc.Create(context.Background(), CreateOptions{TemplateID: "reqdefault", Params: map[string]string{}})
	if !errors.Is(err, ErrMissingParams) {
		t.Fatalf("期望 ErrMissingParams(不應被 default=true 架空),得 %v", err)
	}
	var pe *ParamError
	if !errors.As(err, &pe) || len(pe.Missing) != 1 || pe.Missing[0] != "EULA" {
		t.Fatalf("應指出缺項 EULA,得 %v", err)
	}
	h.assertNoSideEffects(t)

	rec, err := h.svc.Create(context.Background(), CreateOptions{TemplateID: "reqdefault", Params: map[string]string{"EULA": "true"}})
	if err != nil {
		t.Fatalf("Create(明確提供 EULA=true): %v", err)
	}
	if rec.UUID == "" {
		t.Errorf("應成功建立, rec = %+v", rec)
	}
}

func TestInstanceService_UnknownTemplate(t *testing.T) {
	h := newSvcHarness(t)
	_, err := h.svc.Create(context.Background(), CreateOptions{TemplateID: "nope"})
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("期望 ErrTemplateNotFound,得 %v", err)
	}
}

// assertNoSideEffects 斷言未留下任何建立痕跡(用於建立前阻擋的案例)。
func (h *svcHarness) assertNoSideEffects(t *testing.T) {
	t.Helper()
	if insts, _ := h.store.ListInstances(); len(insts) != 0 {
		t.Errorf("不應有 DB 實例紀錄, 得 %+v", insts)
	}
	if ports, _ := h.store.ListPortReservations(); len(ports) != 0 {
		t.Errorf("不應有埠預留, 得 %+v", ports)
	}
	if h.backend.createCount != 0 {
		t.Errorf("不應呼叫代理 Create, 得 %d 次", h.backend.createCount)
	}
	if entries, _ := h.svc.journal.List(); len(entries) != 0 {
		t.Errorf("不應有 journal 紀錄, 得 %+v", entries)
	}
}

func containsSecret(s string) bool {
	return len(s) > 0 && (indexOf(s, "s3cr3t") >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// svc2Template 與 svc 相同,但 game 綁 127.0.0.1:25565(與 svc 的 0.0.0.0:25565 wildcard 重疊)、
// rcon 改 25576(不與 svc 的 25575 完全相同鍵衝突),用以隔離「wildcard 重疊」而非「完全相同鍵」。
const svc2Template = `
schema_version = 1
id = "svc2"
name = "Svc2 Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/svc2:1.0"
image_digest = "sha256:def"
[[variants]]
id = "paper"
[variants.env]
TYPE = "PAPER"
[[ports]]
name = "game"
container = 25565
host_port = 25565
bind_ip = "127.0.0.1"
protocol = "tcp"
required = true
[[ports]]
name = "rcon"
container = 25575
host_port = 25576
bind_ip = "127.0.0.1"
protocol = "tcp"
[[params]]
key = "EULA"
type = "bool"
required = true
[[secrets]]
key = "RCON_PASSWORD"
[[command_protocols]]
protocol_id = "svc2-rcon"
kind = "rcon"
host_port_ref = "rcon"
password_ref = "RCON_PASSWORD"
[health]
kind = "tcp"
port_ref = "game"
`

// TestInstanceService_CreateRollback:於「已建容器、寫 DB 前」注入失敗,驗證分階段原子回滾
// (R2)——不留 DB 完成紀錄、釋放埠、清 journal、移除孤兒容器、刪機密、記 INSTANCE_CREATE_FAILED。
func TestInstanceService_CreateRollback(t *testing.T) {
	h := newSvcHarness(t)
	h.svc.newUUID = func() string { return "rollback-uuid" }
	h.svc.hookAfterAgentCreate = func(uuid string) error {
		return errors.New("injected failure after agent create")
	}

	_, err := h.svc.Create(context.Background(), h.validCreate())
	if err == nil {
		t.Fatal("期望 Create 因注入而失敗,得 nil")
	}

	if insts, _ := h.store.ListInstances(); len(insts) != 0 {
		t.Errorf("回滾後不應有 DB 實例紀錄, 得 %+v", insts)
	}
	if ports, _ := h.store.ListPortReservations(); len(ports) != 0 {
		t.Errorf("回滾後不應有埠預留, 得 %+v", ports)
	}
	if entries, _ := h.svc.journal.List(); len(entries) != 0 {
		t.Errorf("回滾後 journal 應清空, 得 %+v", entries)
	}
	// 容器確曾建立(證明是「建後回滾」),且回滾後代理端無殘留孤兒。
	if h.backend.createCount != 1 {
		t.Errorf("代理 Create 應被呼叫 1 次, 得 %d", h.backend.createCount)
	}
	refs, lerr := h.backend.List(context.Background())
	if lerr != nil {
		t.Fatalf("backend.List: %v", lerr)
	}
	if len(refs) != 0 {
		t.Errorf("回滾後代理不應殘留容器, 得 %d 個", len(refs))
	}
	// 機密已刪(回滾)。
	if _, gerr := h.keyring.Get("test", instanceSecretKey("rollback-uuid", "RCON_PASSWORD")); gerr == nil {
		t.Error("回滾後機密應已從金鑰庫刪除")
	}
	// INSTANCE_CREATE_FAILED 已記。
	if evs := queryEvents(t, h.events, protocol.EventInstanceCreateFailed); len(evs) != 1 {
		t.Errorf("INSTANCE_CREATE_FAILED 事件數 = %d, 期望 1", len(evs))
	}
}

// TestInstanceService_WildcardPortConflict:實例1 綁 0.0.0.0:25565,實例2 綁 127.0.0.1:25565,
// 應依 wildcard 重疊規則(0.0.0.0 覆蓋具體 IP)在建立前被擋(R2),且實例1 不受影響、實例2 無殘留。
func TestInstanceService_WildcardPortConflict(t *testing.T) {
	h := newSvcHarness(t)
	if _, err := h.svc.Create(context.Background(), h.validCreate()); err != nil {
		t.Fatalf("實例1 Create: %v", err)
	}

	writeTemplateFile(t, h.dir, "svc2.toml", svc2Template)
	if _, err := h.eng.LoadDir(h.dir); err != nil {
		t.Fatalf("載入 svc2 範本: %v", err)
	}

	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "svc2",
		Variant:    "paper",
		Params:     map[string]string{"EULA": "true"},
		Secrets:    map[string]string{"RCON_PASSWORD": "x"},
	})
	if !errors.Is(err, ErrPortConflict) {
		t.Fatalf("期望 ErrPortConflict(wildcard 0.0.0.0 vs 127.0.0.1),得 %v", err)
	}

	// 實例1 不受影響(仍在、2 個埠預留);實例2 無殘留、未呼叫代理 Create。
	if insts, _ := h.store.ListInstances(); len(insts) != 1 {
		t.Errorf("應僅實例1 存在, 得 %d 筆", len(insts))
	}
	if ports, _ := h.store.ListPortReservations(); len(ports) != 2 {
		t.Errorf("應僅實例1 的 2 個埠預留, 得 %d", len(ports))
	}
	if h.backend.createCount != 1 {
		t.Errorf("實例2 應在埠階段被擋(不呼叫代理 Create), createCount = %d", h.backend.createCount)
	}
}
