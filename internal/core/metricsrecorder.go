package core

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"servermonitor/internal/protocol"
)

// 指標記錄器的關鍵常數(R13)。
const (
	defaultMetricsBucket    = 15 * time.Second // 聚合 bucket(對齊 UTC)
	defaultMetricsRetention = 36 * time.Hour   // 保留窗
	defaultMetricsPrune     = 1 * time.Hour    // Prune ticker 週期
)

// snapshotSource 是玩家數快照的來源(*MonitorHub 天然滿足,見本檔尾靜態斷言)。CPU/記憶體走
// stats 流(statsSource,見 threshold.go),但玩家數不在 stats 流——寫入當下自快照取(R13)。
type snapshotSource interface {
	Snapshot(uuid string) (MonitorSnapshot, bool)
}

// metricsStore 是 MetricsRecorder 依賴的持久化面(*Store 滿足;測試以 fake 計數注入)。
type metricsStore interface {
	InsertMetric(MetricPoint) error
	PruneMetrics(before time.Time) error
}

// MetricsRecorder 實作 R13 的指標時序持久化:對每個受監控實例訂閱 MonitorHub 的 stats 流(2s),
// 以「對齊 UTC 的 15s bucket」聚合(cpu=樣本平均、memory=bucket 末一筆樣本、玩家數=寫入當下自
// MonitorHub 快照取),於 bucket 邊界寫入 metrics 表;另跑一條獨立於任何實例 watcher 的 Prune
// ticker(每小時批刪 >36h),零實例時仍運作。Close 後所有 goroutine 收束且不再觸碰 DB。
//
// 無效樣本防護(R13):memory 判定以 bucket 末一筆樣本為準——該樣本 memory_bytes==0(native Job
// 降級「留零值=不適用」,見 native.go Stats 註)即記憶體兩欄存 NULL,不回溯取前段有效值、不持久化
// 假 0;cpu 零值合法照存。
type MetricsRecorder struct {
	store metricsStore
	stats statsSource
	snaps snapshotSource

	now        func() time.Time
	bucket     time.Duration
	retention  time.Duration
	pruneEvery time.Duration

	mu       sync.Mutex
	watchers map[string]recorderWatcher // uuid → 正在運行的訂閱(供 Stop)
	closed   bool
	prune    context.CancelFunc

	// closedFlag 使 flush 於 Close 開始後即短路(在 wg.Wait 收束之前也不再寫 DB),
	// 硬保證「關閉後 goroutine 不觸碰 DB」(R13 測試驗證)。
	closedFlag atomic.Bool

	wg sync.WaitGroup // 所有 goroutine(每實例 run + prune),Close 時 Wait

	// testHook 於每次樣本處理結束時呼叫(uuid);production 恆為 nil。僅供同套件測試在非同步
	// channel 消費路徑上同步斷言(避免「已推入樣本」與「已處理樣本」間的競態)。
	testHook func(uuid string)
}

// MetricsRecorderConfig 是 MetricsRecorder 的建構參數。零值的 Bucket/Retention/Prune 採預設。
type MetricsRecorderConfig struct {
	Store     metricsStore
	Stats     statsSource
	Snaps     snapshotSource
	Now       func() time.Time
	Bucket    time.Duration // 聚合 bucket;<=0 用 15s
	Retention time.Duration // 保留窗;<=0 用 36h
	Prune     time.Duration // Prune ticker 週期;<=0 用 1h
}

// NewMetricsRecorder 建立指標記錄器並啟動獨立於實例 watcher 的 Prune ticker(零實例時仍運作)。
func NewMetricsRecorder(cfg MetricsRecorderConfig) *MetricsRecorder {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	bucket := cfg.Bucket
	if bucket <= 0 {
		bucket = defaultMetricsBucket
	}
	retention := cfg.Retention
	if retention <= 0 {
		retention = defaultMetricsRetention
	}
	pruneEvery := cfg.Prune
	if pruneEvery <= 0 {
		pruneEvery = defaultMetricsPrune
	}
	r := &MetricsRecorder{
		store:      cfg.Store,
		stats:      cfg.Stats,
		snaps:      cfg.Snaps,
		now:        now,
		bucket:     bucket,
		retention:  retention,
		pruneEvery: pruneEvery,
		watchers:   make(map[string]recorderWatcher),
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.prune = cancel
	r.wg.Add(1)
	go r.runPrune(ctx)
	return r
}

// Watch 開始為一個實例聚合並寫入指標(冪等:已在監看則 no-op)。由生命週期在實例 StartMonitoring
// 之後呼叫。Close 後呼叫為 no-op。
func (r *MetricsRecorder) Watch(uuid string) {
	if r.stats == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if _, ok := r.watchers[uuid]; ok {
		r.mu.Unlock()
		return
	}
	ch, id := r.stats.SubscribeStats(uuid)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.watchers[uuid] = recorderWatcher{subID: id, cancel: cancel, done: done}
	r.wg.Add(1)
	r.mu.Unlock()

	go r.run(ctx, uuid, ch, done)
}

// recorderWatcher 是單一實例 watcher 的控制柄。done 由 run 返回時關閉,供 Stop 等待該
// goroutine 收束——確保 Stop 返回後不再有本實例的 ingest/flush(R13「停止監控即停寫」)。
type recorderWatcher struct {
	subID  SubID
	cancel context.CancelFunc
	done   chan struct{}
}

// Stop 停止一個實例的指標聚合(冪等)。刻意不 flush 未完成的 bucket:部分 bucket 不寫入,
// 避免半個窗的失真平均;下一個完整 bucket 由下次監控續寫。
func (r *MetricsRecorder) Stop(uuid string) {
	r.mu.Lock()
	h, ok := r.watchers[uuid]
	if ok {
		delete(r.watchers, uuid)
	}
	r.mu.Unlock()
	if ok {
		h.cancel()
		r.stats.UnsubscribeStats(uuid, h.subID)
		// 等 watcher 收束:cancel 與 channel 同時 ready 時 select 可能仍選中緩衝樣本,
		// 不等待則 Stop 返回後仍可能跨界 flush 一筆。Unsubscribe 已關閉 channel,收束即時。
		<-h.done
	}
}

// Close 停止所有實例聚合與 Prune ticker,等待所有 goroutine 收束(冪等)。收束後不再有任何
// goroutine 觸碰 DB(closedFlag 於 Wait 前即短路 flush;R13 測試驗證)。
func (r *MetricsRecorder) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.closedFlag.Store(true)
	watchers := r.watchers
	r.watchers = make(map[string]recorderWatcher)
	prune := r.prune
	r.mu.Unlock()

	if prune != nil {
		prune()
	}
	for uuid, h := range watchers {
		h.cancel()
		r.stats.UnsubscribeStats(uuid, h.subID)
	}
	r.wg.Wait()
}

// run 消費一個實例的 stats channel,逐筆聚合;於 bucket 邊界寫入完成的 bucket。ctx 取消或
// channel 關閉即返回,不寫入未完成的 bucket(對齊 Stop/Close 語意)。
func (r *MetricsRecorder) run(ctx context.Context, uuid string, ch <-chan protocol.ResourceStats, done chan struct{}) {
	defer close(done)
	defer r.wg.Done()
	var acc bucketAccumulator
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-ch:
			if !ok {
				return
			}
			// cancel 與樣本同時 ready 時 select 可能選中樣本:已停止就丟棄,保證 Stop 語意。
			if ctx.Err() != nil {
				return
			}
			r.ingest(uuid, &acc, s)
		}
	}
}

// bucketAccumulator 累積單一實例當前 bucket 的樣本。
type bucketAccumulator struct {
	bucket    time.Time // 當前 bucket 起點(對齊 UTC);hasSample=false 時無意義
	hasSample bool
	cpuSum    float64
	cpuCount  int
	lastMem   uint64 // bucket 末一筆樣本的 memory_bytes
	lastLimit uint64 // bucket 末一筆樣本的 memory_limit
}

func (a *bucketAccumulator) reset(b time.Time) {
	a.bucket = b
	a.hasSample = true
	a.cpuSum = 0
	a.cpuCount = 0
	a.lastMem = 0
	a.lastLimit = 0
}

// ingest 把一筆樣本併入當前 bucket;跨越 bucket 邊界時先 flush 前一個 bucket 再開新 bucket。
func (r *MetricsRecorder) ingest(uuid string, acc *bucketAccumulator, s protocol.ResourceStats) {
	if r.testHook != nil {
		defer r.testHook(uuid)
	}
	ts := s.TsUTC
	if ts.IsZero() {
		ts = r.now()
	}
	b := ts.UTC().Truncate(r.bucket)

	if acc.hasSample && !b.Equal(acc.bucket) {
		r.flush(uuid, acc)
		acc.reset(b)
	} else if !acc.hasSample {
		acc.reset(b)
	}

	acc.cpuSum += s.CPUPercent
	acc.cpuCount++
	acc.lastMem = s.MemoryBytes
	acc.lastLimit = s.MemoryLimit
}

// flush 寫入一個完成的 bucket。closedFlag 已設(Close 進行中/後)即短路不寫,硬保證關閉後不碰 DB。
func (r *MetricsRecorder) flush(uuid string, acc *bucketAccumulator) {
	if !acc.hasSample || acc.cpuCount == 0 {
		return
	}
	if r.closedFlag.Load() {
		return
	}
	p := MetricPoint{
		InstanceUUID: uuid,
		TsUTC:        acc.bucket,
		CPUPercent:   acc.cpuSum / float64(acc.cpuCount),
	}
	// memory:bucket 末一筆樣本;末樣本 memory_bytes==0 視為不可採集 → 兩欄存 NULL(不持久化假 0)。
	if acc.lastMem != 0 {
		mb := int64(acc.lastMem)
		ml := int64(acc.lastLimit)
		p.MemoryBytes = &mb
		p.MemoryLimit = &ml
	}
	// 玩家數:寫入當下自 MonitorHub 快照取(nil 存 NULL);快照回傳的指標已是複本,可直接持有。
	if r.snaps != nil {
		if snap, ok := r.snaps.Snapshot(uuid); ok {
			p.PlayerCount = snap.PlayerCount
		}
	}
	// 寫失敗不中斷聚合(單筆缺點=趨勢圖一個缺口);專案無集中 logger,以 stdlib log 留痕供
	// wails dev / 終端觀測,避免 DB 持續寫失敗完全靜默。
	if err := r.store.InsertMetric(p); err != nil {
		log.Printf("metrics: InsertMetric(%s@%s) 失敗: %v", uuid, acc.bucket.Format(time.RFC3339), err)
	}
}

// runPrune 週期批刪過期指標,獨立於任何實例 watcher(零實例時仍運作)。ctx 取消即返回。
func (r *MetricsRecorder) runPrune(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r.closedFlag.Load() {
				return
			}
			if err := r.store.PruneMetrics(r.now().Add(-r.retention)); err != nil {
				log.Printf("metrics: PruneMetrics 失敗: %v", err)
			}
		}
	}
}

// 確保 *MonitorHub 滿足 snapshotSource(供 bootstrap 直接注入)。
var _ snapshotSource = (*MonitorHub)(nil)

// 確保 *Store 滿足 metricsStore。
var _ metricsStore = (*Store)(nil)
