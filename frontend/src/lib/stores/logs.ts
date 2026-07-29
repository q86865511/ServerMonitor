// 日誌 buffer:Map<uuid, LogBuffer>。每 uuid 上限 3000 行(丟舊留新),入 buffer 時解析等級一次。
//
// 訂閱生命週期不變式(引用計數 + 30s 延遲釋放):
//   - acquire(uuid) 首個引用才 SubscribeLogs + EventsOn;若有待釋放計時器則取消(切分頁往返不抖動)。
//   - release(uuid) 計數歸零後啟動 30s 計時器;期間再 acquire 會取消它,不重訂閱。
//   - 計時器到期且仍無引用才真正 teardown(EventsOff + UnsubscribeLogs);buffer 保留,離開再回不丟。
//   - 防孤兒/防時序反轉:Subscribe/Unsubscribe 一律經 Entry.chain 序列化+世代守衛(見 Entry 註解)。
//
// 效能:高頻事件不逐行 set store,先入 pending 再以 16ms 節流批次落一次(避免每行觸發整頁重渲)。
import { writable } from 'svelte/store';
import type { Readable, Writable } from 'svelte/store';
import { SubscribeLogs, UnsubscribeLogs } from '../../../wailsjs/go/main/App';
import { EventsOn, EventsOff } from '../../../wailsjs/runtime/runtime';
import { errMsg } from '../api';
import type { LogLevel, LogRow } from '../ui/types';

const BUFFER_CAP = 3000;
const FLUSH_MS = 16; // 批次節流:每 16ms 落一次 store
const RELEASE_DELAY_MS = 30_000;
const MONITOR_STREAM = 'gsm-monitor';

export type LogConnState = 'idle' | 'connecting' | 'connected' | 'error';
export interface LogConn {
  state: LogConnState;
  /** state==='error' 時的可讀原因。 */
  error?: string;
}

// --- 純邏輯(與 store 分離,可單獨驗證) ---

/**
 * 解析日誌等級一次:stderr→error;gsm-monitor→info(監控注入行不誤判);
 * 含 ERROR/SEVERE/FATAL→error;含 WARN(ING)?→warn;其餘 info。
 */
export function parseLevel(stream: string, line: string): LogLevel {
  if (stream === 'stderr') return 'error';
  if (stream === MONITOR_STREAM) return 'info';
  if (/\b(ERROR|SEVERE|FATAL)\b/i.test(line)) return 'error';
  if (/\bWARN(ING)?\b/i.test(line)) return 'warn';
  return 'info';
}

/** 追加多行並套用上限(丟舊留新);回傳新陣列(不改動 existing)。空 incoming 直接回原陣列。 */
export function pushLines(existing: LogRow[], incoming: LogRow[], cap = BUFFER_CAP): LogRow[] {
  if (incoming.length === 0) return existing;
  const merged = existing.concat(incoming);
  return merged.length > cap ? merged.slice(merged.length - cap) : merged;
}

// --- store 與訂閱管理 ---

interface RawLine {
  stream?: string;
  line?: string;
}

interface Entry {
  rows: Writable<LogRow[]>;
  conn: Writable<LogConn>;
  refCount: number;
  subscribed: boolean;
  unlisten: (() => void) | null;
  releaseTimer: ReturnType<typeof setTimeout> | null;
  seq: number; // 遞增 id 來源
  pending: LogRow[]; // 批次緩衝
  flushTimer: ReturnType<typeof setTimeout> | null;
  /** 訂閱世代:每次真正建立訂閱 +1。鏈上 Unsubscribe 執行時比對,已被 re-acquire 取代即放棄。 */
  gen: number;
  /** Subscribe/Unsubscribe RPC 序列化操作鏈(語意同 stores/metrics.ts 的 Entry.chain)。 */
  chain: Promise<void>;
  /**
   * B10 轉態基準(上次通報的 running 狀態);null=尚未通報過。刻意存在 entry(隨 uuid 存活)
   * 而非元件實體變數——後者在元件卸載(切分頁)時會重置,導致卸載期間發生的「非 Running→
   * Running」轉態在下次掛載時無法被偵測到,見 resubscribeLogs/noteRunning 註解。
   */
  lastRunning: boolean | null;
}

const entries = new Map<string, Entry>();

function ensure(uuid: string): Entry {
  let e = entries.get(uuid);
  if (!e) {
    e = {
      rows: writable<LogRow[]>([]),
      conn: writable<LogConn>({ state: 'idle' }),
      refCount: 0,
      subscribed: false,
      unlisten: null,
      releaseTimer: null,
      seq: 0,
      pending: [],
      flushTimer: null,
      gen: 0,
      chain: Promise.resolve(),
      lastRunning: null,
    };
    entries.set(uuid, e);
  }
  return e;
}

/** 某 uuid 的日誌 buffer(響應式)。 */
export function logRows(uuid: string): Readable<LogRow[]> {
  return ensure(uuid).rows;
}

/** 某 uuid 的連線狀態(訂閱中/成功/失敗)。 */
export function logConnection(uuid: string): Readable<LogConn> {
  return ensure(uuid).conn;
}

function ingest(e: Entry, raw: RawLine): void {
  const stream = raw.stream ?? 'stdout';
  const line = raw.line ?? '';
  e.pending.push({ id: ++e.seq, stream, line, level: parseLevel(stream, line) });
  scheduleFlush(e);
}

function scheduleFlush(e: Entry): void {
  if (e.flushTimer) return;
  e.flushTimer = setTimeout(() => {
    e.flushTimer = null;
    if (e.pending.length === 0) return;
    const batch = e.pending;
    e.pending = [];
    e.rows.update((list) => pushLines(list, batch));
  }, FLUSH_MS);
}

function subscribe(e: Entry, uuid: string): void {
  e.subscribed = true;
  e.gen += 1; // 新一輪訂閱世代
  const myGen = e.gen;
  e.conn.set({ state: 'connecting' });
  e.unlisten = EventsOn(`logs:${uuid}`, (raw: RawLine) => ingest(e, raw));
  // Subscribe 進序列化操作鏈:與 Unsubscribe 嚴格依 enqueue 順序執行,杜絕 RPC 時序反轉
  // (反轉時後端冪等 no-op + 被舊 Unsubscribe 拆除 → 前端自認已連線、後端已停流)。
  e.chain = e.chain
    .then(() => SubscribeLogs(uuid))
    .then(() => {
      if (e.gen === myGen) e.conn.set({ state: 'connected' });
    })
    .catch((err: unknown) => {
      if (e.gen !== myGen) return;
      const msg = errMsg(err);
      e.conn.set({ state: 'error', error: msg });
      ingest(e, { stream: MONITOR_STREAM, line: `(無法訂閱 log:${msg})` });
    });
}

function teardown(e: Entry, uuid: string): void {
  const myGen = e.gen;
  e.subscribed = false;
  if (e.unlisten) {
    e.unlisten();
    e.unlisten = null;
  }
  EventsOff(`logs:${uuid}`);
  // Unsubscribe 進操作鏈:必然排在本輪 Subscribe 之後執行;鏈上執行當下世代已前進
  // (被 re-acquire 取代)即放棄拆除,讓後端訂閱無縫延續給新一輪。
  e.chain = e.chain
    .then(async () => {
      if (e.gen !== myGen) return;
      await UnsubscribeLogs(uuid);
      e.conn.set({ state: 'idle' });
    })
    .catch(() => {
      /* 忽略卸載期錯誤 */
    });
}

/** 取用某 uuid 的日誌流:首個引用觸發訂閱;有待釋放計時器則取消。 */
export function acquireLogs(uuid: string): void {
  const e = ensure(uuid);
  if (e.releaseTimer) {
    clearTimeout(e.releaseTimer);
    e.releaseTimer = null;
  }
  e.refCount += 1;
  if (!e.subscribed) subscribe(e, uuid);
}

/** 釋放引用:歸零後 30s 才真正取消訂閱(期間再 acquire 取消延遲)。buffer 不清。 */
export function releaseLogs(uuid: string): void {
  const e = entries.get(uuid);
  if (!e) return;
  e.refCount = Math.max(0, e.refCount - 1);
  if (e.refCount > 0 || e.releaseTimer) return;
  e.releaseTimer = setTimeout(() => {
    e.releaseTimer = null;
    if (e.refCount === 0 && e.subscribed) teardown(e, uuid);
  }, RELEASE_DELAY_MS);
}

/** 清空某 uuid 的 buffer(含未落的 pending)。 */
export function clearLogs(uuid: string): void {
  const e = ensure(uuid);
  e.pending = [];
  e.rows.set([]);
}

/**
 * 強制重建訂閱(B10:停止再啟動同一實例後,後端 fanout 隨 channel 關閉結束、前端訂閱狀態殘留而靜默)。
 * 僅在目前仍有引用且已訂閱時作用:teardown(移除舊 EventsOn 監聽;鏈上舊 Unsubscribe 因世代前進被放棄)
 * 後立即 subscribe,重發 SubscribeLogs,使後端(channel 關閉後已清除訂閱 map)開新 fanout。
 * 由 ConsoleTab 於實例「非 Running→Running」轉態時呼叫。
 */
export function resubscribeLogs(uuid: string): void {
  const e = entries.get(uuid);
  if (!e || !e.subscribed || e.refCount === 0) return;
  teardown(e, uuid);
  subscribe(e, uuid);
}

/**
 * 通報目前 running 狀態,於「非 Running→Running」轉態時觸發 resubscribeLogs(B10)。
 * 轉態基準存於 store entry.lastRunning(隨 uuid 存活),不像 ConsoleTab 過去用的元件實體變數
 * 會在卸載/掛載間重置——修正「停止實例→切到其他分頁(元件卸載,進入 30s 延遲釋放但
 * subscribed 仍 true)→啟動→30 秒內切回主控台(元件重新掛載)」序列下,轉態發生在卸載期間
 * 無人通報、掛載當下又把 uuid 首次出現誤判為「換頁/首次」而不重訂閱,導致日誌永久靜默。
 * 首次通報(entry 剛建立、lastRunning 為 null)只記錄基準,不觸發。
 */
export function noteRunning(uuid: string, isRunning: boolean): void {
  const e = ensure(uuid);
  const prev = e.lastRunning;
  e.lastRunning = isRunning;
  if (prev !== null && !prev && isRunning) resubscribeLogs(uuid);
}
