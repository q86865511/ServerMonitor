<script lang="ts">
  import { onMount, onDestroy, createEventDispatcher } from 'svelte';
  import { main, protocol } from '../../wailsjs/go/models';
  import {
    StartInstance,
    StopInstance,
    RestartInstance,
    RemoveInstance,
    SubscribeStats,
    UnsubscribeStats,
  } from '../../wailsjs/go/main/App';
  import { EventsOn, EventsOff } from '../../wailsjs/runtime/runtime';
  import { call } from './api';
  import { pushToast } from './stores';
  import { fmtBytes, fmtCPU, fmtPlayers, fmtOnline, memRatio, stateTone } from './format';
  import StatePill from './StatePill.svelte';
  import Modal from './Modal.svelte';
  import Icon from './Icon.svelte';

  export let inst: main.InstanceDTO;

  const dispatch = createEventDispatcher<{ console: string; changed: void }>();

  let stats: protocol.ResourceStats | null = null;
  let busy = ''; // 進行中的操作名稱(停用按鈕)
  let confirmRemove = false;
  let removing = false;
  // 是否連同 data/backups 一併刪除;預設不勾,避免誤刪伺服器資料(R11)。
  let purgeOnRemove = false;

  const evName = `stats:${inst.uuid}`;
  let unlisten: (() => void) | null = null;
  // 保存訂閱 promise:onDestroy 須等它落地才能 Unsubscribe,避免卸載過快時
  // SubscribeStats 尚未 resolve 就送出取消訂閱,留下孤兒訂閱。
  let subscribed: Promise<void> | null = null;

  onMount(() => {
    unlisten = EventsOn(evName, (s: protocol.ResourceStats) => {
      stats = s;
    });
    subscribed = call(() => SubscribeStats(inst.uuid), { silent: true })
      .then(() => undefined)
      .catch(() => {
        /* 靜默:節點離線時訂閱可能失敗,卡片仍顯示狀態 */
      });
  });

  onDestroy(async () => {
    if (unlisten) unlisten();
    EventsOff(evName);
    if (subscribed) await subscribed;
    try {
      await UnsubscribeStats(inst.uuid);
    } catch {
      /* 忽略卸載期錯誤 */
    }
  });

  async function act(name: string, fn: () => Promise<void>): Promise<void> {
    busy = name;
    try {
      await call(fn);
      dispatch('changed');
    } catch {
      /* toast 已呈現 */
    } finally {
      busy = '';
    }
  }

  function openRemoveConfirm(): void {
    purgeOnRemove = false;
    confirmRemove = true;
  }

  async function doRemove(): Promise<void> {
    removing = true;
    try {
      await call(() => RemoveInstance(inst.uuid, purgeOnRemove));
      pushToast('success', `已移除實例 ${inst.uuid}`);
      confirmRemove = false;
      dispatch('changed');
    } catch {
      /* toast 已呈現 */
    } finally {
      removing = false;
    }
  }

  $: running = inst.observed_state === 'Running';
  $: ratio = stats ? memRatio(stats.memory_bytes, stats.memory_limit) : null;
  $: diskBytes = stats ? stats.data_disk_bytes : undefined;
  $: players = stats ? stats.player_count : undefined;
  $: online = stats ? stats.online : undefined;
  $: tone = stateTone(inst.observed_state);
</script>

<article class="card instance {tone}">
  <div class="top">
    <div class="ident">
      <div class="template-name">{inst.template_id}</div>
      <div class="variant mono">{inst.variant || 'default'}</div>
    </div>
    <StatePill state={inst.observed_state} desired={inst.desired_state} />
  </div>

  <div class="identity-grid">
    <div>
      <span class="identity-label">INSTANCE</span>
      <span class="identity-value mono" title={inst.uuid}>{inst.uuid}</span>
    </div>
    <div>
      <span class="identity-label">NODE</span>
      <span class="identity-value mono" title={inst.node}>{inst.node}</span>
    </div>
  </div>

  <div class="metrics">
    <div class="metric">
      <span class="k">CPU</span>
      <span class="v mono">{fmtCPU(stats?.cpu_percent)}</span>
    </div>
    <div class="metric">
      <span class="k">記憶體</span>
      <span class="v mono">{fmtBytes(stats?.memory_bytes)}</span>
      {#if ratio != null}
        <div class="bar" aria-label={`記憶體使用率 ${Math.round(ratio * 100)}%`}>
          <div class="fill" style="width:{Math.round(ratio * 100)}%"></div>
        </div>
      {/if}
    </div>
    <div class="metric">
      <span class="k">磁碟</span>
      <span class="v mono">{fmtBytes(diskBytes)}</span>
    </div>
    <div class="metric">
      <span class="k">玩家</span>
      <span class="v mono">{fmtPlayers(players)} <span class="online">{fmtOnline(online)}</span></span>
    </div>
  </div>

  <div class="actions">
    <div class="lifecycle-actions">
      {#if running}
        <button class="sm" on:click={() => act('stop', () => StopInstance(inst.uuid))} disabled={!!busy}>
          <Icon name="stop" size={13} />
          <span>{busy === 'stop' ? '停止中…' : '停止'}</span>
        </button>
        <button class="sm" on:click={() => act('restart', () => RestartInstance(inst.uuid))} disabled={!!busy}>
          <Icon name="restart" size={14} />
          <span>{busy === 'restart' ? '重啟中…' : '重啟'}</span>
        </button>
      {:else}
        <button
          class="sm primary"
          on:click={() => act('start', () => StartInstance(inst.uuid))}
          disabled={!!busy}
        >
          <Icon name="play" size={13} />
          <span>{busy === 'start' ? '啟動中…' : '啟動'}</span>
        </button>
      {/if}
      <button class="sm" on:click={() => dispatch('console', inst.uuid)} disabled={!!busy}>
        <Icon name="terminal" size={14} />
        <span>主控台</span>
      </button>
    </div>
    <button class="sm danger remove" on:click={openRemoveConfirm} disabled={!!busy} aria-label={`移除 ${inst.uuid}`}>
      <Icon name="trash" size={14} />
      <span>移除</span>
    </button>
  </div>
</article>

{#if confirmRemove}
  <Modal title="移除實例" on:close={() => (confirmRemove = false)}>
    <p class="msg">確定移除 {inst.uuid}?</p>
    <div class="checkbox-row">
      <input id={`purge-${inst.uuid}`} type="checkbox" bind:checked={purgeOnRemove} disabled={removing} />
      <label for={`purge-${inst.uuid}`}>同時刪除伺服器資料與備份(data 與 backups,無法復原)</label>
    </div>
    <div class="hint" style="margin-top:6px">
      {purgeOnRemove
        ? '將移除實例並清除其資料與備份,無法復原。'
        : '僅移除實例本身,伺服器資料與備份會保留。'}
    </div>
    <div class="actions">
      <button on:click={() => (confirmRemove = false)} disabled={removing}>取消</button>
      <button class="danger" on:click={doRemove} disabled={removing}>
        {removing ? '處理中…' : purgeOnRemove ? '移除並清除資料' : '僅移除'}
      </button>
    </div>
  </Modal>
{/if}

<style>
  .instance {
    position: relative;
    display: flex;
    flex-direction: column;
    gap: 15px;
    min-width: 0;
    overflow: hidden;
    border-left-width: 3px;
  }

  .instance.ok { border-left-color: var(--ok); }
  .instance.busy { border-left-color: var(--busy); }
  .instance.err { border-left-color: var(--err); }
  .instance.idle { border-left-color: var(--idle); }
  .instance.off { border-left-color: var(--off); }

  .top {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px;
  }

  .template-name {
    color: var(--fg-0);
    font-size: 16px;
    font-weight: 700;
    line-height: 1.2;
    text-transform: capitalize;
  }

  .variant {
    margin-top: 3px;
    color: var(--fg-2);
    font-size: 10px;
    letter-spacing: 0.06em;
    text-transform: uppercase;
  }

  .identity-grid {
    display: grid;
    grid-template-columns: minmax(0, 1.6fr) minmax(0, 1fr);
    gap: 10px;
    padding: 10px 11px;
    background: var(--bg-inset);
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
  }

  .identity-grid > div {
    min-width: 0;
  }

  .identity-label {
    display: block;
    margin-bottom: 3px;
    color: var(--fg-3);
    font-size: 9px;
    font-weight: 700;
    letter-spacing: 0.1em;
  }

  .identity-value {
    display: block;
    overflow: hidden;
    color: var(--fg-1);
    font-size: 10px;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .metrics {
    display: grid;
    grid-template-columns: repeat(2, 1fr);
    border-top: 1px solid var(--line);
    border-left: 1px solid var(--line);
  }

  .metric {
    display: flex;
    flex-direction: column;
    min-height: 68px;
    gap: 4px;
    padding: 10px 11px;
    border-right: 1px solid var(--line);
    border-bottom: 1px solid var(--line);
  }

  .metric .k {
    color: var(--fg-3);
    font-size: 9px;
    font-weight: 700;
    letter-spacing: 0.09em;
    text-transform: uppercase;
  }

  .metric .v {
    color: var(--fg-0);
    font-size: 15px;
    font-weight: 600;
  }

  .online {
    margin-left: 3px;
    color: var(--fg-3);
    font-size: 11px;
  }

  .bar {
    height: 3px;
    background: var(--bg-3);
    margin-top: 2px;
    overflow: hidden;
  }

  .fill {
    height: 100%;
    background: var(--accent);
  }

  .msg {
    margin: 0 0 14px;
    color: var(--fg-1);
    white-space: pre-wrap;
    word-break: break-word;
  }

  .actions {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding-top: 1px;
  }

  .lifecycle-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .actions button {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }

  .remove {
    flex: none;
    margin-left: auto;
  }

  @media (max-width: 430px) {
    .actions {
      align-items: stretch;
      flex-direction: column;
    }

    .remove {
      margin-left: 0;
    }
  }
</style>
