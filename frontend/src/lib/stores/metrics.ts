// 指標時序:每 uuid 一個 720 點 ring buffer(3h / 15s)。
//
// 訂閱生命週期不變式(引用計數):
//   - acquire(uuid) 首個引用才 QueryMetrics 回填 + SubscribeStats + EventsOn;後續引用只加計數。
//   - release(uuid) 計數歸零即拆訂閱(stats 流輕量,不需 logs 的 30s 延遲)。
//   - 防孤兒/防時序反轉:Subscribe/Unsubscribe 一律經 Entry.chain 序列化+世代守衛(見 Entry 註解)。
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
  open: OpenBucket | null;
  /** 訂閱世代:每次真正建立訂閱 +1。Unsubscribe 於鏈上執行時比對,已被 re-acquire 取代即放棄。 */
  gen: number;
  /**
   * Subscribe/Unsubscribe RPC 的序列化操作鏈:所有後端訂閱轉換依 enqueue 順序執行,
   * 杜絕「Unsubscribe RPC 在途時 re-acquire 的 Subscribe 先到後端」的時序反轉
   * (反轉時後端冪等 no-op + 隨後被舊 Unsubscribe 拆除 → 前端自認已訂閱、後端已停流)。
   */
  chain: Promise<void>;
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
      open: null,
      gen: 0,
      chain: Promise.resolve(),
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
  // 玩家數不取自 stats 流:agent 端不採集(R13),即時 bucket 一律 null;
  // 權威值由後端 recorder 自快照寫入 DB,經回填帶回(避免流值 0 與「不適用」的歧義)。
  e.open.players = null;
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
  subscribeStats(e, uuid);
}

// 建立一輪 stats 訂閱(回填 + EventsOn + SubscribeStats)。供 acquireMetrics 首訂與 resubscribeMetrics 重訂共用。
function subscribeStats(e: Entry, uuid: string): void {
  e.subscribed = true;
  e.gen += 1; // 新一輪訂閱世代
  const myGen = e.gen;

  // 即時流:2s stats 事件降採樣為 15s bucket 追加。
  e.unlisten = EventsOn(`stats:${uuid}`, (s: protocol.ResourceStats) => onStats(e, s));

  // 回填:now-3h 既有時序。與 ring 現況「合併」而非整批取代——查詢在途時即時流可能已 flush
  // 較新的 bucket,無條件 set 會把那些點蓋掉造成假缺口;重疊 ts 以後端(完整 bucket)為準。
  QueryMetrics(uuid, Math.floor((Date.now() - BACKFILL_MS) / 1000))
    .then((points) => {
      if (e.gen !== myGen) return; // 已被新一輪 acquire 取代,由它自己的回填處理
      const backfill = points.map(fromDTO);
      e.store.update((cur) => mergeSamples(backfill, cur));
    })
    .catch(() => {
      /* 靜默:節點離線/無歷史時留空 ring,趨勢圖顯空態 */
    });

  // Subscribe 進序列化操作鏈(與 Unsubscribe 嚴格依序,見 Entry.chain 註解)。
  e.chain = e.chain
    .then(() => SubscribeStats(uuid))
    .then(() => undefined)
    .catch(() => {
      /* 靜默:訂閱失敗不阻斷回填顯示 */
    });
}

/**
 * 強制重建 stats 訂閱(B10:停止再啟動同一實例後,後端 fanout 隨 channel 關閉結束、前端訂閱狀態殘留而
 * 即時指標靜默)。僅在目前仍有引用且已訂閱時作用:teardown(鏈上舊 Unsubscribe 因世代前進被放棄)後
 * 重新 subscribeStats,重發 SubscribeStats,使後端(channel 關閉後已清除訂閱 map)開新 fanout。
 * 由 OverviewTab 於實例「非 Running→Running」轉態時呼叫。
 */
export function resubscribeMetrics(uuid: string): void {
  const e = entries.get(uuid);
  if (!e || !e.subscribed || e.refCount === 0) return;
  teardown(e, uuid);
  subscribeStats(e, uuid);
}

// 合併回填與 ring 現況:回填段為準,僅保留比回填末點更新的即時點;回填為空則保留現況。
function mergeSamples(backfill: MetricSample[], cur: MetricSample[]): MetricSample[] {
  if (backfill.length === 0) return cur;
  const lastTs = backfill[backfill.length - 1].ts;
  const tail = cur.filter((s) => s.ts > lastTs);
  const merged = backfill.concat(tail);
  return merged.length > RING_CAP ? merged.slice(merged.length - RING_CAP) : merged;
}

/** 釋放引用:歸零即拆訂閱(不延遲)。ring 內容保留,再 acquire 時由回填覆寫。 */
export function releaseMetrics(uuid: string): void {
  const e = entries.get(uuid);
  if (!e) return;
  e.refCount = Math.max(0, e.refCount - 1);
  if (e.refCount > 0) return;
  teardown(e, uuid);
}

function teardown(e: Entry, uuid: string): void {
  const myGen = e.gen; // 本輪訂閱世代
  e.subscribed = false;
  e.open = null;
  if (e.unlisten) {
    e.unlisten();
    e.unlisten = null;
  }
  EventsOff(`stats:${uuid}`);
  // Unsubscribe 進序列化操作鏈:必然排在本輪 Subscribe 之後、下一輪 Subscribe 之前執行。
  // 世代守衛於「鏈上執行當下」評估——已被 re-acquire 取代(gen 前進)即放棄拆除,
  // 讓後端訂閱無縫延續給新一輪(其 Subscribe 對後端是冪等 no-op)。
  e.chain = e.chain
    .then(async () => {
      if (e.gen !== myGen) return;
      await UnsubscribeStats(uuid);
    })
    .catch(() => {
      /* 忽略卸載期錯誤 */
    });
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
