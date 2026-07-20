// 全域操作面板資料源:彙整「進行中的長操作」(建立/啟動/停止/重啟/備份)。
//   - 由 GUI 發起操作處以 trackOperation() 包裝 Wails 呼叫:登記一筆,promise resolve/reject 即結束
//     (完成後短暫保留供使用者看到結果,再自動移除)。
//   - 訂閱全域 provision 事件豐富「建立」進度(建立時實例 uuid 尚未知),及 provision:<uuid> 豐富
//     具 uuid 操作(啟動/重啟)的進度。監聽一律用 EventsOn 回傳的 canceller、不用 EventsOff——
//     同事件另有 stores/provision.ts 與建立精靈的監聽者。
import { get, writable } from 'svelte/store';
import type { Readable } from 'svelte/store';
import { EventsOn } from '../../../wailsjs/runtime/runtime';
import { errMsg } from '../api';
import { byUuid } from './instances';

export type OpKind = 'create' | 'start' | 'stop' | 'restart' | 'backup';
export type OpStatus = 'running' | 'success' | 'error';

/** 一筆進行中/剛完成的長操作。 */
export interface Operation {
  id: number;
  uuid: string; // 建立操作於 resolve 前為空(進度由全域 provision 事件豐富)
  name: string;
  kind: OpKind;
  stage: string;
  percent: number; // <0 表不確定態
  detail: string;
  status: OpStatus;
  error: string;
}

// provision 事件負載(欄位同後端 protocol.ProvisionProgress 的 JSON tag)。
interface RawProgress {
  stage?: string;
  percent?: number;
  detail?: string;
  instance_uuid?: string;
}

const DONE_LINGER_MS = 4000; // 完成後保留顯示時間

const ops = writable<Operation[]>([]);
/** 進行中 + 剛完成(短暫保留)的操作清單(響應式)。 */
export const operations: Readable<Operation[]> = ops;

let seq = 0;
let globalUnlisten: (() => void) | null = null;
const uuidUnlisten = new Map<string, () => void>();

const KIND_LABEL: Record<OpKind, string> = {
  create: '建立',
  start: '啟動',
  stop: '停止',
  restart: '重啟',
  backup: '備份',
};

/** 操作種類 → 中文標籤。 */
export function kindLabel(kind: OpKind): string {
  return KIND_LABEL[kind];
}

function nameOf(uuid: string): string {
  const inst = byUuid(uuid);
  if (!inst) return uuid.slice(0, 8);
  return inst.name?.trim() ? inst.name : `${inst.template_id} #${uuid.slice(0, 8)}`;
}

function applyProgress(o: Operation, raw: RawProgress): Operation {
  const stage = raw.stage ?? o.stage;
  if (stage === 'blocked-mods') return o; // detail 為 JSON,非一般進度
  return {
    ...o,
    stage,
    percent: typeof raw.percent === 'number' ? raw.percent : o.percent,
    detail: raw.detail ?? o.detail,
  };
}

// 全域 provision:建立進度(實例 uuid 尚未知)套用到所有進行中的建立操作。
function enrichGlobal(raw: RawProgress): void {
  ops.update((l) =>
    l.map((o) => (o.kind === 'create' && o.status === 'running' ? applyProgress(o, raw) : o)),
  );
}

// provision:<uuid>:套用到該實例進行中的操作(啟動/重啟)。
function enrichUuid(uuid: string, raw: RawProgress): void {
  ops.update((l) =>
    l.map((o) => (o.uuid === uuid && o.status === 'running' ? applyProgress(o, raw) : o)),
  );
}

function ensureListeners(): void {
  if (!globalUnlisten) {
    globalUnlisten = EventsOn('provision', (raw: RawProgress) => enrichGlobal(raw));
  }
  for (const o of get(ops)) {
    if (o.uuid && !uuidUnlisten.has(o.uuid)) {
      const uuid = o.uuid;
      uuidUnlisten.set(
        uuid,
        EventsOn(`provision:${uuid}`, (raw: RawProgress) => enrichUuid(uuid, raw)),
      );
    }
  }
}

// 移除已無對應操作的 per-uuid 監聽;清單全空時再撤全域監聽。
function teardownIdleListeners(): void {
  const list = get(ops);
  const active = new Set(list.filter((o) => o.uuid).map((o) => o.uuid));
  for (const [uuid, un] of uuidUnlisten) {
    if (!active.has(uuid)) {
      un();
      uuidUnlisten.delete(uuid);
    }
  }
  if (list.length === 0 && globalUnlisten) {
    globalUnlisten();
    globalUnlisten = null;
  }
}

function finish(id: number, status: OpStatus, error = ''): void {
  ops.update((l) =>
    l.map((o) =>
      o.id === id ? { ...o, status, error, percent: status === 'success' ? 100 : o.percent } : o,
    ),
  );
  setTimeout(() => {
    ops.update((l) => l.filter((o) => o.id !== id));
    teardownIdleListeners();
  }, DONE_LINGER_MS);
}

/**
 * 包裝一次 GUI 發起的長操作:登記一筆進行中項,執行 fn,resolve/reject 時結束(短暫保留結果)。
 * name 省略時由 uuid 於 instances store 解析。回傳值與拋出的錯誤與 fn 一致(不吞錯,呼叫端照舊處理)。
 */
export async function trackOperation<T>(
  spec: { uuid?: string; name?: string; kind: OpKind },
  fn: () => Promise<T>,
): Promise<T> {
  const id = ++seq;
  const uuid = spec.uuid ?? '';
  ops.update((l) => [
    ...l,
    {
      id,
      uuid,
      name: spec.name ?? (uuid ? nameOf(uuid) : '新伺服器'),
      kind: spec.kind,
      stage: '',
      percent: -1,
      detail: '',
      status: 'running',
      error: '',
    },
  ]);
  ensureListeners();
  try {
    const res = await fn();
    finish(id, 'success');
    return res;
  } catch (e) {
    finish(id, 'error', errMsg(e));
    throw e;
  }
}
