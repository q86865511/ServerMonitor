// logs store 行為測試:著重 B10(停止→切分頁→啟動→切回導致日誌永久靜默)回歸鎖,
// 以及引用計數/延遲釋放/緩衝上限等既有不變式。wailsjs 生成綁定全數 mock(測試不觸碰真實 Wails runtime)。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { get } from 'svelte/store';
import type { LogRow } from '../ui/types';

const { SubscribeLogs, UnsubscribeLogs } = vi.hoisted(() => ({
  SubscribeLogs: vi.fn(() => Promise.resolve()),
  UnsubscribeLogs: vi.fn(() => Promise.resolve()),
}));
vi.mock('../../../wailsjs/go/main/App', () => ({ SubscribeLogs, UnsubscribeLogs }));

interface RawLine {
  stream?: string;
  line?: string;
}

const { EventsOn, EventsOff } = vi.hoisted(() => ({
  // 每次呼叫回傳獨立的 unlisten mock,供斷言「舊監聽器確實被拆除」。
  EventsOn: vi.fn((_name: string, _cb: (raw: RawLine) => void) => vi.fn()),
  EventsOff: vi.fn(),
}));
vi.mock('../../../wailsjs/runtime/runtime', () => ({ EventsOn, EventsOff }));

import { acquireLogs, releaseLogs, noteRunning, logRows, logConnection, pushLines, parseLevel } from './logs';

let uuidSeq = 0;
/** 每個測試用獨立 uuid,避免 logs.ts 的模組級 entries Map 狀態跨測試污染。 */
function nextUuid(): string {
  uuidSeq += 1;
  return `test-uuid-${uuidSeq}`;
}

/** 取出某次 EventsOn 呼叫註冊的回呼(供測試模擬後端推播事件)。 */
function eventsOnCallback(callIndex: number): (raw: RawLine) => void {
  const call = EventsOn.mock.calls[callIndex];
  return call[1] as (raw: RawLine) => void;
}

/**
 * 讓 subscribe()/teardown() 排進 Entry.chain 的 SubscribeLogs/UnsubscribeLogs 呼叫結算一次
 * microtask——這兩者是經 `chain.then(() => SubscribeLogs(uuid))` 排程,不像 EventsOn/EventsOff
 * 是同步呼叫,斷言呼叫次數前必須先讓步一次。
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

describe('logs store — B10 轉態重訂閱回歸鎖', () => {
  it('停止(release,元件卸載)→啟動→切回(acquire)偵測到 running 轉態,觸發新一輪 SubscribeLogs', async () => {
    const uuid = nextUuid();

    acquireLogs(uuid);
    await flush();
    expect(SubscribeLogs).toHaveBeenCalledTimes(1);

    noteRunning(uuid, true); // 首次通報,只記基準(lastRunning: null → true),不觸發
    noteRunning(uuid, false); // true→false,不觸發,但基準更新為 false

    releaseLogs(uuid); // refCount 歸零,進入 30s 延遲釋放,subscribed 仍為 true(模擬切分頁卸載)

    // 卸載期間無人呼叫 noteRunning——這正是 B10 的成因:實例在此期間 stop→start,
    // 但「false」轉態基準已留在 entry 上,直到重新掛載才會被下面這次通報偵測到。

    acquireLogs(uuid); // 重新掛載(30s 內),取消延遲釋放計時器
    expect(SubscribeLogs).toHaveBeenCalledTimes(1); // 因 subscribed 仍為 true,尚未新增訂閱

    noteRunning(uuid, true); // false→true 轉態,應觸發 resubscribeLogs

    await vi.runAllTimersAsync(); // 讓 Subscribe/Unsubscribe 操作鏈 resolve

    expect(SubscribeLogs).toHaveBeenCalledTimes(2); // 後端收到新一輪 SubscribeLogs
    expect(EventsOff).toHaveBeenCalledWith(`logs:${uuid}`);
    expect(EventsOn).toHaveBeenCalledTimes(2); // 舊監聽器換新
    expect(get(logConnection(uuid)).state).toBe('connected');
    // teardown 緊接著 subscribe 同步呼叫,世代已先於 Unsubscribe 的操作鏈回呼前推進,
    // 依世代守衛該次 Unsubscribe 會被放棄(見 logs.ts teardown 註解)——刻意鎖住這個既有行為,
    // 避免未來誤以為這裡「應該」呼叫 UnsubscribeLogs 而動了設計。
    expect(UnsubscribeLogs).not.toHaveBeenCalled();
  });

  it('狀態全程未變(一直 running)時,切分頁往返(release→30 秒內 acquire)不產生多餘的重訂閱 RPC', async () => {
    const uuid = nextUuid();

    acquireLogs(uuid);
    await flush();
    expect(SubscribeLogs).toHaveBeenCalledTimes(1);

    noteRunning(uuid, true); // 首次通報,只記基準

    releaseLogs(uuid); // refCount 歸零,啟動 30s 延遲釋放
    await vi.advanceTimersByTimeAsync(5_000); // 30s 內切回

    acquireLogs(uuid); // 取消延遲釋放,subscribed 仍為 true,不重訂閱
    noteRunning(uuid, true); // prev=true, isRunning=true → 不轉態,不觸發

    await vi.runAllTimersAsync();

    expect(SubscribeLogs).toHaveBeenCalledTimes(1); // 全程只有首次那一次
    expect(UnsubscribeLogs).not.toHaveBeenCalled();
    expect(EventsOn).toHaveBeenCalledTimes(1); // 監聽器未被重建
  });
});

describe('logs store — 引用計數與延遲釋放', () => {
  it('多個引用時 release 一次不退訂;歸零後需等 30s 延遲期滿才真正 teardown', async () => {
    const uuid = nextUuid();

    acquireLogs(uuid); // refCount=1
    acquireLogs(uuid); // refCount=2
    await flush();
    expect(SubscribeLogs).toHaveBeenCalledTimes(1);

    releaseLogs(uuid); // refCount=1,仍 >0,不啟動計時器
    await vi.runAllTimersAsync();
    expect(UnsubscribeLogs).not.toHaveBeenCalled();

    releaseLogs(uuid); // refCount=0,啟動 30s 延遲釋放計時器
    await vi.advanceTimersByTimeAsync(29_999);
    expect(UnsubscribeLogs).not.toHaveBeenCalled(); // 延遲期未滿,尚未 teardown

    await vi.advanceTimersByTimeAsync(2); // 跨過 30s 門檻
    expect(UnsubscribeLogs).toHaveBeenCalledTimes(1); // 期滿且仍無引用,真正退訂
    expect(get(logConnection(uuid)).state).toBe('idle');
  });
});

describe('logs store — 高流量落地與緩衝上限(既有行為)', () => {
  it('經 EventsOn 推播的日誌以 16ms 節流批次落地', async () => {
    const uuid = nextUuid();
    acquireLogs(uuid);
    const cb = eventsOnCallback(0);

    cb({ stream: 'stdout', line: 'hello' });
    cb({ stream: 'stderr', line: 'boom' });
    expect(get(logRows(uuid))).toHaveLength(0); // 尚未 flush

    await vi.advanceTimersByTimeAsync(16);

    const rows = get(logRows(uuid));
    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatchObject({ stream: 'stdout', line: 'hello', level: 'info' });
    expect(rows[1]).toMatchObject({ stream: 'stderr', line: 'boom', level: 'error' });
  });

  it('單一 uuid 超過 3000 行上限時丟舊留新', async () => {
    const uuid = nextUuid();
    acquireLogs(uuid);
    const cb = eventsOnCallback(0);

    for (let i = 0; i < 3001; i += 1) {
      cb({ stream: 'stdout', line: `line-${i}` });
    }
    await vi.advanceTimersByTimeAsync(16);

    const rows = get(logRows(uuid));
    expect(rows).toHaveLength(3000);
    expect(rows[0].line).toBe('line-1'); // line-0 被丟棄(最舊)
    expect(rows[rows.length - 1].line).toBe('line-3000');
  });
});

describe('logs store — 純函式', () => {
  it('pushLines 空 incoming 直接回傳原陣列', () => {
    const existing: LogRow[] = [{ id: 1, stream: 'stdout', line: 'a', level: 'info' }];
    expect(pushLines(existing, [])).toBe(existing);
  });

  it('pushLines 超過自訂上限時丟舊留新', () => {
    const existing: LogRow[] = Array.from({ length: 5 }, (_, i) => ({
      id: i,
      stream: 'stdout',
      line: `l${i}`,
      level: 'info' as const,
    }));
    const incoming: LogRow[] = [{ id: 100, stream: 'stdout', line: 'new', level: 'info' }];
    const result = pushLines(existing, incoming, 3);
    expect(result.map((r) => r.line)).toEqual(['l3', 'l4', 'new']);
  });

  it('parseLevel:stderr 一律 error,gsm-monitor 一律 info(即使含 ERROR 字樣)', () => {
    expect(parseLevel('stderr', 'anything')).toBe('error');
    expect(parseLevel('gsm-monitor', 'ERROR something')).toBe('info');
    expect(parseLevel('stdout', 'FATAL crash')).toBe('error');
    expect(parseLevel('stdout', 'this is a WARNING')).toBe('warn');
    expect(parseLevel('stdout', 'normal line')).toBe('info');
  });
});
