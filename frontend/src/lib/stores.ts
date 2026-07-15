// 全域 Svelte stores:實例清單、節點狀態、導航、toast。
import { writable } from 'svelte/store';
import { main } from '../../wailsjs/go/models';

/** 主導航頁。主控台/建立精靈為疊加 modal,不佔導航。 */
export type View = 'instances' | 'events' | 'settings';
export const currentView = writable<View>('instances');

/** 由首頁輪詢維護的實例清單(權威來源:ListInstances)。 */
export const instances = writable<main.InstanceDTO[]>([]);

/** 節點狀態(權威來源:NodeStatus);任一離線 → 全域 banner。 */
export const nodeStatuses = writable<main.NodeStatusDTO[]>([]);

// ---- Toast(錯誤狀態矩陣的全域錯誤/成功呈現層) ----
export type ToastKind = 'error' | 'success' | 'info';
export interface Toast {
  id: number;
  kind: ToastKind;
  message: string;
}
export const toasts = writable<Toast[]>([]);
let toastSeq = 0;

export function pushToast(kind: ToastKind, message: string, ttlMs = 6000): number {
  const id = ++toastSeq;
  toasts.update((list) => [...list, { id, kind, message }]);
  if (ttlMs > 0) {
    setTimeout(() => dismissToast(id), ttlMs);
  }
  return id;
}

export function dismissToast(id: number): void {
  toasts.update((list) => list.filter((t) => t.id !== id));
}
