<script lang="ts">
  import type { Snippet } from 'svelte';

  type Variant = 'primary' | 'secondary' | 'danger' | 'ghost';
  type Size = 'sm' | 'md';

  let {
    variant = 'secondary',
    size = 'md',
    type = 'button',
    loading = false,
    disabled = false,
    fullWidth = false,
    onclick,
    children,
  }: {
    variant?: Variant;
    size?: Size;
    type?: 'button' | 'submit' | 'reset';
    loading?: boolean;
    disabled?: boolean;
    fullWidth?: boolean;
    onclick?: (e: MouseEvent) => void;
    children: Snippet;
  } = $props();

  const isDisabled = $derived(disabled || loading);
</script>

<button
  class="btn {variant}"
  class:sm={size === 'sm'}
  class:full={fullWidth}
  {type}
  disabled={isDisabled}
  aria-busy={loading}
  onclick={(e) => !isDisabled && onclick?.(e)}
>
  {#if loading}
    <span class="spinner" aria-hidden="true"></span>
  {/if}
  <span class="label">{@render children()}</span>
</button>

<style>
  .btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-2);
    font: inherit;
    font-size: var(--text-base);
    color: var(--fg-0);
    background-color: var(--bg-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 7px 14px;
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard),
      border-color var(--dur-fast) var(--ease-standard),
      opacity var(--dur-fast) var(--ease-standard);
  }
  .btn.sm {
    padding: 4px 10px;
    font-size: var(--text-sm);
  }
  .btn.full {
    width: 100%;
  }
  .btn:hover:not(:disabled) {
    background-color: var(--line);
    border-color: var(--fg-2);
  }
  .btn:active:not(:disabled) {
    background-color: var(--bg-2);
  }
  .btn:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }

  .btn.primary {
    background-color: var(--accent);
    border-color: var(--accent);
    color: var(--fg-on-accent);
  }
  .btn.primary:hover:not(:disabled) {
    background-color: var(--accent-hover);
    border-color: var(--accent-hover);
  }
  .btn.primary:active:not(:disabled) {
    background-color: var(--accent-hover);
    opacity: 0.9;
  }

  .btn.danger {
    background-color: transparent;
    border-color: var(--err);
    color: var(--err);
  }
  .btn.danger:hover:not(:disabled) {
    background-color: var(--err-bg);
  }
  .btn.danger:active:not(:disabled) {
    background-color: var(--err-bg);
    opacity: 0.85;
  }

  .btn.ghost {
    background-color: transparent;
    border-color: transparent;
  }
  .btn.ghost:hover:not(:disabled) {
    background-color: var(--bg-3);
  }
  .btn.ghost:active:not(:disabled) {
    background-color: var(--bg-2);
  }

  .spinner {
    width: 13px;
    height: 13px;
    border-radius: 50%;
    border: 2px solid currentColor;
    border-top-color: transparent;
    opacity: 0.7;
    animation: spin 0.7s linear infinite;
    flex: none;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>
