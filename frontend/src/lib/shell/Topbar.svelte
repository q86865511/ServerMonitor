<script lang="ts">
  // 頂部工具列(R2):頁面標題、全域搜尋(跳轉伺服器詳細頁)、系統狀態燈、新增伺服器主鈕。
  import { route, navigate, type Page } from '../router';
  import { instances, nodeStatuses, byUuid, filterInstances } from '../stores/instances';
  import Button from '../ui/Button.svelte';
  import Badge from '../ui/Badge.svelte';
  import SearchInput from '../ui/SearchInput.svelte';
  import OperationsPanel from './OperationsPanel.svelte';

  let { onCreate }: { onCreate: () => void } = $props();

  const TITLES: Record<Page, string> = {
    dashboard: '總覽',
    servers: '伺服器',
    'server-detail': '伺服器詳細',
    templates: '遊戲範本',
    nodes: '節點',
    backups: '備份',
    schedules: '排程',
    alerts: '警報',
    events: '事件',
    settings: '設定',
    'not-found': '找不到頁面',
  };

  const title = $derived.by(() => {
    const r = $route;
    if (r.page === 'server-detail' && r.params.uuid) {
      const inst = byUuid(r.params.uuid);
      if (inst) return inst.name || `${inst.template_id} #${inst.uuid.slice(0, 8)}`;
    }
    return TITLES[r.page];
  });

  let query = $state('');
  const results = $derived(query.trim() ? filterInstances($instances, query).slice(0, 8) : []);

  function goToInstance(uuid: string): void {
    query = '';
    navigate(`/servers/${uuid}`);
  }

  const onlineCount = $derived($nodeStatuses.filter((n) => n.online).length);
  const totalCount = $derived($nodeStatuses.length);
  const statusTone = $derived(
    totalCount === 0 ? 'idle' : onlineCount === totalCount ? 'ok' : 'err',
  );
</script>

<header class="topbar">
  <h1 class="title">{title}</h1>

  <div class="search-wrap">
    <SearchInput bind:value={query} placeholder="搜尋伺服器…" />
    {#if results.length > 0}
      <ul class="dropdown" role="listbox">
        {#each results as inst (inst.uuid)}
          <li>
            <button type="button" onclick={() => goToInstance(inst.uuid)}>
              <span class="name">{inst.name || `${inst.template_id} #${inst.uuid.slice(0, 8)}`}</span>
              <span class="uuid mono">{inst.uuid.slice(0, 8)}</span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </div>

  <div class="status" title={`節點在線 ${onlineCount}/${totalCount}`}>
    <Badge tone={statusTone}>節點 {onlineCount}/{totalCount}</Badge>
  </div>

  <OperationsPanel />

  <Button variant="primary" onclick={onCreate}>＋ 新增伺服器</Button>
</header>

<style>
  .topbar {
    height: var(--topbar-h);
    flex: none;
    display: flex;
    align-items: center;
    gap: var(--space-4);
    padding: 0 var(--space-5);
    background: var(--bg-1);
    border-bottom: 1px solid var(--line);
  }
  .title {
    font-size: var(--text-lg);
    white-space: nowrap;
    flex: none;
  }
  .search-wrap {
    position: relative;
    flex: 1;
    max-width: 360px;
    min-width: 0;
  }
  .dropdown {
    position: absolute;
    top: calc(100% + 4px);
    left: 0;
    right: 0;
    background: var(--bg-2);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    box-shadow: var(--shadow-md);
    list-style: none;
    margin: 0;
    padding: 4px;
    z-index: var(--z-dropdown);
    max-height: 320px;
    overflow-y: auto;
  }
  .dropdown li button {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    width: 100%;
    background: transparent;
    border: none;
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    text-align: left;
    color: var(--fg-0);
  }
  .dropdown li button:hover {
    background: var(--bg-3);
  }
  .dropdown li button:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  .dropdown .uuid {
    color: var(--fg-2);
    font-size: var(--text-xs);
  }
  .status {
    flex: none;
  }
</style>
