<script lang="ts">
  export interface TabItem {
    id: string;
    label: string;
    disabled?: boolean;
  }

  let {
    items,
    active,
    onChange,
  }: {
    items: TabItem[];
    active: string;
    onChange: (id: string) => void;
  } = $props();
</script>

<div class="tabs" role="tablist">
  {#each items as item (item.id)}
    <button
      type="button"
      role="tab"
      class="tab"
      class:active={item.id === active}
      aria-selected={item.id === active}
      disabled={item.disabled}
      onclick={() => !item.disabled && onChange(item.id)}
    >
      {item.label}
    </button>
  {/each}
</div>

<style>
  .tabs {
    display: flex;
    gap: var(--space-1);
    border-bottom: 1px solid var(--line);
  }
  .tab {
    font: inherit;
    font-size: var(--text-base);
    color: var(--fg-1);
    background: transparent;
    border: none;
    border-bottom: 2px solid transparent;
    padding: var(--space-2) var(--space-3) 10px;
    cursor: pointer;
    transition: color var(--dur-fast) var(--ease-standard),
      border-color var(--dur-fast) var(--ease-standard);
  }
  .tab:hover:not(:disabled) {
    color: var(--fg-0);
  }
  .tab:active:not(:disabled) {
    color: var(--fg-0);
  }
  .tab:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  .tab:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
  .tab.active {
    color: var(--fg-0);
    border-bottom-color: var(--accent);
    font-weight: 600;
  }
</style>
