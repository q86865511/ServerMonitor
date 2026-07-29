<script lang="ts">
  // 實例維度的備份面板(單一實作;T13 全域頁以實例選擇器複用本元件)。
  // 功能等價舊 lib/BackupsPanel.svelte:列表 / 立即備份 / 還原確認;UI 改用 ui/ 元件 + DataTable。
  import type { protocol } from '../../../../wailsjs/go/models';
  import { ListBackups, BackupNow, RestoreBackup, DeleteBackup } from '../../../../wailsjs/go/main/App';
  import { call, errMsg } from '../../api';
  import { pushToast } from '../../stores/toasts';
  import { trackOperation } from '../../stores/operations';
  import { fmtTime } from '../../format';
  import Button from '../../ui/Button.svelte';
  import DataTable from '../../ui/DataTable.svelte';
  import type { DataTableColumn } from '../../ui/DataTable.svelte';
  import ConfirmDialog from '../../ui/ConfirmDialog.svelte';
  import EmptyState from '../../ui/EmptyState.svelte';
  import ErrorState from '../../ui/ErrorState.svelte';
  import Skeleton from '../../ui/Skeleton.svelte';

  let { uuid }: { uuid: string } = $props();

  let backups = $state<protocol.BackupMeta[]>([]);
  let loading = $state(true);
  let loadError = $state('');
  let backingUp = $state(false);
  let restoreTarget = $state<protocol.BackupMeta | null>(null);
  let restoring = $state(false);
  let deleteTarget = $state<protocol.BackupMeta | null>(null);
  let deleting = $state(false);

  // 遞增序號:load() 可能因 uuid 切換而併發(舊 uuid 慢回應晚到);回應套用前檢查自己仍是
  // 最新一次呼叫,否則丟棄(同 stores/instances.ts 的 refreshSeq)。
  let loadSeq = 0;

  async function load(): Promise<void> {
    const seq = ++loadSeq;
    loading = true;
    loadError = '';
    try {
      const b = await call(() => ListBackups(uuid), { silent: true });
      if (seq !== loadSeq) return; // 已有更新的 load 在途,丟棄此次舊回應
      backups = b;
    } catch (e) {
      if (seq !== loadSeq) return;
      loadError = e instanceof Error ? e.message : String(e);
    } finally {
      if (seq === loadSeq) loading = false;
    }
  }

  // uuid 變動(全域頁切換實例)時重載;首次掛載也由本 effect 觸發(不另掛 onMount,避免雙重載入)。
  let loadedFor = '';
  $effect(() => {
    if (uuid && uuid !== loadedFor) {
      loadedFor = uuid;
      // 清除可能挾帶舊實例備份 id 的選取狀態,避免確認對話框對著 A 的備份、
      // 卻對 RestoreBackup/DeleteBackup 送出當下(已切換)的 uuid=B。
      restoreTarget = null;
      deleteTarget = null;
      load();
    }
  });

  async function backupNow(): Promise<void> {
    backingUp = true;
    try {
      // 全域操作面板登記一筆「備份」(name 由 uuid 於 instances store 解析)。
      const meta = await trackOperation({ uuid, kind: 'backup' }, () => call(() => BackupNow(uuid)));
      pushToast('success', `已建立備份 ${meta.backup_id}`);
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      backingUp = false;
    }
  }

  async function doRestore(): Promise<void> {
    const target = restoreTarget;
    if (!target) return;
    restoring = true;
    try {
      await call(() => RestoreBackup(uuid, target.backup_id));
      pushToast('success', `已還原至 ${target.backup_id}`);
      restoreTarget = null;
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      restoring = false;
    }
  }

  async function doDelete(): Promise<void> {
    const target = deleteTarget;
    if (!target) return;
    deleting = true;
    try {
      await call(() => DeleteBackup(uuid, target.backup_id), { silent: true });
      pushToast('success', `已刪除備份 ${target.backup_id}`);
      deleteTarget = null;
      await load();
    } catch (e) {
      pushToast('error', `刪除備份失敗:${errMsg(e)}`);
    } finally {
      deleting = false;
    }
  }

  // ts_utc 為 Go time,wailsjs 以 any 傳回;統一轉字串交 fmtTime。
  function tsText(b: protocol.BackupMeta): string {
    return fmtTime(typeof b.ts_utc === 'string' ? b.ts_utc : String(b.ts_utc));
  }

  const columns = $derived<DataTableColumn<protocol.BackupMeta>[]>([
    { key: 'ts', label: '時間', width: '30%', cell: tsCell },
    { key: 'backup_id', label: '備份 ID', cell: idCell },
    { key: 'checksum', label: 'Checksum', width: '20%', cell: checksumCell },
    { key: 'actions', label: '', width: '170px', align: 'right', cell: actionsCell },
  ]);
</script>

{#snippet tsCell(b: protocol.BackupMeta)}
  <span>{tsText(b)}</span>
{/snippet}
{#snippet idCell(b: protocol.BackupMeta)}
  <span class="mono">{b.backup_id}</span>
{/snippet}
{#snippet checksumCell(b: protocol.BackupMeta)}
  <span class="mono muted" title={b.checksum}>{(b.checksum || '').slice(0, 12)}…</span>
{/snippet}
{#snippet actionsCell(b: protocol.BackupMeta)}
  <div class="row-actions">
    <Button size="sm" onclick={() => (restoreTarget = b)}>還原</Button>
    <Button size="sm" variant="danger" onclick={() => (deleteTarget = b)}>刪除</Button>
  </div>
{/snippet}

<div class="panel">
  <div class="head">
    <h3>備份</h3>
    <div class="tools">
      <Button size="sm" onclick={load} disabled={loading}>重新整理</Button>
      <Button size="sm" variant="primary" loading={backingUp} onclick={backupNow}>
        立即備份
      </Button>
    </div>
  </div>

  {#if loading}
    <div class="skeletons">
      <Skeleton height="38px" />
      <Skeleton height="38px" />
      <Skeleton height="38px" />
    </div>
  {:else if loadError}
    <ErrorState message={`載入備份失敗:${loadError}`} onRetry={load} />
  {:else if backups.length === 0}
    <EmptyState title="尚無備份" description="點「立即備份」建立第一份備份。" />
  {:else}
    <DataTable {columns} rows={backups} rowKey={(b) => b.backup_id} />
  {/if}
</div>

{#if restoreTarget}
  <ConfirmDialog
    title="還原備份"
    message={`確定以備份 ${restoreTarget.backup_id} 還原?\n若實例執行中會先停機、還原後再回復原狀態。`}
    confirmLabel="停機並還原"
    danger
    busy={restoring}
    onConfirm={doRestore}
    onCancel={() => (restoreTarget = null)}
  />
{/if}

{#if deleteTarget}
  <ConfirmDialog
    title="刪除備份"
    message={`確定刪除備份 ${deleteTarget.backup_id}(${tsText(deleteTarget)})?此操作不可復原。`}
    confirmLabel="刪除"
    danger
    busy={deleting}
    onConfirm={doDelete}
    onCancel={() => (deleteTarget = null)}
  />
{/if}

<style>
  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }
  .head h3 {
    margin: 0;
    font-size: var(--text-md);
  }
  .tools {
    display: flex;
    gap: var(--space-2);
  }
  .skeletons {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .mono {
    font-family: var(--font-mono, monospace);
  }
  .muted {
    color: var(--fg-2);
  }
  .row-actions {
    display: inline-flex;
    gap: var(--space-2);
    justify-content: flex-end;
  }
</style>
