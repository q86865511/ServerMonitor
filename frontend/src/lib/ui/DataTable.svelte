<script lang="ts" module>
  import type { Snippet } from 'svelte';

  export interface DataTableColumn<Row> {
    /** 欄鍵;無 cell snippet 時以此讀取 row[key] 顯示。 */
    key: string;
    label: string;
    /** CSS 寬度(如 '120px'、'20%');省略則自動。 */
    width?: string;
    align?: 'left' | 'right' | 'center';
    /** 自訂儲存格渲染;省略則顯示 row[key] 字串。 */
    cell?: Snippet<[Row]>;
  }
</script>

<script lang="ts" generics="Row">
  let {
    columns,
    rows,
    rowKey,
    onRowClick,
    empty,
  }: {
    columns: DataTableColumn<Row>[];
    rows: Row[];
    /** 穩定鍵取值,供 keyed each。 */
    rowKey: (row: Row) => string | number;
    /** 選配:點列回呼(提供時列可點擊 / 鍵盤操作)。 */
    onRowClick?: (row: Row) => void;
    /** 空資料時的自訂內容(省略則顯示預設「無資料」)。 */
    empty?: Snippet;
  } = $props();

  const clickable = $derived(!!onRowClick);

  function cellText(row: Row, key: string): string {
    const v = (row as Record<string, unknown>)[key];
    return v == null ? '' : String(v);
  }

  function onKey(e: KeyboardEvent, row: Row): void {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      onRowClick?.(row);
    }
  }
</script>

<div class="dt-wrap">
  <table class="dt">
    <thead>
      <tr>
        {#each columns as col (col.key)}
          <th style:width={col.width} style:text-align={col.align ?? 'left'}>{col.label}</th>
        {/each}
      </tr>
    </thead>
    <tbody>
      {#if rows.length === 0}
        <tr class="empty-row">
          <td colspan={columns.length}>
            {#if empty}
              {@render empty()}
            {:else}
              <div class="empty">無資料</div>
            {/if}
          </td>
        </tr>
      {:else}
        {#each rows as row (rowKey(row))}
          <tr
            class:clickable
            role={clickable ? 'button' : undefined}
            tabindex={clickable ? 0 : undefined}
            onclick={clickable ? () => onRowClick?.(row) : undefined}
            onkeydown={clickable ? (e) => onKey(e, row) : undefined}
          >
            {#each columns as col (col.key)}
              <td style:text-align={col.align ?? 'left'}>
                {#if col.cell}
                  {@render col.cell(row)}
                {:else}
                  {cellText(row, col.key)}
                {/if}
              </td>
            {/each}
          </tr>
        {/each}
      {/if}
    </tbody>
  </table>
</div>

<style>
  .dt-wrap {
    width: 100%;
    overflow-x: auto;
  }
  .dt {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }
  .dt th,
  .dt td {
    padding: 8px 10px;
    border-bottom: 1px solid var(--line);
    vertical-align: middle;
  }
  .dt th {
    color: var(--fg-2);
    font-weight: 600;
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    white-space: nowrap;
  }
  .dt td {
    color: var(--fg-0);
  }
  .dt tbody tr.clickable {
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard);
  }
  .dt tbody tr.clickable:hover {
    background-color: var(--bg-2);
  }
  .dt tbody tr.clickable:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  .empty-row td {
    border-bottom: none;
  }
  .empty {
    color: var(--fg-2);
    text-align: center;
    padding: 28px 0;
  }
</style>
