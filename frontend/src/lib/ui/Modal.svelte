<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    title = '',
    wide = false,
    onClose,
    children,
    footer,
  }: {
    title?: string;
    wide?: boolean;
    onClose: () => void;
    children: Snippet;
    footer?: Snippet;
  } = $props();

  let dialogEl: HTMLDivElement | undefined = $state();

  function onKey(e: KeyboardEvent): void {
    if (e.key === 'Escape') onClose();
  }

  $effect(() => {
    // 開啟時把焦點移入對話框,關閉(元件卸載)時瀏覽器會自動歸還焦點給觸發元素。
    dialogEl?.focus();
  });
</script>

<svelte:window onkeydown={onKey} />

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="overlay" onclick={(e) => e.target === e.currentTarget && onClose()}>
  <div class="modal" class:wide bind:this={dialogEl} role="dialog" aria-modal="true" aria-label={title} tabindex="-1">
    <div class="head">
      <h3>{title}</h3>
      <button class="close" onclick={onClose} aria-label="關閉">✕</button>
    </div>
    <div class="body">
      {@render children()}
    </div>
    {#if footer}
      <div class="foot">
        {@render footer()}
      </div>
    {/if}
  </div>
</div>

<style>
  .overlay {
    position: fixed;
    inset: 0;
    background: var(--overlay);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: var(--z-modal);
    padding: var(--space-5);
  }
  .modal {
    background: var(--bg-1);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius);
    width: 100%;
    max-width: 520px;
    max-height: 90vh;
    display: flex;
    flex-direction: column;
    box-shadow: var(--shadow-lg);
  }
  .modal:focus-visible {
    outline: none;
  }
  .modal.wide {
    max-width: 860px;
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--line);
  }
  .close {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    background: transparent;
    border: 1px solid transparent;
    border-radius: var(--radius-sm);
    color: var(--fg-1);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard);
  }
  .close:hover {
    background-color: var(--bg-3);
    color: var(--fg-0);
  }
  .close:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .body {
    padding: var(--space-4);
    overflow-y: auto;
  }
  .foot {
    padding: var(--space-3) var(--space-4);
    border-top: 1px solid var(--line);
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
</style>
