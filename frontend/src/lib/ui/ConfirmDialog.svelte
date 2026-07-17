<script lang="ts">
  import type { Snippet } from 'svelte';
  import Modal from './Modal.svelte';

  let {
    title = '確認',
    message = '',
    confirmLabel = '確認',
    danger = false,
    busy = false,
    onConfirm,
    onCancel,
    children,
  }: {
    title?: string;
    message?: string;
    confirmLabel?: string;
    danger?: boolean;
    busy?: boolean;
    onConfirm: () => void;
    onCancel: () => void;
    /** 額外內容(如 purge 勾選),置於訊息文字下方。 */
    children?: Snippet;
  } = $props();
</script>

<Modal {title} onClose={onCancel}>
  {#if message}
    <p class="msg">{message}</p>
  {/if}
  {#if children}
    {@render children()}
  {/if}
  <div class="actions">
    <button type="button" class="cancel" onclick={onCancel} disabled={busy}>取消</button>
    <button
      type="button"
      class="confirm"
      class:danger
      onclick={onConfirm}
      disabled={busy}
      aria-busy={busy}
    >
      {#if busy}
        <span class="spinner" aria-hidden="true"></span>
      {/if}
      {busy ? '處理中…' : confirmLabel}
    </button>
  </div>
</Modal>

<style>
  .msg {
    margin: 0 0 var(--space-4);
    color: var(--fg-1);
    white-space: pre-wrap;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-4);
  }
  button {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font: inherit;
    font-size: var(--text-base);
    border-radius: var(--radius-sm);
    padding: 7px 14px;
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard),
      opacity var(--dur-fast) var(--ease-standard);
  }
  button:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
  button:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .cancel {
    background-color: var(--bg-3);
    border: 1px solid var(--line-strong);
    color: var(--fg-0);
  }
  .cancel:hover:not(:disabled) {
    background-color: var(--line);
  }
  .confirm {
    background-color: var(--accent);
    border: 1px solid var(--accent);
    color: var(--fg-on-accent);
  }
  .confirm:hover:not(:disabled) {
    background-color: var(--accent-hover);
    border-color: var(--accent-hover);
  }
  .confirm.danger {
    background-color: var(--err);
    border-color: var(--err);
  }
  .confirm.danger:hover:not(:disabled) {
    opacity: 0.88;
  }
  .spinner {
    width: 12px;
    height: 12px;
    border-radius: 50%;
    border: 2px solid currentColor;
    border-top-color: transparent;
    opacity: 0.8;
    animation: spin 0.7s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>
