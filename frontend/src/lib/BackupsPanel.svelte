<script lang="ts">
  import { onMount } from 'svelte';
  import { protocol } from '../../wailsjs/go/models';
  import { ListBackups, BackupNow, RestoreBackup } from '../../wailsjs/go/main/App';
  import { call } from './api';
  import { pushToast } from './stores';
  import { fmtTime } from './format';
  import ConfirmDialog from './ConfirmDialog.svelte';

  export let uuid: string;

  let backups: protocol.BackupMeta[] = [];
  let loading = true;
  let backingUp = false;
  let restoreTarget: protocol.BackupMeta | null = null;
  let restoring = false;

  async function load(): Promise<void> {
    loading = true;
    try {
      backups = await call(() => ListBackups(uuid));
    } catch {
      /* toast 已呈現 */
    } finally {
      loading = false;
    }
  }

  onMount(load);

  async function backupNow(): Promise<void> {
    backingUp = true;
    try {
      const meta = await call(() => BackupNow(uuid));
      pushToast('success', `已建立備份 ${meta.backup_id}`);
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      backingUp = false;
    }
  }

  async function doRestore(): Promise<void> {
    if (!restoreTarget) return;
    restoring = true;
    try {
      await call(() => RestoreBackup(uuid, restoreTarget!.backup_id));
      pushToast('success', `已還原至 ${restoreTarget.backup_id}`);
      restoreTarget = null;
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      restoring = false;
    }
  }
</script>

<div class="spread head">
  <h3>備份</h3>
  <div class="row">
    <button class="sm" on:click={load} disabled={loading}>重新整理</button>
    <button class="sm primary" on:click={backupNow} disabled={backingUp}>
      {backingUp ? '備份中…' : '立即備份'}
    </button>
  </div>
</div>

{#if loading}
  <div class="empty">載入中…</div>
{:else if backups.length === 0}
  <div class="empty">尚無備份。</div>
{:else}
  <div class="scroll-x">
    <table>
      <thead>
        <tr>
          <th>時間</th>
          <th>備份 ID</th>
          <th>checksum</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {#each backups as b (b.backup_id)}
          <tr>
            <td>{fmtTime(b.ts_utc)}</td>
            <td class="mono">{b.backup_id}</td>
            <td class="mono muted" title={b.checksum}>{(b.checksum || '').slice(0, 12)}…</td>
            <td class="right">
              <button class="sm" on:click={() => (restoreTarget = b)}>還原</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}

{#if restoreTarget}
  <ConfirmDialog
    title="還原備份"
    message={`確定以備份 ${restoreTarget.backup_id} 還原?\n若實例執行中會先停機、還原後再回復原狀態。`}
    confirmLabel="停機並還原"
    danger
    busy={restoring}
    on:confirm={doRestore}
    on:cancel={() => (restoreTarget = null)}
  />
{/if}

<style>
  .head {
    margin-bottom: 12px;
  }
  .right {
    text-align: right;
  }
</style>
