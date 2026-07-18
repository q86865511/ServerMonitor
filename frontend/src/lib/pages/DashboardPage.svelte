<script lang="ts">
  // 總覽儀表板(R4):5 張統計卡+伺服器卡片網格(消費 ServerCard,R5)+全體 CPU/RAM 趨勢+
  // 最近事件時間軸+節點狀態。無實例時顯示 EmptyState 引導建立(建立入口在 Topbar,本頁不重複掛精靈)。
  //
  // 資料來源(對齊 design.md R4 對應表):
  //  - 總數/運行中:既有 instances store(App.svelte 已掛 5s 輪詢,本頁只讀不重複啟停)。
  //  - 線上玩家總和:本頁批次輪詢 GetSnapshot(10s),player_count nil 的實例加總時跳過,
  //    全部為 nil(或尚未載入)顯「不適用」而非 0。
  //  - 平均 CPU/RAM 與趨勢圖:同一次 QueryMetricsSummary(now-24h)結果,平均卡取最新一筆非 null
  //    bucket 值——與趨勢圖同一資料來源,不另外聚合各卡片的即時樣本。
  //  - 最近事件:QueryEvents(限 20 筆),依時間新到舊排序。
  import { onDestroy, onMount } from 'svelte';
  import { get } from 'svelte/store';
  import type { main } from '../../../wailsjs/go/models';
  import { GetSnapshot, ListTemplates, QueryEvents } from '../../../wailsjs/go/main/App';
  import { call } from '../api';
  import { fmtCPU, fmtPercent, fmtTime, severityTone } from '../format';
  import { instances, nodeStatuses } from '../stores/instances';
  import { cpuTrend, ramPercentTrend, summary, type MetricSample } from '../stores/metrics';
  import Badge from '../ui/Badge.svelte';
  import Card from '../ui/Card.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
  import MetricCard from '../ui/MetricCard.svelte';
  import ServerCard from '../ui/ServerCard.svelte';
  import Skeleton from '../ui/Skeleton.svelte';
  import TrendChart from '../ui/TrendChart.svelte';
  import type { TrendSeries } from '../ui/types';

  const SNAPSHOT_POLL_MS = 10_000;
  const SUMMARY_POLL_MS = 15_000;
  const TREND_WINDOW_MS = 24 * 60 * 60 * 1000;
  const EVENTS_LIMIT = 20;

  // ---- 範本(供卡片圖示/名稱/位址規則,R5) ----
  let templates = $state<main.TemplateDTO[]>([]);
  let templatesLoading = $state(true);
  const templateMap = $derived.by(() => {
    const m = new Map<string, main.TemplateDTO>();
    for (const t of templates) m.set(t.id, t);
    return m;
  });

  async function loadTemplates(): Promise<void> {
    try {
      templates = await call(() => ListTemplates(), { silent: true });
    } catch {
      templates = [];
    } finally {
      templatesLoading = false;
    }
  }

  // ---- 快照批次輪詢:線上玩家總和(player_count nil 跳過,R4/R15) ----
  let snapshots = $state<Record<string, main.SnapshotDTO | null>>({});
  let snapshotsLoaded = $state(false);
  let snapshotTimer: ReturnType<typeof setInterval> | null = null;

  async function pollSnapshots(): Promise<void> {
    const list = get(instances);
    const results = await Promise.all(
      list.map(async (i): Promise<[string, main.SnapshotDTO | null]> => {
        try {
          return [i.uuid, await GetSnapshot(i.uuid)];
        } catch {
          return [i.uuid, null];
        }
      }),
    );
    const map: Record<string, main.SnapshotDTO | null> = {};
    for (const [uuid, s] of results) map[uuid] = s;
    snapshots = map;
    snapshotsLoaded = true;
  }

  const playerAgg = $derived.by(() => {
    let sum = 0;
    let hasAny = false;
    for (const i of $instances) {
      const s = snapshots[i.uuid];
      if (s && s.player_count != null) {
        sum += s.player_count;
        hasAny = true;
      }
    }
    return { sum, hasAny };
  });

  // ---- 全體 CPU/RAM 趨勢 + 平均統計卡(同一次 QueryMetricsSummary,「聚合自相同資料來源」) ----
  let summarySamples = $state<MetricSample[]>([]);
  let summaryLoaded = $state(false);
  let summaryTimer: ReturnType<typeof setInterval> | null = null;

  /** 由末端往回找最近一筆非 null 值,避免單一 bucket 暫時無樣本時卡片閃爍「不適用」。 */
  function lastNonNull(list: MetricSample[], pick: (s: MetricSample) => number | null): number | null {
    for (let i = list.length - 1; i >= 0; i--) {
      const v = pick(list[i]);
      if (v != null) return v;
    }
    return null;
  }

  async function refreshSummary(): Promise<void> {
    try {
      const since = Math.floor((Date.now() - TREND_WINDOW_MS) / 1000);
      summarySamples = await summary(since);
    } catch {
      /* 靜默:趨勢圖與平均卡顯空態/不適用 */
    } finally {
      summaryLoaded = true;
    }
  }

  const avgCpu = $derived(lastNonNull(summarySamples, (s) => s.cpu));
  const avgRamPercent = $derived(
    lastNonNull(summarySamples, (s) =>
      s.memoryBytes != null && s.memoryLimit != null && s.memoryLimit > 0
        ? (s.memoryBytes / s.memoryLimit) * 100
        : null,
    ),
  );
  const trendSeries = $derived<TrendSeries[]>([
    { label: 'CPU', colorVar: '--chart-cpu', points: cpuTrend(summarySamples), unit: '%' },
    { label: 'RAM', colorVar: '--chart-ram', points: ramPercentTrend(summarySamples), unit: '%' },
  ]);

  // ---- 最近事件(限 20 筆,新到舊,R4) ----
  let events = $state<main.EventDTO[]>([]);
  let eventsLoading = $state(true);
  let eventsTimer: ReturnType<typeof setInterval> | null = null;

  async function loadEvents(): Promise<void> {
    try {
      const list = await call(
        () =>
          QueryEvents({
            instance_uuid: '',
            code: '',
            since_unix: 0,
            until_unix: 0,
            limit: EVENTS_LIMIT,
          }),
        { silent: true },
      );
      events = [...list]
        .sort((a, b) => Date.parse(b.ts_utc) - Date.parse(a.ts_utc))
        .slice(0, EVENTS_LIMIT);
    } catch {
      /* 靜默:輪詢失敗保留舊清單 */
    } finally {
      eventsLoading = false;
    }
  }

  onMount(() => {
    loadTemplates();
    pollSnapshots();
    snapshotTimer = setInterval(pollSnapshots, SNAPSHOT_POLL_MS);
    refreshSummary();
    summaryTimer = setInterval(refreshSummary, SUMMARY_POLL_MS);
    loadEvents();
    eventsTimer = setInterval(loadEvents, SUMMARY_POLL_MS);
  });

  onDestroy(() => {
    if (snapshotTimer) clearInterval(snapshotTimer);
    if (summaryTimer) clearInterval(summaryTimer);
    if (eventsTimer) clearInterval(eventsTimer);
  });

  const runningCount = $derived($instances.filter((i) => i.observed_state === 'Running').length);
  const playerValue = $derived<string | number>(
    !snapshotsLoaded ? '…' : playerAgg.hasAny ? playerAgg.sum : '不適用',
  );
</script>

{#if templatesLoading}
  <Card>
    <Skeleton variant="block" height="140px" />
  </Card>
{:else if $instances.length === 0}
  <Card>
    <EmptyState
      title="尚無任何伺服器"
      description="點擊右上角「新增伺服器」建立第一台伺服器;建立後這裡會顯示總覽統計、卡片與趨勢圖。"
    />
  </Card>
{:else}
  <div class="stat-grid">
    <MetricCard title="伺服器總數" value={$instances.length} />
    <MetricCard title="運行中" value={runningCount} tone={runningCount > 0 ? 'ok' : 'neutral'} />
    <MetricCard title="線上玩家總和" value={playerValue} />
    <MetricCard title="平均 CPU" value={summaryLoaded ? fmtCPU(avgCpu) : '…'} />
    <MetricCard title="平均 RAM" value={summaryLoaded ? fmtPercent(avgRamPercent) : '…'} />
  </div>

  <section class="block">
    <div class="section-title">伺服器</div>
    <div class="server-grid">
      {#each $instances as inst (inst.uuid)}
        <ServerCard
          {inst}
          template={templateMap.get(inst.template_id)}
          externalSnapshot={snapshots[inst.uuid] ?? null}
        />
      {/each}
    </div>
  </section>

  <div class="block">
    <Card>
      <div class="section-title">全體 CPU / RAM 趨勢(近 24 小時)</div>
      <TrendChart series={trendSeries} yMax={100} yUnit="%" emptyText="尚無歷史指標資料" />
    </Card>
  </div>

  <div class="lower-grid">
    <Card>
      <div class="section-title">最近事件</div>
      {#if eventsLoading}
        <Skeleton variant="line" count={4} />
      {:else if events.length === 0}
        <EmptyState title="尚無事件紀錄" />
      {:else}
        <ul class="timeline">
          {#each events as ev (ev.ts_utc + ev.code + ev.instance_uuid)}
            <li class="timeline-item">
              <Badge tone={severityTone(ev.severity)}>{ev.severity || '未知'}</Badge>
              <span class="ev-code">{ev.code}</span>
              <span class="ev-time">{fmtTime(ev.ts_utc)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </Card>

    <Card>
      <div class="section-title">節點狀態</div>
      {#if $nodeStatuses.length === 0}
        <EmptyState title="尚無節點資訊" />
      {:else}
        <ul class="node-list">
          {#each $nodeStatuses as n (n.node)}
            <li class="node-item">
              <span class="node-name">{n.node}</span>
              <Badge tone={n.online ? 'ok' : 'err'}>{n.online ? '在線' : '離線'}</Badge>
              <Badge tone={n.docker_available ? 'ok' : 'idle'}>
                {n.docker_available ? 'Docker 可用' : 'Docker 不可用'}
              </Badge>
            </li>
          {/each}
        </ul>
      {/if}
    </Card>
  </div>
{/if}

<style>
  .stat-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: var(--space-4);
    margin-bottom: var(--space-5);
  }
  .block {
    margin-bottom: var(--space-5);
  }
  .section-title {
    font-size: var(--text-md);
    font-weight: 600;
    color: var(--fg-0);
    margin-bottom: var(--space-3);
  }
  .server-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
    gap: var(--space-4);
  }
  .lower-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
  }
  @media (max-width: 900px) {
    .lower-grid {
      grid-template-columns: 1fr;
    }
  }
  .timeline,
  .node-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .timeline-item,
  .node-item {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }
  .ev-code {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    color: var(--fg-0);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ev-time {
    flex: none;
    color: var(--fg-2);
    font-size: var(--text-xs);
  }
  .node-name {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
