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
  import { fmtBytes, fmtCPU, fmtPlayers, fmtOnline, memRatio } from './format';
  import StatePill from './StatePill.svelte';
  import Modal from './Modal.svelte';

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
</script>

<div class="card instance">
  <div class="spread top">
    <div class="ident">
      <div class="name mono">{inst.uuid}</div>
      <div class="sub muted">{inst.template_id} · {inst.variant || '預設'} · {inst.node}</div>
    </div>
    <StatePill state={inst.observed_state} desired={inst.desired_state} />
  </div>

  <div class="metrics">
    <div class="metric">
      <span class="k">CPU</span>
      <span class="v">{fmtCPU(stats?.cpu_percent)}</span>
    </div>
    <div class="metric">
      <span class="k">記憶體</span>
      <span class="v">{fmtBytes(stats?.memory_bytes)}</span>
      {#if ratio != null}
        <div class="bar"><div class="fill" style="width:{Math.round(ratio * 100)}%"></div></div>
      {/if}
    </div>
    <div class="metric">
      <span class="k">磁碟</span>
      <span class="v">{fmtBytes(diskBytes)}</span>
    </div>
    <div class="metric">
      <span class="k">玩家</span>
      <span class="v">{fmtPlayers(players)} <span class="online muted">{fmtOnline(online)}</span></span>
    </div>
  </div>

  <div class="actions wrap">
    {#if running}
      <button class="sm" on:click={() => act('stop', () => StopInstance(inst.uuid))} disabled={!!busy}
        >{busy === 'stop' ? '停止中…' : '停止'}</button
      >
      <button class="sm" on:click={() => act('restart', () => RestartInstance(inst.uuid))} disabled={!!busy}
        >{busy === 'restart' ? '重啟中…' : '重啟'}</button
      >
    {:else}
      <button
        class="sm primary"
        on:click={() => act('start', () => StartInstance(inst.uuid))}
        disabled={!!busy}>{busy === 'start' ? '啟動中…' : '啟動'}</button
      >
    {/if}
    <button class="sm" on:click={() => dispatch('console', inst.uuid)} disabled={!!busy}>主控台</button>
    <button class="sm danger" on:click={openRemoveConfirm} disabled={!!busy}>移除</button>
  </div>
</div>

{#if confirmRemove}
  <Modal title="移除實例" on:close={() => (confirmRemove = false)}>
    <p class="msg">確定移除 {inst.uuid}?</p>
    <div class="checkbox-row">
      <input id="purge-chk" type="checkbox" bind:checked={purgeOnRemove} disabled={removing} />
      <label for="purge-chk" style="margin:0">同時刪除伺服器資料與備份(data 與 backups,無法復原)</label>
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
    display: flex;
    flex-direction: column;
    gap: 14px;
  }
  .name {
    font-size: 14px;
    font-weight: 600;
    word-break: break-all;
  }
  .sub {
    font-size: 12px;
    margin-top: 2px;
  }
  .metrics {
    display: grid;
    grid-template-columns: repeat(2, 1fr);
    gap: 10px 16px;
  }
  .metric {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .metric .k {
    font-size: 11px;
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .metric .v {
    font-size: 14px;
    font-variant-numeric: tabular-nums;
  }
  .online {
    font-size: 11px;
  }
  .bar {
    height: 4px;
    background: var(--bg-3);
    border-radius: 2px;
    margin-top: 3px;
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
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
    margin-top: 18px;
  }
</style>
