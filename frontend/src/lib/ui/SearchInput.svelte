<script lang="ts">
  let {
    value = $bindable(''),
    placeholder = '搜尋…',
    disabled = false,
    onSearch,
  }: {
    value?: string;
    placeholder?: string;
    disabled?: boolean;
    /** 每次值變更時呼叫(未內建 debounce,需要防抖由呼叫端處理)。 */
    onSearch?: (value: string) => void;
  } = $props();

  function onInput(e: Event): void {
    value = (e.currentTarget as HTMLInputElement).value;
    onSearch?.(value);
  }
  function clear(): void {
    value = '';
    onSearch?.('');
  }
</script>

<div class="search" class:disabled>
  <span class="icon" aria-hidden="true">🔍</span>
  <input
    type="search"
    {placeholder}
    {disabled}
    {value}
    oninput={onInput}
    aria-label={placeholder}
  />
  {#if value}
    <button type="button" class="clear" aria-label="清除搜尋" onclick={clear}>✕</button>
  {/if}
</div>

<style>
  .search {
    display: flex;
    align-items: center;
    gap: 6px;
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 0 8px;
    transition: border-color var(--dur-fast) var(--ease-standard);
  }
  .search:focus-within {
    border-color: var(--accent);
  }
  .search.disabled {
    opacity: 0.5;
  }
  .icon {
    color: var(--fg-2);
    font-size: var(--text-sm);
    flex: none;
  }
  input {
    flex: 1;
    border: none;
    background: transparent;
    padding: 6px 0;
    width: auto;
  }
  input:focus {
    outline: none;
  }
  input::-webkit-search-cancel-button {
    display: none;
  }
  .clear {
    flex: none;
    background: transparent;
    border: none;
    color: var(--fg-2);
    cursor: pointer;
    padding: 2px 4px;
    border-radius: var(--radius-sm);
  }
  .clear:hover {
    color: var(--fg-0);
    background-color: var(--bg-3);
  }
  .clear:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
</style>
