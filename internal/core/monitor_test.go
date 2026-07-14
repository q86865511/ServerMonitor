package core

import (
	"context"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// ---- 測試輔助 ----

// newMonitorAgent 起一個以 MockBackend 為後端、stats 推送週期短的真 agent server(免 Docker),
// 回傳 httptest server 與底層 MockBackend。
func newMonitorAgent(t *testing.T, backend *agent.MockBackend) *httptest.Server {
	t.Helper()
	s, err := agent.NewServer(agent.Config{
		Backend:       backend,
		Token:         agentTestToken,
		StatsInterval: 15 * time.Millisecond,
	})
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

// monNopDialer 是 fanout/pollOnce 白箱測試用的空 StreamDialer(不會被實際撥號)。
type monNopDialer struct{}

func (monNopDialer) StatsWS(context.Context, string) (*websocket.Conn, error) { return nil, nil }
func (monNopDialer) LogsWS(context.Context, string) (*websocket.Conn, error)  { return nil, nil }

// monErrDialer 一律回撥號失敗,驅使 runStats/runLogs 走退避重連迴圈(不 deref nil conn),
// 供生命週期壓力測試在不起真 WS 的前提下讓串流 goroutine 忙碌並可被 ctx 取消收束。
type monErrDialer struct{}

func (monErrDialer) StatsWS(context.Context, string) (*websocket.Conn, error) {
	return nil, ErrNodeUnreachable
}
func (monErrDialer) LogsWS(context.Context, string) (*websocket.Conn, error) {
	return nil, ErrNodeUnreachable
}

// monDrainLogs 非阻塞讀空一個 log channel,回傳其目前緩衝內容。
func monDrainLogs(ch <-chan LogLine) []LogLine {
	var out []LogLine
	for {
		select {
		case ln := <-ch:
			out = append(out, ln)
		default:
			return out
		}
	}
}

// monLookup 回傳預設的實例記錄與範本。
type monLookup struct {
	rec  InstanceRecord
	tmpl *protocol.GameTemplate
	err  error
}

func (l monLookup) Lookup(string) (InstanceRecord, *protocol.GameTemplate, error) {
	return l.rec, l.tmpl, l.err
}

// monSender 回傳預設的指令結果,並記錄最後一次送出的指令。
type monSender struct {
	res  protocol.CommandResult
	err  error
	last protocol.GameCommand
}

func (s *monSender) Send(_ context.Context, _ string, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	s.last = cmd
	return s.res, s.err
}

// monProber 回傳預設的探針結果。
type monProber struct{ err error }

func (p monProber) Probe(context.Context, *protocol.GameTemplate, InstanceRecord) error {
	return p.err
}

// ---- stats / logs 經真 WS ----

func TestMonitorHub_StatsOverWS(t *testing.T) {
	backend := agent.NewMockBackend()
	disk := uint64(4096)
	backend.StatsFunc = func(protocol.RuntimeID) protocol.ResourceStats {
		return protocol.ResourceStats{
			TsUTC:         time.Now().UTC(),
			CPUPercent:    42.5,
			MemoryBytes:   128 << 20,
			MemoryLimit:   1 << 30,
			DataDiskBytes: &disk,
		}
	}
	hs := newMonitorAgent(t, backend)
	const uuid = "stats-uuid"
	if _, err := backend.Create(context.Background(), testSpec(uuid)); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	client := NewNodeClient(hs.URL, agentTestToken, nil)
	hub := NewMonitorHub(client, nil, nil, nil, MonitorConfig{ReconnectBase: 5 * time.Millisecond})

	ch, id := hub.SubscribeStats(uuid)
	hub.StartMonitoring(uuid)

	select {
	case s := <-ch:
		if s.CPUPercent != 42.5 || s.MemoryBytes != 128<<20 {
			t.Fatalf("stats = %+v", s)
		}
		if s.DataDiskBytes == nil || *s.DataDiskBytes != 4096 {
			t.Fatalf("資料磁碟未轉推: %+v", s.DataDiskBytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("逾時未收到 stats 更新")
	}

	// 快照應已有資源取樣。
	snap, ok := hub.Snapshot(uuid)
	if !ok || !snap.HasStats || snap.Stats.CPUPercent != 42.5 {
		t.Fatalf("Snapshot = %+v ok=%v", snap, ok)
	}

	hub.UnsubscribeStats(uuid, id)

	// 實例停止 → 訂閱關閉(上游串流收束、實例移除)。
	hub.StopMonitoring(uuid)
	if _, ok := hub.Snapshot(uuid); ok {
		t.Fatal("StopMonitoring 後 Snapshot 仍存在")
	}
}

func TestMonitorHub_StopClosesSubscribers(t *testing.T) {
	backend := agent.NewMockBackend()
	hs := newMonitorAgent(t, backend)
	const uuid = "close-uuid"
	if _, err := backend.Create(context.Background(), testSpec(uuid)); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	client := NewNodeClient(hs.URL, agentTestToken, nil)
	hub := NewMonitorHub(client, nil, nil, nil, MonitorConfig{ReconnectBase: 5 * time.Millisecond})

	ch, _ := hub.SubscribeStats(uuid)
	hub.StartMonitoring(uuid)
	// 等第一筆確認串流已通。
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("逾時未收到 stats")
	}

	hub.StopMonitoring(uuid)

	// channel 應在有限時間內被關閉(排掉殘餘緩衝後 ok=false)。
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // 已關閉,通過
			}
		case <-deadline:
			t.Fatal("StopMonitoring 後 stats channel 未關閉")
		}
	}
}

func TestMonitorHub_LogsOverWS(t *testing.T) {
	backend := agent.NewMockBackend()
	now := time.Now().UTC()
	backend.LogLinesFunc = func(protocol.RuntimeID) []agent.LogLine {
		return []agent.LogLine{
			{TsUTC: now, Stream: "stdout", Line: "starting server"},
			{TsUTC: now, Stream: "stdout", Line: "done (2.1s)! for help type help"},
		}
	}
	hs := newMonitorAgent(t, backend)
	const uuid = "logs-uuid"
	if _, err := backend.Create(context.Background(), testSpec(uuid)); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	client := NewNodeClient(hs.URL, agentTestToken, nil)
	hub := NewMonitorHub(client, nil, nil, nil, MonitorConfig{ReconnectBase: 5 * time.Millisecond})

	ch, _ := hub.SubscribeLogs(uuid)
	hub.StartMonitoring(uuid)
	t.Cleanup(func() { hub.StopMonitoring(uuid) })

	got := make([]string, 0, 2)
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ln := <-ch:
			got = append(got, ln.Line)
		case <-deadline:
			t.Fatalf("逾時,只收到 %d 行: %v", len(got), got)
		}
	}
	if got[0] != "starting server" || got[1] != "done (2.1s)! for help type help" {
		t.Fatalf("log 行不符: %v", got)
	}
}

// ---- log 背壓(白箱:直接驅動 fanout)----

// TestMonitorHub_LogFanoutNonBlocking:單一慢消費者(緩衝滿且不消費)不阻塞轉推迴圈。
func TestMonitorHub_LogFanoutNonBlocking(t *testing.T) {
	hub := NewMonitorHub(monNopDialer{}, nil, nil, nil, MonitorConfig{LogBufferSize: 8})
	const uuid = "bp-uuid"
	_, _ = hub.SubscribeLogs(uuid) // 慢消費者:永不排空
	mi := hub.insts[uuid]

	now := time.Now()
	start := time.Now()
	for i := 0; i < 500; i++ {
		mi.fanoutLog(LogLine{Stream: "stdout", Line: "L"}, now)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("轉推 500 行耗時 %v,超過 200ms(疑似被慢消費者阻塞)", elapsed)
	}
}

// TestMonitorHub_LogDropOldestAndNotice:緩衝滿時丟最舊、留最新,並注入「已丟棄 N 行」提示行。
func TestMonitorHub_LogDropOldestAndNotice(t *testing.T) {
	hub := NewMonitorHub(monNopDialer{}, nil, nil, nil, MonitorConfig{LogBufferSize: 8})
	const uuid = "drop-uuid"
	ch, _ := hub.SubscribeLogs(uuid)
	mi := hub.insts[uuid]
	now := time.Now()

	for i := 0; i < 100; i++ {
		mi.fanoutLog(LogLine{Stream: "stdout", Line: monLineTag(i)}, now)
	}

	// 緩衝應為最新 8 行(92..99),丟舊留新。
	buffered := monDrainLogs(ch)
	if len(buffered) != 8 {
		t.Fatalf("緩衝行數 = %d,期望 8", len(buffered))
	}
	if buffered[0].Line != monLineTag(92) || buffered[7].Line != monLineTag(99) {
		t.Fatalf("留新失敗:緩衝首尾 = %q..%q", buffered[0].Line, buffered[7].Line)
	}

	// 排空後再推一行 → 應先收到丟棄提示行(92 行),再收到新行。
	mi.fanoutLog(LogLine{Stream: "stdout", Line: monLineTag(100)}, now)
	after := monDrainLogs(ch)
	if len(after) != 2 {
		t.Fatalf("再推一行後緩衝 = %d 筆(期望 提示+新行=2): %+v", len(after), after)
	}
	if after[0].Stream != LogStreamMonitor {
		t.Fatalf("第一筆應為監控提示行,得 Stream=%q Line=%q", after[0].Stream, after[0].Line)
	}
	if want := "已丟棄 92 行"; !strings.Contains(after[0].Line, want) {
		t.Fatalf("提示行內容 = %q,應含 %q", after[0].Line, want)
	}
	if after[1].Line != monLineTag(100) {
		t.Fatalf("提示行後應為新行 %q,得 %q", monLineTag(100), after[1].Line)
	}
}

// TestMonitorHub_SlowSubscriberDoesNotStarveOthers:一個卡住的慢訂閱者不妨礙健康訂閱者收到新行。
func TestMonitorHub_SlowSubscriberDoesNotStarveOthers(t *testing.T) {
	hub := NewMonitorHub(monNopDialer{}, nil, nil, nil, MonitorConfig{LogBufferSize: 8})
	const uuid = "fair-uuid"
	_, _ = hub.SubscribeLogs(uuid)       // 慢訂閱者:永不排空(緩衝將卡滿)
	fastCh, _ := hub.SubscribeLogs(uuid) // 健康訂閱者:每批排空
	mi := hub.insts[uuid]
	now := time.Now()

	// 第一批:填滿兩者緩衝。
	for i := 0; i < 8; i++ {
		mi.fanoutLog(LogLine{Stream: "stdout", Line: monLineTag(i)}, now)
	}
	// 健康訂閱者排空,慢訂閱者維持卡住(緩衝滿)。
	_ = monDrainLogs(fastCh)

	// 第二批新行:健康訂閱者應全收到,不受慢訂閱者(已滿)影響。
	for i := 8; i < 16; i++ {
		mi.fanoutLog(LogLine{Stream: "stdout", Line: monLineTag(i)}, now)
	}
	fresh := monDrainLogs(fastCh)
	if len(fresh) != 8 {
		t.Fatalf("健康訂閱者第二批收到 %d 行,期望 8: %+v", len(fresh), fresh)
	}
	if fresh[0].Line != monLineTag(8) || fresh[7].Line != monLineTag(15) {
		t.Fatalf("健康訂閱者未收到新行 8..15,得 %q..%q", fresh[0].Line, fresh[7].Line)
	}
}

// ---- 玩家數 ----

func TestMonitorHub_PlayerCount(t *testing.T) {
	mcTemplate := &protocol.GameTemplate{
		ID:           "minecraft",
		PlayersQuery: &protocol.PlayersQuery{Kind: "rcon", Command: "list"},
	}

	cases := []struct {
		name   string
		sender CommandSender
		tmpl   *protocol.GameTemplate
		want   *int // nil = 不適用
	}{
		{
			name:   "rcon list 解析成功",
			sender: &monSender{res: protocol.CommandResult{Success: true, Output: "There are 3 of a max of 20 players online: a, b, c"}},
			tmpl:   mcTemplate,
			want:   monIntPtr(3),
		},
		{
			name:   "rcon 回應無法解析 → 不適用",
			sender: &monSender{res: protocol.CommandResult{Success: true, Output: "unknown command"}},
			tmpl:   mcTemplate,
			want:   nil,
		},
		{
			name:   "範本無 players_query → 不適用",
			sender: &monSender{res: protocol.CommandResult{Success: true, Output: "There are 3 of a max of 20 players online"}},
			tmpl:   &protocol.GameTemplate{ID: "palworld"},
			want:   nil,
		},
		{
			name:   "無 sender → 不適用",
			sender: nil,
			tmpl:   mcTemplate,
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := monLookup{rec: InstanceRecord{UUID: "p", ObservedState: protocol.InstanceStateRunning}, tmpl: tc.tmpl}
			hub := NewMonitorHub(monNopDialer{}, tc.sender, nil, lookup, MonitorConfig{})
			const uuid = "p"
			_, _ = hub.SubscribeStats(uuid) // 建立實例容器
			hub.pollOnce(context.Background(), hub.insts[uuid])

			snap, _ := hub.Snapshot(uuid)
			monAssertIntPtr(t, "PlayerCount", snap.PlayerCount, tc.want)
		})
	}
}

// ---- 線上狀態 ----

func TestMonitorHub_OnlineProbe(t *testing.T) {
	withHealth := &protocol.GameTemplate{ID: "mc", Health: &protocol.HealthProbe{Kind: "tcp", PortRef: "game"}}
	noHealth := &protocol.GameTemplate{ID: "mc"}

	cases := []struct {
		name   string
		prober OnlineProber
		tmpl   *protocol.GameTemplate
		want   *bool // nil = 不適用
	}{
		{"探針健康 → 線上", monProber{err: nil}, withHealth, monBoolPtr(true)},
		{"探針失敗 → 離線", monProber{err: context.DeadlineExceeded}, withHealth, monBoolPtr(false)},
		{"nil 探針 → 不適用", nil, withHealth, nil},
		{"範本無 health → 不適用", monProber{err: nil}, noHealth, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := monLookup{rec: InstanceRecord{UUID: "o"}, tmpl: tc.tmpl}
			hub := NewMonitorHub(monNopDialer{}, nil, tc.prober, lookup, MonitorConfig{})
			const uuid = "o"
			_, _ = hub.SubscribeStats(uuid)
			hub.pollOnce(context.Background(), hub.insts[uuid])

			snap, _ := hub.Snapshot(uuid)
			monAssertBoolPtr(t, "Online", snap.Online, tc.want)
		})
	}
}

// ---- 斷線退避重連 ----

// monFlakyDialer 前 statsFails 次 StatsWS 回不可達,之後委派 inner(真 NodeClient)。
type monFlakyDialer struct {
	inner      StreamDialer
	mu         sync.Mutex
	statsFails int
	statsCalls int
}

func (d *monFlakyDialer) statsCallCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statsCalls
}

func (d *monFlakyDialer) StatsWS(ctx context.Context, uuid string) (*websocket.Conn, error) {
	d.mu.Lock()
	d.statsCalls++
	fail := d.statsCalls <= d.statsFails
	d.mu.Unlock()
	if fail {
		return nil, ErrNodeUnreachable
	}
	return d.inner.StatsWS(ctx, uuid)
}

func (d *monFlakyDialer) LogsWS(ctx context.Context, uuid string) (*websocket.Conn, error) {
	return d.inner.LogsWS(ctx, uuid)
}

func TestMonitorHub_ReconnectWithBackoff(t *testing.T) {
	backend := agent.NewMockBackend()
	backend.StatsFunc = func(protocol.RuntimeID) protocol.ResourceStats {
		return protocol.ResourceStats{TsUTC: time.Now().UTC(), CPUPercent: 7}
	}
	hs := newMonitorAgent(t, backend)
	const uuid = "reconnect-uuid"
	if _, err := backend.Create(context.Background(), testSpec(uuid)); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	inner := NewNodeClient(hs.URL, agentTestToken, nil)
	dialer := &monFlakyDialer{inner: inner, statsFails: 2} // 前兩次 stats 撥號失敗

	hub := NewMonitorHub(dialer, nil, nil, nil, MonitorConfig{ReconnectBase: 5 * time.Millisecond, ReconnectMax: 50 * time.Millisecond})
	ch, _ := hub.SubscribeStats(uuid)
	hub.StartMonitoring(uuid)
	t.Cleanup(func() { hub.StopMonitoring(uuid) })

	select {
	case s := <-ch:
		if s.CPUPercent != 7 {
			t.Fatalf("stats = %+v", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("退避重連後仍未收到 stats")
	}
	if n := dialer.statsCallCount(); n < 3 {
		t.Fatalf("StatsWS 撥號次數 = %d,期望 ≥3(2 次失敗 + ≥1 次成功)", n)
	}
}

// ---- 生命週期競態壓力 ----

// TestMonitorHub_LifecycleStress:同一 uuid 上多 goroutine 交錯 Start/Stop/Subscribe/Unsubscribe/
// Snapshot 各 100 輪。搭配 -race 驗證:無資料競爭、無 panic(WaitGroup 誤用/送入已關閉 channel)、
// 全部收束後無實例殘留、串流 goroutine 無洩漏。
func TestMonitorHub_LifecycleStress(t *testing.T) {
	baseGoroutines := runtime.NumGoroutine()

	hub := NewMonitorHub(monErrDialer{}, nil, nil, nil, MonitorConfig{
		ReconnectBase: time.Millisecond,
		ReconnectMax:  2 * time.Millisecond,
		PollInterval:  time.Millisecond,
	})
	const uuid = "stress-uuid"
	const rounds = 100

	var wg sync.WaitGroup
	worker := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				fn()
			}
		}()
	}

	worker(func() { hub.StartMonitoring(uuid) })
	worker(func() { hub.StartMonitoring(uuid) })
	worker(func() { hub.StopMonitoring(uuid) })
	worker(func() { hub.StopMonitoring(uuid) })
	worker(func() {
		_, id := hub.SubscribeStats(uuid)
		hub.UnsubscribeStats(uuid, id)
	})
	worker(func() {
		_, id := hub.SubscribeLogs(uuid)
		hub.UnsubscribeLogs(uuid, id)
	})
	worker(func() { _, _ = hub.Snapshot(uuid) })
	wg.Wait()

	// 收尾:停掉一切,確認實例註冊表清空。
	hub.Close()
	hub.mu.Lock()
	remaining := len(hub.insts)
	hub.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("Close 後仍有 %d 個實例殘留於註冊表", remaining)
	}

	// 串流 goroutine 應在有限時間內全數收束(容忍少量測試框架背景 goroutine)。
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		cur := runtime.NumGoroutine()
		if cur <= baseGoroutines+2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("疑似 goroutine 洩漏:基準 %d,收尾後 %d", baseGoroutines, cur)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- 小工具 ----

func monLineTag(i int) string { return "line-" + strconv.Itoa(i) }

func monIntPtr(v int) *int    { return &v }
func monBoolPtr(v bool) *bool { return &v }

func monAssertIntPtr(t *testing.T, name string, got, want *int) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Fatalf("%s = %v,期望 %v", name, monPtrIntStr(got), monPtrIntStr(want))
	case *got != *want:
		t.Fatalf("%s = %d,期望 %d", name, *got, *want)
	}
}

func monAssertBoolPtr(t *testing.T, name string, got, want *bool) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Fatalf("%s = %v,期望 %v", name, monPtrBoolStr(got), monPtrBoolStr(want))
	case *got != *want:
		t.Fatalf("%s = %v,期望 %v", name, *got, *want)
	}
}

func monPtrIntStr(p *int) string {
	if p == nil {
		return "不適用(nil)"
	}
	return strconv.Itoa(*p)
}

func monPtrBoolStr(p *bool) string {
	if p == nil {
		return "不適用(nil)"
	}
	if *p {
		return "true"
	}
	return "false"
}
