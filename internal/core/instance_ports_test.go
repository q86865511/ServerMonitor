package core

import (
	"context"
	"errors"
	"net"
	"testing"

	"servermonitor/internal/protocol"
)

// dynPortTemplate 宣告兩個 host_port=0 的埠(範本作者要求「由工具挑號」),
// container 埠取高號段,使動態分配的起點不撞常見服務。
const dynPortTemplate = `
schema_version = 1
id = "dynport"
name = "Dyn Port"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/dyn:1.0"
[[ports]]
name = "game"
container = 41565
host_port = 0
bind_ip = "127.0.0.1"
protocol = "tcp"
required = true
[[ports]]
name = "rcon"
container = 41575
host_port = 0
bind_ip = "127.0.0.1"
protocol = "tcp"
[[secrets]]
key = "RCON_PASSWORD"
[[command_protocols]]
protocol_id = "dyn-rcon"
kind = "rcon"
host_port_ref = "rcon"
password_ref = "RCON_PASSWORD"
[health]
kind = "tcp"
port_ref = "game"
`

// loadExtraTemplate 於既有範本目錄補一份範本並重載(LoadDir 為合併語意)。
func (h *svcHarness) loadExtraTemplate(t *testing.T, filename, content string) {
	t.Helper()
	writeTemplateFile(t, h.dir, filename, content)
	if _, err := h.eng.LoadDir(h.dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
}

// portsByName 把預留列轉成 name→預留,供斷言。
func portsByName(ports []PortReservation) map[string]PortReservation {
	out := make(map[string]PortReservation, len(ports))
	for _, p := range ports {
		out[p.Name] = p
	}
	return out
}

// bindingsByName 把傳給代理的埠綁定轉成 name→綁定,供斷言。
func bindingsByName(bs []protocol.PortBinding) map[string]protocol.PortBinding {
	out := make(map[string]protocol.PortBinding, len(bs))
	for _, b := range bs {
		out[b.Name] = b
	}
	return out
}

// (1) 兩台同範本伺服器:第二台以埠覆寫指定另一組埠可成功建立,兩者預留並存。
// 對照組先驗「不覆寫必衝突」——這正是本改動前開箱無法建立第二台任何伺服器的原因。
func TestCreate_PortOverride_AllowsSecondInstance(t *testing.T) {
	h := newSvcHarness(t)
	ctx := context.Background()

	first, err := h.svc.Create(ctx, h.validCreate())
	if err != nil {
		t.Fatalf("建立第一台: %v", err)
	}

	// 對照組:沿用範本埠 → 維持 PortConflictError(不靜默改號)。
	if _, cerr := h.svc.Create(ctx, h.validCreate()); !errors.Is(cerr, ErrPortConflict) {
		t.Fatalf("未覆寫應回 ErrPortConflict,得 %v", cerr)
	}

	opts := h.validCreate()
	opts.PortOverrides = map[string]int{"game": 25665, "rcon": 25675}
	second, err := h.svc.Create(ctx, opts)
	if err != nil {
		t.Fatalf("以埠覆寫建立第二台: %v", err)
	}

	firstPorts, err := h.store.ListPortReservationsForInstance(first.UUID)
	if err != nil {
		t.Fatalf("查第一台預留: %v", err)
	}
	secondPorts, err := h.store.ListPortReservationsForInstance(second.UUID)
	if err != nil {
		t.Fatalf("查第二台預留: %v", err)
	}
	if len(firstPorts) != 2 || len(secondPorts) != 2 {
		t.Fatalf("兩台的預留數 = %d / %d, 期望各 2", len(firstPorts), len(secondPorts))
	}
	fp, sp := portsByName(firstPorts), portsByName(secondPorts)
	if fp["game"].HostPort != 25565 || fp["rcon"].HostPort != 25575 {
		t.Errorf("第一台預留應維持範本值, 得 game=%d rcon=%d", fp["game"].HostPort, fp["rcon"].HostPort)
	}
	if sp["game"].HostPort != 25665 || sp["rcon"].HostPort != 25675 {
		t.Errorf("第二台預留應為覆寫值, 得 game=%d rcon=%d", sp["game"].HostPort, sp["rcon"].HostPort)
	}
	// 兩者並存(全域清單 4 筆)。
	all, _ := h.store.ListPortReservations()
	if len(all) != 4 {
		t.Errorf("全域預留數 = %d, 期望 4(兩台各 2)", len(all))
	}
	// 傳給代理的綁定亦為覆寫後的埠。
	nb := bindingsByName(h.backend.spec().Ports)
	if nb["game"].HostPort != 25665 || nb["rcon"].HostPort != 25675 {
		t.Errorf("代理收到的綁定 = game:%d rcon:%d, 期望覆寫值", nb["game"].HostPort, nb["rcon"].HostPort)
	}
}

// 覆寫後仍與既有實例衝突 → 維持 PortConflictError,不靜默改號。
func TestCreate_PortOverride_ConflictStillReported(t *testing.T) {
	h := newSvcHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Create(ctx, h.validCreate()); err != nil {
		t.Fatalf("建立第一台: %v", err)
	}
	opts := h.validCreate()
	opts.PortOverrides = map[string]int{"game": 25565, "rcon": 25675} // game 撞第一台
	_, err := h.svc.Create(ctx, opts)
	var pc *PortConflictError
	if !errors.As(err, &pc) || pc.HostPort != 25565 {
		t.Fatalf("覆寫撞既有埠應回 PortConflictError(25565),得 %v", err)
	}
}

// 埠覆寫的前置驗證:埠名須存在於範本、值須落在 0..65535(無副作用,建立前阻擋)。
func TestCreate_PortOverride_Validation(t *testing.T) {
	h := newSvcHarness(t)
	ctx := context.Background()

	opts := h.validCreate()
	opts.PortOverrides = map[string]int{"nope": 1234}
	if _, err := h.svc.Create(ctx, opts); err == nil {
		t.Fatalf("覆寫未宣告的埠名應報錯")
	}
	opts.PortOverrides = map[string]int{"game": 70000}
	if _, err := h.svc.Create(ctx, opts); err == nil {
		t.Fatalf("覆寫值超出範圍應報錯")
	}
	if all, _ := h.store.ListPortReservations(); len(all) != 0 {
		t.Errorf("驗證失敗不應留下埠預留, 得 %d 筆", len(all))
	}
}

// (2) host_port=0 由核心分配具體埠:DB 預留為 >0,且傳給代理的 PortBinding 亦為具體埠。
// portFreeHook 讓 41565 佯裝被非本工具的程式占用,驗證挑號會跳過並往上找。
func TestCreate_DynamicPortAllocatedByCore(t *testing.T) {
	h := newSvcHarness(t)
	h.loadExtraTemplate(t, "dynport.toml", dynPortTemplate)
	h.svc.portFreeHook = func(_, _ string, port int) bool { return port != 41565 }

	rec, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "dynport",
		Secrets:    map[string]string{"RCON_PASSWORD": "s3cr3t"},
	})
	if err != nil {
		t.Fatalf("建立動態埠實例: %v", err)
	}

	ports, err := h.store.ListPortReservationsForInstance(rec.UUID)
	if err != nil {
		t.Fatalf("查預留: %v", err)
	}
	if len(ports) != 2 {
		t.Fatalf("動態埠應留下具體預留 2 筆, 得 %d", len(ports))
	}
	byName := portsByName(ports)
	if byName["game"].HostPort != 41566 {
		t.Errorf("game 應跳過被占用的 41565 挑到 41566, 得 %d", byName["game"].HostPort)
	}
	if byName["rcon"].HostPort != 41575 {
		t.Errorf("rcon 應自 container 埠起挑到 41575, 得 %d", byName["rcon"].HostPort)
	}

	// 代理收到的是具體埠,不是 0(否則後端會自行 OS 分配,核心無從得知)。
	nb := bindingsByName(h.backend.spec().Ports)
	if nb["game"].HostPort != 41566 || nb["rcon"].HostPort != 41575 {
		t.Errorf("代理收到的綁定 = game:%d rcon:%d, 期望具體埠", nb["game"].HostPort, nb["rcon"].HostPort)
	}
}

// 動態分配須避開 DB 既有預留:第二台同範本實例應挑到不同號碼,兩者並存。
func TestCreate_DynamicPort_AvoidsExistingReservations(t *testing.T) {
	h := newSvcHarness(t)
	h.loadExtraTemplate(t, "dynport.toml", dynPortTemplate)
	h.svc.portFreeHook = func(string, string, int) bool { return true }
	ctx := context.Background()
	opts := CreateOptions{TemplateID: "dynport", Secrets: map[string]string{"RCON_PASSWORD": "s3cr3t"}}

	a, err := h.svc.Create(ctx, opts)
	if err != nil {
		t.Fatalf("建立第一台: %v", err)
	}
	b, err := h.svc.Create(ctx, opts)
	if err != nil {
		t.Fatalf("建立第二台: %v", err)
	}
	pa := portsByName(mustPorts(t, h, a.UUID))
	pb := portsByName(mustPorts(t, h, b.UUID))
	if pa["game"].HostPort != 41565 || pb["game"].HostPort != 41566 {
		t.Errorf("兩台 game 埠 = %d / %d, 期望 41565 / 41566", pa["game"].HostPort, pb["game"].HostPort)
	}
	if pa["rcon"].HostPort == pb["rcon"].HostPort {
		t.Errorf("兩台 rcon 埠不應相同, 皆為 %d", pa["rcon"].HostPort)
	}
}

func mustPorts(t *testing.T, h *svcHarness, uuid string) []PortReservation {
	t.Helper()
	ports, err := h.store.ListPortReservationsForInstance(uuid)
	if err != nil {
		t.Fatalf("查 %s 的預留: %v", uuid, err)
	}
	return ports
}

// (3a) 埠解析取該實例的實際預留;查無預留(舊實例)才退回範本宣告值。
func TestResolveInstancePort_PrefersInstanceReservation(t *testing.T) {
	store, _ := newTempStore(t)
	tmpl := &protocol.GameTemplate{
		ID: "x",
		Ports: []protocol.PortSpec{
			{Name: "rcon", Container: 25575, HostPort: 25575, BindIP: "127.0.0.1", Protocol: "tcp"},
			{Name: "dyn", Container: 30000, HostPort: 0, BindIP: "127.0.0.1", Protocol: "tcp"},
		},
	}
	if err := store.ReservePort(PortReservation{
		BindIP: "0.0.0.0", Protocol: "tcp", HostPort: 25675, InstanceUUID: "new-inst", Name: "rcon",
	}); err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	if err := store.ReservePort(PortReservation{
		BindIP: "127.0.0.1", Protocol: "tcp", HostPort: 41577, InstanceUUID: "new-inst", Name: "dyn",
	}); err != nil {
		t.Fatalf("ReservePort: %v", err)
	}

	// 有預留 → 取預留值(且 wildcard bind_ip 正規化為 loopback)。
	host, port, err := resolveInstancePort(store, tmpl, "new-inst", "rcon")
	if err != nil || host != "127.0.0.1" || port != 25675 {
		t.Fatalf("應取實例預留 25675, 得 %s:%d err=%v", host, port, err)
	}
	// 範本宣告為動態(0)的埠,亦能由預留解析出具體埠(本改動前一律報錯)。
	if _, port, err = resolveInstancePort(store, tmpl, "new-inst", "dyn"); err != nil || port != 41577 {
		t.Fatalf("動態埠應由預留解析為 41577, 得 %d err=%v", port, err)
	}
	// 相容路徑:舊實例無預留 → 退回範本宣告值。
	if host, port, err = resolveInstancePort(store, tmpl, "old-inst", "rcon"); err != nil || host != "127.0.0.1" || port != 25575 {
		t.Fatalf("無預留應退回範本值 25575, 得 %s:%d err=%v", host, port, err)
	}
	// 範本無此 port_ref → 維持原錯誤語意(不因 DB 有無預留而改變)。
	if _, _, err = resolveInstancePort(store, tmpl, "new-inst", "missing"); err == nil {
		t.Fatalf("範本無對應 [[ports]] 應報錯")
	}
	if _, _, err = resolveInstancePort(store, tmpl, "new-inst", ""); err == nil {
		t.Fatalf("空 port_ref 應報錯")
	}
	// 舊實例 + 範本亦為動態 → 無埠可解析,明確報錯。
	if _, _, err = resolveInstancePort(store, tmpl, "old-inst", "dyn"); err == nil {
		t.Fatalf("無預留且範本為動態應報錯")
	}
}

// (3b) 指令協定與健康探針兩邊都吃到該實例的實際預留埠(共用 resolveInstancePort)。
func TestInstancePortResolution_CommandAndProbe(t *testing.T) {
	h := newSvcHarness(t)
	ctx := context.Background()

	opts := h.validCreate()
	opts.PortOverrides = map[string]int{"game": 25665, "rcon": 25675}
	rec, err := h.svc.Create(ctx, opts)
	if err != nil {
		t.Fatalf("以埠覆寫建立: %v", err)
	}
	tmpl, ok := h.eng.Get("svc")
	if !ok {
		t.Fatalf("範本 svc 未載入")
	}

	// 指令協定(host_port_ref="rcon"):target 應指向覆寫後的 25675,而非範本宣告的 25575。
	cs := NewCommandService(h.store, h.eng, NewSecretStoreWithKeyring("test", h.keyring), h.reg)
	cp, err := activeProtocol(tmpl)
	if err != nil {
		t.Fatalf("activeProtocol: %v", err)
	}
	target, err := cs.buildTarget(rec, tmpl, cp)
	if err != nil {
		t.Fatalf("buildTarget: %v", err)
	}
	if target.Host != "127.0.0.1" || target.Port != 25675 {
		t.Errorf("指令目標 = %s:%d, 期望 127.0.0.1:25675", target.Host, target.Port)
	}
	// 對照:只讀範本宣告值的舊解析路徑仍回 25575——確證上面的 25675 來自實例預留而非範本。
	if _, tport, terr := resolveCommandPort(tmpl, cp.HostPortRef); terr != nil || tport != 25575 {
		t.Fatalf("範本宣告值應為 25575, 得 %d err=%v", tport, terr)
	}

	// 健康探針(kind=tcp, port_ref="game"):應撥覆寫後的 25665。
	var dialed string
	prober := NewHealthProber(ProberConfig{
		Store: h.store, Engine: h.eng, Registry: h.reg,
		Dial: func(_ context.Context, addr string) (net.Conn, error) {
			dialed = addr
			return nil, errors.New("測試不真的連線")
		},
	})
	if _, perr := prober.Ready(ctx, rec.Node, rec.UUID); perr == nil {
		t.Fatalf("注入的 Dial 失敗應向上傳遞")
	}
	if dialed != "127.0.0.1:25665" {
		t.Errorf("tcp 探針撥了 %q, 期望 127.0.0.1:25665", dialed)
	}
}
