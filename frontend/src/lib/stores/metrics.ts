// 指標時序:每 uuid 一個 720 點 ring buffer(3h / 15s)。
//
// 訂閱生命週期不變式(引用計數):
//   - acquire(uuid) 首個引用才 QueryMetrics 回填 + SubscribeStats + EventsOn;後續引用只加計數。
//   - release(uuid) 計數歸零即拆訂閱(stats 流輕量,不需 logs 的 30s 延遲)。
//   - 防孤兒:teardown 前 await subscribePromise,確保 SubscribeStats 已落地才 Unsubscribe。
//
// 即時流語意(對齊後端 15s bucket,見 R13):2s 的 stats 事件降採樣為 15s bucket——
//   cpu=bucket 內樣本平均;memory=bucket 末一筆樣本,末樣本 memory_bytes==0 視為不可採集存 null(畫缺口);
//   bucket 於「下一個 bucket 的首個樣本到達」時關閉並追加,故最新點約落後一個 bucket(與後端寫入節奏一致)。
import { writable } from 'svelte/store';
import type { Readable, Writable } from 'svelte/store';
import type { main, protocol } from '../../../wailsjs/go/models';
import {
  QueryMetrics,
  QueryMetricsSummary,
  SubscribeStats,
  UnsubscribeStats,
} from '../../../wailsjs/go/main/App';
import { EventsOn, EventsOff } from '../../../wailsjs/runtime/runtime';
import type { TrendPoint } from '../ui/types';

const RING_CAP = 720; // 3h / 15s
const BUCKET_MS = 15_000; // 對齊後端聚合 bucket
const BACKFILL_MS = 3 * 60 * 60 * 1000; // 回填視窗 now-3h

/** ring 內單一取樣點。cpu/memory 為 null 代表該 bucket 不可採集(TrendChart 畫缺口,不補 0)。 */
export interface MetricSample {
  /** bucket 起始 epoch ms。 */
  ts: number;
  /** bucket 內 CPU 樣本平均;無樣本 → null(cpu 零值合法,照存 0)。 */
  cpu: number | null;
  /** bucket 末樣本記憶體用量;末樣本為 0/缺 → null。 */
  memoryBytes: number | null;
  /** bucket 末樣本記憶體上限;0/缺 → null。 */
  memoryLimit: number | null;
  /** 末樣本玩家數(stats 流通常不含,回填由後端快照補);缺 → null。 */
  players: number | null;
}

// bucket 累積狀態(尚未關閉的當前 bucket)。
interface OpenBucket {
  ts: number;
  cpuSum: number;
  cpuCount: number;
  memoryBytes: number | null;
  memoryLimit: number | null;
  players: number | null;
}

interface Entry {
  store: Writable<MetricSample[]>;
  refCount: number;
  subscribed: boolean;
  unlisten: (() => void) | null;
  subscribePromise: Promise<void> | null;
  open: OpenBucket | null;
}

const entries = new Map<string, Entry>();

function ensure(uuid: string): Entry {
  let e = entries.get(uuid);
  if (!e) {
    e = {
      store: writable<MetricSample[]>([]),
      refCount: 0,
      subscribed: false,
      unlisten: null,
      subscribePromise: null,
      open: null,
    };
    entries.set(uuid, e);
  }
  return e;
}

/** 取得某 uuid 的 ring buffer(響應式)。元件以 $ 訂閱後經投影 helper 轉 TrendPoint。 */
export function metricSamples(uuid: string): Readable<MetricSample[]> {
  return ensure(uuid).store;
}

function bucketStart(ms: number): number {
  return Math.floor(ms / BUCKET_MS) * BUCKET_MS;
}

// 後端 MetricPointDTO(記憶體欄 nullable)→ MetricSample;NULL 透傳為 null。
function fromDTO(p: main.MetricPointDTO): MetricSample {
  return {
    ts: Date.parse(p.ts_utc),
    cpu: p.cpu_percent,
    memoryBytes: p.memory_bytes ?? null,
    memoryLimit: p.memory_limit ?? null,
    players: p.player_count ?? null,
  };
}

// 追加一點:與末點同 ts 則取代(去重,避免回填末點與首個關閉 bucket 撞點),否則附加;套 720 上限。
function pushSample(list: MetricSample[], s: MetricSample): MetricSample[] {
  const last = list[list.length - 1];
  const next = last && last.ts === s.ts ? list.slice(0, -1) : list.slice();
  next.push(s);
  return next.length > RING_CAP ? next.slice(next.length - RING_CAP) : next;
}

function onStats(e: Entry, s: protocol.ResourceStats): void {
  const b = bucketStart(Date.now());
  if (e.open && e.open.ts !== b) flushBucket(e); // 跨 bucket → 關閉並追加舊 bucket
  if (!e.open) {
    e.open = { ts: b, cpuSum: 0, cpuCount: 0, memoryBytes: null, memoryLimit: null, players: null };
  }
  e.open.cpuSum += s.cpu_percent;
  e.open.cpuCount += 1;
  e.open.memoryBytes = s.memory_bytes > 0 ? s.memory_bytes : null; // 末樣本;0=不可採集
  e.open.memoryLimit = s.memory_limit > 0 ? s.memory_limit : null;
  e.open.players = s.player_count ?? null;
}

function flushBucket(e: Entry): void {
  const o = e.open;
  if (!o) return;
  e.open = null;
  const sample: MetricSample = {
    ts: o.ts,
    cpu: o.cpuCount > 0 ? o.cpuSum / o.cpuCount : null,
    memoryBytes: o.memoryBytes,
    memoryLimit: o.memoryLimit,
    players: o.players,
  };
  e.store.update((list) => pushSample(list, sample));
}

/** 取用某 uuid 的即時指標:首個引用觸發回填 + 訂閱;之後只加計數。 */
export function acquireMetrics(uuid: string): void {
  const e = ensure(uuid);
  e.refCount += 1;
  if (e.subscribed) return;
  e.subscribed = true;

  // 即時流:2s stats 事件降採樣為 15s bucket 追加。
  e.unlisten = EventsOn(`stats:${uuid}`, (s: protocol.ResourceStats) => onStats(e, s));

  // 回填:now-3h 既有時序,整批取代 ring 內容(取尾 720 點)。
  QueryMetrics(uuid, Math.floor((Date.now() - BACKFILL_MS) / 1000))
    .then((points) => {
      const samples = points.map(fromDTO);
      e.store.set(samples.length > RING_CAP ? samples.slice(samples.length - RING_CAP) : samples);
    })
    .catch(() => {
      /* 靜默:節點離線/無歷史時留空 ring,趨勢圖顯空態 */
    });

  e.subscribePromise = SubscribeStats(uuid)
    .then(() => undefined)
    .catch(() => {
      /* 靜默:訂閱失敗不阻斷回填顯示 */
    });
}

/** 釋放引用:歸零即拆訂閱(不延遲)。ring 內容保留,再 acquire 時由回填覆寫。 */
export function releaseMetrics(uuid: string): void {
  const e = entries.get(uuid);
  if (!e) return;
  e.refCount = Math.max(0, e.refCount - 1);
  if (e.refCount > 0) return;
  teardown(e, uuid);
}

async function teardown(e: Entry, uuid: string): Promise<void> {
  e.subscribed = false;
  e.open = null;
  if (e.unlisten) {
    e.unlisten();
    e.unlisten = null;
  }
  EventsOff(`stats:${uuid}`);
  try {
    if (e.subscribePromise) await e.subscribePromise; // 防孤兒:等訂閱落地再取消
    await UnsubscribeStats(uuid);
  } catch {
    /* 忽略卸載期錯誤 */
  }
}

/** 總覽全體平均時序(分母僅計有樣本實例,由後端保證)。回傳 MetricSample[](NULL 欄位透傳)。 */
export async function summary(sinceUnix: number): Promise<MetricSample[]> {
  const points = await QueryMetricsSummary(sinceUnix);
  return points.map(fromDTO);
}

// --- 投影 helper:MetricSample[] → TrendPoint[](null 透傳為缺口)。純函式,供圖表元件消費。 ---

/** CPU 百分比序列。 */
export function cpuTrend(samples: MetricSample[]): TrendPoint[] {
  return samples.map((s) => ({ ts: s.ts, value: s.cpu }));
}

/** 記憶體使用率(%)序列;缺上限或缺樣本 → null 缺口。 */
export function ramPercentTrend(samples: MetricSample[]): TrendPoint[] {
  return samples.map((s) => ({
    ts: s.ts,
    value:
      s.memoryBytes != null && s.memoryLimit != null && s.memoryLimit > 0
        ? (s.memoryBytes / s.memoryLimit) * 100
        : null,
  }));
}

/** 記憶體用量(bytes)序列。 */
export function ramBytesTrend(samples: MetricSample[]): TrendPoint[] {
  return samples.map((s) => ({ ts: s.ts, value: s.memoryBytes }));
}

/** 玩家數序列。 */
export function playersTrend(samples: MetricSample[]): TrendPoint[] {
  return samples.map((s) => ({ ts: s.ts, value: s.players }));
}
