<script lang="ts">
  import { instances } from './stores';
  import BackupsPanel from './BackupsPanel.svelte';
  import SchedulesPanel from './SchedulesPanel.svelte';
  import AlertsPanel from './AlertsPanel.svelte';

  type Tab = 'backups' | 'schedules' | 'alerts';
  let tab: Tab = 'backups';
  let selectedUuid = '';

  // 預設選第一個實例。
  $: if (selectedUuid === '' && $instances.length > 0) {
    selectedUuid = $instances[0].uuid;
  }

  const tabs: { id: Tab; label: string }[] = [
    { id: 'backups', label: '備份' },
    { id: 'schedules', label: '排程' },
    { id: 'alerts', label: '告警' },
  ];
</script>

<header class="page-head settings-head">
  <div class="page-copy">
    <div class="eyebrow">Instance / Operations</div>
    <h2>實例設定</h2>
    <p class="page-description">管理備份、UTC 排程與資源告警。</p>
  </div>
  {#if $instances.length > 0}
    <div class="context-picker">
      <label for="settings-instance">目前實例</label>
      <select id="settings-instance" class="picker mono" bind:value={selectedUuid}>
        {#each $instances as inst (inst.uuid)}
          <option value={inst.uuid}>{inst.template_id} / {inst.uuid}</option>
        {/each}
      </select>
    </div>
  {/if}
</header>

{#if $instances.length === 0}
  <div class="empty">尚無實例;請先於「實例」頁建立。</div>
{:else}
  <div class="tabs" role="tablist" aria-label="實例設定分類">
    {#each tabs as t}
      <button
        class="tab"
        class:active={tab === t.id}
        on:click={() => (tab = t.id)}
        role="tab"
        aria-selected={tab === t.id}
      >{t.label}</button>
    {/each}
  </div>

  <div class="panel workspace">
    {#key selectedUuid}
      {#if tab === 'backups'}
        <BackupsPanel uuid={selectedUuid} />
      {:else if tab === 'schedules'}
        <SchedulesPanel uuid={selectedUuid} />
      {:else}
        <AlertsPanel uuid={selectedUuid} />
      {/if}
    {/key}
  </div>
{/if}

<style>
  .settings-head {
    align-items: flex-end;
  }
  .context-picker {
    width: min(420px, 48%);
  }
  .picker {
    width: 100%;
    font-size: 11px;
  }
  .tabs {
    display: flex;
    gap: 22px;
    border-bottom: 1px solid var(--line);
  }
  .tab {
    position: relative;
    background: transparent;
    border: none;
    border-radius: 0;
    color: var(--fg-2);
    padding: 10px 2px 11px;
    font-size: 12px;
  }
  .tab:hover:not(:disabled) {
    background: transparent;
    color: var(--fg-0);
  }
  .tab.active {
    color: var(--fg-0);
    font-weight: 600;
  }
  .tab.active::after {
    position: absolute;
    right: 0;
    bottom: -1px;
    left: 0;
    height: 2px;
    background: var(--accent);
    content: '';
  }
  .workspace {
    border-top: 0;
    border-top-left-radius: 0;
    border-top-right-radius: 0;
  }
  @media (max-width: 720px) {
    .settings-head { align-items: stretch; }
    .context-picker { width: 100%; }
  }
</style>
