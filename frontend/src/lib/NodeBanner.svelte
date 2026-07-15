<script lang="ts">
  import { nodeStatuses } from './stores';
  import { call } from './api';
  import { RetryDocker } from '../../wailsjs/go/main/App';

  let retrying = false;

  $: offline = $nodeStatuses.filter((n) => !n.online);

  async function retry(): Promise<void> {
    retrying = true;
    try {
      await call(() => RetryDocker());
    } catch {
      /* toast 已由 call 呈現 */
    } finally {
      retrying = false;
    }
  }
</script>

{#if offline.length > 0}
  <div class="banner">
    <span class="icon">⚠</span>
    <div class="text">
      <strong>節點離線</strong>
      <span class="detail"
        >{offline
          .map((n) => `${n.node}${n.last_err ? `(${n.last_err})` : ''}`)
          .join('；')}</span
      >
    </div>
    <button class="sm" on:click={retry} disabled={retrying}>
      {retrying ? '重試中…' : '重試連線 Docker'}
    </button>
  </div>
{/if}

<style>
  .banner {
    display: flex;
    align-items: center;
    gap: 12px;
    background: rgba(248, 81, 73, 0.12);
    border: 1px solid var(--err);
    border-radius: var(--radius-sm);
    padding: 10px 14px;
    margin-bottom: 16px;
  }
  .icon {
    color: var(--err);
    font-size: 18px;
  }
  .text {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-width: 0;
  }
  .detail {
    font-size: 12px;
    color: var(--fg-1);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
