<script lang="ts">
  import { toasts, dismissToast } from './stores';
  import Icon from './Icon.svelte';
</script>

<div class="host" aria-live="polite" aria-atomic="false">
  {#each $toasts as t (t.id)}
    <button
      type="button"
      class="toast {t.kind}"
      on:click={() => dismissToast(t.id)}
      aria-label={`${t.message}，點擊關閉通知`}
    >
      <span class="icon">
        <Icon name={t.kind === 'error' ? 'alert' : t.kind === 'success' ? 'check' : 'info'} size={17} />
      </span>
      <span class="toast-copy">
        <span class="kind mono">{t.kind === 'error' ? 'ERROR' : t.kind === 'success' ? 'COMPLETE' : 'NOTICE'}</span>
        <span class="msg">{t.message}</span>
      </span>
      <span class="dismiss"><Icon name="close" size={12} /></span>
    </button>
  {/each}
</div>

<style>
  .host {
    position: fixed;
    right: 18px;
    bottom: 18px;
    z-index: 200;
    display: flex;
    width: min(430px, calc(100vw - 36px));
    flex-direction: column;
    gap: 8px;
    pointer-events: none;
  }

  .toast {
    width: 100%;
    min-height: 58px;
    display: grid;
    grid-template-columns: 26px minmax(0, 1fr) 18px;
    align-items: start;
    gap: 9px;
    padding: 10px 11px;
    color: var(--fg-0);
    background: var(--bg-2);
    border: 1px solid var(--line-strong);
    border-left-width: 3px;
    border-radius: var(--radius-sm);
    box-shadow: 0 10px 28px rgba(0, 0, 0, 0.36);
    text-align: left;
    pointer-events: auto;
  }

  .toast:hover:not(:disabled) {
    background: var(--bg-3);
  }

  .toast.error { border-left-color: var(--err); }
  .toast.success { border-left-color: var(--ok); }
  .toast.info { border-left-color: var(--info); }

  .icon {
    display: grid;
    height: 24px;
    place-items: center;
  }

  .toast.error .icon { color: var(--err); }
  .toast.success .icon { color: var(--ok); }
  .toast.info .icon { color: var(--info); }

  .toast-copy {
    display: flex;
    min-width: 0;
    flex-direction: column;
  }

  .kind {
    margin-bottom: 2px;
    color: var(--fg-3);
    font-size: 8px;
    font-weight: 700;
    letter-spacing: 0.12em;
  }

  .msg {
    color: var(--fg-1);
    font-size: 12px;
    line-height: 1.45;
    white-space: pre-wrap;
    word-break: break-word;
  }

  .dismiss {
    display: grid;
    height: 22px;
    place-items: center;
    color: var(--fg-3);
  }
</style>
