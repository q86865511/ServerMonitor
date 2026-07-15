<script lang="ts">
  import { nodeStatuses } from './stores';
  import { call } from './api';
  import { RetryDocker } from '../../wailsjs/go/main/App';
  import Icon from './Icon.svelte';

  let retrying = false;

  $: offline = $nodeStatuses.filter((n) => !n.online);
  $: detail = offline
    .map((n) => `${n.node}${n.last_err ? ` (${n.last_err})` : ''}`)
    .join('；');

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
  <section class="system-strip" aria-label="節點連線狀態">
    <div class="strip-mark"><Icon name="alert" size={17} /></div>
    <div class="status-copy">
      <span class="status-code mono">LOCAL NODE / OFFLINE</span>
      <span class="detail" title={detail}>{detail}</span>
    </div>
    <button class="sm retry" on:click={retry} disabled={retrying}>
      <Icon name="refresh" size={14} />
      <span>{retrying ? '重試中…' : '重試 Docker'}</span>
    </button>
  </section>
{/if}

<style>
  .system-strip {
    display: flex;
    align-items: center;
    gap: 11px;
    min-height: 45px;
    margin-bottom: 18px;
    padding: 8px 10px 8px 9px;
    background: rgba(217, 108, 98, 0.07);
    border: 1px solid rgba(217, 108, 98, 0.52);
    border-left-width: 3px;
    border-radius: var(--radius-sm);
  }

  .strip-mark {
    display: grid;
    width: 27px;
    height: 27px;
    flex: none;
    place-items: center;
    color: var(--err);
  }

  .status-copy {
    display: flex;
    min-width: 0;
    flex: 1;
    flex-direction: column;
  }

  .status-code {
    color: #eda29b;
    font-size: 9px;
    font-weight: 700;
    letter-spacing: 0.1em;
  }

  .detail {
    overflow: hidden;
    color: var(--fg-1);
    font-size: 11px;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .retry {
    display: inline-flex;
    flex: none;
    align-items: center;
    gap: 6px;
  }

  @media (max-width: 620px) {
    .system-strip {
      align-items: flex-start;
      flex-wrap: wrap;
    }

    .retry {
      width: 100%;
      justify-content: center;
    }
  }
</style>
