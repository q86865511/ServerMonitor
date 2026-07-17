<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    text,
    placement = 'top',
    children,
  }: {
    text: string;
    placement?: 'top' | 'bottom' | 'left' | 'right';
    children: Snippet;
  } = $props();

  let visible = $state(false);
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<span
  class="tooltip-wrap"
  onmouseenter={() => (visible = true)}
  onmouseleave={() => (visible = false)}
  onfocusin={() => (visible = true)}
  onfocusout={() => (visible = false)}
>
  {@render children()}
  {#if visible && text}
    <span class="bubble {placement}" role="tooltip">{text}</span>
  {/if}
</span>

<style>
  .tooltip-wrap {
    position: relative;
    display: inline-flex;
  }
  .bubble {
    position: absolute;
    z-index: var(--z-tooltip);
    background: var(--bg-3);
    border: 1px solid var(--line-strong);
    color: var(--fg-0);
    font-size: var(--text-xs);
    padding: 4px 8px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    box-shadow: var(--shadow-sm);
    pointer-events: none;
  }
  .bubble.top {
    bottom: calc(100% + 6px);
    left: 50%;
    transform: translateX(-50%);
  }
  .bubble.bottom {
    top: calc(100% + 6px);
    left: 50%;
    transform: translateX(-50%);
  }
  .bubble.left {
    right: calc(100% + 6px);
    top: 50%;
    transform: translateY(-50%);
  }
  .bubble.right {
    left: calc(100% + 6px);
    top: 50%;
    transform: translateY(-50%);
  }
</style>
