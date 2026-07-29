<script lang="ts">
  // Docker 資源管理頁(階段 4):管理本機節點的映像與容器。
  // 兩區塊——映像(列出 / 刪除 / 清理未使用 prune)與容器(列出全部含孤兒 / 刪除)。
  // native-only 節點無 Docker 後端(DockerAvailable=false):以友善提示取代錯誤/空白,不呼叫列出端點。
  // 破壞性動作(刪映像/容器、prune)一律二次確認並顯示對象識別;使用中映像/執行中容器提供強制刪除選項。
  import { onMount } from 'svelte';
  import type { main } from '../../../wailsjs/go/models';
  import {
    DockerAvailable,
    ListImages,
    RemoveImage,
    PruneImages,
    ListContainers,
    RemoveContainer,
  } from '../../../wailsjs/go/main/App';
  import { call, errMsg } from '../api';
  import { instances } from '../stores/instances';
  import { pushToast } from '../stores/toasts';
  import { fmtBytes, fmtTime } from '../format';
  import Badge from '../ui/Badge.svelte';
  import Button from '../ui/Button.svelte';
  import Card from '../ui/Card.svelte';
  import ConfirmDialog from '../ui/ConfirmDialog.svelte';
  import DataTable from '../ui/DataTable.svelte';
  import type { DataTableColumn } from '../ui/DataTable.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
  import ErrorState from '../ui/ErrorState.svelte';
  import Skeleton from '../ui/Skeleton.svelte';

  // Docker 能力:null=尚未探測;true=可列出;false=native-only 或 Docker 不可用(顯友善提示)。
  let dockerAvailable = $state<boolean | null>(null);
  let probing = $state(false);

  // ---- 映像 ----
  let images = $state<main.ImageDTO[]>([]);
  let imagesLoading = $state(false);
  let imagesError = $state('');

  // ---- 容器 ----
  let containers = $state<main.ContainerDTO[]>([]);
  let containersLoading = $state(false);
  let containersError = $state('');

  // 孤兒判定:gsm 管理但對應實例已不存在(以輪詢中的 instances 為權威來源)。
  const knownUuids = $derived(new Set($instances.map((i) => i.uuid)));

  async function loadImages(): Promise<void> {
    imagesLoading = true;
    imagesError = '';
    try {
      images = await call(() => ListImages(), { silent: true });
    } catch (e) {
      imagesError = errMsg(e);
    } finally {
      imagesLoading = false;
    }
  }

  async function loadContainers(): Promise<void> {
    containersLoading = true;
    containersError = '';
    try {
      containers = await call(() => ListContainers(), { silent: true });
    } catch (e) {
      containersError = errMsg(e);
    } finally {
      containersLoading = false;
    }
  }

  // 先探測 Docker 能力(避免對 native-only 節點做註定失敗的列出呼叫),可用才載入兩區塊。
  async function refreshAll(): Promise<void> {
    if (probing) return;
    probing = true;
    try {
      dockerAvailable = await DockerAvailable();
    } catch {
      // 探測本身失敗時視為可用,讓後續列出呼叫如實回報錯誤,不誤判為 native-only。
      dockerAvailable = true;
    } finally {
      probing = false;
    }
    if (dockerAvailable) {
      await Promise.all([loadImages(), loadContainers()]);
    } else {
      images = [];
      containers = [];
    }
  }

  onMount(refreshAll);

  // ---- 顯示輔助 ----
  function shortId(id: string): string {
    const bare = id.replace(/^sha256:/, '');
    return bare.slice(0, 12);
  }
  function imageLabel(im: main.ImageDTO): string {
    return im.tags.length > 0 ? im.tags.join('、') : `<none>(${shortId(im.id)})`;
  }
  function containerLabel(c: main.ContainerDTO): string {
    return c.name || shortId(c.id);
  }

  // Docker 容器狀態(小寫)→ 色調。
  function containerStateTone(state: string): 'ok' | 'busy' | 'idle' | 'err' {
    switch (state) {
      case 'running':
        return 'ok';
      case 'restarting':
      case 'paused':
      case 'removing':
        return 'busy';
      case 'dead':
        return 'err';
      default:
        return 'idle';
    }
  }

  interface Kind {
    label: string;
    tone: 'accent' | 'busy' | 'idle';
  }
  function classify(c: main.ContainerDTO): Kind {
    if (!c.gsm) return { label: '外部', tone: 'idle' };
    if (c.uuid && knownUuids.has(c.uuid)) return { label: '本工具管理', tone: 'accent' };
    return { label: '孤兒', tone: 'busy' };
  }

  // ---- 刪除映像 ----
  let removeImageTarget = $state<main.ImageDTO | null>(null);
  let forceImage = $state(false);
  let removingImage = $state(false);
  function openRemoveImage(im: main.ImageDTO): void {
    removeImageTarget = im;
    forceImage = false;
  }
  async function doRemoveImage(): Promise<void> {
    const t = removeImageTarget;
    if (!t) return;
    removingImage = true;
    try {
      await call(() => RemoveImage(t.id, forceImage), { silent: true });
      pushToast('success', `已刪除映像 ${imageLabel(t)}`);
      removeImageTarget = null;
      await loadImages();
    } catch (e) {
      // containers 為 -1 表 Docker API 未計算(ContainerCount 未生效),視同「未知」一併提示;
      // 僅 containers === 0(確定無容器使用)才不提示。
      const hint = !forceImage && t.containers !== 0 ? '(映像正被容器使用,可勾選「強制刪除」)' : '';
      pushToast('error', `刪除映像失敗:${errMsg(e)}${hint}`);
    } finally {
      removingImage = false;
    }
  }

  // ---- 清理未使用映像(prune)----
  let pruneOpen = $state(false);
  let pruning = $state(false);
  async function doPrune(): Promise<void> {
    pruning = true;
    try {
      const res = await call(() => PruneImages(), { silent: true });
      pushToast('success', `已清理 ${res.deleted.length} 份映像,回收 ${fmtBytes(res.reclaimed_bytes)}`);
      pruneOpen = false;
      await loadImages();
    } catch (e) {
      pushToast('error', `清理映像失敗:${errMsg(e)}`);
    } finally {
      pruning = false;
    }
  }

  // ---- 刪除容器 ----
  let removeContainerTarget = $state<main.ContainerDTO | null>(null);
  let forceContainer = $state(false);
  let removingContainer = $state(false);
  function openRemoveContainer(c: main.ContainerDTO): void {
    removeContainerTarget = c;
    forceContainer = false;
  }
  async function doRemoveContainer(): Promise<void> {
    const t = removeContainerTarget;
    if (!t) return;
    removingContainer = true;
    try {
      await call(() => RemoveContainer(t.id, forceContainer), { silent: true });
      pushToast('success', `已刪除容器 ${containerLabel(t)}`);
      removeContainerTarget = null;
      await loadContainers();
    } catch (e) {
      const hint =
        !forceContainer && t.state === 'running' ? '(容器執行中,可勾選「強制刪除」)' : '';
      pushToast('error', `刪除容器失敗:${errMsg(e)}${hint}`);
    } finally {
      removingContainer = false;
    }
  }

  const imageColumns = $derived<DataTableColumn<main.ImageDTO>[]>([
    { key: 'tags', label: '標籤', cell: imgTagsCell },
    { key: 'size', label: '大小', width: '110px', align: 'right', cell: imgSizeCell },
    { key: 'containers', label: '容器', width: '80px', align: 'right', cell: imgContainersCell },
    { key: 'created', label: '建立時間', width: '20%', cell: imgCreatedCell },
    { key: 'actions', label: '', width: '90px', align: 'right', cell: imgActionsCell },
  ]);

  const containerColumns = $derived<DataTableColumn<main.ContainerDTO>[]>([
    { key: 'name', label: '名稱', cell: cName },
    { key: 'image', label: '映像', cell: cImage },
    { key: 'state', label: '狀態', width: '110px', cell: cState },
    { key: 'kind', label: '類型', width: '130px', cell: cKind },
    { key: 'actions', label: '', width: '90px', align: 'right', cell: cActions },
  ]);
</script>

{#snippet imgTagsCell(im: main.ImageDTO)}
  <span class="mono" class:muted={im.tags.length === 0}>{imageLabel(im)}</span>
{/snippet}
{#snippet imgSizeCell(im: main.ImageDTO)}
  <span>{fmtBytes(im.size_bytes)}</span>
{/snippet}
{#snippet imgContainersCell(im: main.ImageDTO)}
  {#if im.containers < 0}
    <span class="muted">不適用</span>
  {:else}
    <span class:muted={im.containers === 0}>{im.containers}</span>
  {/if}
{/snippet}
{#snippet imgCreatedCell(im: main.ImageDTO)}
  <span class="muted">{fmtTime(im.created)}</span>
{/snippet}
{#snippet imgActionsCell(im: main.ImageDTO)}
  <Button size="sm" variant="danger" onclick={() => openRemoveImage(im)}>刪除</Button>
{/snippet}

{#snippet cName(c: main.ContainerDTO)}
  <span class="mono">{containerLabel(c)}</span>
{/snippet}
{#snippet cImage(c: main.ContainerDTO)}
  <span class="mono muted image">{c.image || '—'}</span>
{/snippet}
{#snippet cState(c: main.ContainerDTO)}
  <Badge tone={containerStateTone(c.state)}>{c.state || '未知'}</Badge>
{/snippet}
{#snippet cKind(c: main.ContainerDTO)}
  {@const k = classify(c)}
  <Badge tone={k.tone}>{k.label}</Badge>
{/snippet}
{#snippet cActions(c: main.ContainerDTO)}
  <Button size="sm" variant="danger" onclick={() => openRemoveContainer(c)}>刪除</Button>
{/snippet}

<div class="toolbar">
  <p class="hint">管理本機節點的 Docker 映像與容器。刪除與清理不可復原。</p>
  <Button size="sm" loading={probing} onclick={refreshAll}>重新整理</Button>
</div>

{#if dockerAvailable === false}
  <Card>
    <EmptyState
      title="此節點無 Docker 後端"
      description="目前節點為 native-only(無 Docker),或 Docker 目前不可用。可於「節點」頁重試連線 Docker 後再重新整理。"
    />
  </Card>
{:else}
  <div class="sections">
  <!-- 映像 -->
  <Card>
    <div class="sec-head">
      <h3 class="sect-title">映像</h3>
      <div class="actions">
        <Button size="sm" onclick={loadImages} disabled={imagesLoading}>重新整理</Button>
        <Button
          size="sm"
          variant="danger"
          onclick={() => (pruneOpen = true)}
          disabled={imagesLoading}
        >
          清理未使用(prune)
        </Button>
      </div>
    </div>

    {#if imagesLoading && images.length === 0}
      <Skeleton height="38px" />
    {:else if imagesError}
      <ErrorState message={`載入映像失敗:${imagesError}`} onRetry={loadImages} />
    {:else if images.length === 0}
      <EmptyState title="尚無映像" description="此節點目前沒有任何 Docker 映像。" />
    {:else}
      <DataTable columns={imageColumns} rows={images} rowKey={(im) => im.id} />
    {/if}
  </Card>

  <!-- 容器 -->
  <Card>
    <div class="sec-head">
      <h3 class="sect-title">容器</h3>
      <div class="actions">
        <Button size="sm" onclick={loadContainers} disabled={containersLoading}>重新整理</Button>
      </div>
    </div>

    {#if containersLoading && containers.length === 0}
      <Skeleton height="38px" />
    {:else if containersError}
      <ErrorState message={`載入容器失敗:${containersError}`} onRetry={loadContainers} />
    {:else if containers.length === 0}
      <EmptyState title="尚無容器" description="此節點目前沒有任何 Docker 容器。" />
    {:else}
      <DataTable columns={containerColumns} rows={containers} rowKey={(c) => c.id} />
    {/if}
  </Card>
  </div>
{/if}

{#if removeImageTarget}
  <ConfirmDialog
    title="刪除映像"
    message={`確定刪除映像「${imageLabel(removeImageTarget)}」?此操作不可復原。`}
    confirmLabel="刪除"
    danger
    busy={removingImage}
    onConfirm={doRemoveImage}
    onCancel={() => (removeImageTarget = null)}
  >
    {#snippet children()}
      {#if removeImageTarget && removeImageTarget.containers > 0}
        <p class="warn">此映像有 {removeImageTarget.containers} 個容器正在使用,需勾選強制刪除。</p>
      {/if}
      <label class="opt">
        <input type="checkbox" bind:checked={forceImage} disabled={removingImage} />
        <span>強制刪除(-f)</span>
      </label>
    {/snippet}
  </ConfirmDialog>
{/if}

{#if pruneOpen}
  <ConfirmDialog
    title="清理未使用映像"
    message={'清理所有未使用(懸掛)的映像?此操作會刪除未被任何容器或標籤引用的映像層,不可復原。'}
    confirmLabel="清理"
    danger
    busy={pruning}
    onConfirm={doPrune}
    onCancel={() => (pruneOpen = false)}
  />
{/if}

{#if removeContainerTarget}
  <ConfirmDialog
    title="刪除容器"
    message={`確定刪除容器「${containerLabel(removeContainerTarget)}」?此操作不可復原。`}
    confirmLabel="刪除"
    danger
    busy={removingContainer}
    onConfirm={doRemoveContainer}
    onCancel={() => (removeContainerTarget = null)}
  >
    {#snippet children()}
      {#if removeContainerTarget && removeContainerTarget.state === 'running'}
        <p class="warn">此容器執行中,需勾選強制刪除才能移除。</p>
      {/if}
      <label class="opt">
        <input type="checkbox" bind:checked={forceContainer} disabled={removingContainer} />
        <span>強制刪除執行中的容器(-f)</span>
      </label>
    {/snippet}
  </ConfirmDialog>
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
  .sections {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .sec-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    margin-bottom: var(--space-3);
    flex-wrap: wrap;
  }
  .sect-title {
    margin: 0;
    font-size: var(--text-md);
    color: var(--fg-0);
  }
  .actions {
    display: flex;
    gap: var(--space-2);
  }
  .mono {
    font-family: var(--font-mono);
  }
  .muted {
    color: var(--fg-2);
  }
  .image {
    word-break: break-all;
  }
  .warn {
    margin: 0 0 var(--space-3);
    font-size: var(--text-sm);
    color: var(--busy);
  }
  .opt {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
</style>
