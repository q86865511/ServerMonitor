<script lang="ts">
  // 節點頁(R9):節點清單,雙狀態(在線 / Docker 可用,NodeStatusDTO.docker_available)+
  // RetryDocker(語意:僅重建本機 docker 子後端連線,不代表重連節點本身)+ last_err 顯示。
  // 節點管理區塊(R5 多節點 GUI):以 ListNodes()(main.NodeInfoDTO,含 fingerprint/base_url/
  // removable)呼叫式刷新(非輪詢——新增/移除頻率低,手動按鈕+操作後自動刷新已足夠),
  // 與上方輪詢中的快照表(NodeStatusDTO)分工——上方為「在線/Docker 快照」,下方為「新增/移除/
  // 指紋管理」。
  import { onMount } from 'svelte';
  import type { main } from '../../../wailsjs/go/models';
  import { ListNodes, RemoveNode, RetryDocker } from '../../../wailsjs/go/main/App';
  import { call, errMsg } from '../api';
  import { nodeStatuses, refresh } from '../stores/instances';
  import { pushToast } from '../stores/toasts';
  import Badge from '../ui/Badge.svelte';
  import Button from '../ui/Button.svelte';
  import Card from '../ui/Card.svelte';
  import ConfirmDialog from '../ui/ConfirmDialog.svelte';
  import DataTable from '../ui/DataTable.svelte';
  import type { DataTableColumn } from '../ui/DataTable.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
  import ErrorState from '../ui/ErrorState.svelte';
  import Skeleton from '../ui/Skeleton.svelte';
  import Tooltip from '../ui/Tooltip.svelte';
  import AddNodeModal from './nodes/AddNodeModal.svelte';

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

  // ---- 節點管理(新增/移除/指紋) ----
  let nodeInfos = $state<main.NodeInfoDTO[]>([]);
  let loadingNodes = $state(true);
  let loadNodesError = $state('');
  let showAddModal = $state(false);
  let removeTarget = $state<main.NodeInfoDTO | null>(null);
  let removing = $state(false);
  let expandedFp = $state<Record<string, boolean>>({});
  let copiedFp = $state<string>('');

  async function loadNodes(): Promise<void> {
    loadingNodes = true;
    loadNodesError = '';
    try {
      nodeInfos = await call(() => ListNodes(), { silent: true });
    } catch (e) {
      loadNodesError = errMsg(e);
    } finally {
      loadingNodes = false;
    }
  }

  onMount(loadNodes);

  function shortFingerprint(fp: string): string {
    const parts = fp.split(':');
    return parts.length > 8 ? `${parts.slice(0, 8).join(':')}…` : fp;
  }

  async function copyFingerprint(fp: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(fp);
      copiedFp = fp;
      setTimeout(() => (copiedFp = copiedFp === fp ? '' : copiedFp), 1500);
    } catch {
      /* 剪貼簿不可用時靜默略過 */
    }
  }

  async function doRemove(): Promise<void> {
    const target = removeTarget;
    if (!target) return;
    removing = true;
    try {
      await call(() => RemoveNode(target.name));
      pushToast('success', `已移除節點 ${target.name}`);
      removeTarget = null;
      await loadNodes();
      await refresh(false);
    } catch {
      /* 錯誤 toast 已由 call() 呈現 */
    } finally {
      removing = false;
    }
  }

  function onAdded(): void {
    loadNodes();
    refresh(false);
  }

  const manageColumns = $derived<DataTableColumn<main.NodeInfoDTO>[]>([
    { key: 'name', label: '名稱', cell: nameCell },
    { key: 'base_url', label: '位址', cell: urlCell },
    { key: 'online', label: '狀態', width: '110px', cell: statusCell },
    { key: 'docker_available', label: 'Docker', width: '130px', cell: dockerAvailCell },
    { key: 'fingerprint', label: '指紋', cell: fpCell },
    { key: 'actions', label: '', width: '90px', align: 'right', cell: actionsCell },
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

{#snippet nameCell(n: main.NodeInfoDTO)}
  <span class="mono">{n.name}</span>
{/snippet}
{#snippet urlCell(n: main.NodeInfoDTO)}
  <span class="mono muted url">{n.base_url || '—'}</span>
{/snippet}
{#snippet statusCell(n: main.NodeInfoDTO)}
  {#if !n.online && n.last_err}
    <Tooltip text={n.last_err}>
      <Badge tone="err">離線</Badge>
    </Tooltip>
  {:else}
    <Badge tone={n.online ? 'ok' : 'err'}>{n.online ? '在線' : '離線'}</Badge>
  {/if}
{/snippet}
{#snippet dockerAvailCell(n: main.NodeInfoDTO)}
  {#if n.name === 'local'}
    <Badge tone={n.docker_available ? 'ok' : 'idle'}>
      {n.docker_available ? '可用' : '不可用'}
    </Badge>
  {:else}
    <Tooltip text="遠端節點的 Docker 能力尚未由 health 回報,以節點端實際為準。">
      <Badge tone="idle">未知</Badge>
    </Tooltip>
  {/if}
{/snippet}
{#snippet fpCell(n: main.NodeInfoDTO)}
  {#if !n.fingerprint}
    <span class="muted">—</span>
  {:else}
    <div class="fp-row">
      <span class="mono fp">
        {expandedFp[n.name] ? n.fingerprint : shortFingerprint(n.fingerprint)}
      </span>
      <button type="button" class="link-btn" onclick={() => (expandedFp[n.name] = !expandedFp[n.name])}>
        {expandedFp[n.name] ? '收合' : '展開'}
      </button>
      <button type="button" class="link-btn" onclick={() => copyFingerprint(n.fingerprint)}>
        {copiedFp === n.fingerprint ? '已複製' : '複製'}
      </button>
    </div>
  {/if}
{/snippet}
{#snippet actionsCell(n: main.NodeInfoDTO)}
  {#if n.name !== 'local'}
    <Button
      size="sm"
      variant="danger"
      disabled={!n.removable}
      onclick={() => (removeTarget = n)}
    >
      移除
    </Button>
  {/if}
{/snippet}

<div class="toolbar manage-toolbar">
  <h3 class="sect-title">節點管理</h3>
  <div class="manage-actions">
    <Button size="sm" onclick={loadNodes} disabled={loadingNodes}>重新整理</Button>
    <Button size="sm" variant="primary" onclick={() => (showAddModal = true)}>新增節點</Button>
  </div>
</div>

{#if loadingNodes && nodeInfos.length === 0}
  <Card>
    <Skeleton height="38px" />
  </Card>
{:else if loadNodesError}
  <Card>
    <ErrorState message={`載入節點清單失敗:${loadNodesError}`} onRetry={loadNodes} />
  </Card>
{:else if nodeInfos.length === 0}
  <Card>
    <EmptyState title="尚無節點" description="至少會有本機 local 節點;若清單為空請按「重新整理」。" />
  </Card>
{:else}
  <Card>
    <DataTable columns={manageColumns} rows={nodeInfos} rowKey={(n) => n.name} />
  </Card>
{/if}

{#if showAddModal}
  <AddNodeModal onClose={() => (showAddModal = false)} {onAdded} />
{/if}

{#if removeTarget}
  <ConfirmDialog
    title="移除節點"
    message={`確定要移除節點「${removeTarget.name}」嗎?此操作會清除其連線設定與金鑰庫 token。`}
    confirmLabel="移除"
    danger
    busy={removing}
    onConfirm={doRemove}
    onCancel={() => (removeTarget = null)}
  />
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
  .manage-toolbar {
    margin-top: var(--space-5);
  }
  .sect-title {
    margin: 0;
    font-size: var(--text-md);
    color: var(--fg-0);
  }
  .manage-actions {
    display: flex;
    gap: var(--space-2);
  }
  .url {
    word-break: break-all;
  }
  .fp-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .fp {
    word-break: break-all;
  }
  .link-btn {
    font: inherit;
    font-size: var(--text-xs);
    color: var(--accent);
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    text-decoration: underline;
    flex: none;
  }
  .link-btn:hover {
    color: var(--accent-hover);
  }
</style>
