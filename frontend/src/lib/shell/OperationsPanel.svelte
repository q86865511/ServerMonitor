<script lang="ts">
  // 全域操作面板(階段 3B):頂欄觸發鈕(進行中數量 + 轉圈)+ 下拉列出進行中/剛完成的長操作。
  // 資料源與事件訂閱集中於 stores/operations;本元件僅呈現(不自行 EventsOn)。
  import { operations, kindLabel, type Operation } from '../stores/operations';
  import { stageLabel } from '../stores/provision';
  import Dropdown from '../ui/Dropdown.svelte';
  import ProgressBar from '../ui/ProgressBar.svelte';

  const runningCount = $derived($operations.filter((o) => o.status === 'running').length);

  function statusText(o: Operation): string {
    if (o.status === 'success') return '完成';
    if (o.status === 'error') return '失敗';
    return stageLabel(o.stage) || '處理中';
  }
</script>

<Dropdown align="right">
  {#snippet trigger({ toggle })}
    <button
      class="trigger"
      class:active={runningCount > 0}
      type="button"
      onclick={toggle}
      aria-label="操作活動"
      title="操作活動"
    >
      <span class="glyph" class:spin={runningCount > 0}>⟳</span>
      <span class="lbl">活動</span>
      {#if runningCount > 0}<span class="count">{runningCount}</span>{/if}
    </button>
  {/snippet}
  {#snippet children()}
    <div class="panel">
      <div class="panel-head">操作活動</div>
      {#if $operations.length === 0}
        <div class="empty">目前沒有進行中的操作</div>
      {:else}
        <ul class="op-list">
          {#each $operations as op (op.id)}
            <li class="op" class:done={op.status !== 'running'}>
              <div class="op-top">
                <span class="op-name" title={op.name}>{op.name}</span>
                <span class="op-kind">{kindLabel(op.kind)}</span>
              </div>
              <div class="op-bar">
                {#if op.status === 'running' && op.percent >= 0}
                  <ProgressBar value={Math.min(100, op.percent)} tone="busy" />
                {:else if op.status === 'running'}
                  <div class="indet-track"><div class="indet-fill"></div></div>
                {:else}
                  <ProgressBar value={100} tone={op.status === 'error' ? 'err' : 'ok'} />
                {/if}
              </div>
              <div class="op-sub">
                <span class="op-status {op.status}">{statusText(op)}</span>
                {#if op.status === 'error' && op.error}
                  <span class="op-detail" title={op.error}>{op.error}</span>
                {:else if op.detail}
                  <span class="op-detail" title={op.detail}>{op.detail}</span>
                {/if}
              </div>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/snippet}
</Dropdown>

<style>
  .trigger {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    background-color: var(--bg-3);
    border: 1px solid var(--line-strong);
    color: var(--fg-1);
    padding: 6px 10px;
    white-space: nowrap;
  }
  .trigger.active {
    color: var(--fg-0);
    border-color: var(--accent);
  }
  .glyph {
    display: inline-block;
    font-size: var(--text-md);
    line-height: 1;
  }
  .glyph.spin {
    animation: op-spin 1.1s linear infinite;
    color: var(--accent);
  }
  @keyframes op-spin {
    to {
      transform: rotate(360deg);
    }
  }
  .lbl {
    font-size: var(--text-sm);
  }
  .count {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 18px;
    height: 18px;
    padding: 0 5px;
    border-radius: var(--radius-full);
    background-color: var(--accent);
    color: var(--fg-on-accent);
    font-size: var(--text-xs);
    font-weight: 700;
  }

  .panel {
    width: 320px;
    max-width: 80vw;
  }
  .panel-head {
    font-size: var(--text-xs);
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    padding: 6px 8px;
  }
  .empty {
    color: var(--fg-2);
    font-size: var(--text-sm);
    padding: 12px 8px;
    text-align: center;
  }
  .op-list {
    list-style: none;
    margin: 0;
    padding: 0;
    max-height: 60vh;
    overflow-y: auto;
  }
  .op {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 10px 8px;
    border-top: 1px solid var(--line);
  }
  .op.done {
    opacity: 0.75;
  }
  .op-top {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
  }
  .op-name {
    color: var(--fg-0);
    font-size: var(--text-sm);
    font-weight: 600;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .op-kind {
    flex: none;
    color: var(--fg-2);
    font-size: var(--text-xs);
  }
  .op-sub {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    min-width: 0;
  }
  .op-status {
    flex: none;
    font-size: var(--text-xs);
    color: var(--busy);
  }
  .op-status.success {
    color: var(--ok);
  }
  .op-status.error {
    color: var(--err);
  }
  .op-detail {
    color: var(--fg-2);
    font-size: var(--text-xs);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  /* 不確定態(percent<0):動畫條表達進行中(同建立精靈)。 */
  .indet-track {
    height: 6px;
    background-color: var(--bg-3);
    border-radius: var(--radius-full);
    overflow: hidden;
  }
  .indet-fill {
    height: 100%;
    width: 40%;
    background-color: var(--busy);
    border-radius: var(--radius-full);
    animation: op-indet 1.1s ease-in-out infinite;
  }
  @keyframes op-indet {
    0% {
      margin-left: -40%;
    }
    100% {
      margin-left: 100%;
    }
  }
</style>
