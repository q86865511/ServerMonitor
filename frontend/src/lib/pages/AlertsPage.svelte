<script lang="ts">
  // 全域警報頁(R9):實例選擇器 + 複用 pages/server/AlertsPanel(單一實作,同 Backups/Schedules 模式)。
  import { instances } from '../stores/instances';
  import { templates, instanceLabel } from '../stores/templates';
  const tpls = templates();
  import Card from '../ui/Card.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
  import Select from '../ui/Select.svelte';
  import AlertsPanel from './server/AlertsPanel.svelte';

  let selectedUuid = $state('');
  $effect(() => {
    if (selectedUuid === '' && $instances.length > 0) {
      selectedUuid = $instances[0].uuid;
    }
  });

  const options = $derived(
    $instances.map((i) => ({
      value: i.uuid,
      label: instanceLabel(i, $tpls),
    })),
  );
</script>

<div class="toolbar">
  <div class="picker">
    <Select label="實例" bind:value={selectedUuid} {options} disabled={$instances.length === 0} />
  </div>
</div>

{#if $instances.length === 0}
  <Card>
    <EmptyState title="尚無任何實例" description="請先於「伺服器」頁建立實例,建立後可於此設定告警。" />
  </Card>
{:else if !selectedUuid}
  <Card>
    <EmptyState title="請選擇一個實例" description="於上方選擇器選擇要設定告警的實例。" />
  </Card>
{:else}
  <Card>
    <AlertsPanel uuid={selectedUuid} />
  </Card>
{/if}

<style>
  .toolbar {
    margin-bottom: var(--space-4);
  }
  .picker {
    max-width: 360px;
  }
</style>
