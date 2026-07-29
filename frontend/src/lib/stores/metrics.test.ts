// metrics store 行為測試:著重 B10(停止→啟動導致即時指標永久靜默)回歸鎖,
// 以及引用計數(無延遲釋放)、15s bucket 降採樣、ring buffer 上限等既有不變式。
// wailsjs 生成綁定全數 mock(測試不觸碰真實 Wails runtime)。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { get } from 'svelte/store';

const { QueryMetrics, QueryMetricsSummary, SubscribeStats, UnsubscribeStats } = vi.hoisted(() => ({
  QueryMetrics: vi.fn(() => Promise.resolve([])),
  QueryMetricsSummary: vi.fn(() => Promise.resolve([])),
  SubscribeStats: vi.fn(() => Promise.resolve()),
  UnsubscribeStats: vi.fn(() => Promise.resolve()),
}));
vi.mock('../../../wailsjs/go/main/App', () => ({
  QueryMetrics,
  QueryMetricsSummary,
  SubscribeStats,
  UnsubscribeStats,
}));

const { EventsOn, EventsOff } = vi.hoisted(() => ({
  // 每次呼叫回傳獨立的 unlisten mock,供斷言「舊監聽器確實被拆除」。
  EventsOn: vi.fn((_name: string, _cb: (s: unknown) => void) => vi.fn()),
  EventsOff: vi.fn(),
}));
vi.mock('../../../wailsjs/runtime/runtime', () => ({ EventsOn, EventsOff }));

import { acquireMetrics, releaseMetrics, noteRunning, metricSamples } from './metrics';
import { protocol } from '../../../wailsjs/go/models';

let uuidSeq = 0;
/** 每個測試用獨立 uuid,避免 metrics.ts 的模組級 entries Map 狀態跨測試污染。 */
function nextUuid(): string {
  uuidSeq += 1;
  return `test-uuid-${uuidSeq}`;
}

/** 取出某次 EventsOn 呼叫註冊的回呼(供測試模擬後端 stats 推播)。 */
function eventsOnCallback(callIndex: number): (s: protocol.ResourceStats) => void {
  const call = EventsOn.mock.calls[callIndex];
  return call[1] as (s: protocol.ResourceStats) => void;
}

/** 建構一筆 stats 樣本(僅 onStats 實際使用的三個欄位)。 */
function stat(cpu: number, memoryBytes: number, memoryLimit: number): protocol.ResourceStats {
  return new protocol.ResourceStats({ cpu_percent: cpu, memory_bytes: memoryBytes, memory_limit: memoryLimit });
}

/**
 * 讓 subscribeStats()/teardown() 排進 Entry.chain 的 SubscribeStats/UnsubscribeStats 呼叫結算一次
 * microtask——這兩者是經 `chain.then(() => SubscribeStats(uuid))` 排程,不像 EventsOn/EventsOff/
 * QueryMetrics 是同步呼叫,斷言呼叫次數前必須先讓步一次。
 */
async function flush(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0);
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('metrics store — B10 轉態重訂閱回歸鎖', () => {
  it('常駐引用未歸零、subscribed 全程為 true 時,running 轉態仍應觸發新一輪 SubscribeStats + 回填', async () => {
    const uuid = nextUuid();

    acquireMetrics(uuid); // ref=1(模擬另一常駐消費者,如側欄簡表)
    acquireMetrics(uuid); // ref=2(模擬 OverviewTab 掛載),因已訂閱只加計數
    await flush();
    expect(SubscribeStats).toHaveBeenCalledTimes(1);
    expect(QueryMetrics).toHaveBeenCalledTimes(1);

    noteRunning(uuid, true); // 首次通報,只記基準
    noteRunning(uuid, false); // true→false,不觸發,但基準更新為 false

    releaseMetrics(uuid); // OverviewTab 卸載;ref=1(常駐引用仍在)→ 不 teardown,subscribed 仍為 true
    expect(UnsubscribeStats).not.toHaveBeenCalled();

    // 卸載期間實例被重啟,但常駐引用仍持有訂閱、無人在此期間呼叫 noteRunning——
    // 這正是 metrics 版 B10 的成因:「false」轉態基準留在 entry 上,直到重新掛載才被偵測到。

    acquireMetrics(uuid); // OverviewTab 重新掛載;ref=2,因 subscribed 仍為 true,不重新訂閱
    expect(SubscribeStats).toHaveBeenCalledTimes(1);

    noteRunning(uuid, true); // false→true 轉態,應觸發 resubscribeMetrics

    await vi.runAllTimersAsync();

    expect(SubscribeStats).toHaveBeenCalledTimes(2); // 後端收到新一輪訂閱
    expect(QueryMetrics).toHaveBeenCalledTimes(2); // 一併重新回填(見 subscribeStats 實作)
    expect(EventsOff).toHaveBeenCalledWith(`stats:${uuid}`);
    expect(EventsOn).toHaveBeenCalledTimes(2); // 舊監聽器換新
    // teardown 緊接著 subscribeStats 同步呼叫,世代已先於 Unsubscribe 的操作鏈回呼前推進,
    // 依世代守衛該次 Unsubscribe 會被放棄(見 metrics.ts teardown 註解)——與 logs.ts 同一設計,
    // 刻意鎖住這個既有行為,避免未來誤以為這裡「應該」呼叫 UnsubscribeStats。
    expect(UnsubscribeStats).not.toHaveBeenCalled();
  });

  it('狀態全程未變(一直 running)時不產生多餘的重訂閱 RPC', async () => {
    const uuid = nextUuid();

    acquireMetrics(uuid);
    await flush();
    expect(SubscribeStats).toHaveBeenCalledTimes(1);

    noteRunning(uuid, true); // 基準
    noteRunning(uuid, true); // 仍為 true,不轉態,不觸發

    await vi.runAllTimersAsync();

    expect(SubscribeStats).toHaveBeenCalledTimes(1);
    expect(UnsubscribeStats).not.toHaveBeenCalled();
    expect(EventsOn).toHaveBeenCalledTimes(1);
  });
});

describe('metrics store — 引用計數與即時釋放(無延遲,對照 logs 的 30s 延遲)', () => {
  it('多重引用只訂閱一次;release 未歸零不觸發任何 RPC', async () => {
    const uuid = nextUuid();

    acquireMetrics(uuid); // ref=1
    acquireMetrics(uuid); // ref=2,因已訂閱只加計數
    await flush();
    expect(SubscribeStats).toHaveBeenCalledTimes(1);
    expect(QueryMetrics).toHaveBeenCalledTimes(1);

    releaseMetrics(uuid); // ref=1,未歸零,不 teardown
    expect(UnsubscribeStats).not.toHaveBeenCalled();
  });

  it('release 歸零立即 teardown(不像 logs 需等 30s),UnsubscribeStats 確實送出', async () => {
    const uuid = nextUuid();

    acquireMetrics(uuid); // ref=1
    releaseMetrics(uuid); // ref=0 → 立即 teardown(僅此單一呼叫,世代未被搶先推進,Unsubscribe 真正執行)

    await vi.runAllTimersAsync();

    expect(UnsubscribeStats).toHaveBeenCalledTimes(1);
    expect(EventsOff).toHaveBeenCalledWith(`stats:${uuid}`);
  });
});

describe('metrics store — 15s bucket 降採樣與 ring buffer 上限(既有行為)', () => {
  it('2s 節奏的 stats 樣本依 15s bucket 聚合(cpu 平均、memory 末樣本),下一 bucket 首樣本到達才關閉並追加', () => {
    const uuid = nextUuid();
    vi.setSystemTime(0);
    acquireMetrics(uuid);
    const cb = eventsOnCallback(0);

    cb(stat(10, 100, 200)); // bucket0 樣本1
    cb(stat(20, 150, 200)); // bucket0 樣本2(末樣本,memory 取這筆)
    expect(get(metricSamples(uuid))).toHaveLength(0); // bucket0 尚未關閉,不落地

    vi.setSystemTime(15_000); // 進入 bucket1 → 觸發 bucket0 關閉並追加
    cb(stat(5, 300, 400));

    const afterFirstFlush = get(metricSamples(uuid));
    expect(afterFirstFlush).toHaveLength(1);
    expect(afterFirstFlush[0]).toMatchObject({ ts: 0, cpu: 15, memoryBytes: 150, memoryLimit: 200 });

    vi.setSystemTime(30_000); // 進入 bucket2 → 觸發 bucket1 關閉並追加
    cb(stat(1, 1, 1));

    const afterSecondFlush = get(metricSamples(uuid));
    expect(afterSecondFlush).toHaveLength(2);
    expect(afterSecondFlush[1]).toMatchObject({ ts: 15_000, cpu: 5, memoryBytes: 300, memoryLimit: 400 });
  });

  it('memory_bytes/limit 為 0(不可採集)時該 bucket 存 null,畫缺口不補 0', () => {
    const uuid = nextUuid();
    vi.setSystemTime(0);
    acquireMetrics(uuid);
    const cb = eventsOnCallback(0);

    cb(stat(50, 0, 0)); // 不可採集
    vi.setSystemTime(15_000);
    cb(stat(1, 1, 1)); // 觸發前一 bucket 關閉

    const samples = get(metricSamples(uuid));
    expect(samples[0]).toMatchObject({ cpu: 50, memoryBytes: null, memoryLimit: null });
  });

  it('ring buffer 超過 720 點上限時丟舊留新', () => {
    const uuid = nextUuid();
    vi.setSystemTime(0);
    acquireMetrics(uuid);
    const cb = eventsOnCallback(0);

    const totalBuckets = 722; // 722 個 bucket 的首樣本,實際關閉並落地 721 個 bucket(最後一個仍 open)
    for (let i = 0; i < totalBuckets; i += 1) {
      vi.setSystemTime(i * 15_000);
      cb(stat(i, 100, 200));
    }

    const samples = get(metricSamples(uuid));
    expect(samples).toHaveLength(720);
    expect(samples[0].cpu).toBe(1); // bucket0(cpu=0)最舊,被丟棄
    expect(samples[samples.length - 1].cpu).toBe(720); // 最新關閉的是 bucket720
  });
});
