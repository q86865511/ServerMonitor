<script lang="ts">
  import { instances } from './stores';
  import BackupsPanel from './BackupsPanel.svelte';
  import SchedulesPanel from './SchedulesPanel.svelte';
  import AlertsPanel from './AlertsPanel.svelte';
  import CurseForgeSettings from './CurseForgeSettings.svelte';

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

<div class="spread head">
  <h2>設定</h2>
</div>

<!-- 節點層級全域設定(不隨實例;恆顯示)。 -->
<CurseForgeSettings />

<div class="spread head sub">
  <h3>實例設定</h3>
  {#if $instances.length > 0}
    <select class="picker" bind:value={selectedUuid}>
      {#each $instances as inst (inst.uuid)}
        <option value={inst.uuid}>{inst.uuid}({inst.template_id})</option>
      {/each}
    </select>
  {/if}
</div>

{#if $instances.length === 0}
  <div class="empty">尚無實例;請先於「實例」頁建立實例後,可於此設定其備份、排程與告警。</div>
{:else}
  <div class="tabs">
    {#each tabs as t}
      <button class="tab" class:active={tab === t.id} on:click={() => (tab = t.id)}>{t.label}</button>
    {/each}
  </div>

  <div class="panel">
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
  .head {
    margin-bottom: 16px;
  }
  .head.sub {
    margin-top: 4px;
  }
  .head.sub h3 {
    margin: 0;
    font-size: 15px;
    color: var(--fg-1);
  }
  .picker {
    width: auto;
    min-width: 240px;
  }
  .tabs {
    display: flex;
    gap: 4px;
    margin-bottom: 16px;
    border-bottom: 1px solid var(--line);
  }
  .tab {
    background: transparent;
    border: none;
    border-bottom: 2px solid transparent;
    border-radius: 0;
    color: var(--fg-1);
    padding: 8px 16px;
  }
  .tab:hover:not(:disabled) {
    background: transparent;
    color: var(--fg-0);
  }
  .tab.active {
    color: var(--fg-0);
    border-bottom-color: var(--accent);
    font-weight: 600;
  }
</style>
