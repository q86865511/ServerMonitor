package core

import (
	"context"
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
		return p.probeTCP(ctx, tmpl, hp.PortRef)
	case "rest":
		return p.probeREST(ctx, tmpl, hp.PortRef)
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

// probeTCP 撥範本 port_ref 對應的宿主埠;連得上即健康。
func (p *HealthProber) probeTCP(ctx context.Context, tmpl *protocol.GameTemplate, portRef string) (bool, error) {
	host, port, err := resolveCommandPort(tmpl, portRef)
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

// probeREST 對 port_ref 對應宿主埠的根路徑發 GET,2xx 視為健康。範本 [health] 僅含 kind+port_ref
// (無路徑/認證),故探測根路徑;需認證的具體健康端點屬日後範本增量。
func (p *HealthProber) probeREST(ctx context.Context, tmpl *protocol.GameTemplate, portRef string) (bool, error) {
	host, port, err := resolveCommandPort(tmpl, portRef)
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
