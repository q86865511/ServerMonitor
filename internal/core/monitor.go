package core

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"servermonitor/internal/protocol"
)

// MonitorHub 是 core 側的監控聚合(R6)。它對每個「執行中實例」維護 agent 的 stats/logs WS
// 訂閱(斷線指數退避重連),把資源取樣與即時 log 轉推給 GUI 消費者,並週期性查詢玩家數與
// 線上狀態。log 轉推路徑對每個訂閱者採「有界緩衝 + 丟舊留新 + 丟棄提示行」的背壓策略,使單一
// 慢消費者不阻塞其他訂閱者或轉推迴圈(定義:不因 log 阻塞逾 200ms)。
//
// 依賴以「消費端介面」注入以維持解耦與可測:StreamDialer 開 WS(生產接 NodeClient,單機亦然,
// 預留多節點 routing);CommandSender 送玩家數查詢指令(生產接 CommandService);OnlineProber
// 判線上(生產接 T11 探針);InstanceLookup 取實例記錄與範本(生產接 Store+TemplateEngine)。
// 任一相依為 nil 時對應維度以「不適用」呈現而不報錯。併發安全。
type MonitorHub struct {
	dialer StreamDialer
	sender CommandSender
	prober OnlineProber
	lookup InstanceLookup
	cfg    MonitorConfig
	now    func() time.Time

	mu    sync.Mutex
	insts map[string]*monitoredInstance
	seq   uint64 // 訂閱者 ID 產生器
}

// SubID 標識一個訂閱者,供 Unsubscribe 取消。
type SubID uint64

// StreamDialer 開啟某實例的 stats/logs WS 串流。*core.NodeClient 已滿足此介面(單機直接注入);
// 多節點時由 routing 配接實作。測試以 fake 精準控制斷線/burst。
type StreamDialer interface {
	StatsWS(ctx context.Context, uuid string) (*websocket.Conn, error)
	LogsWS(ctx context.Context, uuid string) (*websocket.Conn, error)
}

// CommandSender 送出一則遊戲指令並回顯(供玩家數 rcon 查詢)。*core.CommandService 已滿足此介面。
type CommandSender interface {
	Send(ctx context.Context, uuid string, cmd protocol.GameCommand) (protocol.CommandResult, error)
}

// OnlineProber 對一個實例做線上探針(rest/rcon/tcp,由 T11 實作);回 nil 視為線上,非 nil 視為離線。
type OnlineProber interface {
	Probe(ctx context.Context, tmpl *protocol.GameTemplate, rec InstanceRecord) error
}

// InstanceLookup 取監控所需的實例記錄與其(已載入)範本。生產以 Store+TemplateEngine 配接
// (見 NewStoreInstanceLookup);測試用 fake。
type InstanceLookup interface {
	Lookup(uuid string) (InstanceRecord, *protocol.GameTemplate, error)
}

// MonitorConfig 是 MonitorHub 的可調參數;零值欄位採預設。
type MonitorConfig struct {
	ReconnectBase   time.Duration // 初始重連退避(預設 500ms)
	ReconnectMax    time.Duration // 退避上限(預設 15s)
	PollInterval    time.Duration // 玩家數/線上探針輪詢週期(預設 10s)
	LogBufferSize   int           // 每個 log 訂閱者的環形緩衝行數(預設 256)
	StatsBufferSize int           // 每個 stats 訂閱者的緩衝筆數(預設 8)
}

const (
	monitorReconnectBase   = 500 * time.Millisecond
	monitorReconnectMax    = 15 * time.Second
	monitorPollInterval    = 10 * time.Second
	monitorLogBufferSize   = 256
	monitorStatsBufferSize = 8

	// LogStreamMonitor 是監控注入之提示行(如「已丟棄 N 行」)的 Stream 值,供 GUI 與真實
	// stdout/stderr 區分並施以不同樣式。
	LogStreamMonitor = "gsm-monitor"
)

// LogLine 是轉推給 log 訂閱者的一行伺服器輸出。刻意於 core 定義本地型別(對齊 agent WS /logs 的
// wire 格式 ts_utc/stream/line),而不相依 agent 套件,維持 core↔agent 僅經 HTTP 契約解耦的架構約定。
type LogLine struct {
	TsUTC  time.Time `json:"ts_utc"`
	Stream string    `json:"stream"` // "stdout" | "stderr" | LogStreamMonitor
	Line   string    `json:"line"`
}

// MonitorSnapshot 是一個實例的聚合監控快照,供 GUI 卡片顯示(R6)。PlayerCount/Online 為 nil
// 表示「不適用」(範本未定義查詢/探針,或相依未注入)。
type MonitorSnapshot struct {
	UUID          string
	HasStats      bool                   // 是否已收到至少一筆資源取樣(否則 Stats 為零值)
	Stats         protocol.ResourceStats // 最新資源取樣:CPU%(正規化)/記憶體/資料磁碟
	PlayerCount   *int                   // 權威玩家數;nil = 不適用
	Online        *bool                  // 權威線上狀態;nil = 不適用
	ObservedState protocol.InstanceState // 最近一次輪詢取得的觀測狀態
}

// NewMonitorHub 建立監控聚合。dialer 必填(否則無法訂閱 WS);sender/prober/lookup 可為 nil
// (對應維度顯示「不適用」)。cfg 零值欄位採預設。
func NewMonitorHub(dialer StreamDialer, sender CommandSender, prober OnlineProber, lookup InstanceLookup, cfg MonitorConfig) *MonitorHub {
	if cfg.ReconnectBase <= 0 {
		cfg.ReconnectBase = monitorReconnectBase
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = monitorReconnectMax
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = monitorPollInterval
	}
	if cfg.LogBufferSize <= 0 {
		cfg.LogBufferSize = monitorLogBufferSize
	}
	if cfg.StatsBufferSize <= 0 {
		cfg.StatsBufferSize = monitorStatsBufferSize
	}
	return &MonitorHub{
		dialer: dialer,
		sender: sender,
		prober: prober,
		lookup: lookup,
		cfg:    cfg,
		now:    time.Now,
		insts:  make(map[string]*monitoredInstance),
	}
}

// StartMonitoring 開始為一個執行中實例維護 stats/logs 訂閱與週期輪詢(冪等:已在監控則 no-op)。
// 由生命週期編排在實例轉為 Running 時呼叫。
func (h *MonitorHub) StartMonitoring(uuid string) {
	// 全程持 h.mu:map 只保存「未 stop」的實例,持鎖跨越「取得 mi → 提交 streaming/wg.Add」
	// 使並發的 StopMonitoring 無法在中途移除並 stop() 此 mi(否則會對孤兒 mi 起 goroutine,
	// 且 wg.Add 會與 stop() 的 wg.Wait 並發 → WaitGroup 誤用)。鎖序固定 h.mu → mi.mu。
	h.mu.Lock()
	mi := h.getOrCreateLocked(uuid)
	mi.mu.Lock()
	if mi.streaming {
		mi.mu.Unlock()
		h.mu.Unlock()
		return
	}
	mi.streaming = true
	ctx, cancel := context.WithCancel(context.Background())
	mi.cancel = cancel
	mi.wg.Add(3) // 在 mi 仍於 map(h.mu 持有)時 Add,嚴格 happens-before 任何 stop() 的 Wait
	mi.mu.Unlock()
	h.mu.Unlock()

	go h.runStats(ctx, mi)
	go h.runLogs(ctx, mi)
	go h.runPoll(ctx, mi)
}

// StopMonitoring 停止一個實例的監控:取消所有串流/輪詢,關閉上游 WS,並關閉其所有訂閱者 channel
// (消費端 range 收束)。由生命週期編排在實例停止時呼叫。冪等。
func (h *MonitorHub) StopMonitoring(uuid string) {
	h.mu.Lock()
	mi, ok := h.insts[uuid]
	if ok {
		delete(h.insts, uuid)
	}
	h.mu.Unlock()
	if ok {
		mi.stop()
	}
}

// Close 停止所有實例的監控(供整體關閉)。
func (h *MonitorHub) Close() {
	h.mu.Lock()
	insts := make([]*monitoredInstance, 0, len(h.insts))
	for _, mi := range h.insts {
		insts = append(insts, mi)
	}
	h.insts = make(map[string]*monitoredInstance)
	h.mu.Unlock()
	for _, mi := range insts {
		mi.stop()
	}
}

// SubscribeStats 註冊一個資源取樣訂閱者,回傳其接收 channel 與取消用 SubID。訂閱在 StartMonitoring
// 之前或之後皆可;取樣僅於監控啟用期間流入。慢消費者採「丟舊留新」不阻塞轉推。
func (h *MonitorHub) SubscribeStats(uuid string) (<-chan protocol.ResourceStats, SubID) {
	ch := make(chan protocol.ResourceStats, h.cfg.StatsBufferSize)
	// 持 h.mu 跨越「取得 mi → 掛上訂閱」:避免掛到已被 Stop 移除並關閉訂閱的孤兒 mi(channel 永不關閉)。
	h.mu.Lock()
	mi := h.getOrCreateLocked(uuid)
	id := SubID(h.nextSeqLocked())
	mi.mu.Lock()
	mi.statsSubs[id] = ch
	mi.mu.Unlock()
	h.mu.Unlock()
	return ch, id
}

// UnsubscribeStats 取消一個資源取樣訂閱並關閉其 channel。
func (h *MonitorHub) UnsubscribeStats(uuid string, id SubID) {
	// 持 h.mu 跨越查找與關閉:與 Stop/Close(需 h.mu 才能移除實例後 stop() 關閉訂閱)互斥,
	// 保證同一 channel 只被關閉一次。
	h.mu.Lock()
	defer h.mu.Unlock()
	mi, ok := h.insts[uuid]
	if !ok {
		return
	}
	mi.mu.Lock()
	if ch, ok := mi.statsSubs[id]; ok {
		delete(mi.statsSubs, id)
		close(ch)
	}
	mi.mu.Unlock()
}

// SubscribeLogs 註冊一個即時 log 訂閱者,回傳其接收 channel 與取消用 SubID。channel 帶有界緩衝;
// 消費過慢時丟最舊、留最新,並在有空位時注入一行 Stream=LogStreamMonitor 的「已丟棄 N 行」提示。
func (h *MonitorHub) SubscribeLogs(uuid string) (<-chan LogLine, SubID) {
	sub := &logSubscriber{ch: make(chan LogLine, h.cfg.LogBufferSize)}
	// 持 h.mu 跨越「取得 mi → 掛上訂閱」:理由同 SubscribeStats。
	h.mu.Lock()
	mi := h.getOrCreateLocked(uuid)
	id := SubID(h.nextSeqLocked())
	mi.mu.Lock()
	mi.logSubs[id] = sub
	mi.mu.Unlock()
	h.mu.Unlock()
	return sub.ch, id
}

// UnsubscribeLogs 取消一個 log 訂閱並關閉其 channel。
func (h *MonitorHub) UnsubscribeLogs(uuid string, id SubID) {
	// 持 h.mu 跨越查找與關閉:理由同 UnsubscribeStats。
	h.mu.Lock()
	defer h.mu.Unlock()
	mi, ok := h.insts[uuid]
	if !ok {
		return
	}
	mi.mu.Lock()
	if sub, ok := mi.logSubs[id]; ok {
		delete(mi.logSubs, id)
		close(sub.ch)
	}
	mi.mu.Unlock()
}

// Snapshot 回傳一個實例的聚合監控快照;第二回傳值表示該實例是否在監控中(或曾被訂閱)。
func (h *MonitorHub) Snapshot(uuid string) (MonitorSnapshot, bool) {
	h.mu.Lock()
	mi, ok := h.insts[uuid]
	h.mu.Unlock()
	if !ok {
		return MonitorSnapshot{}, false
	}
	mi.mu.Lock()
	defer mi.mu.Unlock()
	return MonitorSnapshot{
		UUID:          uuid,
		HasStats:      mi.hasStats,
		Stats:         mi.lastStats,
		PlayerCount:   copyIntPtr(mi.playerCount),
		Online:        copyBoolPtr(mi.online),
		ObservedState: mi.observed,
	}, true
}

// getOrCreateLocked 取得(或建立一個尚未串流的)實例容器。呼叫者須持 h.mu。
func (h *MonitorHub) getOrCreateLocked(uuid string) *monitoredInstance {
	mi, ok := h.insts[uuid]
	if !ok {
		mi = &monitoredInstance{
			uuid:      uuid,
			statsSubs: make(map[SubID]chan protocol.ResourceStats),
			logSubs:   make(map[SubID]*logSubscriber),
		}
		h.insts[uuid] = mi
	}
	return mi
}

func (h *MonitorHub) nextSeqLocked() uint64 {
	h.seq++
	return h.seq
}

// ---- 串流迴圈 ----

// runStats 維護 stats WS 訂閱:斷線指數退避重連,直到 ctx 取消。
func (h *MonitorHub) runStats(ctx context.Context, mi *monitoredInstance) {
	defer mi.wg.Done()
	backoff := h.cfg.ReconnectBase
	for ctx.Err() == nil {
		conn, err := h.dialer.StatsWS(ctx, mi.uuid)
		if err != nil {
			if !h.backoffSleep(ctx, &backoff) {
				return
			}
			continue
		}
		backoff = h.cfg.ReconnectBase // 連上即重置退避
		h.pumpStats(ctx, mi, conn)
		if !h.backoffSleep(ctx, &backoff) {
			return
		}
	}
}

// pumpStats 讀取一個 stats 連線的取樣 frame 並轉推,直到斷線或 ctx 取消。
func (h *MonitorHub) pumpStats(ctx context.Context, mi *monitoredInstance, conn *websocket.Conn) {
	stop := closeOnCancel(ctx, conn)
	defer stop()
	defer conn.Close()
	for {
		var s protocol.ResourceStats
		if err := conn.ReadJSON(&s); err != nil {
			return
		}
		mi.fanoutStats(s)
	}
}

// runLogs 維護 logs WS 訂閱:斷線指數退避重連,直到 ctx 取消。
func (h *MonitorHub) runLogs(ctx context.Context, mi *monitoredInstance) {
	defer mi.wg.Done()
	backoff := h.cfg.ReconnectBase
	for ctx.Err() == nil {
		conn, err := h.dialer.LogsWS(ctx, mi.uuid)
		if err != nil {
			if !h.backoffSleep(ctx, &backoff) {
				return
			}
			continue
		}
		backoff = h.cfg.ReconnectBase
		h.pumpLogs(ctx, mi, conn)
		if !h.backoffSleep(ctx, &backoff) {
			return
		}
	}
}

// pumpLogs 讀取一個 logs 連線的行 frame 並轉推,直到斷線或 ctx 取消。
func (h *MonitorHub) pumpLogs(ctx context.Context, mi *monitoredInstance, conn *websocket.Conn) {
	stop := closeOnCancel(ctx, conn)
	defer stop()
	defer conn.Close()
	for {
		var ln LogLine
		if err := conn.ReadJSON(&ln); err != nil {
			return
		}
		mi.fanoutLog(ln, h.now())
	}
}

// runPoll 週期查詢玩家數與線上狀態並更新快照,直到 ctx 取消。啟動時立即先跑一次。
func (h *MonitorHub) runPoll(ctx context.Context, mi *monitoredInstance) {
	defer mi.wg.Done()
	h.pollOnce(ctx, mi)
	t := time.NewTicker(h.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.pollOnce(ctx, mi)
		}
	}
}

// pollOnce 執行一輪玩家數 + 線上狀態查詢並寫回實例快照。任一查詢失敗/不支援 → 對應維度為「不適用」。
func (h *MonitorHub) pollOnce(ctx context.Context, mi *monitoredInstance) {
	var (
		rec  InstanceRecord
		tmpl *protocol.GameTemplate
		ok   bool
	)
	if h.lookup != nil {
		if r, t, err := h.lookup.Lookup(mi.uuid); err == nil {
			rec, tmpl, ok = r, t, true
		}
	}
	online := h.probeOnline(ctx, tmpl, rec, ok)
	players := h.queryPlayers(ctx, mi.uuid, tmpl, ok)

	mi.mu.Lock()
	if !mi.stopped {
		if ok {
			mi.observed = rec.ObservedState
		}
		mi.online = online
		mi.playerCount = players
	}
	mi.mu.Unlock()
}

// probeOnline 以注入的探針判定線上狀態;無探針、查無實例或範本無 health → nil(不適用)。
func (h *MonitorHub) probeOnline(ctx context.Context, tmpl *protocol.GameTemplate, rec InstanceRecord, ok bool) *bool {
	if h.prober == nil || !ok || tmpl == nil || tmpl.Health == nil {
		return nil
	}
	up := h.prober.Probe(ctx, tmpl, rec) == nil
	return &up
}

// queryPlayers 依範本 players_query 查玩家數;無 sender、查無實例、範本未定義查詢、非 rcon 或
// 解析失敗 → nil(不適用)。首版內建 Minecraft rcon `list` 回應解析。
func (h *MonitorHub) queryPlayers(ctx context.Context, uuid string, tmpl *protocol.GameTemplate, ok bool) *int {
	if h.sender == nil || !ok || tmpl == nil || tmpl.PlayersQuery == nil {
		return nil
	}
	pq := tmpl.PlayersQuery
	if pq.Kind != "rcon" || pq.Command == "" {
		return nil // 首版僅支援 rcon 查詢的內建解析;其餘顯示不適用而不報錯
	}
	res, err := h.sender.Send(ctx, uuid, protocol.GameCommand{Raw: pq.Command})
	if err != nil || !res.Success {
		return nil
	}
	n, parsed := parsePlayerCount(res.Output)
	if !parsed {
		return nil
	}
	return &n
}

// backoffSleep 依 ctx 睡眠 *d,然後把 *d 指數放大(capped ReconnectMax)。ctx 取消回 false。
func (h *MonitorHub) backoffSleep(ctx context.Context, d *time.Duration) bool {
	wait := *d
	if wait <= 0 {
		wait = h.cfg.ReconnectBase
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	}
	next := *d * 2
	if next > h.cfg.ReconnectMax {
		next = h.cfg.ReconnectMax
	}
	*d = next
	return true
}

// ---- 每實例狀態 ----

// monitoredInstance 是一個被監控實例的執行期狀態:串流生命週期(cancel/wg/streaming/stopped)、
// 訂閱者集合與最新聚合值。data 欄位、訂閱者集合與生命週期旗標皆由 mu 保護。
//
// 生命週期語意:mi 一旦被 hub 從 insts map 移除即呼叫 stop() 進入終態(stopped=true),此後
// 不再處理串流轉推,且 stop() 冪等。因 map 僅保存未 stop 的 mi、且增刪由 h.mu 序列化,
// StartMonitoring/Subscribe* 取得的 mi 必為存活狀態,不會對孤兒 mi 起 goroutine 或掛訂閱。
type monitoredInstance struct {
	uuid string

	mu        sync.Mutex
	streaming bool
	stopped   bool // 終態:已(或正在)stop,轉推短路、stop() 冪等
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	statsSubs map[SubID]chan protocol.ResourceStats
	logSubs   map[SubID]*logSubscriber

	lastStats   protocol.ResourceStats
	hasStats    bool
	playerCount *int
	online      *bool
	observed    protocol.InstanceState
}

// stop 取消串流、等待 goroutine 收束,再關閉所有訂閱者 channel(此時無 sender 殘留,關閉安全)。
// 冪等:重複呼叫在標記 stopped 後即返回,不重覆 Wait/close。wg.Wait 不在任何鎖內執行(避免與
// 轉推路徑死結)。
func (mi *monitoredInstance) stop() {
	mi.mu.Lock()
	if mi.stopped {
		mi.mu.Unlock()
		return
	}
	mi.stopped = true
	cancel := mi.cancel
	mi.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	mi.wg.Wait()

	mi.mu.Lock()
	for id, ch := range mi.statsSubs {
		close(ch)
		delete(mi.statsSubs, id)
	}
	for id, sub := range mi.logSubs {
		close(sub.ch)
		delete(mi.logSubs, id)
	}
	mi.streaming = false
	mi.cancel = nil
	mi.mu.Unlock()
}

// fanoutStats 更新最新取樣並以「丟舊留新」非阻塞方式轉推給所有 stats 訂閱者。
func (mi *monitoredInstance) fanoutStats(s protocol.ResourceStats) {
	mi.mu.Lock()
	if mi.stopped {
		mi.mu.Unlock()
		return
	}
	mi.lastStats = s
	mi.hasStats = true
	for _, ch := range mi.statsSubs {
		if trySend(ch, s) {
			continue
		}
		tryRecv(ch)    // 滿:丟最舊一筆
		trySend(ch, s) // 留最新
	}
	mi.mu.Unlock()
}

// fanoutLog 以背壓策略非阻塞轉推一行 log 給所有訂閱者(見 logSubscriber.push)。
func (mi *monitoredInstance) fanoutLog(ln LogLine, now time.Time) {
	mi.mu.Lock()
	if mi.stopped {
		mi.mu.Unlock()
		return
	}
	for _, sub := range mi.logSubs {
		sub.push(ln, now)
	}
	mi.mu.Unlock()
}

// logSubscriber 是一個 log 訂閱者的有界緩衝與待決丟棄計數。
type logSubscriber struct {
	ch      chan LogLine
	dropped int // 尚未以提示行告知消費者的累計丟棄行數
}

// push 把一行 log 塞入訂閱者緩衝,全程非阻塞(不阻塞轉推迴圈):
//   - 若有待決丟棄且緩衝有空位,先送一行「已丟棄 N 行」提示並清零計數;
//   - 緩衝滿時丟最舊一行、計數 +1,再塞入新行(留新)。
func (s *logSubscriber) push(ln LogLine, now time.Time) {
	if s.dropped > 0 {
		notice := LogLine{
			TsUTC:  now,
			Stream: LogStreamMonitor,
			Line:   fmt.Sprintf("已丟棄 %d 行(消費過慢)", s.dropped),
		}
		if trySend(s.ch, notice) {
			s.dropped = 0
		}
	}
	if trySend(s.ch, ln) {
		return
	}
	if _, ok := tryRecv(s.ch); ok {
		s.dropped++
	}
	if !trySend(s.ch, ln) {
		s.dropped++
	}
}

// ---- 輔助 ----

// mcListRe 解析 Minecraft rcon `list` 回應:「There are N of a max of M players online」。
var mcListRe = regexp.MustCompile(`There are (\d+) of a max of \d+ players online`)

// parsePlayerCount 從 rcon `list` 回應解析線上玩家數;不符格式回 (0,false)(呼叫端轉「不適用」)。
func parsePlayerCount(out string) (int, bool) {
	m := mcListRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// closeOnCancel 在 ctx 取消時關閉 conn(以中斷阻塞中的 ReadJSON);回傳的函式須於讀迴圈結束時呼叫
// 以收束 watcher goroutine(避免洩漏)。
func closeOnCancel(ctx context.Context, conn *websocket.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func trySend[T any](ch chan T, v T) bool {
	select {
	case ch <- v:
		return true
	default:
		return false
	}
}

func tryRecv[T any](ch chan T) (T, bool) {
	select {
	case v := <-ch:
		return v, true
	default:
		var zero T
		return zero, false
	}
}

func copyIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyBoolPtr(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// storeInstanceLookup 以 Store + TemplateEngine 實作 InstanceLookup(生產配接)。
type storeInstanceLookup struct {
	store  *Store
	engine *TemplateEngine
}

// NewStoreInstanceLookup 建立以 Store + TemplateEngine 為後端的 InstanceLookup(供 GUI 組裝 MonitorHub)。
func NewStoreInstanceLookup(store *Store, engine *TemplateEngine) InstanceLookup {
	return storeInstanceLookup{store: store, engine: engine}
}

func (l storeInstanceLookup) Lookup(uuid string) (InstanceRecord, *protocol.GameTemplate, error) {
	rec, err := l.store.GetInstance(uuid)
	if err != nil {
		return InstanceRecord{}, nil, err
	}
	tmpl, ok := l.engine.Get(rec.TemplateID)
	if !ok {
		return rec, nil, fmt.Errorf("實例 %s 的範本 %q 未載入", uuid, rec.TemplateID)
	}
	return rec, tmpl, nil
}

// registryStreamDialer 是委派 NodeRegistry 的 StreamDialer:每次串流呼叫都自 registry 取「當前」
// 節點 NodeClient(而非建構時固定注入的一份),藉此一次解決兩個問題(T15 雙審 #1):
//
//	(a) 離線啟動時代理尚未建立,不再需要把 nil client 注入 MonitorHub(避免 stats/logs goroutine
//	    對 nil 撥號 deref);registry 已註冊佔位 client,撥號自然失敗回 ErrNodeUnreachable。
//	(b) RetryDocker 重建代理後以新 client 覆蓋 registry 登錄,MonitorHub 下一次(重連)撥號即
//	    自動改用新連線,無需重啟應用。
//
// registry 尚無登錄(未註冊)時回 ErrNodeUnreachable 包裝,交 MonitorHub 既有退避重連。
type registryStreamDialer struct {
	registry *NodeRegistry
	node     string
}

// NewRegistryStreamDialer 建立委派 registry 的 StreamDialer(供單機 MonitorHub 注入)。
func NewRegistryStreamDialer(registry *NodeRegistry, node string) StreamDialer {
	return registryStreamDialer{registry: registry, node: node}
}

func (d registryStreamDialer) StatsWS(ctx context.Context, uuid string) (*websocket.Conn, error) {
	c, err := d.registry.Client(d.node)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNodeUnreachable, err)
	}
	return c.StatsWS(ctx, uuid)
}

func (d registryStreamDialer) LogsWS(ctx context.Context, uuid string) (*websocket.Conn, error) {
	c, err := d.registry.Client(d.node)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNodeUnreachable, err)
	}
	return c.LogsWS(ctx, uuid)
}

// 確保 *NodeClient 滿足 StreamDialer(單機直接注入)。
var _ StreamDialer = (*NodeClient)(nil)

// 確保 registryStreamDialer 滿足 StreamDialer。
var _ StreamDialer = registryStreamDialer{}

// 確保 *CommandService 滿足 CommandSender。
var _ CommandSender = (*CommandService)(nil)
