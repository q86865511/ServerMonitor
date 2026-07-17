package core

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// ---- fake 相依(store 計數 + snapshot 玩家數;stats 流複用 threshold_test 的 fakeStatsSource) ----

type fakeMetricsStore struct {
	mu         sync.Mutex
	inserts    []MetricPoint
	pruneCalls []time.Time
}

func (f *fakeMetricsStore) InsertMetric(p MetricPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserts = append(f.inserts, p)
	return nil
}

func (f *fakeMetricsStore) PruneMetrics(before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneCalls = append(f.pruneCalls, before)
	return nil
}

func (f *fakeMetricsStore) insertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inserts)
}

func (f *fakeMetricsStore) snapshotInserts() []MetricPoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MetricPoint(nil), f.inserts...)
}

func (f *fakeMetricsStore) pruneCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pruneCalls)
}

func (f *fakeMetricsStore) firstPrune() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pruneCalls[0]
}

type fakeSnapSource struct {
	mu      sync.Mutex
	players *int
	found   bool
}

func (f *fakeSnapSource) Snapshot(uuid string) (MonitorSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return MonitorSnapshot{UUID: uuid, PlayerCount: copyIntPtr(f.players)}, f.found
}

// rs 建一筆資源取樣(限額固定 1000,便於斷言)。
func rs(ts time.Time, cpu float64, mem uint64) protocol.ResourceStats {
	return protocol.ResourceStats{TsUTC: ts, CPUPercent: cpu, MemoryBytes: mem, MemoryLimit: 1000}
}

// newRecorderForTest 建一個不會於測試期間觸發 Prune ticker 的記錄器(Prune 設 1h)。
func newRecorderForTest(store metricsStore, stats statsSource, snaps snapshotSource) (*MetricsRecorder, *int32) {
	var processed int32
	rec := NewMetricsRecorder(MetricsRecorderConfig{
		Store: store, Stats: stats, Snaps: snaps,
		Now:   func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		Prune: time.Hour,
	})
	rec.testHook = func(string) { atomic.AddInt32(&processed, 1) }
	return rec, &processed
}

// TestMetricsRecorder_Aggregate15s 驗證 15s bucket 聚合:cpu=樣本平均、memory=末一筆樣本、
// 玩家數自快照取(R13)。
func TestMetricsRecorder_Aggregate15s(t *testing.T) {
	store := &fakeMetricsStore{}
	stats := newFakeStatsSource()
	snap := &fakeSnapSource{players: intp(7), found: true}
	rec, processed := newRecorderForTest(store, stats, snap)
	defer rec.Close()

	rec.Watch("A")
	waitCond(t, time.Second, func() bool { return stats.subscribeCount("A") == 1 })

	b0 := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	stats.push("A", rs(b0, 10, 300))
	stats.push("A", rs(b0.Add(2*time.Second), 20, 400))
	stats.push("A", rs(b0.Add(4*time.Second), 30, 500))  // 末樣本 mem=500
	stats.push("A", rs(b0.Add(15*time.Second), 99, 600)) // 跨 bucket → flush b0
	waitCond(t, time.Second, func() bool { return atomic.LoadInt32(processed) == 4 })

	ins := store.snapshotInserts()
	if len(ins) != 1 {
		t.Fatalf("應恰寫入一個完成的 bucket, got %d: %+v", len(ins), ins)
	}
	p := ins[0]
	if !p.TsUTC.Equal(b0) {
		t.Errorf("bucket 起點應對齊 b0, got %v", p.TsUTC)
	}
	if p.CPUPercent != 20 { // (10+20+30)/3
		t.Errorf("cpu 應為樣本平均 20, got %v", p.CPUPercent)
	}
	if p.MemoryBytes == nil || *p.MemoryBytes != 500 {
		t.Errorf("memory 應為 bucket 末一筆樣本 500, got %v", p.MemoryBytes)
	}
	if p.PlayerCount == nil || *p.PlayerCount != 7 {
		t.Errorf("玩家數應自快照取 7, got %v", p.PlayerCount)
	}
}

// TestMetricsRecorder_MemoryZeroStoresNull 驗證 bucket 末一筆樣本 memory_bytes==0(native Job 降級)
// 時記憶體兩欄存 NULL;cpu 零值/非零皆照存(R13 無效樣本防護)。
func TestMetricsRecorder_MemoryZeroStoresNull(t *testing.T) {
	store := &fakeMetricsStore{}
	stats := newFakeStatsSource()
	rec, processed := newRecorderForTest(store, stats, &fakeSnapSource{found: false})
	defer rec.Close()

	rec.Watch("A")
	waitCond(t, time.Second, func() bool { return stats.subscribeCount("A") == 1 })

	b0 := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	stats.push("A", rs(b0, 10, 100))
	stats.push("A", rs(b0.Add(2*time.Second), 20, 0)) // 末樣本 mem=0 → NULL
	stats.push("A", rs(b0.Add(15*time.Second), 0, 0)) // 跨 bucket → flush b0
	waitCond(t, time.Second, func() bool { return atomic.LoadInt32(processed) == 3 })

	ins := store.snapshotInserts()
	if len(ins) != 1 {
		t.Fatalf("應寫入一個 bucket, got %d", len(ins))
	}
	p := ins[0]
	if p.MemoryBytes != nil || p.MemoryLimit != nil {
		t.Errorf("末樣本 mem==0 應存 NULL, got bytes=%v limit=%v", p.MemoryBytes, p.MemoryLimit)
	}
	if p.CPUPercent != 15 { // (10+20)/2,cpu 零值不影響此 bucket 均值
		t.Errorf("cpu 均值應為 15, got %v", p.CPUPercent)
	}
	if p.PlayerCount != nil {
		t.Errorf("快照不可用時玩家數應為 nil, got %v", p.PlayerCount)
	}
}

// TestMetricsRecorder_CPUZeroStored 驗證 cpu 零值(整個 bucket 皆 0)仍照存(非 NULL)。
func TestMetricsRecorder_CPUZeroStored(t *testing.T) {
	store := &fakeMetricsStore{}
	stats := newFakeStatsSource()
	rec, processed := newRecorderForTest(store, stats, &fakeSnapSource{found: false})
	defer rec.Close()

	rec.Watch("A")
	waitCond(t, time.Second, func() bool { return stats.subscribeCount("A") == 1 })

	b0 := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	stats.push("A", rs(b0, 0, 200))
	stats.push("A", rs(b0.Add(15*time.Second), 0, 200)) // 跨 bucket → flush b0
	waitCond(t, time.Second, func() bool { return atomic.LoadInt32(processed) == 2 })

	ins := store.snapshotInserts()
	if len(ins) != 1 || ins[0].CPUPercent != 0 {
		t.Fatalf("cpu 0 應照存: %+v", ins)
	}
}

// TestMetricsRecorder_CloseDoesNotFlush 驗證 Close 後不寫入:未完成的 bucket 不被 flush,
// 收束後 goroutine 不再觸碰 DB(R13)。
func TestMetricsRecorder_CloseDoesNotFlush(t *testing.T) {
	store := &fakeMetricsStore{}
	stats := newFakeStatsSource()
	rec, processed := newRecorderForTest(store, stats, &fakeSnapSource{found: false})

	rec.Watch("A")
	waitCond(t, time.Second, func() bool { return stats.subscribeCount("A") == 1 })

	b0 := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	// 同一 bucket 兩筆,不跨界 → 尚未 flush。
	stats.push("A", rs(b0, 10, 100))
	stats.push("A", rs(b0.Add(2*time.Second), 20, 200))
	waitCond(t, time.Second, func() bool { return atomic.LoadInt32(processed) == 2 })

	if store.insertCount() != 0 {
		t.Fatalf("未跨 bucket 前不應有寫入, got %d", store.insertCount())
	}
	rec.Close()
	// Close 後部分 bucket 仍不得被 flush(goroutine 不觸碰 DB)。
	if store.insertCount() != 0 {
		t.Errorf("Close 後不應寫入部分 bucket, got %d", store.insertCount())
	}
}

// TestMetricsRecorder_PruneIndependentOfWatchers 驗證 Prune ticker 獨立於實例 watcher——
// 零實例(未 Watch)時仍週期批刪 >retention,before=now-retention(R13)。
func TestMetricsRecorder_PruneIndependentOfWatchers(t *testing.T) {
	store := &fakeMetricsStore{}
	fixed := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	rec := NewMetricsRecorder(MetricsRecorderConfig{
		Store: store, Stats: newFakeStatsSource(), Snaps: &fakeSnapSource{},
		Now:       func() time.Time { return fixed },
		Prune:     10 * time.Millisecond,
		Retention: time.Hour,
	})
	defer rec.Close()

	// 未 Watch 任何實例:Prune 仍應運作。
	waitCond(t, 2*time.Second, func() bool { return store.pruneCount() >= 1 })
	if got := store.firstPrune(); !got.Equal(fixed.Add(-time.Hour)) {
		t.Errorf("Prune before 應為 now-retention=%v, got %v", fixed.Add(-time.Hour), got)
	}
}
