<script lang="ts">
  // 固定左側導覽(R2):9 項導覽,可收合為 56px icon 列(狀態存 localStorage)。
  import { route, navigate, type Page } from '../router';

  const STORAGE_KEY = 'gsm.sidebar.collapsed';

  interface NavItem {
    label: string;
    icon: string;
    path: string;
    /** 判定「目前路由屬於此項」的頁面清單(伺服器詳細頁併入「伺服器」)。 */
    pages: Page[];
  }

  const items: NavItem[] = [
    { label: '總覽', icon: '⌂', path: '/', pages: ['dashboard'] },
    { label: '伺服器', icon: '▦', path: '/servers', pages: ['servers', 'server-detail'] },
    { label: '遊戲範本', icon: '▧', path: '/templates', pages: ['templates'] },
    { label: '節點', icon: '◈', path: '/nodes', pages: ['nodes'] },
    { label: '備份', icon: '⛁', path: '/backups', pages: ['backups'] },
    { label: '排程', icon: '◷', path: '/schedules', pages: ['schedules'] },
    { label: '警報', icon: '⚠', path: '/alerts', pages: ['alerts'] },
    { label: '事件', icon: '≣', path: '/events', pages: ['events'] },
    { label: '設定', icon: '⚙', path: '/settings', pages: ['settings'] },
  ];

  function readCollapsed(): boolean {
    try {
      return window.localStorage.getItem(STORAGE_KEY) === '1';
    } catch {
      return false;
    }
  }

  let collapsed = $state(readCollapsed());

  function toggle(): void {
    collapsed = !collapsed;
    try {
      window.localStorage.setItem(STORAGE_KEY, collapsed ? '1' : '0');
    } catch {
      /* localStorage 不可用時退化為 session 內收合,不影響功能 */
    }
  }

  const currentPage = $derived($route.page);
</script>

<nav class="sidebar" class:collapsed>
  <div class="brand">
    <span class="logo">◆</span>
    {#if !collapsed}
      <span class="name">ServerMonitor</span>
    {/if}
  </div>

  <div class="items">
    {#each items as it (it.path)}
      <button
        type="button"
        class="nav-item"
        class:active={it.pages.includes(currentPage)}
        title={collapsed ? it.label : undefined}
        onclick={() => navigate(it.path)}
      >
        <span class="nav-icon">{it.icon}</span>
        {#if !collapsed}
          <span class="label">{it.label}</span>
        {/if}
      </button>
    {/each}
  </div>

  <button
    type="button"
    class="collapse-btn"
    title={collapsed ? '展開側欄' : '收合側欄'}
    aria-label={collapsed ? '展開側欄' : '收合側欄'}
    onclick={toggle}
  >
    <span class="nav-icon">{collapsed ? '»' : '«'}</span>
    {#if !collapsed}
      <span class="label">收合</span>
    {/if}
  </button>
</nav>

<style>
  .sidebar {
    width: var(--sidebar-w);
    flex: none;
    background: var(--bg-1);
    border-right: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    padding: 14px 10px;
    gap: 4px;
    transition: width var(--dur-base) var(--ease-standard);
    overflow: hidden;
  }
  .sidebar.collapsed {
    width: var(--sidebar-w-collapsed);
    padding: 14px 8px;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 8px 16px;
  }
  .logo {
    color: var(--accent);
    font-size: 18px;
    flex: none;
  }
  .name {
    font-weight: 700;
    font-size: 15px;
    white-space: nowrap;
  }
  .items {
    display: flex;
    flex-direction: column;
    gap: 4px;
    flex: 1;
    min-height: 0;
    overflow-y: auto;
  }
  .nav-item,
  .collapse-btn {
    display: flex;
    align-items: center;
    gap: 10px;
    background: transparent;
    border: 1px solid transparent;
    border-radius: var(--radius-sm);
    padding: 9px 12px;
    text-align: left;
    color: var(--fg-1);
    width: 100%;
    transition: background-color var(--dur-fast) var(--ease-standard),
      border-color var(--dur-fast) var(--ease-standard),
      color var(--dur-fast) var(--ease-standard);
  }
  .nav-item:hover,
  .collapse-btn:hover {
    background: var(--bg-3);
  }
  .nav-item:focus-visible,
  .collapse-btn:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  .nav-item.active {
    background: var(--bg-3);
    color: var(--fg-0);
    font-weight: 600;
  }
  .nav-icon {
    width: 18px;
    text-align: center;
    color: var(--fg-2);
    flex: none;
    font-size: 15px;
  }
  .nav-item.active .nav-icon {
    color: var(--accent);
  }
  .label {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .collapse-btn {
    margin-top: 4px;
    border-top: 1px solid var(--line);
    border-radius: 0;
    padding-top: 12px;
    color: var(--fg-2);
  }
</style>
