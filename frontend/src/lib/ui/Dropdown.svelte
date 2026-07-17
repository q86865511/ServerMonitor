<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    open = $bindable(false),
    align = 'left',
    trigger,
    children,
  }: {
    open?: boolean;
    align?: 'left' | 'right';
    trigger: Snippet<[{ toggle: () => void }]>;
    children: Snippet<[{ close: () => void }]>;
  } = $props();

  let rootEl: HTMLDivElement | undefined = $state();

  function toggle(): void {
    open = !open;
  }
  function close(): void {
    open = false;
  }
  function onDocClick(e: MouseEvent): void {
    if (open && rootEl && !rootEl.contains(e.target as Node)) close();
  }
  function onKey(e: KeyboardEvent): void {
    if (open && e.key === 'Escape') close();
  }
</script>

<svelte:window onclick={onDocClick} onkeydown={onKey} />

<div class="dropdown" bind:this={rootEl}>
  {@render trigger({ toggle })}
  {#if open}
    <div class="menu" class:right={align === 'right'}>
      {@render children({ close })}
    </div>
  {/if}
</div>

<style>
  .dropdown {
    position: relative;
    display: inline-block;
  }
  .menu {
    position: absolute;
    top: calc(100% + 4px);
    left: 0;
    min-width: 180px;
    background: var(--bg-2);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    box-shadow: var(--shadow-md);
    z-index: var(--z-dropdown);
    padding: var(--space-1);
  }
  .menu.right {
    left: auto;
    right: 0;
  }
</style>
