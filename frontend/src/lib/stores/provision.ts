// 供應/啟動進度:集中訂閱 provision:<uuid> 事件(引用計數,語意同 stores/logs.ts),
// 對外提供某 uuid 的最新進度(stage/percent/detail)。供 ConsoleTab 顯示啟動/供應進度橫幅。
//
// 訂閱生命週期(引用計數):
//   - acquire(uuid) 首個引用才 EventsOn(provision:<uuid>);後續引用只加計數。
//   - release(uuid) 計數歸零即移除監聽——僅用 EventsOn 回傳的 canceller,不用 EventsOff:
//     同一事件另有 stores/operations.ts 的監聽者,EventsOff 依事件名會連帶移除他人監聽。
//   - progress 值於 teardown 不清(切分頁往返不抖動);stage="ready" 後由計時器延遲清為 null。
import { writable } from 'svelte/store';
import type { Readable, Writable } from 'svelte/store';
import { EventsOn } from '../../../wailsjs/runtime/runtime';

const CLEAR_DELAY_MS = 3000; // stage="ready" 後自動清除延遲

/** 某 uuid 的最新供應/啟動進度。percent<0 表不確定態。 */
export interface Progress {
  stage: string;
  percent: number;
  detail: string;
}

// provision:<uuid> 事件負載(欄位同後端 protocol.ProvisionProgress 的 JSON tag)。
interface RawProgress {
  stage?: string;
  percent?: number;
  detail?: string;
  instance_uuid?: string;
}

interface Entry {
  progress: Writable<Progress | null>;
  refCount: number;
  unlisten: (() => void) | null;
  clearTimer: ReturnType<typeof setTimeout> | null;
}

const entries = new Map<string, Entry>();

function ensure(uuid: string): Entry {
  let e = entries.get(uuid);
  if (!e) {
    e = {
      progress: writable<Progress | null>(null),
      refCount: 0,
      unlisten: null,
      clearTimer: null,
    };
    entries.set(uuid, e);
  }
  return e;
}

/** 某 uuid 的最新進度(響應式);無進行中進度時為 null。 */
export function provisionProgress(uuid: string): Readable<Progress | null> {
  return ensure(uuid).progress;
}

function ingest(e: Entry, raw: RawProgress): void {
  if (e.clearTimer) {
    clearTimeout(e.clearTimer);
    e.clearTimer = null;
  }
  const stage = raw.stage ?? '';
  // blocked-mods 的 detail 為 JSON(建立精靈專屬),非一般進度,不顯示於主控台橫幅。
  if (stage === 'blocked-mods') return;
  e.progress.set({
    stage,
    percent: typeof raw.percent === 'number' ? raw.percent : -1,
    detail: raw.detail ?? '',
  });
  // 終態(就緒或失敗)後短延遲自動清除橫幅——失敗亦收束,不再永久卡在「啟動中/等待就緒」。
  if (stage === 'ready' || stage === 'failed') {
    e.clearTimer = setTimeout(() => {
      e.clearTimer = null;
      e.progress.set(null);
    }, CLEAR_DELAY_MS);
  }
}

/** 取用某 uuid 的進度流:首個引用觸發訂閱;之後只加計數。 */
export function acquireProvision(uuid: string): void {
  const e = ensure(uuid);
  e.refCount += 1;
  if (e.unlisten) return;
  e.unlisten = EventsOn(`provision:${uuid}`, (raw: RawProgress) => ingest(e, raw));
}

/** 釋放引用:歸零即移除監聽(progress 值保留,再 acquire 由新事件覆寫)。 */
export function releaseProvision(uuid: string): void {
  const e = entries.get(uuid);
  if (!e) return;
  e.refCount = Math.max(0, e.refCount - 1);
  if (e.refCount > 0) return;
  if (e.unlisten) {
    e.unlisten();
    e.unlisten = null;
  }
  if (e.clearTimer) {
    clearTimeout(e.clearTimer);
    e.clearTimer = null;
  }
}

// 階段代碼 → 友善中文標籤。後端部分階段已中文化(如「下載 Java 17」),未列於表者原樣回傳。
const STAGE_LABEL: Record<string, string> = {
  starting: '啟動中',
  'awaiting-ready': '等待就緒',
  ready: '就緒',
  failed: '啟動失敗',
  'pull-image': '拉取映像',
  modpack: '安裝模組包',
  'import-blocked': '匯入模組',
  'blocked-mods': '需手動下載模組',
  jre: '安裝 Java',
  'server-jar': '下載伺服器檔',
  steamcmd: 'SteamCMD',
};

/** 供應/啟動階段代碼 → 友善中文標籤;未知(後端已中文化)階段原樣回傳。 */
export function stageLabel(stage: string): string {
  if (!stage) return '';
  return STAGE_LABEL[stage] ?? stage;
}
