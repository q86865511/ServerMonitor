<script lang="ts">
  import { createEventDispatcher } from 'svelte';

  export let title = '';
  export let wide = false;

  const dispatch = createEventDispatcher<{ close: void }>();

  function onKey(e: KeyboardEvent): void {
    if (e.key === 'Escape') dispatch('close');
  }
</script>

<svelte:window on:keydown={onKey} />

<div class="overlay" on:click|self={() => dispatch('close')}>
  <div class="modal" class:wide role="dialog" aria-modal="true">
    <div class="head">
      <h3>{title}</h3>
      <button class="ghost sm" on:click={() => dispatch('close')} aria-label="關閉">✕</button>
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
    background: rgba(0, 0, 0, 0.6);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 100;
    padding: 24px;
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
    box-shadow: 0 12px 40px rgba(0, 0, 0, 0.5);
  }
  .modal.wide {
    max-width: 860px;
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 14px 18px;
    border-bottom: 1px solid var(--line);
  }
  .body {
    padding: 18px;
    overflow-y: auto;
  }
</style>
