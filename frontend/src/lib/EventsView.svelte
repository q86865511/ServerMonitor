<script lang="ts">
  import { onMount } from 'svelte';
  import { main } from '../../wailsjs/go/models';
  import { QueryEvents } from '../../wailsjs/go/main/App';
  import { call } from './api';
  import { instances } from './stores';
  import { fmtTime, severityTone } from './format';
  import Icon from './Icon.svelte';

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

<header class="page-head">
  <div class="page-copy">
    <div class="eyebrow">System / Event log</div>
    <h2>事件記錄</h2>
    <p class="page-description">追蹤生命週期、健康檢查、備份與告警活動。</p>
  </div>
</header>

<section class="filters card" aria-labelledby="filter-title">
  <div class="filter-head">
    <div>
      <div class="filter-code mono">QUERY CONTROLS</div>
      <h3 id="filter-title">篩選條件</h3>
    </div>
    <div class="row actions">
      <button class="sm" on:click={reset} disabled={loading}>重設</button>
      <button class="sm primary" on:click={query} disabled={loading}>
        <Icon name="events" size={14} />
        <span>{loading ? '查詢中…' : '執行查詢'}</span>
      </button>
    </div>
  </div>
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
</section>

{#if events.length === 0 && !loading}
  <div class="empty-state event-empty">
    <div class="empty-state-inner">
      <div class="empty-mark"><Icon name="events" size={26} /></div>
      <div class="empty-title">沒有符合條件的事件</div>
      <p class="empty-copy">調整篩選條件後重新查詢，或等待系統產生新的活動記錄。</p>
    </div>
  </div>
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
          <tr class="ev">
            <td class="nowrap mono time">{fmtTime(e.ts_utc)}</td>
            <td><span class="sev {severityTone(e.severity)}"><i></i>{e.severity}</span></td>
            <td class="mono">{e.code}</td>
            <td class="mono muted nowrap">{e.instance_uuid || '—'}</td>
            <td class="detail-cell">
              {#if e.details}
                <button
                  class="ghost sm expand"
                  on:click={() => (expanded = { ...expanded, [i]: !expanded[i] })}
                  aria-expanded={!!expanded[i]}
                  aria-label={`${expanded[i] ? '收合' : '展開'} ${e.code} 詳細資料`}
                >
                  <span class:open={expanded[i]}><Icon name="chevron" size={13} /></span>
                  <span>{expanded[i] ? '收合' : '展開'}</span>
                </button>
              {:else}
                <span class="muted">—</span>
              {/if}
            </td>
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
  .filters {
    margin-bottom: 16px;
  }
  .filter-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
    margin-bottom: 15px;
    padding-bottom: 12px;
    border-bottom: 1px solid var(--line);
  }
  .filter-code {
    margin-bottom: 2px;
    color: var(--fg-3);
    font-size: 8px;
    font-weight: 700;
    letter-spacing: 0.12em;
  }
  .actions button {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .fgrid {
    display: grid;
    grid-template-columns: 1.15fr 1.15fr repeat(2, minmax(160px, 1fr)) 96px;
    gap: 11px;
  }
  .fgrid .field {
    margin-bottom: 0;
  }
  .limit {
    max-width: 120px;
  }
  .sev {
    display: inline-flex;
    align-items: center;
    gap: 7px;
    font-family: var(--font-mono);
    font-size: 9px;
    font-weight: 700;
    letter-spacing: 0.07em;
    text-transform: uppercase;
  }
  .sev i {
    display: block;
    width: 3px;
    height: 12px;
    background: currentColor;
  }
  .sev.ok { color: var(--ok); }
  .sev.busy { color: var(--busy); }
  .sev.err { color: var(--err); }
  .time { color: var(--fg-1); font-size: 11px; }
  .detail-cell { width: 92px; }
  .expand { display: inline-flex; align-items: center; gap: 5px; color: var(--fg-2); }
  .expand > span:first-child { display: grid; transition: transform 120ms ease; }
  .expand > span:first-child.open { transform: rotate(90deg); }
  .detail-row td {
    padding: 0;
    background: var(--bg-inset);
  }
  pre {
    margin: 0;
    padding: 14px 16px;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-word;
    color: var(--fg-1);
  }
  .event-empty { min-height: 270px; }
  @media (max-width: 1180px) {
    .fgrid { grid-template-columns: repeat(2, minmax(160px, 1fr)); }
    .limit { max-width: none; }
  }
  @media (max-width: 620px) {
    .filter-head { align-items: flex-start; flex-direction: column; }
    .actions { width: 100%; }
    .actions button { flex: 1; justify-content: center; }
    .fgrid { grid-template-columns: 1fr; }
  }
</style>
