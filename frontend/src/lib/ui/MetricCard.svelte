<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    title,
    value,
    sub = '',
    tone = 'neutral',
    icon,
  }: {
    title: string;
    value: string | number;
    sub?: string;
    tone?: 'neutral' | 'ok' | 'busy' | 'err';
    icon?: Snippet;
  } = $props();
</script>

<div class="metric-card">
  <div class="head">
    <span class="title">{title}</span>
    {#if icon}
      <span class="icon">{@render icon()}</span>
    {/if}
  </div>
  <div class="value {tone}">{value}</div>
  {#if sub}
    <div class="sub">{sub}</div>
  {/if}
</div>

<style>
  .metric-card {
    background-color: var(--bg-1);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }
  .title {
    font-size: var(--text-sm);
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .icon {
    color: var(--fg-2);
    display: inline-flex;
  }
  .value {
    font-size: var(--text-2xl);
    font-weight: 700;
    line-height: 1.2;
    color: var(--fg-0);
  }
  .value.ok {
    color: var(--ok);
  }
  .value.busy {
    color: var(--busy);
  }
  .value.err {
    color: var(--err);
  }
  .sub {
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
</style>
