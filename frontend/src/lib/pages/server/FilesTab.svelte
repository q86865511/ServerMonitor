<script lang="ts">
  // 檔案分頁(階段 5):瀏覽 / 下載 / 刪除 / 上傳實例資料目錄。
  // 路徑一律由「點擊導覽」組出(相對實例資料根、以 "/" 分隔,根為 ""),不提供手打路徑的入口;
  // 路徑合法性(遍歷、中繼檔、連結)由節點端把關,前端只負責不讓使用者按下必然失敗的按鈕。
  import type { main } from '../../../../wailsjs/go/models';
  import { ListFiles, DownloadFile, UploadFile, DeleteFile } from '../../../../wailsjs/go/main/App';
  import { call, errMsg } from '../../api';
  import { pushToast } from '../../stores/toasts';
  import { fmtBytes, fmtTime } from '../../format';
  import Button from '../../ui/Button.svelte';
  import Badge from '../../ui/Badge.svelte';
  import Tooltip from '../../ui/Tooltip.svelte';
  import DataTable from '../../ui/DataTable.svelte';
  import type { DataTableColumn } from '../../ui/DataTable.svelte';
  import ConfirmDialog from '../../ui/ConfirmDialog.svelte';
  import EmptyState from '../../ui/EmptyState.svelte';
  import ErrorState from '../../ui/ErrorState.svelte';
  import Skeleton from '../../ui/Skeleton.svelte';

  let { uuid }: { uuid: string } = $props();

  let cwd = $state(''); // 目前目錄(相對資料根;"" = 根)
  let entries = $state<main.FileEntryDTO[]>([]);
  let loading = $state(true);
  let loadError = $state('');
  let uploading = $state(false);
  let downloadingPath = $state('');
  // 刪除確認的目標「連同當下的 uuid 一起凍結」:確認框開著時使用者可能切到別台伺服器(uuid 變),
  // 若確認處理讀當前 uuid 會用新伺服器的 uuid 去刪舊路徑 → 誤刪。故確認一律用開框當下捕捉的 uuid。
  let deleteTarget = $state<{ entry: main.FileEntryDTO; uuid: string } | null>(null);
  let deleteAck = $state(false); // 目錄遞迴刪除的明示同意
  let deleting = $state(false);

  /**
   * 載入 dir 的直接子項。非同步回來時 uuid/cwd 可能已前進,故以參數的區域副本比對後才寫入,
   * 避免舊目錄的結果覆蓋新目錄的畫面。quiet=true 供操作後重載用(不閃 skeleton)。
   */
  async function load(id: string, dir: string, opts: { quiet?: boolean } = {}): Promise<void> {
    if (!opts.quiet) loading = true;
    loadError = '';
    try {
      const list = await call(() => ListFiles(id, dir), { silent: true });
      if (id !== uuid || dir !== cwd) return;
      entries = list;
    } catch (e) {
      if (id !== uuid || dir !== cwd) return;
      entries = [];
      loadError = errMsg(e);
    } finally {
      if (id === uuid && dir === cwd) loading = false;
    }
  }

  // uuid 變動(如全域頁切換實例)→ 回到資料根,並關掉可能開著的刪除確認框(它綁的是舊實例的目標,
  // 換了實例後語意已不成立)。cwd 只寫不讀,不會與下方載入 effect 互相觸發迴圈。
  let lastUuid = '';
  $effect(() => {
    const id = uuid;
    if (id !== lastUuid) {
      lastUuid = id;
      cwd = '';
      deleteTarget = null;
      deleteAck = false;
    }
  });

  // uuid 或 cwd 變動即重載;首次掛載亦由本 effect 觸發(不另掛 onMount,避免雙重載入)。
  let loadedKey = '';
  $effect(() => {
    const id = uuid;
    const dir = cwd;
    if (!id) return;
    const key = `${id} ${dir}`;
    if (key === loadedKey) return;
    loadedKey = key;
    void load(id, dir);
  });

  function reload(): void {
    // loadedKey 只擋 effect 的重複觸發;手動重載直接呼叫載入即可,不動它。
    void load(uuid, cwd);
  }

  // ---- 導覽 ----

  const crumbs = $derived.by(() => {
    if (!cwd) return [] as { name: string; path: string }[];
    const out: { name: string; path: string }[] = [];
    let acc = '';
    for (const seg of cwd.split('/')) {
      acc = acc ? `${acc}/${seg}` : seg;
      out.push({ name: seg, path: acc });
    }
    return out;
  });

  const parentPath = $derived(cwd.includes('/') ? cwd.slice(0, cwd.lastIndexOf('/')) : '');

  function goto(path: string): void {
    if (path === cwd) return;
    cwd = path;
  }

  function enter(e: main.FileEntryDTO): void {
    if (!e.is_dir || e.is_symlink) return;
    goto(e.path);
  }

  /**
   * 是否為節點端的唯讀伺服器日誌(server.log 及其輪替檔)。判定沿用後端 isReadOnlyMeta:
   * 頂層(路徑不含 "/")且檔名以 server.log 起始。可列舉、可下載,但刪除/覆寫必被拒。
   */
  function isReadOnlyLog(e: main.FileEntryDTO): boolean {
    return !e.is_dir && !e.path.includes('/') && e.name.startsWith('server.log');
  }

  // ---- 操作 ----

  // 內容指紋:上傳後用來判斷「真的有檔案進來」(新增或被覆寫),藉此區分使用者取消對話框的情形。
  function entryKey(e: main.FileEntryDTO): string {
    return `${e.name} ${e.size_bytes} ${e.modified}`;
  }

  async function upload(): Promise<void> {
    if (uploading) return;
    uploading = true;
    const id = uuid;
    const dir = cwd;
    const before = new Set(entries.map(entryKey));
    try {
      await call(() => UploadFile(id, dir));
    } catch {
      uploading = false;
      return; // toast 已呈現
    }
    uploading = false;
    if (id !== uuid || dir !== cwd) return;
    await load(id, dir, { quiet: true });
    // 後端在使用者取消開檔對話框時同樣回成功,故以清單差異(新增或指紋變動)判定是否真的上傳了。
    const added = entries.filter((e) => !before.has(entryKey(e)));
    if (added.length > 0) {
      pushToast('success', `已上傳 ${added.map((e) => e.name).join('、')}`);
    }
  }

  async function download(e: main.FileEntryDTO): Promise<void> {
    if (downloadingPath) return;
    downloadingPath = e.path;
    try {
      await call(() => DownloadFile(uuid, e.path));
      // 刻意不彈成功 toast:後端在使用者取消儲存對話框時亦回成功,無法區分「已存檔」與「取消」,
      // 報成功會是誤導。真正失敗(讀取被拒/傳輸中斷)已由 call() 彈錯誤 toast。
    } catch {
      /* toast 已呈現 */
    } finally {
      downloadingPath = '';
    }
  }

  function openDelete(e: main.FileEntryDTO): void {
    deleteAck = false;
    deleteTarget = { entry: e, uuid }; // 凍結目標與當下 uuid,確認時不受後續實例切換影響
  }

  async function doDelete(): Promise<void> {
    const target = deleteTarget;
    if (!target) return;
    const { entry, uuid: targetUuid } = target;
    if (entry.is_dir && !deleteAck) return; // 目錄需明示同意遞迴刪除(確認鈕此時亦為停用)
    deleting = true;
    try {
      await call(() => DeleteFile(targetUuid, entry.path, entry.is_dir), { silent: true });
      pushToast('success', `已刪除 ${entry.name}`);
      deleteTarget = null;
      // 僅在仍停留於同一實例、同一目錄時重載清單(切走了就不動別的實例畫面)。
      if (targetUuid === uuid) await load(uuid, cwd, { quiet: true });
    } catch (e) {
      pushToast('error', `刪除失敗:${errMsg(e)}`);
    } finally {
      deleting = false;
    }
  }

  const columns = $derived<DataTableColumn<main.FileEntryDTO>[]>([
    { key: 'name', label: '名稱', cell: nameCell },
    { key: 'size', label: '大小', width: '110px', align: 'right', cell: sizeCell },
    { key: 'modified', label: '修改時間', width: '25%', cell: timeCell },
    { key: 'actions', label: '', width: '170px', align: 'right', cell: actionsCell },
  ]);
</script>

{#snippet nameCell(e: main.FileEntryDTO)}
  <div class="name-cell">
    <span class="ico" aria-hidden="true">{e.is_symlink ? '🔗' : e.is_dir ? '📁' : '📄'}</span>
    {#if e.is_dir && !e.is_symlink}
      <button type="button" class="dir-link" onclick={() => enter(e)}>{e.name}</button>
    {:else}
      <span class="fname">{e.name}</span>
    {/if}
    {#if e.is_symlink}
      <Badge tone="off">連結</Badge>
    {:else if isReadOnlyLog(e)}
      <Badge tone="idle">唯讀</Badge>
    {/if}
  </div>
{/snippet}
{#snippet sizeCell(e: main.FileEntryDTO)}
  <span class="mono">{e.is_dir ? '—' : fmtBytes(e.size_bytes)}</span>
{/snippet}
{#snippet timeCell(e: main.FileEntryDTO)}
  <span class="muted">{fmtTime(e.modified)}</span>
{/snippet}
{#snippet actionsCell(e: main.FileEntryDTO)}
  <div class="row-actions">
    {#if e.is_symlink}
      <!-- 連結項:節點端一律拒絕跟隨(讀/寫/刪皆錯),故不給任何動作,只說明原因。 -->
      <Tooltip text="連結項目不可進入、下載或刪除(節點端不跟隨連結,避免動到資料根外的目標)">
        <Badge tone="neutral">不可操作</Badge>
      </Tooltip>
    {:else}
      {#if e.is_dir}
        <Button size="sm" onclick={() => enter(e)}>開啟</Button>
      {:else}
        <Button
          size="sm"
          loading={downloadingPath === e.path}
          disabled={downloadingPath !== ''}
          onclick={() => download(e)}
        >
          下載
        </Button>
      {/if}
      {#if isReadOnlyLog(e)}
        <Tooltip text="伺服器日誌為唯讀:執行中的伺服器正在寫入與輪替,刪除會干擾記錄(仍可下載)">
          <Button size="sm" variant="danger" disabled>刪除</Button>
        </Tooltip>
      {:else}
        <Button size="sm" variant="danger" onclick={() => openDelete(e)}>刪除</Button>
      {/if}
    {/if}
  </div>
{/snippet}

<div class="panel">
  <div class="head">
    <nav class="crumbs" aria-label="路徑">
      <button type="button" class="crumb" class:current={cwd === ''} onclick={() => goto('')}>
        資料根
      </button>
      {#each crumbs as c (c.path)}
        <span class="sep" aria-hidden="true">/</span>
        <button
          type="button"
          class="crumb"
          class:current={c.path === cwd}
          onclick={() => goto(c.path)}
        >
          {c.name}
        </button>
      {/each}
    </nav>
    <div class="tools">
      {#if cwd !== ''}
        <Button size="sm" onclick={() => goto(parentPath)}>上一層</Button>
      {/if}
      <Button size="sm" onclick={reload} disabled={loading}>重新整理</Button>
      <Button size="sm" variant="primary" loading={uploading} onclick={upload}>上傳到此目錄</Button>
    </div>
  </div>

  {#if loading}
    <div class="skeletons">
      <Skeleton height="38px" />
      <Skeleton height="38px" />
      <Skeleton height="38px" />
    </div>
  {:else if loadError}
    <ErrorState message={`載入檔案清單失敗:${loadError}`} onRetry={reload} />
  {:else if entries.length === 0}
    <EmptyState
      title="這個目錄是空的"
      description="可用「上傳到此目錄」把本機檔案放進來。實例中繼檔不會出現在清單中。"
    />
  {:else}
    <DataTable {columns} rows={entries} rowKey={(e) => e.path} />
  {/if}
</div>

{#if deleteTarget}
  <ConfirmDialog
    title={deleteTarget.entry.is_dir ? '刪除資料夾' : '刪除檔案'}
    message={deleteTarget.entry.is_dir
      ? `確定刪除資料夾「${deleteTarget.entry.name}」?\n將遞迴刪除整個資料夾及其中所有內容,此操作不可復原。`
      : `確定刪除檔案「${deleteTarget.entry.name}」(${fmtBytes(deleteTarget.entry.size_bytes)})?此操作不可復原。`}
    confirmLabel="刪除"
    danger
    busy={deleting}
    confirmDisabled={deleteTarget.entry.is_dir && !deleteAck}
    onConfirm={doDelete}
    onCancel={() => (deleteTarget = null)}
  >
    {#snippet children()}
      {#if deleteTarget?.entry.is_dir}
        <label class="ack">
          <input type="checkbox" bind:checked={deleteAck} disabled={deleting} />
          <span>我了解這會遞迴刪除整個資料夾與其中所有檔案</span>
        </label>
      {/if}
    {/snippet}
  </ConfirmDialog>
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
    flex-wrap: wrap;
  }
  .crumbs {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    flex-wrap: wrap;
    min-width: 0;
    font-size: var(--text-sm);
  }
  .crumb {
    font: inherit;
    color: var(--accent);
    background: transparent;
    border: none;
    border-radius: var(--radius-sm);
    padding: 2px 4px;
    cursor: pointer;
  }
  .crumb:hover {
    background-color: var(--bg-3);
  }
  .crumb.current {
    color: var(--fg-0);
    font-weight: 600;
    cursor: default;
  }
  .crumb:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
  .sep {
    color: var(--fg-2);
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
  .name-cell {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }
  .ico {
    flex: none;
    font-size: var(--text-base);
  }
  .fname {
    word-break: break-all;
  }
  .dir-link {
    font: inherit;
    font-size: var(--text-base);
    color: var(--accent);
    background: transparent;
    border: none;
    padding: 0;
    text-align: left;
    word-break: break-all;
    cursor: pointer;
  }
  .dir-link:hover {
    text-decoration: underline;
  }
  .dir-link:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .mono {
    font-family: var(--font-mono);
  }
  .muted {
    color: var(--fg-2);
  }
  .row-actions {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    justify-content: flex-end;
  }
  .ack {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
</style>
