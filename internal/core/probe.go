package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// 存活探針的關鍵常數預設(R8「關鍵常數預設」;就緒寬限 60s 沿用 lifecycle 的 defaultReadyTimeout)。
const (
	defaultLivenessInterval  = 15 * time.Second // 存活探針週期
	defaultLivenessThreshold = 3                // 連續失敗達此次數→視為卡死
	defaultProbeTimeout      = 5 * time.Second  // 單次網路探測逾時(tcp/rest)
)

// commandSender 是 HealthProber 對「送出遊戲指令」的最小依賴(由 *CommandService 滿足),供
// rcon 探針複用既有 command adapter(RconAdapter,含 Source RCON 認證),不在 core 重寫協定。
type commandSender interface {
	Send(ctx context.Context, uuid string, cmd protocol.GameCommand) (protocol.CommandResult, error)
}

// ProberConfig 是 HealthProber 的建構參數。網路相依(dial/http)可注入以利測試。
type ProberConfig struct {
	Store     *Store
	Engine    *TemplateEngine
	Registry  *NodeRegistry
	Commander commandSender // rcon 探針用;nil 時 rcon kind 回不健康
	// Dial 撥一條 TCP 連線(tcp 探針);nil 用預設帶逾時的 net.Dialer。
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// HTTPClient 供 rest 探針;nil 用預設(帶逾時)。
	HTTPClient *http.Client
	// Timeout 單次探測逾時;<=0 用 defaultProbeTimeout。
	Timeout time.Duration
}

// HealthProber 依範本 [health] 定義執行健康/就緒探針(R8),統一實作 ReadinessProber:
//   - 就緒(Orchestrator.awaitReady):啟動後於就緒寬限內輪詢至健康。
//   - 存活(HealthMonitor):running 期間週期性探測,連續失敗判卡死。
//
// 依 kind 分派:docker(或未定義 [health])→容器 running 即健康(等同 runningProber);
// tcp→撥宿主埠;rcon→經 CommandService 觸發輕量認證+空指令;rest→GET 埠根路徑,2xx 為健康。
type HealthProber struct {
	store     *Store
	engine    *TemplateEngine
	registry  *NodeRegistry
	commander commandSender
	dial      func(ctx context.Context, addr string) (net.Conn, error)
	hc        *http.Client
	timeout   time.Duration
}

// NewHealthProber 建立 HealthProber。
func NewHealthProber(cfg ProberConfig) *HealthProber {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	dial := cfg.Dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	return &HealthProber{
		store:     cfg.Store,
		engine:    cfg.Engine,
		registry:  cfg.Registry,
		commander: cfg.Commander,
		dial:      dial,
		hc:        hc,
		timeout:   timeout,
	}
}

var _ ReadinessProber = (*HealthProber)(nil)

// errInstanceNotOnline 表示線上探針判定實例未就緒/存活(HealthProber.Ready 回 false 但無錯誤)。
var errInstanceNotOnline = errors.New("core: 實例線上探針判定未就緒")

// healthOnlineProber 以 HealthProber 適配 MonitorHub 所需的 OnlineProber(T15 雙審 #10):把
// Ready(ctx, node, uuid)(bool, error) 轉為 Probe(ctx, tmpl, rec) error 契約——就緒回 nil、未就緒
// 回 errInstanceNotOnline、探測錯誤原樣回傳。MonitorHub 僅於範本定義了 [health] 時呼叫本探針,
// 故此處不重複 nil/health 判斷(見 MonitorHub.probeOnline)。
type healthOnlineProber struct{ prober *HealthProber }

// NewHealthOnlineProber 以 HealthProber 建立 MonitorHub 用的 OnlineProber(供 Bootstrap 注入,
// 取代先前恆 nil 導致線上維度永遠「不適用」的佔位)。
func NewHealthOnlineProber(prober *HealthProber) OnlineProber {
	return healthOnlineProber{prober: prober}
}

func (a healthOnlineProber) Probe(ctx context.Context, _ *protocol.GameTemplate, rec InstanceRecord) error {
	ok, err := a.prober.Ready(ctx, rec.Node, rec.UUID)
	if err != nil {
		return err
	}
	if !ok {
		return errInstanceNotOnline
	}
	return nil
}

var _ OnlineProber = healthOnlineProber{}

// Ready 實作 ReadinessProber:解析實例範本的 [health] 並依 kind 探測。範本未載入或
// 未定義 [health]→退回容器 running 檢查(與 runningProber 一致)。探測失敗回 (false, err),
// 由呼叫端(就緒輪詢/存活監控)據以決定逾時或計失敗。
func (p *HealthProber) Ready(ctx context.Context, node, uuid string) (bool, error) {
	rec, err := p.store.GetInstance(uuid)
	if err != nil {
		return false, err
	}
	tmpl, ok := p.engine.Get(rec.TemplateID)
	if !ok {
		// 範本未載入:退回 running 檢查,不因設定缺失而永遠不就緒。
		return p.probeRunning(ctx, node, uuid)
	}
	hp := tmpl.Health
	if hp == nil || hp.Kind == "" || hp.Kind == "docker" {
		return p.probeRunning(ctx, node, uuid)
	}
	switch hp.Kind {
	case "tcp":
		return p.probeTCP(ctx, tmpl, uuid, hp.PortRef)
	case "rest":
		return p.probeREST(ctx, tmpl, uuid, hp)
	case "rcon":
		return p.probeRcon(ctx, uuid)
	default:
		// 未知 kind:保守退回 running 檢查。
		return p.probeRunning(ctx, node, uuid)
	}
}

// probeRunning 查代理 runtime 狀態,running 即健康(docker/預設 kind)。
func (p *HealthProber) probeRunning(ctx context.Context, node, uuid string) (bool, error) {
	var st protocol.RuntimeStatus
	err := p.registry.Call(node, func(c *NodeClient) error {
		var e error
		st, e = c.Status(ctx, uuid)
		return e
	})
	if err != nil {
		return false, err
	}
	return st.Running, nil
}

// Running 查代理 runtime 狀態,回容器是否 running(以及查詢錯誤)。供存活監控(B3)於就緒探測失敗時
// 再判「容器仍 running(代理可達)=真卡死」vs「代理/狀態端點不可達=基礎設施瞬斷」。實作 runningChecker。
func (p *HealthProber) Running(ctx context.Context, node, uuid string) (bool, error) {
	return p.probeRunning(ctx, node, uuid)
}

// probeTCP 撥 port_ref 對應的宿主埠(取該實例實際預留的埠,見 resolveInstancePort);連得上即健康。
func (p *HealthProber) probeTCP(ctx context.Context, tmpl *protocol.GameTemplate, uuid, portRef string) (bool, error) {
	host, port, err := resolveInstancePort(p.store, tmpl, uuid, portRef)
	if err != nil {
		return false, fmt.Errorf("tcp 探針: %w", err)
	}
	conn, derr := p.dial(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	if derr != nil {
		return false, derr
	}
	_ = conn.Close()
	return true, nil
}

// probeREST 探測 rest 健康。範本 [health] 指定 action_id 時(建議):經 CommandService 送該具名 REST
// 動作(複用該 rest 協定的 Basic Auth 認證與埠映射),Send 成功且 result.Success=true 為健康——這是
// 「全端點需認證」的伺服器(如 Palworld,任何未認證請求皆非 2xx)唯一可行的就緒判定,對稱 rcon 探針
// (認證/協定不在 core 重寫,交 CommandService/adapter)。省略 action_id 時退回未認證 GET port_ref
// 根路徑、2xx 為健康(僅適合無認證的簡單服務)。
func (p *HealthProber) probeREST(ctx context.Context, tmpl *protocol.GameTemplate, uuid string, hp *protocol.HealthProbe) (bool, error) {
	if hp.ActionID != "" {
		return p.probeRestAction(ctx, tmpl, uuid, hp.ActionID)
	}
	host, port, err := resolveInstancePort(p.store, tmpl, uuid, hp.PortRef)
	if err != nil {
		return false, fmt.Errorf("rest 探針: %w", err)
	}
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if rerr != nil {
		return false, rerr
	}
	resp, herr := p.hc.Do(req)
	if herr != nil {
		return false, herr
	}
	_ = resp.Body.Close()
	return resp.StatusCode/100 == 2, nil
}

// probeRestAction 經 CommandService 送出範本 health.action_id 指定的具名 REST 動作(含 Basic Auth),
// 判 Send 成功且 result.Success=true 為健康。未配置 commander 或範本無非 legacy rest 協定→回錯誤。
func (p *HealthProber) probeRestAction(ctx context.Context, tmpl *protocol.GameTemplate, uuid, actionID string) (bool, error) {
	if p.commander == nil {
		return false, fmt.Errorf("rest 探針: 未配置 command sender")
	}
	cp, ok := activeRestProtocol(tmpl)
	if !ok {
		return false, fmt.Errorf("rest 探針: health.action_id=%q 但範本無非 legacy rest 指令協定", actionID)
	}
	result, err := p.commander.Send(ctx, uuid, protocol.GameCommand{ProtocolID: cp.ProtocolID, ActionID: actionID})
	if err != nil {
		return false, err
	}
	if !result.Success {
		return false, fmt.Errorf("rest 探針: 動作 %q 未成功(output=%s)", actionID, result.Output)
	}
	return true, nil
}

// activeRestProtocol 回傳範本第一個「非 legacy 且 kind=rest」的指令協定(供 rest 就緒探測選定認證協定)。
func activeRestProtocol(tmpl *protocol.GameTemplate) (protocol.CommandProtocol, bool) {
	for _, cp := range tmpl.CommandProtocols {
		if cp.Legacy || cp.Kind != "rest" {
			continue
		}
		return cp, true
	}
	return protocol.CommandProtocol{}, false
}

// probeRcon 經 CommandService 觸發一次輕量指令(空 raw):RconAdapter 會完成連線+認證+執行,
// 認證/連線失敗即回錯誤。複用 agent 端 RconAdapter,不於 core 重寫 RCON 協定。
func (p *HealthProber) probeRcon(ctx context.Context, uuid string) (bool, error) {
	if p.commander == nil {
		return false, fmt.Errorf("rcon 探針: 未配置 command sender")
	}
	result, err := p.commander.Send(ctx, uuid, protocol.GameCommand{})
	if err != nil {
		return false, err
	}
	if !result.Success {
		return false, fmt.Errorf("rcon 探針: 指令未成功(output=%s)", result.Output)
	}
	return true, nil
}

// ---- 存活監控(R8:週期探測 + 連續失敗判卡死)----

// stuckRecoverer 是 HealthMonitor 對 Orchestrator 的最小依賴:偵測卡死時觸發與 die 相同的
// 復原路徑(標 Crashed + CrashHook,於 per-instance lock 內)。
type stuckRecoverer interface {
	RecoverStuck(uuid string)
}

// runningChecker 由能查「容器是否仍 running(且代理可達)」的探針實作(生產 HealthProber),供存活
// 監控(B3)於就緒探測失敗時區分「容器仍 running=真卡死(計入)」與「代理不可達=基礎設施瞬斷(不計入)」。
type runningChecker interface {
	Running(ctx context.Context, node, uuid string) (bool, error)
}

// HealthMonitor 對所有 observed=Running 的實例週期性執行存活探針(R8):連續失敗達門檻
// (預設 15s 週期、連續 3 次)→ 判定「running 但卡死」,經 stuckRecoverer 納入復原;單次瞬斷
// 只累計、不立即觸發(下次成功即歸零)。探測本身無鎖(唯讀 Status/撥號);復原經 per-instance
// lock 序列化,不與 Start/Stop/Restart/備份互撞。
type HealthMonitor struct {
	store     *Store
	prober    ReadinessProber
	recoverer stuckRecoverer
	interval  time.Duration
	threshold int

	mu    sync.Mutex
	fails map[string]int
}

// HealthMonitorConfig 是 HealthMonitor 的建構參數。
type HealthMonitorConfig struct {
	Store     *Store
	Prober    ReadinessProber
	Recoverer stuckRecoverer // 通常為 *Orchestrator
	Interval  time.Duration  // <=0 用 15s
	Threshold int            // <=0 用 3
}

// NewHealthMonitor 建立 HealthMonitor。
func NewHealthMonitor(cfg HealthMonitorConfig) *HealthMonitor {
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultLivenessInterval
	}
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = defaultLivenessThreshold
	}
	return &HealthMonitor{
		store:     cfg.Store,
		prober:    cfg.Prober,
		recoverer: cfg.Recoverer,
		interval:  interval,
		threshold: threshold,
		fails:     make(map[string]int),
	}
}

// Run 以 interval 週期執行 checkOnce,阻塞至 ctx 取消。
func (m *HealthMonitor) Run(ctx context.Context, node string) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkOnce(ctx, node)
		}
	}
}

// checkOnce 對所有 Running 實例做一輪存活探測並更新連續失敗計數;達門檻→復原並歸零。
// 公開以利測試逐輪驅動(免真實 15s 等待)。
func (m *HealthMonitor) checkOnce(ctx context.Context, node string) {
	insts, err := m.store.ListInstances()
	if err != nil {
		return
	}
	for _, rec := range insts {
		if rec.Node != node {
			continue
		}
		if rec.ObservedState != protocol.InstanceStateRunning {
			// 非 Running:清除其失敗計數(避免陳舊計數跨狀態累積)。
			m.reset(rec.UUID)
			continue
		}
		ok, perr := m.prober.Ready(ctx, node, rec.UUID)
		if perr == nil && ok {
			m.reset(rec.UUID)
			continue
		}
		// B3:就緒探測失敗時,僅在「確認容器仍 running(代理可達)」才計入卡死門檻——否則(代理/狀態端點
		// 瞬斷、或容器已非 running)視為不確定、不計入,避免遠端節點狀態端點瞬斷把健康 Running 伺服器誤判
		// 卡死並強停+標 Crashed+自動重啟。容器已退出的路徑由 die 事件處理,不靠此存活監控。無 runningChecker
		// 的探針(測試 mock)維持原行為(任何失敗即累計)。
		if rc, hasRC := m.prober.(runningChecker); hasRC {
			running, rerr := rc.Running(ctx, node, rec.UUID)
			if rerr != nil || !running {
				continue
			}
		}
		if m.bump(rec.UUID) >= m.threshold {
			m.reset(rec.UUID)
			m.recoverer.RecoverStuck(rec.UUID)
		}
	}
}

func (m *HealthMonitor) bump(uuid string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails[uuid]++
	return m.fails[uuid]
}

func (m *HealthMonitor) reset(uuid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.fails, uuid)
}
