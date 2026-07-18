<script lang="ts">
  // 伺服器清單頁(R5):ServerCard 網格+SearchInput 過濾,取代舊 InstanceList/InstanceCard。
  // 建立入口沿用 Topbar 既有「新增伺服器」(App.svelte 持有 CreateWizard 狀態,本頁不重複掛);
  // 主控台入口一律 navigate(`/servers/{uuid}/console`),不再掛舊 Console modal(卡片「更多」選單)。
  import { onMount } from 'svelte';
  import type { main } from '../../../wailsjs/go/models';
  import { ListTemplates } from '../../../wailsjs/go/main/App';
  import { call } from '../api';
  import { filterInstances, instances } from '../stores/instances';
  import Card from '../ui/Card.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
  import SearchInput from '../ui/SearchInput.svelte';
  import ServerCard from '../ui/ServerCard.svelte';
  import Skeleton from '../ui/Skeleton.svelte';

  let templates = $state<main.TemplateDTO[]>([]);
  let templatesLoading = $state(true);
  const templateMap = $derived.by(() => {
    const m = new Map<string, main.TemplateDTO>();
    for (const t of templates) m.set(t.id, t);
    return m;
  });

  async function loadTemplates(): Promise<void> {
    try {
      templates = await call(() => ListTemplates(), { silent: true });
    } catch {
      templates = [];
    } finally {
      templatesLoading = false;
    }
  }

  onMount(() => {
    loadTemplates();
  });

  let query = $state('');
  const filtered = $derived(filterInstances($instances, query));
</script>

<div class="toolbar">
  <div class="search-wrap">
    <SearchInput bind:value={query} placeholder="搜尋名稱 / uuid / 範本…" />
  </div>
  <span class="count">{filtered.length} / {$instances.length} 台</span>
</div>

{#if templatesLoading}
  <Card>
    <Skeleton variant="block" height="140px" />
  </Card>
{:else if $instances.length === 0}
  <Card>
    <EmptyState
      title="尚無任何伺服器"
      description="點擊右上角「新增伺服器」建立第一台伺服器。"
    />
  </Card>
{:else if filtered.length === 0}
  <Card>
    <EmptyState title="沒有符合的伺服器" description={`找不到符合「${query}」的伺服器,換個關鍵字試試。`} />
  </Card>
{:else}
  <div class="server-grid">
    {#each filtered as inst (inst.uuid)}
      <ServerCard {inst} template={templateMap.get(inst.template_id)} />
    {/each}
  </div>
{/if}

<style>
  .toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    margin-bottom: var(--space-4);
  }
  .search-wrap {
    max-width: 360px;
    flex: 1;
  }
  .count {
    flex: none;
    font-size: var(--text-sm);
    color: var(--fg-2);
  }
  .server-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
    gap: var(--space-4);
  }
</style>
