<script lang="ts">
  // 伺服器卡片(R5):由 InstanceDTO+TemplateDTO+SnapshotDTO 動態產生,一切由範本欄位驅動,
  // 不得以 template_id/遊戲名分支呈現邏輯。範本圖走 /tpl-icons/{id},onerror 換佔位(見 lib/tplicon.ts,
  // 該檔由並行任務 T12 建立,本卡消費既有 export,不重複定義)。
  //
  // 資料來源:
  //  - 玩家數/線上/運行時間:本卡自行輪詢 GetSnapshot(10s;僅 binding 輪詢,不自行 EventsOn)。
  //  - CPU/RAM:掛 stores/metrics 既有引用計數訂閱(acquireMetrics/releaseMetrics),取 ring 最新點。
  //  - 位址:依 R12 規則——範本第一個 required 埠(依 name 對照)之 host_port;
  //    無 required 或 name 對不上取實例埠清單首項;無保留埠顯「—」。
  import { onDestroy, onMount } from 'svelte';
  import type { main } from '../../../wailsjs/go/models';
  import {
    GetSnapshot,
    RemoveInstance,
    RestartInstance,
    StartInstance,
    StopInstance,
  } from '../../../wailsjs/go/main/App';
  import { call } from '../api';
  import { fmtPlayers, fmtUptime } from '../format';
  import { navigate } from '../router';
  import { refresh } from '../stores/instances';
  import { acquireMetrics, metricSamples, releaseMetrics, type MetricSample } from '../stores/metrics';
  import { pushToast } from '../stores/toasts';
  import { tplIconColor, tplIconInitial, tplIconSrc } from '../tplicon';
  import Badge from './Badge.svelte';
  import Button from './Button.svelte';
  import Card from './Card.svelte';
  import ConfirmDialog from './ConfirmDialog.svelte';
  import Dropdown from './Dropdown.svelte';
  import IconButton from './IconButton.svelte';
  import ProgressBar from './ProgressBar.svelte';
  import Skeleton from './Skeleton.svelte';
  import StatusBadge from './StatusBadge.svelte';

  const SNAPSHOT_POLL_MS = 10_000;
  const CLOCK_TICK_MS = 1000;
  const METRICS_LOAD_TIMEOUT_MS = 5000;

  let {
    inst,
    template,
    externalSnapshot,
  }: {
    inst: main.InstanceDTO;
    /** 對應範本資料;未找到(範本剛被移除等)時仍可渲染,圖示/名稱退化為 fallback。 */
    template: main.TemplateDTO | undefined;
    /**
     * 呼叫端已持有的快照(如總覽頁的批次輪詢)。有提供(非 undefined)時本卡不自行輪詢
     * GetSnapshot,避免同一實例雙重輪詢;undefined(如伺服器清單頁)才自輪詢。
     */
    externalSnapshot?: main.SnapshotDTO | null;
  } = $props();

  const displayName = $derived(
    inst.name && inst.name.trim() !== ''
      ? inst.name
      : `${template?.name ?? inst.template_id} #${inst.uuid.slice(0, 8)}`,
  );

  const runtimeLabel = $derived(
    inst.runtime === 'native' ? 'Native' : inst.runtime === 'docker' ? 'Docker' : inst.runtime || '—',
  );

  // ---- 範本圖示(有 icon 才嘗試載入,否則直接佔位;載入失敗換佔位) ----
  let iconFailed = $state(false);
  const showPlaceholder = $derived(!template?.has_icon || iconFailed);

  // ---- 位址:R12 規則,範本能力驅動,不以 template_id 特判 ----
  function resolveAddress(i: main.InstanceDTO, t: main.TemplateDTO | undefined): string {
    const ports = i.ports ?? [];
    if (ports.length === 0) return '—';
    let target = ports[0];
    const requiredSpec = (t?.ports ?? []).find((p) => p.required);
    if (requiredSpec) {
      const matched = ports.find((p) => p.name === requiredSpec.name);
      if (matched) target = matched;
    }
    const host = target.bind_ip && target.bind_ip !== '0.0.0.0' ? target.bind_ip : 'localhost';
    return `${host}:${target.host_port}`;
  }
  const address = $derived(resolveAddress(inst, template));

  // ---- 快照(玩家數/線上/運行時間):外部提供則直用,否則自輪詢 ----
  let polledSnapshot = $state<main.SnapshotDTO | null>(null);
  let snapshotTimer: ReturnType<typeof setInterval> | null = null;
  const snapshot = $derived(externalSnapshot !== undefined ? externalSnapshot : polledSnapshot);

  async function pollSnapshot(): Promise<void> {
    try {
      polledSnapshot = await GetSnapshot(inst.uuid);
    } catch {
      /* 靜默:輪詢失敗保留舊值,uptime/玩家數暫顯既有或「—」 */
    }
  }

  // ---- 本地時鐘(驅動運行時間遞增顯示,不倚賴輪詢節奏) ----
  let now = $state(Date.now());
  let clockTimer: ReturnType<typeof setInterval> | null = null;
  const uptimeText = $derived(fmtUptime(snapshot?.started_at, now));

  // ---- CPU/RAM:掛既有 metrics store 引用計數訂閱 ----
  // uuid 取自 props 初值即可:卡片由呼叫端以 uuid 作為 each key,同一卡片實例存續期間 inst.uuid 不變。
  let samples = $state<MetricSample[]>([]);
  let unsubscribeSamples: (() => void) | null = null;
  // 區分「載入中」與「確定無資料」(R15/R16):停機或從未監控的實例,回填會以空陣列落地,
  // 不能永遠停在 Skeleton。store 首次 subscribe 回呼是訂閱當下的既有值,第二次落值才代表
  // 本次 acquireMetrics 觸發的回填/即時樣本已到位,視為 settled;保底逾時防離線/查詢失敗卡死。
  let metricsLoaded = $state(false);
  let metricsLoadTimer: ReturnType<typeof setTimeout> | null = null;

  const latestSample = $derived<MetricSample | null>(samples.length > 0 ? samples[samples.length - 1] : null);
  const ramPercent = $derived(
    latestSample && latestSample.memoryBytes != null && latestSample.memoryLimit != null && latestSample.memoryLimit > 0
      ? (latestSample.memoryBytes / latestSample.memoryLimit) * 100
      : null,
  );

  onMount(() => {
    if (externalSnapshot === undefined) {
      pollSnapshot();
      snapshotTimer = setInterval(pollSnapshot, SNAPSHOT_POLL_MS);
    }
    clockTimer = setInterval(() => {
      now = Date.now();
    }, CLOCK_TICK_MS);
    acquireMetrics(inst.uuid);
    let firstEmit = true;
    unsubscribeSamples = metricSamples(inst.uuid).subscribe((v) => {
      samples = v;
      if (!firstEmit) metricsLoaded = true;
      firstEmit = false;
    });
    metricsLoadTimer = setTimeout(() => {
      metricsLoaded = true;
    }, METRICS_LOAD_TIMEOUT_MS);
  });

  onDestroy(() => {
    if (snapshotTimer) clearInterval(snapshotTimer);
    if (clockTimer) clearInterval(clockTimer);
    if (metricsLoadTimer) clearTimeout(metricsLoadTimer);
    if (unsubscribeSamples) unsubscribeSamples();
    releaseMetrics(inst.uuid);
  });

  // ---- 操作:per-card in-flight 鎖 + 成功/失敗 toast ----
  type Busy = '' | 'start' | 'stop' | 'restart' | 'remove';
  let busy = $state<Busy>('');
  let confirmRemove = $state(false);
  let purge = $state(false);

  const running = $derived(inst.observed_state === 'Running');

  async function act(name: Exclude<Busy, '' | 'remove'>, fn: () => Promise<void>, successMsg: string): Promise<void> {
    if (busy) return;
    busy = name;
    try {
      await call(fn);
      pushToast('success', successMsg);
      refresh(false);
    } catch {
      /* 錯誤 toast 已由 call() 呈現 */
    } finally {
      busy = '';
    }
  }

  function openRemove(): void {
    purge = false;
    confirmRemove = true;
  }

  async function doRemove(): Promise<void> {
    busy = 'remove';
    try {
      await call(() => RemoveInstance(inst.uuid, purge));
      pushToast('success', `已移除 ${displayName}`);
      confirmRemove = false;
      refresh(false);
    } catch {
      /* 錯誤 toast 已由 call() 呈現 */
    } finally {
      busy = '';
    }
  }

  function goManage(): void {
    navigate(`/servers/${inst.uuid}`);
  }
  function goConsole(close: () => void): void {
    close();
    navigate(`/servers/${inst.uuid}/console`);
  }
</script>

<Card>
  <div class="server-card">
    <div class="head">
      <div class="icon-wrap">
        {#if showPlaceholder}
          <div class="placeholder" style={`background:${tplIconColor(inst.template_id)}`}>
            {tplIconInitial(template?.name ?? '', inst.template_id)}
          </div>
        {:else}
          <img
            class="icon"
            src={tplIconSrc(inst.template_id)}
            alt=""
            onerror={() => (iconFailed = true)}
          />
        {/if}
      </div>
      <div class="ident">
        <div class="name" title={displayName}>{displayName}</div>
        <div class="sub">
          {template?.name ?? inst.template_id}{inst.variant ? ` · ${inst.variant}` : ''} · {inst.node}
        </div>
      </div>
      <Badge tone="neutral">{runtimeLabel}</Badge>
    </div>

    <div class="status-row">
      <StatusBadge state={inst.observed_state} desired={inst.desired_state} />
    </div>

    <div class="stats">
      <div class="stat">
        <span class="stat-k">位址</span>
        <span class="stat-v mono">{address}</span>
      </div>
      <div class="stat">
        <span class="stat-k">玩家</span>
        <span class="stat-v">{fmtPlayers(snapshot?.player_count)}</span>
      </div>
      <div class="stat">
        <span class="stat-k">運行時間</span>
        <span class="stat-v">{uptimeText}</span>
      </div>
    </div>

    <div class="metrics">
      {#if !metricsLoaded}
        <Skeleton variant="line" width="100%" count={2} />
      {:else}
        {#if latestSample?.cpu != null}
          <ProgressBar value={latestSample.cpu} label="CPU" />
        {:else}
          <div class="metric-na"><span>CPU</span><span>—</span></div>
        {/if}
        {#if ramPercent != null}
          <ProgressBar value={ramPercent} label="RAM" />
        {:else}
          <div class="metric-na"><span>RAM</span><span>—</span></div>
        {/if}
      {/if}
    </div>

    <div class="actions">
      <Button size="sm" onclick={goManage} disabled={!!busy}>管理</Button>
      {#if running}
        <Button
          size="sm"
          disabled={!!busy}
          loading={busy === 'stop'}
          onclick={() => act('stop', () => StopInstance(inst.uuid), `已停止 ${displayName}`)}
        >
          停止
        </Button>
        <Button
          size="sm"
          disabled={!!busy}
          loading={busy === 'restart'}
          onclick={() => act('restart', () => RestartInstance(inst.uuid), `已重啟 ${displayName}`)}
        >
          重啟
        </Button>
      {:else}
        <Button
          size="sm"
          variant="primary"
          disabled={!!busy}
          loading={busy === 'start'}
          onclick={() => act('start', () => StartInstance(inst.uuid), `已啟動 ${displayName}`)}
        >
          啟動
        </Button>
      {/if}
      <Dropdown align="right">
        {#snippet trigger({ toggle })}
          <IconButton label="更多操作" size="sm" disabled={!!busy} onclick={toggle}>⋯</IconButton>
        {/snippet}
        {#snippet children({ close })}
          <button type="button" class="menu-item" onclick={() => goConsole(close)}>主控台</button>
          <button
            type="button"
            class="menu-item danger"
            onclick={() => {
              close();
              openRemove();
            }}
          >
            移除…
          </button>
        {/snippet}
      </Dropdown>
    </div>
  </div>
</Card>

{#if confirmRemove}
  <ConfirmDialog
    title="移除伺服器"
    message={`確定移除「${displayName}」?`}
    confirmLabel={purge ? '移除並清除資料' : '僅移除'}
    danger
    busy={busy === 'remove'}
    onConfirm={doRemove}
    onCancel={() => (confirmRemove = false)}
  >
    {#snippet children()}
      <label class="purge-row">
        <input type="checkbox" bind:checked={purge} disabled={busy === 'remove'} />
        同時刪除伺服器資料與備份(data 與 backups,無法復原)
      </label>
    {/snippet}
  </ConfirmDialog>
{/if}

<style>
  .server-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .head {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .icon-wrap {
    flex: none;
    width: 44px;
    height: 44px;
  }
  .icon,
  .placeholder {
    width: 44px;
    height: 44px;
    border-radius: var(--radius-sm);
    object-fit: cover;
  }
  .placeholder {
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--fg-on-accent);
    font-size: var(--text-lg);
    font-weight: 700;
  }
  .ident {
    flex: 1;
    min-width: 0;
  }
  .name {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-0);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .sub {
    font-size: var(--text-xs);
    color: var(--fg-2);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .status-row {
    display: flex;
  }
  .stats {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: var(--space-2);
    padding: var(--space-2) 0;
    border-top: 1px solid var(--line);
    border-bottom: 1px solid var(--line);
  }
  .stat {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .stat-k {
    font-size: var(--text-xs);
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .stat-v {
    font-size: var(--text-sm);
    color: var(--fg-0);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .metrics {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .metric-na {
    display: flex;
    justify-content: space-between;
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .menu-item {
    display: block;
    width: 100%;
    text-align: left;
    background: transparent;
    border: none;
    color: var(--fg-0);
    font: inherit;
    font-size: var(--text-sm);
    padding: 7px 10px;
    border-radius: var(--radius-sm);
    cursor: pointer;
  }
  .menu-item:hover {
    background-color: var(--bg-3);
  }
  .menu-item.danger {
    color: var(--err);
  }
  .menu-item.danger:hover {
    background-color: var(--err-bg);
  }
  .purge-row {
    display: flex;
    align-items: flex-start;
    gap: 8px;
    font-size: var(--text-sm);
    color: var(--fg-1);
    cursor: pointer;
  }
  .purge-row input {
    margin-top: 2px;
  }
</style>
