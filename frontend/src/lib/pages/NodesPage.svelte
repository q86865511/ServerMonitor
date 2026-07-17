<script lang="ts">
  // 節點頁(R9):節點清單,雙狀態(在線 / Docker 可用,NodeStatusDTO.docker_available)+
  // RetryDocker(語意:僅重建本機 docker 子後端連線,不代表重連節點本身)+ last_err 顯示。
  import type { main } from '../../../wailsjs/go/models';
  import { RetryDocker } from '../../../wailsjs/go/main/App';
  import { call } from '../api';
  import { nodeStatuses, refresh } from '../stores/instances';
  import { pushToast } from '../stores/toasts';
  import Badge from '../ui/Badge.svelte';
  import Button from '../ui/Button.svelte';
  import Card from '../ui/Card.svelte';
  import DataTable from '../ui/DataTable.svelte';
  import type { DataTableColumn } from '../ui/DataTable.svelte';
  import EmptyState from '../ui/EmptyState.svelte';

  let retrying = $state(false);

  async function retry(): Promise<void> {
    if (retrying) return;
    retrying = true;
    try {
      await call(() => RetryDocker());
      pushToast('success', '已重試連線 Docker');
      await refresh(false);
    } catch {
      /* 錯誤 toast 已由 call() 呈現 */
    } finally {
      retrying = false;
    }
  }

  const columns = $derived<DataTableColumn<main.NodeStatusDTO>[]>([
    { key: 'node', label: '節點', cell: nodeCell },
    { key: 'online', label: '在線', width: '110px', cell: onlineCell },
    { key: 'docker', label: 'Docker 可用', width: '150px', cell: dockerCell },
    { key: 'last_err', label: '錯誤', cell: errCell },
  ]);
</script>

{#snippet nodeCell(n: main.NodeStatusDTO)}
  <span class="mono">{n.node}</span>
{/snippet}
{#snippet onlineCell(n: main.NodeStatusDTO)}
  <Badge tone={n.online ? 'ok' : 'err'}>{n.online ? '在線' : '離線'}</Badge>
{/snippet}
{#snippet dockerCell(n: main.NodeStatusDTO)}
  <Badge tone={n.docker_available ? 'ok' : 'idle'}>
    {n.docker_available ? 'Docker 可用' : 'Docker 不可用'}
  </Badge>
{/snippet}
{#snippet errCell(n: main.NodeStatusDTO)}
  <span class="muted">{n.last_err || '—'}</span>
{/snippet}

<div class="toolbar">
  <p class="hint">RetryDocker 僅重建本機 Docker 子後端連線,不影響節點在線狀態本身。</p>
  <Button size="sm" loading={retrying} onclick={retry}>重試連線 Docker</Button>
</div>

{#if $nodeStatuses.length === 0}
  <Card>
    <EmptyState title="尚無節點資訊" description="節點狀態將於首次輪詢後顯示。" />
  </Card>
{:else}
  <Card>
    <DataTable {columns} rows={$nodeStatuses} rowKey={(n) => n.node} />
  </Card>
{/if}

<style>
  .toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    margin-bottom: var(--space-4);
    flex-wrap: wrap;
  }
  .hint {
    margin: 0;
    font-size: var(--text-sm);
    color: var(--fg-2);
  }
  .mono {
    font-family: var(--font-mono);
  }
  .muted {
    color: var(--fg-2);
  }
</style>
