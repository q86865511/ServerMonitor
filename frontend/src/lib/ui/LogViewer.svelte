<script lang="ts">
  import type { Snippet } from 'svelte';
  import type { LogRow, LogLevel } from './types';

  let {
    rows,
    connected = true,
    onClear,
    commandBar,
    emptyHint = '等待輸出…',
  }: {
    /** 日誌行(呼叫端 / store 持有並裁切上限,元件不複製大陣列)。 */
    rows: LogRow[];
    /** 串流連線狀態,顯示於工具列。 */
    connected?: boolean;
    /** 清除回呼;未提供時不顯示清除鈕(buffer 所有權在呼叫端)。 */
    onClear?: () => void;
    /** 指令輸入列(由 ConsoleTab 注入;LogViewer 本身純呈現日誌)。 */
    commandBar?: Snippet;
    /** 無任何日誌時的提示文字。 */
    emptyHint?: string;
  } = $props();

  // ---- 篩選狀態(元件內,即時生效)----
  let search = $state('');
  let levels = $state<Record<LogLevel, boolean>>({ info: true, warn: true, error: true });

  const filtered = $derived.by(() => {
    const q = search.trim().toLowerCase();
    // 單次過濾,不產生中間陣列;rows 上限由呼叫端(3000)控制。
    return rows.filter(
      (r) => levels[r.level] && (q === '' || r.line.toLowerCase().includes(q)),
    );
  });

  // ---- 自動捲底 / 暫停 ----
  let autoScroll = $state(true);
  let hasNew = $state(false);
  let scrollBox = $state<HTMLDivElement | null>(null);

  // 追蹤最後一行 id:僅在「真的有新行追加」時觸發捲動 / 新日誌提示,
  // 與純篩選變更區分(篩選變更不應誤報有新日誌)。
  const lastId = $derived(rows.length ? rows[rows.length - 1].id : 0);

  $effect(() => {
    // 依賴 lastId(新行追加)與 autoScroll。
    lastId;
    if (!scrollBox) return;
    if (autoScroll) {
      // $effect 於 DOM patch 後執行,scrollHeight 已反映新行。
      scrollBox.scrollTop = scrollBox.scrollHeight;
      hasNew = false;
    } else {
      hasNew = true;
    }
  });

  function onScroll(): void {
    const el = scrollBox;
    if (!el) return;
    // 使用者上捲即暫停;捲回底部自動恢復並清除新日誌提示。
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
    autoScroll = atBottom;
    if (atBottom) hasNew = false;
  }

  function jumpToBottom(): void {
    autoScroll = true;
    hasNew = false;
    if (scrollBox) scrollBox.scrollTop = scrollBox.scrollHeight;
  }

  function toggleLevel(l: LogLevel): void {
    levels = { ...levels, [l]: !levels[l] };
  }

  // ---- 複製 / 匯出(作用於目前篩選後的視圖)----
  let copied = $state(false);
  async function copyView(): Promise<void> {
    const text = filtered.map((r) => r.line).join('\n');
    try {
      await navigator.clipboard.writeText(text);
      copied = true;
      setTimeout(() => (copied = false), 1500);
    } catch {
      /* 剪貼簿不可用時靜默略過 */
    }
  }

  function exportView(): void {
    const text = filtered.map((r) => r.line).join('\n');
    const blob = new Blob([text], { type: 'text/plain;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `logs-${new Date().toISOString().replace(/[:.]/g, '-')}.log`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  }

  // stream 優先於 level 決定樣式(監控 / 回顯行有專屬色)。
  function lineClass(r: LogRow): LogLevel | 'monitor' | 'echo' | 'echo-out' {
    switch (r.stream) {
      case 'gsm-monitor':
        return 'monitor';
      case 'echo':
        return 'echo';
      case 'echo-out':
        return 'echo-out';
      default:
        return r.level;
    }
  }

  const LEVEL_LABELS: Record<LogLevel, string> = { info: 'INFO', warn: 'WARN', error: 'ERROR' };
</script>

<div class="log-viewer">
  <div class="toolbar">
    <span class="conn" class:on={connected} title={connected ? '串流連線中' : '串流未連線'}>
      <span class="dot" aria-hidden="true"></span>
      {connected ? '連線中' : '未連線'}
    </span>

    <span class="count">{filtered.length} / {rows.length} 行</span>

    <div class="levels" role="group" aria-label="等級篩選">
      {#each ['info', 'warn', 'error'] as const as l}
        <button
          type="button"
          class="lvl {l}"
          class:active={levels[l]}
          aria-pressed={levels[l]}
          onclick={() => toggleLevel(l)}
        >
          {LEVEL_LABELS[l]}
        </button>
      {/each}
    </div>

    <div class="search">
      <span class="s-icon" aria-hidden="true">🔍</span>
      <input
        type="search"
        placeholder="搜尋日誌…"
        aria-label="搜尋日誌"
        bind:value={search}
      />
      {#if search}
        <button type="button" class="s-clear" aria-label="清除搜尋" onclick={() => (search = '')}>✕</button>
      {/if}
    </div>

    <div class="spacer"></div>

    <button type="button" class="act" onclick={copyView} disabled={filtered.length === 0} title="複製目前檢視的日誌">
      {copied ? '已複製' : '複製'}
    </button>
    <button type="button" class="act" onclick={exportView} disabled={filtered.length === 0} title="匯出目前檢視為 .log 檔">
      匯出
    </button>
    {#if onClear}
      <button type="button" class="act danger" onclick={onClear} disabled={rows.length === 0} title="清除日誌">
        清除
      </button>
    {/if}
  </div>

  <div class="log-area">
    <div class="log mono" bind:this={scrollBox} onscroll={onScroll} tabindex="-1">
      {#each filtered as r (r.id)}
        <div class="line {lineClass(r)}">{r.line}</div>
      {/each}
      {#if rows.length === 0}
        <div class="placeholder">{emptyHint}</div>
      {:else if filtered.length === 0}
        <div class="placeholder">無符合條件的日誌</div>
      {/if}
    </div>

    {#if !autoScroll && hasNew}
      <button type="button" class="jump" onclick={jumpToBottom}>
        ↓ 有新日誌
      </button>
    {/if}
  </div>

  {#if commandBar}
    <div class="cmd-slot">{@render commandBar()}</div>
  {/if}
</div>

<style>
  .log-viewer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    height: 100%;
    min-height: 0;
  }

  /* ---- 工具列 ---- */
  .toolbar {
    flex: none;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .conn {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
  .conn .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--idle);
    flex: none;
  }
  .conn.on .dot {
    background: var(--ok);
    box-shadow: 0 0 6px var(--ok);
  }
  .count {
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
  .spacer {
    flex: 1 1 auto;
  }

  .levels {
    display: inline-flex;
    gap: 4px;
  }
  .lvl {
    font-size: var(--text-xs);
    font-weight: 600;
    letter-spacing: 0.03em;
    padding: 3px 8px;
    border-radius: var(--radius-full);
    border: 1px solid var(--line-strong);
    background: transparent;
    color: var(--fg-2);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard),
      color var(--dur-fast) var(--ease-standard),
      border-color var(--dur-fast) var(--ease-standard);
  }
  .lvl:hover:not(.active) {
    color: var(--fg-1);
    border-color: var(--fg-2);
  }
  .lvl:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
  .lvl.info.active {
    background: var(--accent-bg);
    border-color: var(--accent);
    color: var(--accent);
  }
  .lvl.warn.active {
    background: var(--busy-bg);
    border-color: var(--busy);
    color: var(--busy);
  }
  .lvl.error.active {
    background: var(--err-bg);
    border-color: var(--err);
    color: var(--err);
  }

  .search {
    display: flex;
    align-items: center;
    gap: 5px;
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 0 8px;
    min-width: 160px;
    transition: border-color var(--dur-fast) var(--ease-standard);
  }
  .search:focus-within {
    border-color: var(--accent);
  }
  .s-icon {
    color: var(--fg-2);
    font-size: var(--text-sm);
    flex: none;
  }
  .search input {
    flex: 1;
    border: none;
    background: transparent;
    padding: 5px 0;
    width: auto;
    font-size: var(--text-sm);
  }
  .search input:focus {
    outline: none;
  }
  .search input::-webkit-search-cancel-button {
    display: none;
  }
  .s-clear {
    flex: none;
    background: transparent;
    border: none;
    color: var(--fg-2);
    cursor: pointer;
    padding: 2px 3px;
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
  }
  .s-clear:hover {
    color: var(--fg-0);
  }

  .act {
    font-size: var(--text-sm);
    padding: 4px 10px;
    background-color: var(--bg-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    color: var(--fg-0);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard),
      border-color var(--dur-fast) var(--ease-standard);
  }
  .act:hover:not(:disabled) {
    background-color: var(--line);
    border-color: var(--fg-2);
  }
  .act:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .act:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
  .act.danger {
    color: var(--err);
    border-color: var(--err);
    background: transparent;
  }
  .act.danger:hover:not(:disabled) {
    background-color: var(--err-bg);
  }

  /* ---- 日誌區 ---- */
  .log-area {
    position: relative;
    flex: 1 1 auto;
    min-height: 0;
  }
  .log {
    height: 100%;
    overflow-y: auto;
    background: var(--bg-0);
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    padding: 10px;
    font-size: 12.5px;
    line-height: 1.55;
  }
  .log:focus-visible {
    outline: none;
  }
  .line {
    /* 效能:超出視窗的行不參與排版 / 繪製;估計行高 20px 供捲軸尺寸估算。 */
    content-visibility: auto;
    contain-intrinsic-size: auto 20px;
    white-space: pre-wrap;
    word-break: break-word;
    color: var(--fg-1);
  }
  .line.info {
    color: var(--fg-1);
  }
  .line.warn {
    color: var(--busy);
  }
  .line.error {
    color: var(--err);
  }
  .line.monitor {
    color: var(--busy);
    font-style: italic;
  }
  .line.echo {
    color: var(--accent);
  }
  .line.echo-out {
    color: var(--fg-1);
  }
  .placeholder {
    color: var(--fg-2);
    padding: 4px 0;
  }

  .jump {
    position: absolute;
    right: 14px;
    bottom: 12px;
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: var(--text-sm);
    padding: 5px 12px;
    background-color: var(--accent);
    border: 1px solid var(--accent);
    color: var(--fg-on-accent);
    border-radius: var(--radius-full);
    box-shadow: var(--shadow-md);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard);
  }
  .jump:hover {
    background-color: var(--accent-hover);
  }
  .jump:focus-visible {
    outline: 2px solid var(--fg-on-accent);
    outline-offset: 2px;
  }

  .cmd-slot {
    flex: none;
  }
</style>
