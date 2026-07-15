<script lang="ts">
  import { onMount } from 'svelte';
  import { main } from '../../wailsjs/go/models';
  import { QueryEvents } from '../../wailsjs/go/main/App';
  import { call } from './api';
  import { instances } from './stores';
  import { fmtTime, severityTone } from './format';

  // 必備 event code 目錄(R14;對齊 internal/protocol/event.go)。
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

  let fInstance = '';
  let fCode = '';
  let fSince = '';
  let fUntil = '';
  let fLimit = 100;

  let events: main.EventDTO[] = [];
  let loading = false;
  let expanded: Record<number, boolean> = {};

  function toUnix(local: string): number {
    if (!local) return 0;
    const t = new Date(local).getTime();
    return isNaN(t) ? 0 : Math.floor(t / 1000);
  }

  async function query(): Promise<void> {
    loading = true;
    expanded = {};
    try {
      events = await call(() =>
        QueryEvents(
          main.QueryEventsRequest.createFrom({
            instance_uuid: fInstance,
            code: fCode,
            since_unix: toUnix(fSince),
            until_unix: toUnix(fUntil),
            limit: Number(fLimit) || 0,
          }),
        ),
      );
    } catch {
      /* toast 已呈現 */
    } finally {
      loading = false;
    }
  }

  onMount(query);

  function prettyDetails(raw: string): string {
    if (!raw) return '(無)';
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  }

  function reset(): void {
    fInstance = '';
    fCode = '';
    fSince = '';
    fUntil = '';
    fLimit = 100;
    query();
  }
</script>

<div class="spread head">
  <h2>事件</h2>
</div>

<div class="filters card">
  <div class="fgrid">
    <div class="field">
      <label for="f-inst">實例</label>
      <select id="f-inst" bind:value={fInstance}>
        <option value="">全部</option>
        {#each $instances as inst (inst.uuid)}
          <option value={inst.uuid}>{inst.uuid}</option>
        {/each}
      </select>
    </div>
    <div class="field">
      <label for="f-code">事件碼</label>
      <select id="f-code" bind:value={fCode}>
        <option value="">全部</option>
        {#each EVENT_CODES as c}
          <option value={c}>{c}</option>
        {/each}
      </select>
    </div>
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
  <div class="row actions">
    <button class="primary" on:click={query} disabled={loading}>{loading ? '查詢中…' : '查詢'}</button>
    <button on:click={reset} disabled={loading}>重設</button>
  </div>
</div>

{#if events.length === 0 && !loading}
  <div class="empty">無符合條件的事件。</div>
{:else}
  <div class="scroll-x">
    <table>
      <thead>
        <tr>
          <th>時間</th>
          <th>嚴重度</th>
          <th>事件碼</th>
          <th>實例</th>
          <th>details</th>
        </tr>
      </thead>
      <tbody>
        {#each events as e, i}
          <tr class="ev" on:click={() => (expanded = { ...expanded, [i]: !expanded[i] })}>
            <td class="nowrap">{fmtTime(e.ts_utc)}</td>
            <td><span class="sev {severityTone(e.severity)}">{e.severity}</span></td>
            <td class="mono">{e.code}</td>
            <td class="mono muted nowrap">{e.instance_uuid || '—'}</td>
            <td class="muted">{e.details ? (expanded[i] ? '▾' : '▸ 展開') : '(無)'}</td>
          </tr>
          {#if expanded[i] && e.details}
            <tr class="detail-row">
              <td colspan="5"><pre class="mono">{prettyDetails(e.details)}</pre></td>
            </tr>
          {/if}
        {/each}
      </tbody>
    </table>
  </div>
{/if}

<style>
  .head {
    margin-bottom: 16px;
  }
  .filters {
    margin-bottom: 16px;
  }
  .fgrid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
    gap: 12px;
  }
  .fgrid .field {
    margin-bottom: 0;
  }
  .limit {
    max-width: 120px;
  }
  .actions {
    margin-top: 14px;
  }
  .sev {
    display: inline-block;
    padding: 1px 8px;
    border-radius: 10px;
    font-size: 11px;
    border: 1px solid;
  }
  .sev.ok {
    color: var(--ok);
    border-color: var(--ok);
  }
  .sev.busy {
    color: var(--busy);
    border-color: var(--busy);
  }
  .sev.err {
    color: var(--err);
    border-color: var(--err);
  }
  .nowrap {
    white-space: nowrap;
  }
  .ev {
    cursor: pointer;
  }
  .detail-row td {
    background: var(--bg-0);
  }
  pre {
    margin: 0;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-word;
    color: var(--fg-1);
  }
</style>
