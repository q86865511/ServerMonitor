<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    interactive = false,
    padding = true,
    onclick,
    children,
  }: {
    /** 可點擊卡片(如伺服器卡):加 hover 提升與 focus 樣式。 */
    interactive?: boolean;
    padding?: boolean;
    onclick?: (e: MouseEvent) => void;
    children: Snippet;
  } = $props();
</script>

{#if interactive}
  <button type="button" class="card interactive" class:no-padding={!padding} onclick={(e) => onclick?.(e)}>
    {@render children()}
  </button>
{:else}
  <div class="card" class:no-padding={!padding}>
    {@render children()}
  </div>
{/if}

<style>
  .card {
    display: block;
    width: 100%;
    text-align: left;
    background-color: var(--bg-1);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: var(--space-4);
    font: inherit;
    color: inherit;
  }
  .card.no-padding {
    padding: 0;
  }
  .card.interactive {
    cursor: pointer;
    transition: border-color var(--dur-fast) var(--ease-standard),
      box-shadow var(--dur-fast) var(--ease-standard),
      background-color var(--dur-fast) var(--ease-standard);
  }
  .card.interactive:hover {
    border-color: var(--line-strong);
    box-shadow: var(--shadow-sm);
  }
  .card.interactive:active {
    background-color: var(--bg-2);
  }
  .card.interactive:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
</style>
