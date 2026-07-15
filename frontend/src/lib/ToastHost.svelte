<script lang="ts">
  import { toasts, dismissToast } from './stores';
</script>

<div class="host">
  {#each $toasts as t (t.id)}
    <div class="toast {t.kind}" on:click={() => dismissToast(t.id)}>
      <span class="icon">{t.kind === 'error' ? '⚠' : t.kind === 'success' ? '✓' : 'ℹ'}</span>
      <span class="msg">{t.message}</span>
    </div>
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
    z-index: 200;
    max-width: 460px;
  }
  .toast {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    padding: 11px 14px;
    border-radius: var(--radius-sm);
    background: var(--bg-2);
    border: 1px solid var(--line-strong);
    border-left-width: 3px;
    cursor: pointer;
    box-shadow: 0 6px 20px rgba(0, 0, 0, 0.4);
    font-size: 13px;
  }
  .toast.error {
    border-left-color: var(--err);
  }
  .toast.success {
    border-left-color: var(--ok);
  }
  .toast.info {
    border-left-color: var(--accent);
  }
  .icon {
    font-weight: 700;
  }
  .toast.error .icon {
    color: var(--err);
  }
  .toast.success .icon {
    color: var(--ok);
  }
  .msg {
    white-space: pre-wrap;
    word-break: break-word;
  }
</style>
