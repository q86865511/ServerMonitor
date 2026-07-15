package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// tcpHealthTemplate 產生一份「tcp 探針指向給定宿主埠」的最小範本。
func tcpHealthTemplate(hostPort int) string {
	return fmt.Sprintf(`
schema_version = 1
id = "tcphealth"
name = "TCP Health"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/tcp:1.0"
[[ports]]
name = "game"
container = 25565
host_port = %d
bind_ip = "127.0.0.1"
protocol = "tcp"
[health]
kind = "tcp"
port_ref = "game"
`, hostPort)
}

const rconHealthTemplate = `
schema_version = 1
id = "rconhealth"
name = "RCON Health"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/rcon:1.0"
[health]
kind = "rcon"
port_ref = "rcon"
`

// fakeProber 是可控健康結果的 prober(供存活監控/就緒逾時測試)。
type fakeProber struct {
	mu      sync.Mutex
	healthy bool
	err     error
}

func (f *fakeProber) set(healthy bool, err error) {
	f.mu.Lock()
	f.healthy, f.err = healthy, err
	f.mu.Unlock()
}

func (f *fakeProber) Ready(context.Context, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy, f.err
}

// fakeCommander 是 rcon 探針的 command sender 替身。err 非 nil 時模擬連線/認證失敗;
// err 為 nil 但 resultFail 為 true 時模擬「連線成功但指令執行失敗」(result.Success=false,
// 見 T11 雙審 #9:探針須檢查 Success,不能只憑 err==nil 判健康)。
type fakeCommander struct {
	err        error
	resultFail bool
	output     string
	calls      int
}

func (c *fakeCommander) Send(context.Context, string, protocol.GameCommand) (protocol.CommandResult, error) {
	c.calls++
	if c.err != nil {
		return protocol.CommandResult{}, c.err
	}
	return protocol.CommandResult{Success: !c.resultFail, Output: c.output}, nil
}

// ---- 探針:docker(running)----

func TestHealthProber_DockerRunning(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	env.create("life", "dk-1")
	orch := NewOrchestrator(OrchestratorConfig{Store: env.store, Events: env.events, Registry: env.reg})
	if err := orch.Start(context.Background(), "dk-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	p := NewHealthProber(ProberConfig{Store: env.store, Engine: env.eng, Registry: env.reg})
	ok, err := p.Ready(context.Background(), "local", "dk-1")
	if err != nil || !ok {
		t.Fatalf("running 實例應健康, 得 ok=%v err=%v", ok, err)
	}

	if err := orch.Stop(context.Background(), "dk-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ok, _ = p.Ready(context.Background(), "local", "dk-1")
	if ok {
		t.Errorf("已停止實例不應健康")
	}
}

// ---- OnlineProber 適配器(T15 雙審 #10)----

// TestHealthOnlineProber_Adapts 驗證 HealthProber→OnlineProber 適配器把 Ready 結果轉為
// Probe 契約:健康→nil、不健康→非 nil。經真實 tcp 探針(埠開/關)驅動,涵蓋 MonitorHub 注入路徑。
func TestHealthOnlineProber_Adapts(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port

	env := newT11Env(t, map[string]string{"tcphealth": tcpHealthTemplate(port)}, false)
	rec := InstanceRecord{
		UUID: "on-1", TemplateID: "tcphealth", Node: "local",
		ObservedState: protocol.InstanceStateRunning,
	}
	if err := env.store.UpsertInstance(rec); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}
	tmpl, _ := env.eng.Get("tcphealth")

	prober := NewHealthProber(ProberConfig{Store: env.store, Engine: env.eng, Registry: env.reg})
	online := NewHealthOnlineProber(prober)

	// 埠開放:適配器應回 nil(線上)。
	if perr := online.Probe(context.Background(), tmpl, rec); perr != nil {
		t.Fatalf("埠開放時 Probe 應回 nil(線上),得 %v", perr)
	}

	// 埠關閉:適配器應回非 nil(離線)。
	_ = lis.Close()
	if perr := online.Probe(context.Background(), tmpl, rec); perr == nil {
		t.Fatal("埠關閉時 Probe 應回非 nil(離線),得 nil")
	}
}

// ---- 探針:tcp(撥宿主埠)----

func TestHealthProber_TCP(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port

	env := newT11Env(t, map[string]string{"tcphealth": tcpHealthTemplate(port)}, false)
	// 直接寫入實例列(tcp 探針只讀範本埠,不需經代理建立容器)。
	if err := env.store.UpsertInstance(InstanceRecord{
		UUID: "tc-1", TemplateID: "tcphealth", Node: "local",
		ObservedState: protocol.InstanceStateRunning,
	}); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}

	p := NewHealthProber(ProberConfig{Store: env.store, Engine: env.eng, Registry: env.reg})
	ok, perr := p.Ready(context.Background(), "local", "tc-1")
	if perr != nil || !ok {
		t.Fatalf("埠開放時應健康, 得 ok=%v err=%v", ok, perr)
	}

	_ = lis.Close()
	ok, _ = p.Ready(context.Background(), "local", "tc-1")
	if ok {
		t.Errorf("埠關閉後不應健康")
	}
}

// ---- 探針:rcon(經 CommandService 介面)----

func TestHealthProber_RconViaCommander(t *testing.T) {
	env := newT11Env(t, map[string]string{"rconhealth": rconHealthTemplate}, false)
	if err := env.store.UpsertInstance(InstanceRecord{
		UUID: "rc-1", TemplateID: "rconhealth", Node: "local",
		ObservedState: protocol.InstanceStateRunning,
	}); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}

	cmd := &fakeCommander{}
	p := NewHealthProber(ProberConfig{Store: env.store, Engine: env.eng, Registry: env.reg, Commander: cmd})
	ok, err := p.Ready(context.Background(), "local", "rc-1")
	if err != nil || !ok {
		t.Fatalf("rcon 指令成功應健康, 得 ok=%v err=%v", ok, err)
	}
	if cmd.calls != 1 {
		t.Errorf("應經 commander 送出 1 次探測指令, 得 %d", cmd.calls)
	}

	cmd.err = errors.New("auth failed")
	ok, _ = p.Ready(context.Background(), "local", "rc-1")
	if ok {
		t.Errorf("rcon 認證失敗不應健康")
	}
}

// TestHealthProber_RconResultSuccessFalseUnhealthy 驗證 T11 雙審 #9:rcon 探針即使
// Send 未回傳 error,只要 CommandResult.Success 為 false(指令本身執行失敗,如伺服器仍在
// 載入而拒絕指令),也應判為不健康,不能只憑「連線/認證成功」放行。
func TestHealthProber_RconResultSuccessFalseUnhealthy(t *testing.T) {
	env := newT11Env(t, map[string]string{"rconhealth": rconHealthTemplate}, false)
	if err := env.store.UpsertInstance(InstanceRecord{
		UUID: "rc-2", TemplateID: "rconhealth", Node: "local",
		ObservedState: protocol.InstanceStateRunning,
	}); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}

	cmd := &fakeCommander{resultFail: true, output: "server still loading"}
	p := NewHealthProber(ProberConfig{Store: env.store, Engine: env.eng, Registry: env.reg, Commander: cmd})
	ok, err := p.Ready(context.Background(), "local", "rc-2")
	if ok {
		t.Errorf("Success=false 不應視為健康")
	}
	if err == nil {
		t.Fatal("Success=false 應回錯誤(供呼叫端記錄/計失敗),得 nil")
	}
}

// ---- 就緒探針接管 Start:不健康 → 就緒逾時 → Error ----

func TestHealthProber_ReadinessTimeout(t *testing.T) {
	// 取一個保證空閒的埠(開後即關),使 tcp 就緒探針恆失敗。
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	deadPort := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()

	env := newT11Env(t, map[string]string{"tcphealth": tcpHealthTemplate(deadPort)}, false)
	env.create("tcphealth", "rt-1")

	prober := NewHealthProber(ProberConfig{Store: env.store, Engine: env.eng, Registry: env.reg})
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg,
		Prober: prober, ReadyTimeout: 150 * time.Millisecond, ReadyPoll: 20 * time.Millisecond,
	})

	err = orch.Start(context.Background(), "rt-1")
	if !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("就緒探針恆不健康應致 ErrStartTimeout, 得 %v", err)
	}
	if got := env.state("rt-1"); got != protocol.InstanceStateError {
		t.Errorf("就緒逾時後 observed = %s, 期望 Error", got)
	}
}

// ---- 存活監控:連續 3 次失敗 → 卡死復原;單次瞬斷不觸發 ----

func TestHealthMonitor_StuckAfterThreeFailures(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	env.create("life", "lv-1")
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg,
		CrashHook: env.recordCrash,
	})
	if err := orch.Start(context.Background(), "lv-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	fp := &fakeProber{healthy: false}
	mon := NewHealthMonitor(HealthMonitorConfig{Store: env.store, Prober: fp, Recoverer: orch, Threshold: 3})

	ctx := context.Background()
	mon.checkOnce(ctx, "local") // 失敗 1
	mon.checkOnce(ctx, "local") // 失敗 2
	if got := env.state("lv-1"); got != protocol.InstanceStateRunning {
		t.Fatalf("連續失敗未達門檻不應復原, observed = %s", got)
	}
	if env.crashCount() != 0 {
		t.Fatalf("未達門檻不應交棒 crashHook, 得 %d", env.crashCount())
	}

	mon.checkOnce(ctx, "local") // 失敗 3 → 卡死復原
	if got := env.state("lv-1"); got != protocol.InstanceStateCrashed {
		t.Errorf("達門檻後 observed = %s, 期望 Crashed", got)
	}
	if env.crashCount() != 1 {
		t.Errorf("卡死應交棒 crashHook 1 次, 得 %d", env.crashCount())
	}
	if n := env.countEvents(protocol.EventHealthProbeFailed); n != 1 {
		t.Errorf("HEALTH_PROBE_FAILED = %d, 期望 1", n)
	}
	if n := env.countEvents(protocol.EventInstanceCrashed); n != 1 {
		t.Errorf("INSTANCE_CRASHED = %d, 期望 1", n)
	}
}

func TestHealthMonitor_TransientFailureResets(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	env.create("life", "tr-1")
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg,
		CrashHook: env.recordCrash,
	})
	if err := orch.Start(context.Background(), "tr-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	fp := &fakeProber{healthy: false}
	mon := NewHealthMonitor(HealthMonitorConfig{Store: env.store, Prober: fp, Recoverer: orch, Threshold: 3})
	ctx := context.Background()

	mon.checkOnce(ctx, "local") // 失敗 1
	fp.set(true, nil)
	mon.checkOnce(ctx, "local") // 成功 → 計數歸零
	fp.set(false, nil)
	mon.checkOnce(ctx, "local") // 失敗 1(重新起算)
	mon.checkOnce(ctx, "local") // 失敗 2

	if got := env.state("tr-1"); got != protocol.InstanceStateRunning {
		t.Errorf("瞬斷後成功應重置計數, 不應復原, observed = %s", got)
	}
	if env.crashCount() != 0 {
		t.Errorf("不應交棒 crashHook, 得 %d", env.crashCount())
	}
}

// 確保 details JSON 可解析(防事件細節格式退化)。
func TestHealthMonitor_ProbeFailedDetailsWellFormed(t *testing.T) {
	env := newT11Env(t, map[string]string{"life": lifeTestTemplate}, false)
	env.create("life", "wf-1")
	orch := NewOrchestrator(OrchestratorConfig{
		Store: env.store, Events: env.events, Registry: env.reg, CrashHook: env.recordCrash,
	})
	if err := orch.Start(context.Background(), "wf-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fp := &fakeProber{healthy: false}
	mon := NewHealthMonitor(HealthMonitorConfig{Store: env.store, Prober: fp, Recoverer: orch, Threshold: 1})
	mon.checkOnce(context.Background(), "local")

	evs := queryEvents(t, env.events, protocol.EventHealthProbeFailed)
	if len(evs) != 1 {
		t.Fatalf("HEALTH_PROBE_FAILED = %d, 期望 1", len(evs))
	}
	var d map[string]any
	if err := json.Unmarshal(evs[0].DetailsJSON, &d); err != nil {
		t.Fatalf("details 非合法 JSON: %v", err)
	}
}
