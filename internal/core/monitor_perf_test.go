//go:build docker

// log 高流量效能基準(T16;以 -tags docker 執行,與其他 E2E/基準同批跑,不拖慢預設單元套件)。
// 既有背壓單元測試已斷言「500 行 fanout <200ms 不阻塞」與「緩衝滿丟最舊留最新 + 提示行」;本測
// 補量級數字回填 design.md 效能基準,白箱直接驅動 fanoutLog(沿用背壓測試的 monNopDialer 手法):
//
//	A) 常態流量(NFR 示例 1000 行/秒 × 10s):健康消費者下應零丟棄(無損轉推)。
//	B) 突發上限:盡速灌固定行數,量 fanout 吞吐上限(行/秒);消費者跟不上時丟舊留新、絕不阻塞。
package core

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestPerf_LogThroughput(t *testing.T) {
	// LogBufferSize 放寬至 4096(預設 256):消費者 goroutine 與量測 goroutine 併發,-race 下排程
	// 抖動可能讓常態階段短暫積壓;加大餘裕使「零丟棄」斷言反映的是背壓機制的無損轉推,而非受制於
	// 偏小緩衝的排程競態。加大緩衝不影響突發階段(B)對「丟舊留新、不阻塞」的展示——200k 無節流突發
	// 遠超任何有界緩衝,消費者仍跟不上而丟棄,fanout 仍非阻塞。
	hub := NewMonitorHub(monNopDialer{}, nil, nil, nil, nil, MonitorConfig{LogBufferSize: 4096})
	const uuid = "perf-uuid"
	ch, sid := hub.SubscribeLogs(uuid)
	mi := hub.insts[uuid]

	// 健康消費者:持續排空,統計實收與監控提示行(丟棄提示行)。received/notices 以 atomic 讀寫:
	// 消費者 goroutine 寫、量測主 goroutine 於各階段前後讀(取差),消弭 -race 偵測到的資料競爭。
	done := make(chan struct{})
	var received, notices atomic.Int64
	go func() {
		for ln := range ch {
			received.Add(1)
			if ln.Stream == LogStreamMonitor {
				notices.Add(1)
			}
		}
		close(done)
	}()

	now := time.Now()

	// ---- A) 常態流量 1000 行/秒 × 10s:健康消費者應零丟棄 ----
	const (
		rate    = 1000
		sustain = 10 * time.Second
		batch   = 10
		slot    = time.Duration(batch) * time.Second / rate // 10 行 / 10ms
	)
	recvBefore := received.Load()
	noticeBefore := notices.Load()
	start := time.Now()
	next := start
	var pushedA int
	for time.Since(start) < sustain {
		for i := 0; i < batch; i++ {
			mi.fanoutLog(LogLine{Stream: "stdout", Line: "sustained"}, now)
			pushedA++
		}
		next = next.Add(slot)
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
	}
	// 讓消費者收完在途行(排空短暫等待)。
	time.Sleep(200 * time.Millisecond)
	recvA := received.Load() - recvBefore
	noticeA := notices.Load() - noticeBefore
	droppedA := int64(pushedA) - (recvA - noticeA)

	// ---- B) 突發上限:盡速灌 20 萬行,量 fanout 吞吐上限 ----
	const burst = 200000
	recvBeforeB := received.Load()
	noticeBeforeB := notices.Load()
	bstart := time.Now()
	for i := 0; i < burst; i++ {
		mi.fanoutLog(LogLine{Stream: "stdout", Line: "burst"}, now)
	}
	bElapsed := time.Since(bstart)

	hub.UnsubscribeLogs(uuid, sid) // 關閉訂閱者 channel,使消費者 goroutine 收束
	<-done
	recvB := received.Load() - recvBeforeB
	noticeB := notices.Load() - noticeBeforeB
	burstRate := float64(burst) / bElapsed.Seconds()

	t.Logf("[基準A] 常態 %d 行/秒 × %s: 注入=%d 有效收=%d 丟棄=%d(NFR 示例流量應零丟棄)",
		rate, sustain, pushedA, recvA-noticeA, droppedA)
	t.Logf("[基準B] 突發上限: 盡速灌 %d 行耗時 %v = %.0f 行/秒 fanout 吞吐;消費者跟不上時丟舊留新(有效收=%d、提示行=%d),絕不阻塞",
		burst, bElapsed, burstRate, recvB-noticeB, noticeB)

	if droppedA != 0 {
		t.Fatalf("常態 1000 行/秒 不應丟棄,實丟 %d(pushed=%d recv=%d notice=%d)", droppedA, pushedA, recvA, noticeA)
	}
	if burstRate < 100000 { // 遠高於 1000/秒 目標;健全下限(不阻塞的證據)
		t.Fatalf("fanout 突發吞吐 %.0f 行/秒 過低(疑似阻塞)", burstRate)
	}
}
