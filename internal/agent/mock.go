package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// ErrNotFound 表示指定的 RuntimeID 或 BackupID 不存在。
var ErrNotFound = errors.New("agent: runtime or backup not found")

// ErrBackendClosed 表示後端已 Close,無法再開新串流。
var ErrBackendClosed = errors.New("agent: backend closed")

// ErrSlowConsumer 表示事件串流消費過慢、緩衝溢出而被關閉;消費端應以最後游標重連對帳。
var ErrSlowConsumer = errors.New("agent: event stream consumer too slow")

const (
	defaultEventHistory = 1024 // Events 緩衝保留的事件數上限
	defaultSubBuffer    = 64   // 每個訂閱者的即時通道緩衝
)

// mockInstance 是 MockBackend 記憶體中的單一實例狀態。
type mockInstance struct {
	id         protocol.RuntimeID
	spec       protocol.InstanceSpec
	state      protocol.RuntimeState
	image      string
	labels     map[string]string
	ports      []protocol.PortBinding
	createdAt  time.Time
	startedAt  *time.Time
	finishedAt *time.Time
	exitCode   *int
	health     string
}

// mockBackup 是一份記憶體備份:中繼 + 供 Restore 重建的 spec 快照。
type mockBackup struct {
	meta protocol.BackupMeta
	spec protocol.InstanceSpec
}

// MockBackend 是 RuntimeBackend 的記憶體實作(R4),供單元/整合測試在無 Docker 下
// 跑通生命週期與崩潰事件路徑。所有狀態存記憶體;Events 支援外部注入事件
// (InjectDie/InjectHealth/InjectEvent)以模擬容器 die 崩潰等,並支援 cursor 重連對帳。
// 併發安全。
type MockBackend struct {
	mu sync.Mutex

	instSeq   int64
	backupSeq int64
	evSeq     int64
	subSeq    int

	insts   map[protocol.RuntimeID]*mockInstance
	backups map[protocol.BackupID]mockBackup

	history     []RuntimeEvent // 事件緩衝(cursor 遞增),供漏事件對帳
	historyMax  int
	evictedUpTo int64 // 已逐出的最大 cursor 數值;請求游標 < 此值即判定漏事件

	subs map[int]*eventSub

	// 可控假資料鉤子(nil 時用內建預設)。
	LogLinesFunc func(protocol.RuntimeID) []LogLine
	StatsFunc    func(protocol.RuntimeID) protocol.ResourceStats

	closed bool
}

// NewMockBackend 建立一個空的 MockBackend。
func NewMockBackend() *MockBackend {
	return &MockBackend{
		insts:      make(map[protocol.RuntimeID]*mockInstance),
		backups:    make(map[protocol.BackupID]mockBackup),
		subs:       make(map[int]*eventSub),
		historyMax: defaultEventHistory,
	}
}

var _ RuntimeBackend = (*MockBackend)(nil)

// Create 依規格建立一個實例(Created 狀態),回傳指派的 RuntimeID。
func (m *MockBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	m.instSeq++
	id := protocol.RuntimeID(fmt.Sprintf("mock-%d", m.instSeq))
	m.insts[id] = &mockInstance{
		id:        id,
		spec:      spec,
		state:     protocol.RuntimeStateCreated,
		image:     spec.Image,
		labels:    cloneStringMap(spec.Labels),
		ports:     append([]protocol.PortBinding(nil), spec.Ports...),
		createdAt: time.Now().UTC(),
		health:    "none",
	}
	return id, nil
}

// Start 啟動實例,轉為 Running 並發出 start 事件。
func (m *MockBackend) Start(ctx context.Context, id protocol.RuntimeID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	inst, ok := m.insts[id]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	inst.state = protocol.RuntimeStateRunning
	inst.startedAt = &now
	inst.finishedAt = nil
	inst.exitCode = nil
	inst.health = "healthy"
	m.emitLocked(RuntimeEvent{ID: id, Kind: RuntimeEventStart, TsUTC: now})
	return nil
}

// Stop 計畫性停止實例,轉為 Stopped 並發出 die 事件(exit 0)。
// opts.Grace 於 Mock 無實際程序可等待,故忽略;planned 與否不落在事件上(見 StopOpts)。
func (m *MockBackend) Stop(ctx context.Context, id protocol.RuntimeID, _ StopOpts) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	inst, ok := m.insts[id]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	code := 0
	inst.state = protocol.RuntimeStateStopped
	inst.finishedAt = &now
	inst.exitCode = &code
	inst.health = "none"
	m.emitLocked(RuntimeEvent{ID: id, Kind: RuntimeEventDie, TsUTC: now, ExitCode: &code})
	return nil
}

// Status 回傳 runtime 層即時狀態。
func (m *MockBackend) Status(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return protocol.RuntimeStatus{}, err
	}
	inst, ok := m.insts[id]
	if !ok {
		return protocol.RuntimeStatus{}, ErrNotFound
	}
	return statusOf(inst), nil
}

// List 列舉所有 runtime(依 ID 排序,輸出穩定)。
func (m *MockBackend) List(ctx context.Context) ([]protocol.RuntimeRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	refs := make([]protocol.RuntimeRef, 0, len(m.insts))
	for _, inst := range m.insts {
		refs = append(refs, protocol.RuntimeRef{
			ID:     inst.id,
			State:  inst.state,
			Labels: cloneStringMap(inst.labels),
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

// Inspect 回傳單一 runtime 的詳細資訊。
func (m *MockBackend) Inspect(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return protocol.RuntimeInfo{}, err
	}
	inst, ok := m.insts[id]
	if !ok {
		return protocol.RuntimeInfo{}, ErrNotFound
	}
	return protocol.RuntimeInfo{
		ID:        inst.id,
		State:     inst.state,
		Image:     inst.image,
		Labels:    cloneStringMap(inst.labels),
		Ports:     append([]protocol.PortBinding(nil), inst.ports...),
		CreatedAt: inst.createdAt,
		Status:    statusOf(inst),
	}, nil
}

// Remove 移除實例;Purge 時一併刪除該實例 UUID 的備份。
func (m *MockBackend) Remove(ctx context.Context, id protocol.RuntimeID, opts RemoveOpts) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	inst, ok := m.insts[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.insts, id)
	if opts.Purge {
		uuid := inst.spec.UUID
		for bid, b := range m.backups {
			if b.meta.InstanceUUID == uuid {
				delete(m.backups, bid)
			}
		}
	}
	return nil
}

// Logs 回傳 log 串流,吐出可控的假 log 行後(非 follow)關閉。
func (m *MockBackend) Logs(ctx context.Context, id protocol.RuntimeID, opts LogOpts) (LogStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if _, ok := m.insts[id]; !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	lines := m.logLinesLocked(id)
	m.mu.Unlock()
	return newMockLogStream(ctx, lines, opts.Follow), nil
}

// ExecProcess 在容器內執行程序(Mock 回可控假結果)。
func (m *MockBackend) ExecProcess(ctx context.Context, id protocol.RuntimeID, cmd ExecCmd) (ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return ExecResult{}, err
	}
	if _, ok := m.insts[id]; !ok {
		return ExecResult{}, ErrNotFound
	}
	return ExecResult{ExitCode: 0, Stdout: "mock exec: " + strings.Join(cmd.Cmd, " ")}, nil
}

// Stats 回傳可控的假資源取樣。
func (m *MockBackend) Stats(ctx context.Context, id protocol.RuntimeID) (protocol.ResourceStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return protocol.ResourceStats{}, err
	}
	if _, ok := m.insts[id]; !ok {
		return protocol.ResourceStats{}, ErrNotFound
	}
	if m.StatsFunc != nil {
		return m.StatsFunc(id), nil
	}
	return protocol.ResourceStats{
		TsUTC:       time.Now().UTC(),
		CPUPercent:  12.5,
		MemoryBytes: 256 << 20,
		MemoryLimit: 1 << 30,
	}, nil
}

// Archive 產生一份記憶體備份(含 spec 快照與假 checksum),回傳 opaque BackupID。
func (m *MockBackend) Archive(ctx context.Context, id protocol.RuntimeID) (protocol.BackupID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	inst, ok := m.insts[id]
	if !ok {
		return "", ErrNotFound
	}
	m.backupSeq++
	bid := protocol.BackupID(fmt.Sprintf("mock-backup-%d", m.backupSeq))
	m.backups[bid] = mockBackup{
		meta: protocol.BackupMeta{
			BackupID:     bid,
			InstanceUUID: inst.spec.UUID,
			Game:         inst.spec.TemplateID,
			TsUTC:        time.Now().UTC(),
			Checksum:     fmt.Sprintf("mock-sha256-%d", m.backupSeq),
		},
		spec: inst.spec,
	}
	return bid, nil
}

// Restore 以備份重建一個「新的」實例(Created 狀態),回傳新 RuntimeID(R9)。
// 舊實例保留,原子切換/移除由 core 編排。
func (m *MockBackend) Restore(ctx context.Context, id protocol.RuntimeID, b protocol.BackupID) (protocol.RuntimeID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	if _, ok := m.insts[id]; !ok {
		return "", ErrNotFound
	}
	bk, ok := m.backups[b]
	if !ok {
		return "", ErrNotFound
	}
	m.instSeq++
	newID := protocol.RuntimeID(fmt.Sprintf("mock-%d", m.instSeq))
	m.insts[newID] = &mockInstance{
		id:        newID,
		spec:      bk.spec,
		state:     protocol.RuntimeStateCreated,
		image:     bk.spec.Image,
		labels:    cloneStringMap(bk.spec.Labels),
		ports:     append([]protocol.PortBinding(nil), bk.spec.Ports...),
		createdAt: time.Now().UTC(),
		health:    "none",
	}
	return newID, nil
}

// Events 訂閱執行事件串流。since 語意見 RuntimeBackend.Events。
func (m *MockBackend) Events(ctx context.Context, since protocol.Cursor) (EventStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrBackendClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sinceN, hasSince := parseCursor(since)
	var replay []RuntimeEvent
	gap := false
	if hasSince {
		if sinceN < m.evictedUpTo {
			gap = true // 請求游標之後的部分事件已被逐出 → 漏事件
		}
		for _, ev := range m.history {
			if cn, ok := parseCursor(ev.Cursor); ok && cn > sinceN {
				replay = append(replay, ev)
			}
		}
	}

	buf := len(replay) + defaultSubBuffer
	if gap {
		buf++
	}
	m.subSeq++
	sub := &eventSub{
		id:   m.subSeq,
		in:   make(chan RuntimeEvent, buf),
		quit: make(chan struct{}),
	}
	// 補送資料在註冊前預載進緩衝(容量已計入),不會阻塞;之後的即時事件由 emit 續推。
	if gap {
		sub.in <- RuntimeEvent{Kind: RuntimeEventResync, TsUTC: time.Now().UTC()}
	}
	for _, ev := range replay {
		sub.in <- ev
	}
	m.subs[sub.id] = sub
	return newMockEventStream(ctx, m, sub), nil
}

// Close 關閉後端並終止所有事件串流。
func (m *MockBackend) Close() error {
	m.mu.Lock()
	m.closed = true
	subs := make([]*eventSub, 0, len(m.subs))
	for _, s := range m.subs {
		subs = append(subs, s)
	}
	m.subs = make(map[int]*eventSub)
	m.mu.Unlock()
	for _, s := range subs {
		s.stop(nil)
	}
	return nil
}

// ---- 事件注入(供測試 / T8 上層模擬)----

// InjectEvent 注入一則自訂執行事件,回傳指派的 Cursor。
func (m *MockBackend) InjectEvent(ev RuntimeEvent) protocol.Cursor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.emitLocked(ev)
}

// InjectDie 模擬容器非計畫性結束(崩潰路徑,R8):發出帶 exitCode 的 die 事件,
// 並把該實例(若存在)轉為 exited。回傳指派的 Cursor。
func (m *MockBackend) InjectDie(id protocol.RuntimeID, exitCode int) protocol.Cursor {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	code := exitCode
	if inst, ok := m.insts[id]; ok {
		inst.state = protocol.RuntimeStateExited
		inst.finishedAt = &now
		inst.exitCode = &code
		inst.health = "none"
	}
	return m.emitLocked(RuntimeEvent{ID: id, Kind: RuntimeEventDie, TsUTC: now, ExitCode: &code})
}

// InjectHealth 注入健康狀態變更事件(R8 存活探針路徑)。
func (m *MockBackend) InjectHealth(id protocol.RuntimeID, health string) protocol.Cursor {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst, ok := m.insts[id]; ok {
		inst.health = health
	}
	return m.emitLocked(RuntimeEvent{ID: id, Kind: RuntimeEventHealth, TsUTC: time.Now().UTC(), Health: health})
}

// ---- 內部輔助(呼叫端須持 m.mu)----

// emitLocked 指派 cursor、寫入緩衝(必要時逐出最舊),並非阻塞地推給所有訂閱者。
// 消費過慢的訂閱者會被關閉並移除(回傳 ErrSlowConsumer),消費端應以最後游標重連。
func (m *MockBackend) emitLocked(ev RuntimeEvent) protocol.Cursor {
	m.evSeq++
	cur := protocol.Cursor(strconv.FormatInt(m.evSeq, 10))
	ev.Cursor = cur
	if ev.TsUTC.IsZero() {
		ev.TsUTC = time.Now().UTC()
	}
	m.history = append(m.history, ev)
	if m.historyMax > 0 && len(m.history) > m.historyMax {
		drop := len(m.history) - m.historyMax
		if dn, ok := parseCursor(m.history[drop-1].Cursor); ok && dn > m.evictedUpTo {
			m.evictedUpTo = dn
		}
		m.history = append([]RuntimeEvent(nil), m.history[drop:]...)
	}
	for sid, sub := range m.subs {
		select {
		case sub.in <- ev:
		default:
			sub.stop(ErrSlowConsumer)
			delete(m.subs, sid)
		}
	}
	return cur
}

func (m *MockBackend) logLinesLocked(id protocol.RuntimeID) []LogLine {
	if m.LogLinesFunc != nil {
		return m.LogLinesFunc(id)
	}
	now := time.Now().UTC()
	return []LogLine{
		{TsUTC: now, Stream: "stdout", Line: fmt.Sprintf("mock %s: starting", id)},
		{TsUTC: now, Stream: "stdout", Line: fmt.Sprintf("mock %s: ready", id)},
	}
}

// unregister 從 subs 移除並終止一個訂閱者(供串流關閉時呼叫)。
func (m *MockBackend) unregister(sub *eventSub) {
	m.mu.Lock()
	delete(m.subs, sub.id)
	m.mu.Unlock()
	sub.stop(nil)
}

func statusOf(inst *mockInstance) protocol.RuntimeStatus {
	return protocol.RuntimeStatus{
		ID:         inst.id,
		State:      inst.state,
		Running:    inst.state == protocol.RuntimeStateRunning,
		ExitCode:   copyIntPtr(inst.exitCode),
		Health:     inst.health,
		StartedAt:  copyTimePtr(inst.startedAt),
		FinishedAt: copyTimePtr(inst.finishedAt),
	}
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func parseCursor(c protocol.Cursor) (int64, bool) {
	if c == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(string(c), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyTimePtr(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// ---- 串流實作 ----

// eventSub 是一個事件訂閱者:in 由 emit(持 m.mu)寫入,pump 由此讀出轉推給消費端。
type eventSub struct {
	id   int
	in   chan RuntimeEvent
	quit chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func (s *eventSub) stop(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.quit)
	})
}

func (s *eventSub) getErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// mockEventStream 以一條 pump goroutine 把 sub.in 轉推到 out,並在 ctx 取消 /
// Close() / quit 時收束、關閉 out 並自 subs 移除。
type mockEventStream struct {
	out chan RuntimeEvent
	sub *eventSub
	m   *MockBackend
}

func newMockEventStream(ctx context.Context, m *MockBackend, sub *eventSub) *mockEventStream {
	out := make(chan RuntimeEvent)
	st := &mockEventStream{out: out, sub: sub, m: m}
	go func() {
		defer close(out)
		defer m.unregister(sub)
		for {
			select {
			case <-ctx.Done():
				sub.stop(ctx.Err())
				return
			case <-sub.quit:
				return
			case ev := <-sub.in:
				select {
				case out <- ev:
				case <-ctx.Done():
					sub.stop(ctx.Err())
					return
				case <-sub.quit:
					return
				}
			}
		}
	}()
	return st
}

func (s *mockEventStream) Events() <-chan RuntimeEvent { return s.out }
func (s *mockEventStream) Err() error                  { return s.sub.getErr() }
func (s *mockEventStream) Close() error {
	s.m.unregister(s.sub)
	return nil
}

// mockLogStream 是 Logs 的串流實作。
type mockLogStream struct {
	ch     chan LogLine
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
}

func newMockLogStream(ctx context.Context, lines []LogLine, follow bool) *mockLogStream {
	s := &mockLogStream{ch: make(chan LogLine, len(lines)+1)}
	if !follow {
		// 非 follow:假 log 行全數放得下緩衝,同步預載後關閉,無需 goroutine 與 cancel。
		for _, ln := range lines {
			s.ch <- ln
		}
		close(s.ch)
		return s
	}
	cctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go func() {
		defer close(s.ch)
		for _, ln := range lines {
			select {
			case s.ch <- ln:
			case <-cctx.Done():
				s.setErr(cctx.Err())
				return
			}
		}
		<-cctx.Done()
		s.setErr(cctx.Err())
	}()
	return s
}

func (s *mockLogStream) Lines() <-chan LogLine { return s.ch }
func (s *mockLogStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}
func (s *mockLogStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
func (s *mockLogStream) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}
