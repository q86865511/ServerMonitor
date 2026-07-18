<script lang="ts">
  import Toast, { type ToastKind } from './Toast.svelte';

  export interface ToastItem {
    id: string | number;
    kind: ToastKind;
    message: string;
  }

  let {
    toasts,
    onDismiss,
  }: {
    /** 顯示中的 toast 清單;資料來源(store)由呼叫端管理,本元件純呈現。 */
    toasts: ToastItem[];
    onDismiss: (id: string | number) => void;
  } = $props();
</script>

<div class="host">
  {#each toasts as t (t.id)}
    <Toast kind={t.kind} message={t.message} onDismiss={() => onDismiss(t.id)} />
  {/each}
</div>

<style>
  .host {
    position: fixed;
    right: 18px;
    bottom: 18px;
    display: flex;
    flex-direction: column;
    gap: 10px;
    z-index: var(--z-toast);
    max-width: 460px;
  }
</style>
