<script context="module" lang="ts">
  let modalSequence = 0;
</script>

<script lang="ts">
  import { createEventDispatcher, onMount, tick } from 'svelte';
  import Icon from './Icon.svelte';

  export let title = '';
  export let wide = false;

  const dispatch = createEventDispatcher<{ close: void }>();
  const titleId = `modal-title-${++modalSequence}`;
  let dialog: HTMLDivElement;
  let previousActive: HTMLElement | null = null;

  function getFocusable(): HTMLElement[] {
    if (!dialog) return [];
    return Array.from(
      dialog.querySelectorAll<HTMLElement>(
        'button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [href], [tabindex]:not([tabindex="-1"])',
      ),
    ).filter((element) => element.offsetParent !== null);
  }

  function close(): void {
    dispatch('close');
  }

  function onKey(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }
    if (e.key !== 'Tab') return;

    const focusable = getFocusable();
    if (focusable.length === 0) {
      e.preventDefault();
      dialog?.focus();
      return;
    }

    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  onMount(() => {
    previousActive = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    void tick().then(() => {
      const preferred = dialog?.querySelector<HTMLElement>(
        '[data-autofocus], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), button:not(:disabled):not(.modal-close)',
      );
      (preferred ?? dialog)?.focus();
    });

    return () => {
      previousActive?.focus();
    };
  });
</script>

<svelte:window on:keydown={onKey} />

<div class="overlay">
  <button class="backdrop" type="button" on:click={close} aria-label="關閉對話框"></button>
  <div
    bind:this={dialog}
    class="modal"
    class:wide
    role="dialog"
    aria-modal="true"
    aria-labelledby={titleId}
    tabindex="-1"
  >
    <div class="head">
      <div>
        <div class="window-code mono">WORKSPACE / DIALOG</div>
        <h3 id={titleId}>{title}</h3>
      </div>
      <button class="ghost icon-button modal-close" type="button" on:click={close} aria-label="關閉">
        <Icon name="close" size={16} />
      </button>
    </div>
    <div class="body">
      <slot />
    </div>
  </div>
</div>

<style>
  .overlay {
    position: fixed;
    inset: 0;
    z-index: 100;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 24px;
  }

  .backdrop {
    position: absolute;
    inset: 0;
    width: 100%;
    height: 100%;
    min-height: 0;
    padding: 0;
    background: rgba(5, 6, 5, 0.78);
    border: 0;
    border-radius: 0;
    cursor: default;
  }

  .backdrop:hover:not(:disabled) {
    background: rgba(5, 6, 5, 0.78);
    border: 0;
  }

  .modal {
    position: relative;
    width: 100%;
    max-width: 540px;
    max-height: 90vh;
    display: flex;
    flex-direction: column;
    background: var(--bg-1);
    border: 1px solid var(--line-strong);
    border-top: 2px solid var(--accent);
    border-radius: var(--radius);
    box-shadow: 0 18px 54px rgba(0, 0, 0, 0.46);
  }

  .modal:focus {
    outline: none;
  }

  .modal.wide {
    max-width: 900px;
  }

  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
    min-height: 64px;
    padding: 11px 14px 11px 18px;
    background: var(--bg-2);
    border-bottom: 1px solid var(--line);
  }

  .window-code {
    margin-bottom: 2px;
    color: var(--fg-3);
    font-size: 8px;
    font-weight: 700;
    letter-spacing: 0.12em;
  }

  .head h3 {
    font-size: 16px;
  }

  .body {
    min-height: 0;
    padding: 20px;
    overflow-y: auto;
  }

  @media (max-width: 820px) {
    .overlay {
      padding: 12px;
    }

    .modal,
    .modal.wide {
      max-width: none;
      max-height: calc(100vh - 24px);
    }

    .body {
      padding: 16px;
    }
  }
</style>
