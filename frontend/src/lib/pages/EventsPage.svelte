<script lang="ts">
  // 事件頁(R9):QueryEvents + 篩選(實例/事件碼/時間區間/筆數上限),改用 ui/DataTable+Select/SearchInput。
  // 功能等價舊 lib/EventsView.svelte:查詢欄位與 reset 語意相同;明細改為點列開 Modal 呈現
  // (DataTable 無內建展開列機制,以 Modal 取代原本的行內展開,顯示內容等價)。
  import { onMount } from 'svelte';
  import type { main } from '../../../wailsjs/go/models';
  import { QueryEvents } from '../../../wailsjs/go/main/App';
  import { call } from '../api';
  import { fmtTime, severityTone } from '../format';
  import { instances } from '../stores/instances';
  import Badge from '../ui/Badge.svelte';
  import Button from '../ui/Button.svelte';
  import Card from '../ui/Card.svelte';
  import DataTable from '../ui/DataTable.svelte';
  import type { DataTableColumn } from '../ui/DataTable.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
  import Modal from '../ui/Modal.svelte';
  import SearchInput from '../ui/SearchInput.svelte';
  import Select from '../ui/Select.svelte';

  // 必備 event code 目錄(R14;對齊 internal/protocol/event.go),功能等價舊 EventsView。
  const EVENT_CODES = [
    'TEMPLATE_LOAD_FAILED',
    'INSTANCE_CREATED',
    'INSTANCE_CREATE_FAILED',
    'INSTANCE_STARTED',
    'INSTANCE_STOPPED',
    'INSTANCE_CRASHED',
    'INSTANCE_RESTARTED',
    'RESTART_GIVEUP',
    'HEALTH_PROBE_FAILED',
    'BACKUP_STARTED',
    'BACKUP_COMPLETED',
    'BACKUP_FAILED',
    'RESTORE_STARTED',
    'RESTORE_COMPLETED',
    'RESTORE_FAILED',
    'ALERT_SENT',
    'ALERT_FAILED',
    'RECONCILE_ORPHAN',
    'RECONCILE_MISMATCH',
    'NODE_OFFLINE',
    'DB_QUARANTINE',
  ];

  // 事件碼 → 色調(補「類型 Badge 標色」;依語意分類的明確表,不用字串前綴猜測)。
  const CODE_TONE: Record<string, 'ok' | 'busy' | 'err'> = {
    TEMPLATE_LOAD_FAILED: 'err',
    INSTANCE_CREATED: 'ok',
    INSTANCE_CREATE_FAILED: 'err',
    INSTANCE_STARTED: 'ok',
    INSTANCE_STOPPED: 'busy',
    INSTANCE_CRASHED: 'err',
    INSTANCE_RESTARTED: 'ok',
    RESTART_GIVEUP: 'err',
    HEALTH_PROBE_FAILED: 'err',
    BACKUP_STARTED: 'busy',
    BACKUP_COMPLETED: 'ok',
    BACKUP_FAILED: 'err',
    RESTORE_STARTED: 'busy',
    RESTORE_COMPLETED: 'ok',
    RESTORE_FAILED: 'err',
    ALERT_SENT: 'busy',
    ALERT_FAILED: 'err',
    RECONCILE_ORPHAN: 'busy',
    RECONCILE_MISMATCH: 'busy',
    NODE_OFFLINE: 'err',
    DB_QUARANTINE: 'err',
  };
  function codeTone(code: string): 'ok' | 'busy' | 'err' | 'neutral' {
    return CODE_TONE[code] ?? 'neutral';
  }

  interface EventRow {
    key: number;
    ev: main.EventDTO;
  }

  let fInstance = $state('');
  let fCode = $state('');
  let fSince = $state('');
  let fUntil = $state('');
  let fLimit = $state(100);
  let quick = $state('');

  let events = $state<main.EventDTO[]>([]);
  let loading = $state(false);
  let detail = $state<main.EventDTO | null>(null);

  const instanceOptions = $derived([
    { value: '', label: '全部' },
    ...$instances.map((i) => ({
      value: i.uuid,
      label: i.name?.trim() ? i.name : `${i.template_id} #${i.uuid.slice(0, 8)}`,
    })),
  ]);
  const codeOptions = [{ value: '', label: '全部' }, ...EVENT_CODES.map((c) => ({ value: c, label: c }))];

  const filteredEvents = $derived.by(() => {
    const q = quick.trim().toLowerCase();
    if (!q) return events;
    return events.filter(
      (e) =>
        e.code.toLowerCase().includes(q) ||
        (e.instance_uuid ?? '').toLowerCase().includes(q) ||
        (e.details ?? '').toLowerCase().includes(q),
    );
  });
  const rows = $derived<EventRow[]>(filteredEvents.map((ev, key) => ({ key, ev })));

  const columns = $derived<DataTableColumn<EventRow>[]>([
    { key: 'ts', label: '時間', width: '190px', cell: tsCell },
    { key: 'severity', label: '嚴重度', width: '100px', cell: sevCell },
    { key: 'code', label: '事件碼', cell: codeCell },
    { key: 'instance', label: '實例', cell: instCell },
  ]);

  function toUnix(local: string): number {
    if (!local) return 0;
    const t = new Date(local).getTime();
    return isNaN(t) ? 0 : Math.floor(t / 1000);
  }

  async function query(): Promise<void> {
    loading = true;
    try {
      events = await call(() =>
        QueryEvents({
          instance_uuid: fInstance,
          code: fCode,
          since_unix: toUnix(fSince),
          until_unix: toUnix(fUntil),
          limit: Number(fLimit) || 0,
        }),
      );
    } catch {
      /* toast 已呈現 */
    } finally {
      loading = false;
    }
  }

  onMount(query);

  function reset(): void {
    fInstance = '';
    fCode = '';
    fSince = '';
    fUntil = '';
    fLimit = 100;
    quick = '';
    query();
  }

  function prettyDetails(raw: string): string {
    if (!raw) return '(無)';
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  }
</script>

{#snippet tsCell(r: EventRow)}
  <span class="nowrap">{fmtTime(r.ev.ts_utc)}</span>
{/snippet}
{#snippet sevCell(r: EventRow)}
  <Badge tone={severityTone(r.ev.severity)}>{r.ev.severity || '未知'}</Badge>
{/snippet}
{#snippet codeCell(r: EventRow)}
  <Badge tone={codeTone(r.ev.code)}>{r.ev.code}</Badge>
{/snippet}
{#snippet instCell(r: EventRow)}
  <span class="mono muted nowrap">{r.ev.instance_uuid || '—'}</span>
{/snippet}

<div class="section">
  <Card>
    <div class="filters">
      <Select label="實例" bind:value={fInstance} options={instanceOptions} />
      <Select label="事件碼" bind:value={fCode} options={codeOptions} />
      <div class="field">
        <label for="f-since">起(本地時間)</label>
        <input id="f-since" type="datetime-local" bind:value={fSince} />
      </div>
      <div class="field">
        <label for="f-until">迄(本地時間)</label>
        <input id="f-until" type="datetime-local" bind:value={fUntil} />
      </div>
      <div class="field limit">
        <label for="f-limit">筆數上限</label>
        <input id="f-limit" type="number" min="1" bind:value={fLimit} />
      </div>
    </div>
    <div class="row">
      <Button variant="primary" loading={loading} onclick={query}>查詢</Button>
      <Button onclick={reset} disabled={loading}>重設</Button>
      <div class="quick">
        <SearchInput bind:value={quick} placeholder="於目前結果中快速篩選…" />
      </div>
    </div>
  </Card>

  {#if !loading && rows.length === 0}
    <Card>
      <EmptyState title="無符合條件的事件" description="調整篩選條件後重新查詢。" />
    </Card>
  {:else}
    <Card>
      <DataTable {columns} {rows} rowKey={(r) => r.key} onRowClick={(r) => (detail = r.ev)} />
    </Card>
  {/if}
</div>

{#if detail}
  <Modal title="事件詳情" onClose={() => (detail = null)}>
    <dl class="kv">
      <dt>時間</dt>
      <dd>{fmtTime(detail.ts_utc)}</dd>
      <dt>嚴重度</dt>
      <dd><Badge tone={severityTone(detail.severity)}>{detail.severity || '未知'}</Badge></dd>
      <dt>事件碼</dt>
      <dd class="mono">{detail.code}</dd>
      <dt>實例</dt>
      <dd class="mono">{detail.instance_uuid || '—'}</dd>
    </dl>
    <pre class="mono detail-json">{prettyDetails(detail.details)}</pre>
  </Modal>
{/if}

<style>
  .section {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .filters {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
    gap: var(--space-3);
  }
  .filters .field {
    margin-bottom: 0;
  }
  .limit {
    max-width: 120px;
  }
  .row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    margin-top: var(--space-4);
    flex-wrap: wrap;
  }
  .quick {
    flex: 1;
    min-width: 200px;
    max-width: 320px;
    margin-left: auto;
  }
  .nowrap {
    white-space: nowrap;
  }
  .mono {
    font-family: var(--font-mono);
  }
  .muted {
    color: var(--fg-2);
  }
  .kv {
    display: grid;
    grid-template-columns: 80px 1fr;
    gap: var(--space-2) var(--space-3);
    margin: 0 0 var(--space-4);
  }
  .kv dt {
    color: var(--fg-2);
    font-size: var(--text-sm);
  }
  .kv dd {
    margin: 0;
    color: var(--fg-0);
    font-size: var(--text-sm);
  }
  .detail-json {
    margin: 0;
    padding: var(--space-3);
    background: var(--bg-0);
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    white-space: pre-wrap;
    word-break: break-word;
    color: var(--fg-1);
    max-height: 320px;
    overflow-y: auto;
  }
</style>
