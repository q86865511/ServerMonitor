<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { instances } from './stores';
  import InstanceCard from './InstanceCard.svelte';
  import Icon from './Icon.svelte';

  const dispatch = createEventDispatcher<{
    console: string;
    create: void;
    changed: void;
  }>();
</script>

<header class="page-head">
  <div class="page-copy">
    <div class="eyebrow">Server / Overview</div>
    <h2>實例控制室</h2>
    <p class="page-description">監看遊戲伺服器狀態、資源與生命週期操作。</p>
  </div>
  <button class="primary create-button" on:click={() => dispatch('create')}>
    <Icon name="plus" size={16} />
    <span>建立伺服器</span>
  </button>
</header>

{#if $instances.length === 0}
  <section class="empty-state">
    <div class="empty-state-inner">
      <div class="empty-mark"><Icon name="server" size={27} /></div>
      <div class="empty-title">尚未部署任何實例</div>
      <p class="empty-copy">選擇遊戲範本並建立第一台伺服器，完成後會在此顯示即時狀態。</p>
      <button class="primary" on:click={() => dispatch('create')}>
        <Icon name="plus" size={15} />
        <span>建立第一個實例</span>
      </button>
    </div>
  </section>
{:else}
  <div class="grid" aria-label="伺服器實例">
    {#each $instances as inst (inst.uuid)}
      <InstanceCard
        {inst}
        on:console={(e) => dispatch('console', e.detail)}
        on:changed={() => dispatch('changed')}
      />
    {/each}
  </div>
{/if}

<style>
  .create-button,
  .empty-state button {
    display: inline-flex;
    align-items: center;
    gap: 8px;
  }

  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
    gap: 14px;
  }

  @media (max-width: 920px) {
    .grid {
      grid-template-columns: 1fr;
    }
  }
</style>
