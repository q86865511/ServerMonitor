<script lang="ts">
  import type { Snippet } from 'svelte';

  type Variant = 'secondary' | 'ghost' | 'danger';
  type Size = 'sm' | 'md';

  let {
    label,
    variant = 'ghost',
    size = 'md',
    disabled = false,
    onclick,
    children,
  }: {
    /** aria-label,無障礙必填(圖示按鈕無可見文字)。 */
    label: string;
    variant?: Variant;
    size?: Size;
    disabled?: boolean;
    onclick?: (e: MouseEvent) => void;
    children: Snippet;
  } = $props();
</script>

<button
  class="icon-btn {variant}"
  class:sm={size === 'sm'}
  type="button"
  aria-label={label}
  title={label}
  {disabled}
  onclick={(e) => !disabled && onclick?.(e)}
>
  {@render children()}
</button>

<style>
  .icon-btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 32px;
    height: 32px;
    color: var(--fg-1);
    background-color: transparent;
    border: 1px solid transparent;
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard),
      color var(--dur-fast) var(--ease-standard),
      border-color var(--dur-fast) var(--ease-standard);
  }
  .icon-btn.sm {
    width: 26px;
    height: 26px;
  }
  .icon-btn.secondary {
    background-color: var(--bg-3);
    border-color: var(--line-strong);
    color: var(--fg-0);
  }
  .icon-btn:hover:not(:disabled) {
    background-color: var(--bg-3);
    color: var(--fg-0);
  }
  .icon-btn:active:not(:disabled) {
    background-color: var(--bg-2);
  }
  .icon-btn:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .icon-btn:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
  .icon-btn.danger {
    color: var(--err);
  }
  .icon-btn.danger:hover:not(:disabled) {
    background-color: var(--err-bg);
  }
</style>
