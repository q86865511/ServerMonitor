<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { instances } from './stores';
  import InstanceCard from './InstanceCard.svelte';

  const dispatch = createEventDispatcher<{
    console: string;
    create: void;
    changed: void;
  }>();
</script>

<div class="head spread">
  <h2>實例</h2>
  <button class="primary" on:click={() => dispatch('create')}>＋ 建立伺服器</button>
</div>

{#if $instances.length === 0}
  <div class="empty">
    尚無實例。點右上「建立伺服器」開始。
  </div>
{:else}
  <div class="grid">
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
  .head {
    margin-bottom: 18px;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
    gap: 16px;
  }
</style>
