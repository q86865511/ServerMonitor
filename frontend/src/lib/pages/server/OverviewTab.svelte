<script lang="ts">
  // 概覽分頁(R6):CPU/RAM 歷史趨勢(回填+即時)、連接埠清單、磁碟用量、快速操作。
  import type { main } from '../../../../wailsjs/go/models';
  import { BackupNow } from '../../../../wailsjs/go/main/App';
  import {
    metricSamples,
    acquireMetrics,
    releaseMetrics,
    cpuTrend,
    ramPercentTrend,
    type MetricSample,
  } from '../../stores/metrics';
  import { call } from '../../api';
  import { pushToast } from '../../stores/toasts';
  import { navigate } from '../../router';
  import { fmtBytes } from '../../format';
  import type { TrendSeries } from '../../ui/types';
  import Card from '../../ui/Card.svelte';
  import Button from '../../ui/Button.svelte';
  import TrendChart from '../../ui/TrendChart.svelte';
  import EmptyState from '../../ui/EmptyState.svelte';
  import Skeleton from '../../ui/Skeleton.svelte';

  const METRICS_LOAD_TIMEOUT_MS = 5000;

  let {
    uuid,
    inst,
    snapshot,
  }: {
    uuid: string;
    inst: main.InstanceDTO;
    snapshot: main.SnapshotDTO | null;
  } = $props();

  // ---- 指標訂閱(引用計數;uuid 變動時自動接手,unmount 釋放)----
  let samples = $state<MetricSample[]>([]);
  // 區分載入中/確定無資料(R15/R16),作法同 ui/ServerCard:store 第二次落值才視為 settled,
  // 保底逾時防離線/查詢失敗時卡在 Skeleton;uuid 變動時重置(切換伺服器不沿用舊 settled 狀態)。
  let metricsLoaded = $state(false);
  $effect(() => {
    metricsLoaded = false;
    acquireMetrics(uuid);
    let firstEmit = true;
    const unsub = metricSamples(uuid).subscribe((v) => {
      samples = v;
      if (!firstEmit) metricsLoaded = true;
      firstEmit = false;
    });
    const loadTimer = setTimeout(() => {
      metricsLoaded = true;
    }, METRICS_LOAD_TIMEOUT_MS);
    return () => {
      unsub();
      releaseMetrics(uuid);
      clearTimeout(loadTimer);
    };
  });

  const cpuSeries = $derived<TrendSeries[]>([
    { label: 'CPU', colorVar: '--chart-cpu', points: cpuTrend(samples), unit: '%' },
  ]);
  const ramSeries = $derived<TrendSeries[]>([
    { label: 'RAM', colorVar: '--chart-ram', points: ramPercentTrend(samples), unit: '%' },
  ]);

  // 磁碟用量:SnapshotDTO.stats.data_disk_bytes;無法採集 → fmtBytes 顯示「不適用」。
  const diskBytes = $derived(snapshot?.stats?.data_disk_bytes ?? null);

  const ports = $derived(inst.ports ?? []);

  let backingUp = $state(false);
  async function backupNow(): Promise<void> {
    backingUp = true;
    try {
      const meta = await call(() => BackupNow(uuid));
      pushToast('success', `已建立備份 ${meta.backup_id}`);
    } catch {
      /* toast 已呈現 */
    } finally {
      backingUp = false;
    }
  }

  function fmtIp(ip: string): string {
    return !ip || ip === '0.0.0.0' ? '全介面' : ip;
  }
</script>

<div class="overview">
  <div class="charts">
    <Card>
      <div class="chart-head">CPU 使用率</div>
      {#if !metricsLoaded}
        <Skeleton variant="block" height="180px" />
      {:else}
        <TrendChart series={cpuSeries} yMax={100} yUnit="%" height={180} />
      {/if}
    </Card>
    <Card>
      <div class="chart-head">記憶體使用率</div>
      {#if !metricsLoaded}
        <Skeleton variant="block" height="180px" />
      {:else}
        <TrendChart series={ramSeries} yMax={100} yUnit="%" height={180} />
      {/if}
    </Card>
  </div>

  <div class="grid">
    <Card>
      <div class="sec-head">連接埠</div>
      {#if ports.length === 0}
        <div class="na">尚無保留連接埠</div>
      {:else}
        <div class="ports">
          {#each ports as p (p.name + p.protocol + p.host_port)}
            <div class="port-row">
              <span class="p-name">{p.name || '(未命名)'}</span>
              <span class="p-addr mono">{fmtIp(p.bind_ip)}:{p.host_port}</span>
              <span class="p-proto">{(p.protocol || 'tcp').toUpperCase()}</span>
            </div>
          {/each}
        </div>
      {/if}
    </Card>

    <Card>
      <div class="sec-head">磁碟用量</div>
      <div class="disk-val">{fmtBytes(diskBytes)}</div>
      <div class="disk-sub">資料目錄佔用</div>
    </Card>

    <Card>
      <div class="sec-head">快速操作</div>
      <div class="quick">
        <Button variant="primary" loading={backingUp} onclick={backupNow}>建立備份</Button>
        <Button onclick={() => navigate(`/servers/${uuid}/console`)}>開啟主控台</Button>
      </div>
    </Card>
  </div>

  {#if metricsLoaded && samples.length === 0}
    <Card>
      <EmptyState
        title="尚無指標歷史"
        description="開始監控後,CPU/RAM 時序將於此累積(每 15 秒一點,保留 36 小時)。"
      />
    </Card>
  {/if}
</div>

<style>
  .overview {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .charts {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
  }
  .grid {
    display: grid;
    grid-template-columns: 2fr 1fr 1fr;
    gap: var(--space-4);
  }
  @media (max-width: 1100px) {
    .charts,
    .grid {
      grid-template-columns: 1fr;
    }
  }
  .chart-head,
  .sec-head {
    font-size: var(--text-sm);
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    margin-bottom: var(--space-3);
  }
  .ports {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .port-row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }
  .p-name {
    color: var(--fg-1);
    min-width: 88px;
  }
  .p-addr {
    color: var(--fg-0);
    flex: 1;
  }
  .p-proto {
    color: var(--fg-2);
    font-size: var(--text-xs);
  }
  .na {
    color: var(--fg-2);
    font-size: var(--text-sm);
  }
  .disk-val {
    font-size: var(--text-xl);
    font-weight: 700;
    color: var(--fg-0);
  }
  .disk-sub {
    font-size: var(--text-xs);
    color: var(--fg-2);
    margin-top: var(--space-1);
  }
  .quick {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
  }
  .mono {
    font-family: var(--font-mono);
  }
</style>
