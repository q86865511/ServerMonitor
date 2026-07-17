<script lang="ts">
  // 實例維度的排程面板(單一實作;T13 全域頁以實例選擇器複用本元件)。
  // 功能等價舊 lib/SchedulesPanel.svelte:排程 CRUD;UI 改用 ui/ 元件 + DataTable + Modal。
  import { onMount } from 'svelte';
  import { main } from '../../../../wailsjs/go/models';
  import { ListSchedules, UpsertSchedule, DeleteSchedule } from '../../../../wailsjs/go/main/App';
  import { call } from '../../api';
  import { pushToast } from '../../stores/toasts';
  import { fmtWeekdays, fmtTime, WEEKDAY_LABELS } from '../../format';
  import Button from '../../ui/Button.svelte';
  import DataTable from '../../ui/DataTable.svelte';
  import type { DataTableColumn } from '../../ui/DataTable.svelte';
  import Modal from '../../ui/Modal.svelte';
  import Select from '../../ui/Select.svelte';
  import ConfirmDialog from '../../ui/ConfirmDialog.svelte';
  import EmptyState from '../../ui/EmptyState.svelte';
  import ErrorState from '../../ui/ErrorState.svelte';
  import Skeleton from '../../ui/Skeleton.svelte';

  let { uuid }: { uuid: string } = $props();

  let schedules = $state<main.ScheduleDTO[]>([]);
  let loading = $state(true);
  let loadError = $state('');

  // 編輯器狀態(id 空=新增)。
  let editing = $state(false);
  let saving = $state(false);
  let editId = $state('');
  let editKind = $state('restart');
  let editAt = $state('03:00');
  let editWeekdays = $state<number[]>([]);
  let editEnabled = $state(true);

  let deleteTarget = $state<main.ScheduleDTO | null>(null);
  let deleting = $state(false);

  const KIND_LABEL: Record<string, string> = { restart: '重啟', backup: '備份' };

  async function load(): Promise<void> {
    loading = true;
    loadError = '';
    try {
      schedules = await call(() => ListSchedules(uuid), { silent: true });
    } catch (e) {
      loadError = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  let loadedFor = '';
  $effect(() => {
    if (uuid && uuid !== loadedFor) {
      loadedFor = uuid;
      load();
    }
  });

  onMount(load);

  function openNew(): void {
    editId = '';
    editKind = 'restart';
    editAt = '03:00';
    editWeekdays = [];
    editEnabled = true;
    editing = true;
  }

  function openEdit(s: main.ScheduleDTO): void {
    editId = s.id;
    editKind = s.kind;
    editAt = s.at || '03:00';
    editWeekdays = [...(s.weekdays ?? [])];
    editEnabled = s.enabled;
    editing = true;
  }

  function toggleDay(d: number): void {
    editWeekdays = editWeekdays.includes(d)
      ? editWeekdays.filter((x) => x !== d)
      : [...editWeekdays, d];
  }

  async function save(): Promise<void> {
    saving = true;
    try {
      await call(() =>
        UpsertSchedule(
          main.UpsertScheduleRequest.createFrom({
            id: editId,
            instance_uuid: uuid,
            kind: editKind,
            at: editAt,
            weekdays: editWeekdays,
            enabled: editEnabled,
          }),
        ),
      );
      pushToast('success', editId ? '已更新排程' : '已新增排程');
      editing = false;
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      saving = false;
    }
  }

  async function toggleEnabled(s: main.ScheduleDTO): Promise<void> {
    try {
      await call(() =>
        UpsertSchedule(
          main.UpsertScheduleRequest.createFrom({
            id: s.id,
            instance_uuid: s.instance_uuid,
            kind: s.kind,
            at: s.at,
            weekdays: s.weekdays,
            enabled: !s.enabled,
          }),
        ),
      );
      await load();
    } catch {
      /* toast 已呈現 */
    }
  }

  async function doDelete(): Promise<void> {
    const target = deleteTarget;
    if (!target) return;
    deleting = true;
    try {
      await call(() => DeleteSchedule(target.id));
      pushToast('success', '已刪除排程');
      deleteTarget = null;
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      deleting = false;
    }
  }

  const columns = $derived<DataTableColumn<main.ScheduleDTO>[]>([
    { key: 'kind', label: '種類', width: '90px', cell: kindCell },
    { key: 'at', label: '時刻(UTC)', width: '110px', cell: atCell },
    { key: 'weekdays', label: '週期', cell: weekdaysCell },
    { key: 'enabled', label: '啟用', width: '110px', cell: enabledCell },
    { key: 'last', label: '上次觸發', cell: lastCell },
    { key: 'actions', label: '', width: '150px', align: 'right', cell: actionsCell },
  ]);
</script>

{#snippet kindCell(s: main.ScheduleDTO)}
  <span>{KIND_LABEL[s.kind] ?? s.kind}</span>
{/snippet}
{#snippet atCell(s: main.ScheduleDTO)}
  <span class="mono">{s.at}</span>
{/snippet}
{#snippet weekdaysCell(s: main.ScheduleDTO)}
  <span>{fmtWeekdays(s.weekdays)}</span>
{/snippet}
{#snippet enabledCell(s: main.ScheduleDTO)}
  <Button size="sm" variant="ghost" onclick={() => toggleEnabled(s)}>
    {s.enabled ? '✅ 啟用' : '⛔ 停用'}
  </Button>
{/snippet}
{#snippet lastCell(s: main.ScheduleDTO)}
  <span class="muted">{fmtTime(s.last_fired_utc)}</span>
{/snippet}
{#snippet actionsCell(s: main.ScheduleDTO)}
  <div class="row-actions">
    <Button size="sm" onclick={() => openEdit(s)}>編輯</Button>
    <Button size="sm" variant="danger" onclick={() => (deleteTarget = s)}>刪除</Button>
  </div>
{/snippet}

<div class="panel">
  <div class="head">
    <h3>排程</h3>
    <div class="tools">
      <Button size="sm" onclick={load} disabled={loading}>重新整理</Button>
      <Button size="sm" variant="primary" onclick={openNew}>＋ 新增排程</Button>
    </div>
  </div>

  {#if loading}
    <div class="skeletons">
      <Skeleton height="38px" />
      <Skeleton height="38px" />
      <Skeleton height="38px" />
    </div>
  {:else if loadError}
    <ErrorState message={`載入排程失敗:${loadError}`} onRetry={load} />
  {:else if schedules.length === 0}
    <EmptyState title="尚無排程" description="新增定時重啟或備份排程。" />
  {:else}
    <DataTable {columns} rows={schedules} rowKey={(s) => s.id} />
  {/if}
</div>

{#if editing}
  <Modal title={editId ? '編輯排程' : '新增排程'} onClose={() => (editing = false)}>
    <div class="form">
      <Select
        label="種類"
        bind:value={editKind}
        options={[
          { value: 'restart', label: '重啟' },
          { value: 'backup', label: '備份' },
        ]}
      />
      <div class="field">
        <label for="s-at">時刻(UTC，24 小時)</label>
        <input id="s-at" type="time" bind:value={editAt} />
      </div>
      <div class="field">
        <span class="lbl">週期(不選=每天)</span>
        <div class="days">
          {#each WEEKDAY_LABELS as lbl, d (d)}
            <button
              type="button"
              class="day"
              class:on={editWeekdays.includes(d)}
              onclick={() => toggleDay(d)}>{lbl}</button
            >
          {/each}
        </div>
      </div>
      <label class="check">
        <input type="checkbox" bind:checked={editEnabled} />
        <span>啟用</span>
      </label>
    </div>
    {#snippet footer()}
      <Button onclick={() => (editing = false)} disabled={saving}>取消</Button>
      <Button variant="primary" loading={saving} disabled={!editAt} onclick={save}>儲存</Button>
    {/snippet}
  </Modal>
{/if}

{#if deleteTarget}
  <ConfirmDialog
    title="刪除排程"
    message={`確定刪除此${KIND_LABEL[deleteTarget.kind] ?? deleteTarget.kind}排程(${deleteTarget.at} UTC)?`}
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
  .row-actions {
    display: inline-flex;
    gap: var(--space-2);
    justify-content: flex-end;
  }
  .form {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .field label,
  .field .lbl {
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .field input[type='time'] {
    font: inherit;
    color: var(--fg-0);
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 6px 9px;
    width: 100%;
  }
  .field input[type='time']:focus-visible {
    outline: none;
    border-color: var(--accent);
  }
  .days {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .day {
    font: inherit;
    font-size: var(--text-sm);
    min-width: 34px;
    padding: 5px 8px;
    color: var(--fg-1);
    background-color: var(--bg-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard),
      border-color var(--dur-fast) var(--ease-standard),
      color var(--dur-fast) var(--ease-standard);
  }
  .day:hover {
    border-color: var(--fg-2);
  }
  .day.on {
    background-color: var(--accent);
    border-color: var(--accent);
    color: var(--fg-on-accent);
  }
  .day:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .mono {
    font-family: var(--font-mono);
  }
  .muted {
    color: var(--fg-2);
  }
</style>
