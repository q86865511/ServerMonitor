<script lang="ts">
  import { onMount } from 'svelte';
  import { main } from '../../wailsjs/go/models';
  import { ListSchedules, UpsertSchedule, DeleteSchedule } from '../../wailsjs/go/main/App';
  import { call } from './api';
  import { pushToast } from './stores';
  import { fmtWeekdays, fmtTime, WEEKDAY_LABELS } from './format';
  import Modal from './Modal.svelte';
  import ConfirmDialog from './ConfirmDialog.svelte';

  export let uuid: string;

  let schedules: main.ScheduleDTO[] = [];
  let loading = true;

  // 編輯器狀態(id 空=新增)。
  let editing = false;
  let saving = false;
  let editId = '';
  let editKind = 'restart';
  let editAt = '03:00';
  let editWeekdays: number[] = [];
  let editEnabled = true;

  let deleteTarget: main.ScheduleDTO | null = null;
  let deleting = false;

  async function load(): Promise<void> {
    loading = true;
    try {
      schedules = await call(() => ListSchedules(uuid));
    } catch {
      /* toast 已呈現 */
    } finally {
      loading = false;
    }
  }

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
    if (!deleteTarget) return;
    deleting = true;
    try {
      await call(() => DeleteSchedule(deleteTarget!.id));
      pushToast('success', '已刪除排程');
      deleteTarget = null;
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      deleting = false;
    }
  }

  const KIND_LABEL: Record<string, string> = { restart: '重啟', backup: '備份' };
</script>

<div class="spread head">
  <h3>排程</h3>
  <div class="row">
    <button class="sm" on:click={load} disabled={loading}>重新整理</button>
    <button class="sm primary" on:click={openNew}>＋ 新增排程</button>
  </div>
</div>

{#if loading}
  <div class="empty">載入中…</div>
{:else if schedules.length === 0}
  <div class="empty">尚無排程。</div>
{:else}
  <div class="scroll-x">
    <table>
      <thead>
        <tr>
          <th>種類</th>
          <th>時刻(UTC)</th>
          <th>週期</th>
          <th>啟用</th>
          <th>上次觸發</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {#each schedules as s (s.id)}
          <tr>
            <td>{KIND_LABEL[s.kind] ?? s.kind}</td>
            <td class="mono">{s.at}</td>
            <td>{fmtWeekdays(s.weekdays)}</td>
            <td>
              <button class="sm ghost" on:click={() => toggleEnabled(s)}>
                {s.enabled ? '✅ 啟用' : '⛔ 停用'}
              </button>
            </td>
            <td class="muted">{fmtTime(s.last_fired_utc)}</td>
            <td class="right">
              <button class="sm" on:click={() => openEdit(s)}>編輯</button>
              <button class="sm danger" on:click={() => (deleteTarget = s)}>刪除</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}

{#if editing}
  <Modal title={editId ? '編輯排程' : '新增排程'} on:close={() => (editing = false)}>
    <div class="field">
      <label for="s-kind">種類</label>
      <select id="s-kind" bind:value={editKind}>
        <option value="restart">重啟</option>
        <option value="backup">備份</option>
      </select>
    </div>
    <div class="field">
      <label for="s-at">時刻(UTC，24 小時)</label>
      <input id="s-at" type="time" bind:value={editAt} />
    </div>
    <div class="field">
      <label>週期(不選=每天)</label>
      <div class="wrap">
        {#each WEEKDAY_LABELS as lbl, d}
          <button
            class="sm day"
            class:on={editWeekdays.includes(d)}
            on:click={() => toggleDay(d)}
            type="button">{lbl}</button
          >
        {/each}
      </div>
    </div>
    <div class="field checkbox-row">
      <input id="s-en" type="checkbox" bind:checked={editEnabled} />
      <label for="s-en" style="margin:0">啟用</label>
    </div>
    <div class="actions">
      <button on:click={() => (editing = false)} disabled={saving}>取消</button>
      <button class="primary" on:click={save} disabled={saving || !editAt}>
        {saving ? '儲存中…' : '儲存'}
      </button>
    </div>
  </Modal>
{/if}

{#if deleteTarget}
  <ConfirmDialog
    title="刪除排程"
    message={`確定刪除此${KIND_LABEL[deleteTarget.kind] ?? deleteTarget.kind}排程(${deleteTarget.at} UTC)?`}
    confirmLabel="刪除"
    danger
    busy={deleting}
    on:confirm={doDelete}
    on:cancel={() => (deleteTarget = null)}
  />
{/if}

<style>
  .head {
    margin-bottom: 12px;
  }
  .right {
    text-align: right;
    white-space: nowrap;
  }
  .day.on {
    background: var(--accent);
    border-color: var(--accent);
    color: #fff;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
    margin-top: 8px;
  }
</style>
