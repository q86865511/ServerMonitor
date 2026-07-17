// 日誌 buffer:Map<uuid, LogBuffer>。每 uuid 上限 3000 行(丟舊留新),入 buffer 時解析等級一次。
//
// 訂閱生命週期不變式(引用計數 + 30s 延遲釋放):
//   - acquire(uuid) 首個引用才 SubscribeLogs + EventsOn;若有待釋放計時器則取消(切分頁往返不抖動)。
//   - release(uuid) 計數歸零後啟動 30s 計時器;期間再 acquire 會取消它,不重訂閱。
//   - 計時器到期且仍無引用才真正 teardown(EventsOff + UnsubscribeLogs);buffer 保留,離開再回不丟。
//   - 防孤兒:teardown 前 await subscribePromise,確保 SubscribeLogs 已落地才 Unsubscribe。
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
  subscribePromise: Promise<void> | null;
  releaseTimer: ReturnType<typeof setTimeout> | null;
  seq: number; // 遞增 id 來源
  pending: LogRow[]; // 批次緩衝
  flushTimer: ReturnType<typeof setTimeout> | null;
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
      subscribePromise: null,
      releaseTimer: null,
      seq: 0,
      pending: [],
      flushTimer: null,
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
  e.conn.set({ state: 'connecting' });
  e.unlisten = EventsOn(`logs:${uuid}`, (raw: RawLine) => ingest(e, raw));
  e.subscribePromise = SubscribeLogs(uuid)
    .then(() => {
      e.conn.set({ state: 'connected' });
    })
    .catch((err: unknown) => {
      const msg = errMsg(err);
      e.conn.set({ state: 'error', error: msg });
      ingest(e, { stream: MONITOR_STREAM, line: `(無法訂閱 log:${msg})` });
    });
}

async function teardown(e: Entry, uuid: string): Promise<void> {
  e.subscribed = false;
  if (e.unlisten) {
    e.unlisten();
    e.unlisten = null;
  }
  EventsOff(`logs:${uuid}`);
  try {
    if (e.subscribePromise) await e.subscribePromise; // 防孤兒:等訂閱落地再取消
    await UnsubscribeLogs(uuid);
  } catch {
    /* 忽略卸載期錯誤 */
  }
  e.conn.set({ state: 'idle' });
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
