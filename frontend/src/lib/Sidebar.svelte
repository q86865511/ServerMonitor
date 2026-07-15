<script lang="ts">
  import { currentView, type View } from './stores';
  import Icon from './Icon.svelte';

  const items: { id: View; label: string; code: string; icon: 'server' | 'events' | 'settings' }[] = [
    { id: 'instances', label: '實例', code: '01', icon: 'server' },
    { id: 'events', label: '事件', code: '02', icon: 'events' },
    { id: 'settings', label: '設定', code: '03', icon: 'settings' },
  ];
</script>

<aside class="sidebar">
  <div class="brand" aria-label="ServerMonitor">
    <span class="rack-mark" aria-hidden="true">
      <i></i><i></i><i></i><i></i>
    </span>
    <span class="brand-copy">
      <strong>SERVER</strong>
      <span>MONITOR</span>
    </span>
  </div>

  <div class="nav-label">CONTROL</div>
  <nav aria-label="主要導覽">
    {#each items as it}
      <button
        class="nav-item"
        class:active={$currentView === it.id}
        on:click={() => currentView.set(it.id)}
        aria-current={$currentView === it.id ? 'page' : undefined}
        title={it.label}
      >
        <span class="nav-code mono">{it.code}</span>
        <span class="nav-icon"><Icon name={it.icon} size={17} /></span>
        <span class="nav-text">{it.label}</span>
      </button>
    {/each}
  </nav>

  <div class="sidebar-foot">
    <span class="foot-line"></span>
    <span class="foot-copy mono">LOCAL CONTROL</span>
  </div>
</aside>

<style>
  .sidebar {
    position: relative;
    width: var(--sidebar-width);
    flex: none;
    display: flex;
    flex-direction: column;
    padding: 18px 12px 14px;
    background: var(--bg-1);
    border-right: 1px solid var(--line);
    overflow: hidden;
  }

  .brand {
    display: flex;
    align-items: center;
    gap: 11px;
    height: 46px;
    padding: 0 7px;
    margin-bottom: 25px;
  }

  .rack-mark {
    display: grid;
    width: 30px;
    height: 30px;
    flex: none;
    grid-template-columns: repeat(2, 7px);
    grid-auto-rows: 7px;
    place-content: center;
    gap: 3px;
    color: var(--accent);
    background: var(--bg-inset);
    border: 1px solid var(--line-strong);
    border-radius: 3px;
  }

  .rack-mark i {
    display: block;
    border: 1px solid currentColor;
  }

  .rack-mark i:first-child,
  .rack-mark i:last-child {
    background: currentColor;
  }

  .brand-copy {
    display: flex;
    flex-direction: column;
    line-height: 1.05;
    letter-spacing: 0.08em;
  }

  .brand-copy strong {
    font-size: 12px;
    font-weight: 750;
  }

  .brand-copy span {
    margin-top: 3px;
    color: var(--fg-2);
    font-size: 10px;
    letter-spacing: 0.18em;
  }

  .nav-label {
    padding: 0 9px 8px;
    color: var(--fg-3);
    font-family: var(--font-mono);
    font-size: 9px;
    letter-spacing: 0.16em;
  }

  nav {
    display: flex;
    flex-direction: column;
    gap: 3px;
  }

  .nav-item {
    position: relative;
    display: grid;
    min-height: 43px;
    grid-template-columns: 23px 20px 1fr;
    align-items: center;
    gap: 7px;
    padding: 7px 9px;
    color: var(--fg-2);
    background: transparent;
    border: 0;
    border-radius: 2px;
    text-align: left;
  }

  .nav-item::before {
    position: absolute;
    top: 8px;
    bottom: 8px;
    left: -12px;
    width: 2px;
    background: transparent;
    content: '';
  }

  .nav-item:hover:not(:disabled) {
    color: var(--fg-0);
    background: var(--bg-2);
    border-color: transparent;
  }

  .nav-item.active {
    color: var(--fg-0);
    background: var(--bg-2);
  }

  .nav-item.active::before {
    background: var(--accent);
  }

  .nav-code {
    color: var(--fg-3);
    font-size: 9px;
  }

  .nav-item.active .nav-code,
  .nav-item.active .nav-icon {
    color: var(--accent);
  }

  .nav-icon {
    display: grid;
    place-items: center;
  }

  .nav-text {
    font-size: 13px;
    font-weight: 600;
  }

  .sidebar-foot {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: auto;
    padding: 12px 8px 0;
  }

  .foot-line {
    width: 18px;
    height: 1px;
    flex: none;
    background: var(--accent);
  }

  .foot-copy {
    color: var(--fg-3);
    font-size: 8px;
    letter-spacing: 0.12em;
    white-space: nowrap;
  }

  @media (max-width: 820px) {
    .sidebar {
      align-items: center;
      padding-inline: 8px;
    }

    .brand {
      padding: 0;
    }

    .brand-copy,
    .nav-label,
    .nav-code,
    .nav-text,
    .sidebar-foot {
      display: none;
    }

    nav {
      width: 100%;
    }

    .nav-item {
      display: grid;
      width: 100%;
      grid-template-columns: 1fr;
      place-items: center;
      padding: 7px;
    }

    .nav-item::before {
      left: -8px;
    }
  }
</style>
