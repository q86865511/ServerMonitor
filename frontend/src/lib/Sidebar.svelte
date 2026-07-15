<script lang="ts">
  import { currentView, type View } from './stores';

  const items: { id: View; label: string; icon: string }[] = [
    { id: 'instances', label: '實例', icon: '▦' },
    { id: 'events', label: '事件', icon: '≣' },
    { id: 'settings', label: '設定', icon: '⚙' },
  ];
</script>

<nav class="sidebar">
  <div class="brand">
    <span class="logo">◆</span>
    <span class="name">ServerMonitor</span>
  </div>
  {#each items as it}
    <button
      class="nav-item"
      class:active={$currentView === it.id}
      on:click={() => currentView.set(it.id)}
    >
      <span class="nav-icon">{it.icon}</span>
      <span>{it.label}</span>
    </button>
  {/each}
</nav>

<style>
  .sidebar {
    width: 176px;
    flex: none;
    background: var(--bg-1);
    border-right: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    padding: 14px 10px;
    gap: 4px;
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
  }
  .name {
    font-weight: 700;
    font-size: 15px;
  }
  .nav-item {
    display: flex;
    align-items: center;
    gap: 10px;
    background: transparent;
    border: none;
    border-radius: var(--radius-sm);
    padding: 9px 12px;
    text-align: left;
    color: var(--fg-1);
  }
  .nav-item:hover:not(:disabled) {
    background: var(--bg-3);
    border-color: transparent;
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
  }
  .nav-item.active .nav-icon {
    color: var(--accent);
  }
</style>
