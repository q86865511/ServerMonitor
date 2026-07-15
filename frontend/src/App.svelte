<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { ListInstances, NodeStatus } from '../wailsjs/go/main/App';
  import { currentView, instances, nodeStatuses } from './lib/stores';
  import { call } from './lib/api';
  import Sidebar from './lib/Sidebar.svelte';
  import NodeBanner from './lib/NodeBanner.svelte';
  import ToastHost from './lib/ToastHost.svelte';
  import InstanceList from './lib/InstanceList.svelte';
  import EventsView from './lib/EventsView.svelte';
  import SettingsView from './lib/SettingsView.svelte';
  import CreateWizard from './lib/CreateWizard.svelte';
  import Console from './lib/Console.svelte';

  const POLL_MS = 5000;
  let timer: ReturnType<typeof setInterval> | null = null;

  let showCreate = false;
  let consoleUuid: string | null = null;

  // 遞增序號:輪詢與操作後 refresh 可能並發,舊回應晚到會覆寫新狀態
  // (例如停止後短暫回跳 Running)。回應套用前檢查自己仍是最新一次呼叫。
  let refreshSeq = 0;

  async function refresh(silent = true): Promise<void> {
    const seq = ++refreshSeq;
    try {
      const [insts, nodes] = await Promise.all([
        call(() => ListInstances(), { silent }),
        call(() => NodeStatus(), { silent }),
      ]);
      if (seq !== refreshSeq) return; // 已有更新的 refresh 在途,丟棄此次舊回應
      instances.set(insts);
      nodeStatuses.set(nodes);
    } catch {
      /* 輪詢失敗靜默(避免 banner 洗版);首次載入由節點 banner 反映 */
    }
  }

  onMount(() => {
    refresh(false);
    timer = setInterval(() => refresh(true), POLL_MS);
  });

  onDestroy(() => {
    if (timer) clearInterval(timer);
  });

  function onCreated(): void {
    showCreate = false;
    refresh(false);
  }
</script>

<div class="app">
  <Sidebar />
  <main class="content">
    <NodeBanner />
    {#if $currentView === 'instances'}
      <InstanceList
        on:create={() => (showCreate = true)}
        on:console={(e) => (consoleUuid = e.detail)}
        on:changed={() => refresh(false)}
      />
    {:else if $currentView === 'events'}
      <EventsView />
    {:else if $currentView === 'settings'}
      <SettingsView />
    {/if}
  </main>
</div>

{#if showCreate}
  <CreateWizard on:close={() => (showCreate = false)} on:created={onCreated} />
{/if}

{#if consoleUuid}
  <Console uuid={consoleUuid} on:close={() => (consoleUuid = null)} />
{/if}

<ToastHost />

<style>
  .app {
    display: flex;
    height: 100vh;
    text-align: left;
  }
  .content {
    flex: 1;
    min-width: 0;
    overflow-y: auto;
    padding: 22px 26px;
  }
</style>
