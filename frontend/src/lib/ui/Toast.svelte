<script lang="ts">
  export type ToastKind = 'info' | 'success' | 'error';

  let {
    kind = 'info',
    message,
    onDismiss,
  }: {
    kind?: ToastKind;
    message: string;
    onDismiss?: () => void;
  } = $props();
</script>

<div class="toast {kind}" role="status">
  <span class="icon" aria-hidden="true">{kind === 'error' ? '⚠' : kind === 'success' ? '✓' : 'ℹ'}</span>
  <span class="msg">{message}</span>
  {#if onDismiss}
    <button type="button" class="dismiss" aria-label="關閉通知" onclick={onDismiss}>✕</button>
  {/if}
</div>

<style>
  .toast {
    display: flex;
    gap: var(--space-2);
    align-items: flex-start;
    padding: 11px 14px;
    border-radius: var(--radius-sm);
    background: var(--bg-2);
    border: 1px solid var(--line-strong);
    border-left-width: 3px;
    box-shadow: var(--shadow-md);
    font-size: var(--text-sm);
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
  .toast.info .icon {
    color: var(--accent);
  }
  .msg {
    flex: 1;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .dismiss {
    flex: none;
    background: transparent;
    border: none;
    color: var(--fg-2);
    cursor: pointer;
    padding: 0 2px;
    line-height: 1.4;
    border-radius: var(--radius-sm);
  }
  .dismiss:hover {
    color: var(--fg-0);
  }
  .dismiss:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
</style>
