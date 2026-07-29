<script lang="ts">
  // 概覽分頁(R6):CPU/RAM 歷史趨勢(回填+即時)、連接埠清單、磁碟用量、快速操作。
  import type { main } from '../../../../wailsjs/go/models';
  import { BackupNow, InstanceDiskUsage } from '../../../../wailsjs/go/main/App';
  import {
    metricSamples,
    acquireMetrics,
    releaseMetrics,
    noteRunning,
    cpuTrend,
    ramPercentTrend,
    type MetricSample,
  } from '../../stores/metrics';
  import { call } from '../../api';
  import { pushToast } from '../../stores/toasts';
  import { trackOperation } from '../../stores/operations';
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
    // 捕捉當輪 uuid:cleanup 讀反應式 uuid 會拿到「已前進」的新值(uuid 變動觸發 effect 重跑,
    // 先跑上輪 cleanup 再跑本輪 body),必須用區域副本才能釋放本輪 acquire 的同一實例,否則洩漏。
    const id = uuid;
    metricsLoaded = false;
    acquireMetrics(id);
    let firstEmit = true;
    const unsub = metricSamples(id).subscribe((v) => {
      samples = v;
      if (!firstEmit) metricsLoaded = true;
      firstEmit = false;
    });
    const loadTimer = setTimeout(() => {
      metricsLoaded = true;
    }, METRICS_LOAD_TIMEOUT_MS);
    return () => {
      unsub();
      releaseMetrics(id);
      clearTimeout(loadTimer);
    };
  });

  // ---- B10:停止再啟動同實例後即時指標靜默——轉態基準交給 metrics store(entry.lastRunning)
  // 判定,不用元件實體變數(後者在切分頁卸載/掛載間會重置,無法偵測「卸載期間發生的轉態」,
  // 見 stores/metrics.ts noteRunning 註解)。
  $effect(() => {
    noteRunning(uuid, inst.observed_state === 'Running');
  });

  const cpuSeries = $derived<TrendSeries[]>([
    { label: 'CPU', colorVar: '--chart-cpu', points: cpuTrend(samples), unit: '%' },
  ]);
  const ramSeries = $derived<TrendSeries[]>([
    { label: 'RAM', colorVar: '--chart-ram', points: ramPercentTrend(samples), unit: '%' },
  ]);

  // 磁碟用量(資料 + 備份):以 InstanceDiskUsage(uuid) 查詢宿主佔用;uuid 變動時重載。
  // 查詢失敗(native-only 回不支援/無法採集)→ 保持 null,由 fmtBytes 顯「不適用」,不捏造。
  // 資料維度另以輪詢快照 data_disk_bytes 兜底(查詢尚未回或失敗時仍有近似值可顯)。
  let diskUsage = $state<main.DiskUsageDTO | null>(null);
  $effect(() => {
    const id = uuid;
    diskUsage = null;
    let cancelled = false;
    call(() => InstanceDiskUsage(id), { silent: true })
      .then((du) => {
        if (!cancelled) diskUsage = du;
      })
      .catch(() => {
        /* 保持 null;顯「不適用」/兜底快照 */
      });
    return () => {
      cancelled = true;
    };
  });
  const dataBytes = $derived(diskUsage ? diskUsage.data_bytes : (snapshot?.stats?.data_disk_bytes ?? null));
  const backupBytes = $derived(diskUsage ? diskUsage.backup_bytes : null);

  const ports = $derived(inst.ports ?? []);

  let backingUp = $state(false);
  async function backupNow(): Promise<void> {
    backingUp = true;
    try {
      // 全域操作面板登記一筆「備份」(name 由 uuid 於 instances store 解析)。
      const meta = await trackOperation({ uuid, kind: 'backup' }, () => call(() => BackupNow(uuid)));
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
      <div class="disk-rows">
        <div class="disk-row">
          <span class="disk-label">資料</span>
          <span class="disk-val">{fmtBytes(dataBytes)}</span>
        </div>
        <div class="disk-row">
          <span class="disk-label">備份</span>
          <span class="disk-val">{fmtBytes(backupBytes)}</span>
        </div>
      </div>
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
  .disk-rows {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .disk-row {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-3);
  }
  .disk-label {
    font-size: var(--text-sm);
    color: var(--fg-2);
  }
  .disk-val {
    font-size: var(--text-lg);
    font-weight: 700;
    color: var(--fg-0);
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
