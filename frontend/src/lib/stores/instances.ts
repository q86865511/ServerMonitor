// 實例清單 / 節點狀態的集中管理:5s 輪詢(refreshSeq 防舊回應覆寫)、操作後手動刷新、
// 派生查詢 helper。輪詢啟停由 shell 掛載點驅動(T9),本檔僅匯出 API。
// 底層 store 直接沿用 stores.ts 既有 writable(單一真相來源;App.svelte 等舊頁尚在消費同一份)。
import { get } from 'svelte/store';
import type { main } from '../../../wailsjs/go/models';
import { ListInstances, NodeStatus } from '../../../wailsjs/go/main/App';
import { call } from '../api';
import { instances, nodeStatuses } from '../stores';

export { instances, nodeStatuses };

const POLL_MS = 5000;
let timer: ReturnType<typeof setInterval> | null = null;

// 遞增序號:輪詢與操作後 refresh 可能並發,舊回應晚到會覆寫新狀態(如停止後短暫回跳 Running)。
// 回應套用前檢查自己仍是最新一次呼叫,否則丟棄。
let refreshSeq = 0;

/**
 * 拉一次 ListInstances + NodeStatus 並更新 store。silent=true 時失敗不彈 toast(供輪詢用)。
 * 操作(啟停/建立/移除)後應以 silent=false 呼叫,讓錯誤浮現。
 */
export async function refresh(silent = true): Promise<void> {
  const seq = ++refreshSeq;
  try {
    const [insts, nodes] = await Promise.all([
      call(() => ListInstances(), { silent }),
      call(() => NodeStatus(), { silent }),
    ]);
    if (seq !== refreshSeq) return; // 已有更新的 refresh 在途,丟棄此次舊回應
    instances.set(insts);
    nodeStatuses.set(nodes);
  } catch {
    /* 輪詢失敗靜默(避免 banner 洗版);首次載入由節點 banner 反映 */
  }
}

/** 啟動 5s 輪詢(冪等:重複呼叫不會疊計時器)。先立即刷新一次。 */
export function startPolling(): void {
  if (timer) return;
  refresh(false);
  timer = setInterval(() => refresh(true), POLL_MS);
}

/** 停止輪詢。 */
export function stopPolling(): void {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
}

/** 依 uuid 取當前實例快照(非響應式;響應式請 $instances 後自行 find)。 */
export function byUuid(uuid: string): main.InstanceDTO | undefined {
  return get(instances).find((i) => i.uuid === uuid);
}

/** 全域搜尋比對:對名稱 / uuid / 範本 id 做不分大小寫子字串比對;空查詢回原清單。 */
export function filterInstances(list: main.InstanceDTO[], query: string): main.InstanceDTO[] {
  const q = query.trim().toLowerCase();
  if (!q) return list;
  return list.filter(
    (i) =>
      (i.name ?? '').toLowerCase().includes(q) ||
      i.uuid.toLowerCase().includes(q) ||
      i.template_id.toLowerCase().includes(q),
  );
}
