// toast 遷移點:re-export 既有 stores.ts 的實作(單一真相來源,不另立第二個 store),
// 供 ui/ToastHost(純 props 版)與各頁接線。舊 lib/*.svelte 仍從 '../stores' 匯入,兩路同源不分岔。
export { toasts, pushToast, dismissToast } from '../stores';
export type { Toast, ToastKind } from '../stores';
