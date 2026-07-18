<script lang="ts">
  // Application Shell(R2)+ hash 路由 outlet(R3)。輪詢與 toast 集中於此掛載/釋放;
  // 頁面切換不重建 shell,僅 main 區塊依路由渲染對應頁面元件。
  import { onMount, onDestroy } from 'svelte';
  import { route, navigate } from './lib/router';
  import { startPolling, stopPolling, refresh } from './lib/stores/instances';
  import { toasts, dismissToast } from './lib/stores/toasts';
  import Sidebar from './lib/shell/Sidebar.svelte';
  import Topbar from './lib/shell/Topbar.svelte';
  import NodeBanner from './lib/shell/NodeBanner.svelte';
  import ToastHost from './lib/ui/ToastHost.svelte';
  import CreateWizardModal from './lib/pages/wizard/CreateWizardModal.svelte';

  import DashboardPage from './lib/pages/DashboardPage.svelte';
  import ServersPage from './lib/pages/ServersPage.svelte';
  import ServerDetailPage from './lib/pages/server/ServerDetailPage.svelte';
  import TemplatesPage from './lib/pages/TemplatesPage.svelte';
  import NodesPage from './lib/pages/NodesPage.svelte';
  import BackupsPage from './lib/pages/BackupsPage.svelte';
  import SchedulesPage from './lib/pages/SchedulesPage.svelte';
  import AlertsPage from './lib/pages/AlertsPage.svelte';
  import EventsPage from './lib/pages/EventsPage.svelte';
  import SettingsPage from './lib/pages/SettingsPage.svelte';
  import NotFoundPage from './lib/pages/NotFoundPage.svelte';

  // 全域「新增伺服器」主鈕(Topbar)掛載 T12 四步驟精靈。
  let showCreate = $state(false);

  function onCreated(uuid: string): void {
    showCreate = false;
    refresh(false);
    navigate(`/servers/${uuid}`);
  }

  function onToastDismiss(id: string | number): void {
    dismissToast(id as number);
  }

  onMount(() => {
    startPolling();
  });

  onDestroy(() => {
    stopPolling();
  });
</script>

<div class="shell">
  <Sidebar />
  <div class="main">
    <Topbar onCreate={() => (showCreate = true)} />
    <div class="content">
      <NodeBanner />
      {#if $route.page === 'dashboard'}
        <DashboardPage />
      {:else if $route.page === 'servers'}
        <ServersPage />
      {:else if $route.page === 'server-detail'}
        <ServerDetailPage />
      {:else if $route.page === 'templates'}
        <TemplatesPage />
      {:else if $route.page === 'nodes'}
        <NodesPage />
      {:else if $route.page === 'backups'}
        <BackupsPage />
      {:else if $route.page === 'schedules'}
        <SchedulesPage />
      {:else if $route.page === 'alerts'}
        <AlertsPage />
      {:else if $route.page === 'events'}
        <EventsPage />
      {:else if $route.page === 'settings'}
        <SettingsPage />
      {:else}
        <NotFoundPage />
      {/if}
    </div>
  </div>
</div>

{#if showCreate}
  <CreateWizardModal onClose={() => (showCreate = false)} onCreated={onCreated} />
{/if}

<ToastHost toasts={$toasts} onDismiss={onToastDismiss} />

<style>
  .shell {
    display: flex;
    height: 100vh;
    text-align: left;
  }
  .main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .content {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: var(--space-5);
  }
</style>
