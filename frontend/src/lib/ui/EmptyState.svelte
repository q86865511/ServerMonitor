<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    title,
    description = '',
    icon,
    action,
  }: {
    title: string;
    description?: string;
    icon?: Snippet;
    action?: Snippet;
  } = $props();
</script>

<div class="empty-state">
  {#if icon}
    <div class="icon">{@render icon()}</div>
  {/if}
  <div class="title">{title}</div>
  {#if description}
    <div class="desc">{description}</div>
  {/if}
  {#if action}
    <div class="action">{@render action()}</div>
  {/if}
</div>

<style>
  .empty-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: var(--space-2);
    padding: var(--space-7) var(--space-4);
    color: var(--fg-2);
  }
  .icon {
    font-size: 32px;
    color: var(--fg-2);
    margin-bottom: var(--space-1);
  }
  .title {
    font-size: var(--text-md);
    font-weight: 600;
    color: var(--fg-1);
  }
  .desc {
    font-size: var(--text-sm);
    max-width: 380px;
  }
  .action {
    margin-top: var(--space-3);
  }
</style>
