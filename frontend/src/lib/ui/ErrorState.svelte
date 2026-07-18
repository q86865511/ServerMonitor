<script lang="ts">
  let {
    message,
    retryLabel = '重試',
    onRetry,
  }: {
    message: string;
    retryLabel?: string;
    onRetry?: () => void;
  } = $props();
</script>

<div class="error-state">
  <div class="icon" aria-hidden="true">⚠</div>
  <div class="msg">{message}</div>
  {#if onRetry}
    <button type="button" class="retry" onclick={onRetry}>{retryLabel}</button>
  {/if}
</div>

<style>
  .error-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: var(--space-2);
    padding: var(--space-6) var(--space-4);
  }
  .icon {
    font-size: 28px;
    color: var(--err);
  }
  .msg {
    font-size: var(--text-sm);
    color: var(--fg-1);
    max-width: 420px;
    white-space: pre-wrap;
  }
  .retry {
    margin-top: var(--space-1);
    font: inherit;
    font-size: var(--text-sm);
    color: var(--fg-0);
    background-color: var(--bg-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 6px 14px;
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard);
  }
  .retry:hover {
    background-color: var(--line);
  }
  .retry:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
</style>
