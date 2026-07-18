<script lang="ts">
  import type { Snippet } from 'svelte';

  type Tone = 'accent' | 'ok' | 'busy' | 'idle' | 'err' | 'off' | 'neutral';

  let {
    tone = 'neutral',
    children,
  }: {
    tone?: Tone;
    children: Snippet;
  } = $props();
</script>

<span class="badge {tone}">{@render children()}</span>

<style>
  .badge {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 2px 8px;
    border-radius: var(--radius-full);
    font-size: var(--text-xs);
    font-weight: 600;
    letter-spacing: 0.02em;
    line-height: 1.6;
    white-space: nowrap;
  }
  .badge.neutral {
    background-color: var(--bg-3);
    color: var(--fg-1);
  }
  .badge.accent {
    background-color: var(--accent-bg);
    color: var(--accent);
  }
  .badge.ok {
    background-color: var(--ok-bg);
    color: var(--ok);
  }
  .badge.busy {
    background-color: var(--busy-bg);
    color: var(--busy);
  }
  .badge.idle {
    background-color: var(--idle-bg);
    color: var(--fg-1);
  }
  .badge.err {
    background-color: var(--err-bg);
    color: var(--err);
  }
  .badge.off {
    background-color: var(--off-bg);
    color: var(--off);
  }
</style>
