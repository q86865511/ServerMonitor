<script lang="ts">
  // 全域排程頁(R9):實例選擇器 + 複用 T11 產出的 pages/server/SchedulesPanel(單一實作,不另寫)。
  // SchedulesPanel 內建 uuid 變動即重載(見該檔 loadedFor 追蹤),本頁只需傳入目前選中的 uuid。
  import { instances } from '../stores/instances';
  import Card from '../ui/Card.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
  import Select from '../ui/Select.svelte';
  import SchedulesPanel from './server/SchedulesPanel.svelte';

  let selectedUuid = $state('');
  $effect(() => {
    if (selectedUuid === '' && $instances.length > 0) {
      selectedUuid = $instances[0].uuid;
    }
  });

  const options = $derived(
    $instances.map((i) => ({
      value: i.uuid,
      label: i.name?.trim() ? i.name : `${i.template_id} #${i.uuid.slice(0, 8)}`,
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
    <EmptyState title="尚無任何實例" description="請先於「伺服器」頁建立實例,建立後可於此管理排程。" />
  </Card>
{:else if !selectedUuid}
  <Card>
    <EmptyState title="請選擇一個實例" description="於上方選擇器選擇要管理排程的實例。" />
  </Card>
{:else}
  <Card>
    <SchedulesPanel uuid={selectedUuid} />
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
