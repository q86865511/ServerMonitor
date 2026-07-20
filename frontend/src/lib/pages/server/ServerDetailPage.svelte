<script lang="ts">
  // 伺服器詳細頁(R6/R7/R11):頁首(名稱/範本/節點/runtime/玩家/運行時間/狀態/啟停移除)+
  // 六分頁(概覽/主控台/備份/檔案/排程/設定)。分頁由 hash 路由 :tab 驅動,切換不重建整頁。
  // 實例被外部移除(輪詢後 byUuid 消失)→ 錯誤態 + 釋放子分頁訂閱(子元件 unmount 自行 release)。
  import { onMount } from 'svelte';
  import type { main } from '../../../../wailsjs/go/models';
  import {
    GetSnapshot,
    StartInstance,
    StopInstance,
    RestartInstance,
    RemoveInstance,
    ListTemplates,
  } from '../../../../wailsjs/go/main/App';
  import { route, navigate } from '../../router';
  import { instances, refresh } from '../../stores/instances';
  import { trackOperation, type OpKind } from '../../stores/operations';
  import { call } from '../../api';
  import { pushToast } from '../../stores/toasts';
  import { fmtPlayers } from '../../format';
  import Card from '../../ui/Card.svelte';
  import Button from '../../ui/Button.svelte';
  import Badge from '../../ui/Badge.svelte';
  import Tabs from '../../ui/Tabs.svelte';
  import type { TabItem } from '../../ui/Tabs.svelte';
  import StatusBadge from '../../ui/StatusBadge.svelte';
  import Dropdown from '../../ui/Dropdown.svelte';
  import ConfirmDialog from '../../ui/ConfirmDialog.svelte';
  import ErrorState from '../../ui/ErrorState.svelte';
  import Skeleton from '../../ui/Skeleton.svelte';
  import OverviewTab from './OverviewTab.svelte';
  import ConsoleTab from './ConsoleTab.svelte';
  import BackupsPanel from './BackupsPanel.svelte';
  import FilesTab from './FilesTab.svelte';
  import SchedulesPanel from './SchedulesPanel.svelte';
  import SettingsTab from './SettingsTab.svelte';

  const uuid = $derived($route.params.uuid ?? '');
  const tab = $derived($route.params.tab ?? 'overview');

  // ---- 實例存在性(權威來源:instances 輪詢)----
  const inst = $derived($instances.find((i) => i.uuid === uuid));
  let gracePassed = $state(false); // 避免直接進入(instances 尚未載入)時誤判消失
  const missing = $derived(gracePassed && !inst);

  // ---- 範本(供顯示名 fallback 與設定分頁)----
  let templates = $state<main.TemplateDTO[]>([]);
  const template = $derived(inst ? (templates.find((t) => t.id === inst.template_id) ?? null) : null);
  const templateName = $derived(template?.name || inst?.template_id || '');
  const displayName = $derived(
    inst?.name?.trim() ? inst.name : `${templateName || '實例'} #${uuid.slice(0, 8)}`,
  );

  onMount(() => {
    refresh();
    ListTemplates()
      .then((t) => (templates = t))
      .catch(() => {
        /* 佔位圖 / id fallback 兜底 */
      });
    const t = setTimeout(() => (gracePassed = true), 2000);
    return () => clearTimeout(t);
  });

  // ---- 快照輪詢(R11:started_at/玩家/磁碟;5s,實例消失即停)----
  let snapshot = $state<main.SnapshotDTO | null>(null);
  let snapUuid = '';
  $effect(() => {
    const id = uuid;
    if (id !== snapUuid) {
      snapUuid = id;
      snapshot = null;
    }
    if (!id || missing) return;
    let alive = true;
    const tick = async (): Promise<void> => {
      try {
        const s = await GetSnapshot(id);
        if (alive && id === uuid) snapshot = s;
      } catch {
        /* 節點離線 / 未監控時靜默 */
      }
    };
    tick();
    const timer = setInterval(tick, 5000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  });

  // ---- 本地每秒 tick(運行時間遞增)----
  let nowTick = $state(Date.now());
  $effect(() => {
    const t = setInterval(() => (nowTick = Date.now()), 1000);
    return () => clearInterval(t);
  });

  function fmtUptime(ms: number): string {
    let s = Math.floor(Math.max(0, ms) / 1000);
    const d = Math.floor(s / 86400);
    s -= d * 86400;
    const h = Math.floor(s / 3600);
    s -= h * 3600;
    const m = Math.floor(s / 60);
    s -= m * 60;
    const p = (n: number): string => String(n).padStart(2, '0');
    return d > 0 ? `${d}天 ${p(h)}:${p(m)}:${p(s)}` : `${p(h)}:${p(m)}:${p(s)}`;
  }

  const startedMs = $derived(snapshot?.started_at ? Date.parse(snapshot.started_at) : NaN);
  const uptime = $derived(Number.isNaN(startedMs) ? '—' : fmtUptime(nowTick - startedMs));
  const players = $derived(fmtPlayers(snapshot?.player_count));
  const running = $derived(inst?.observed_state === 'Running');

  // ---- 頁首操作(in-flight 鎖 + toast)----
  let busyAction = $state('');
  async function act(name: OpKind, fn: () => Promise<void>, ok: string): Promise<void> {
    if (busyAction) return;
    busyAction = name;
    try {
      // 全域操作面板登記一筆(進度由 provision:<uuid> 事件豐富);call() 仍負責失敗 toast。
      await trackOperation({ uuid, name: displayName, kind: name }, () => call(fn));
      pushToast('success', ok);
      await refresh();
    } catch {
      /* toast 已呈現 */
    } finally {
      busyAction = '';
    }
  }

  // ---- 移除(purge 勾選 → 成功導回清單)----
  let confirmRemove = $state(false);
  let purge = $state(false);
  let removing = $state(false);
  function openRemove(): void {
    purge = false;
    confirmRemove = true;
  }
  async function doRemove(): Promise<void> {
    removing = true;
    try {
      await call(() => RemoveInstance(uuid, purge));
      pushToast('success', `已移除實例 ${displayName}`);
      confirmRemove = false;
      await refresh();
      navigate('/servers');
    } catch {
      /* toast 已呈現 */
    } finally {
      removing = false;
    }
  }

  const tabItems: TabItem[] = [
    { id: 'overview', label: '概覽' },
    { id: 'console', label: '主控台' },
    { id: 'backups', label: '備份' },
    { id: 'files', label: '檔案' },
    { id: 'schedules', label: '排程' },
    { id: 'settings', label: '設定' },
  ];
  function onTab(id: string): void {
    navigate(`/servers/${uuid}/${id}`);
  }
</script>

{#if missing}
  <Card>
    <ErrorState
      message={`找不到伺服器 ${uuid.slice(0, 8)}…\n它可能已被移除或節點離線。`}
      retryLabel="返回伺服器清單"
      onRetry={() => navigate('/servers')}
    />
  </Card>
{:else if !inst}
  <div class="loading">
    <Skeleton height="72px" />
    <Skeleton height="40px" />
    <Skeleton variant="block" height="220px" />
  </div>
{:else}
  <div class="detail">
    <header class="hd">
      <div class="left">
        <button class="back" type="button" onclick={() => navigate('/servers')} aria-label="返回伺服器清單">←</button>
        <div class="ident">
          <h1 class="name">{displayName}</h1>
          <div class="meta">
            <Badge tone="neutral">{templateName || inst.template_id}</Badge>
            <span class="dot">·</span>
            <span class="node">節點 {inst.node || 'local'}</span>
            <span class="dot">·</span>
            <Badge tone={inst.runtime === 'docker' ? 'accent' : 'off'}>{inst.runtime || '未知'}</Badge>
          </div>
        </div>
      </div>

      <div class="mid">
        <StatusBadge state={inst.observed_state} desired={inst.desired_state} />
        <div class="kpi"><span class="k-lbl">玩家</span><span class="k-val">{players}</span></div>
        <div class="kpi"><span class="k-lbl">運行時間</span><span class="k-val mono">{uptime}</span></div>
      </div>

      <div class="actions">
        <Button
          variant="primary"
          loading={busyAction === 'start'}
          disabled={running || busyAction !== ''}
          onclick={() => act('start', () => StartInstance(uuid), '已送出啟動')}
        >
          啟動
        </Button>
        <Button
          loading={busyAction === 'stop'}
          disabled={!running || busyAction !== ''}
          onclick={() => act('stop', () => StopInstance(uuid), '已送出停止')}
        >
          停止
        </Button>
        <Button
          loading={busyAction === 'restart'}
          disabled={!running || busyAction !== ''}
          onclick={() => act('restart', () => RestartInstance(uuid), '已送出重啟')}
        >
          重啟
        </Button>
        <Dropdown align="right">
          {#snippet trigger({ toggle })}
            <Button onclick={toggle}>更多 ▾</Button>
          {/snippet}
          {#snippet children({ close })}
            <button
              type="button"
              class="menu-item danger"
              onclick={() => {
                close();
                openRemove();
              }}
            >
              移除實例…
            </button>
          {/snippet}
        </Dropdown>
      </div>
    </header>

    <Tabs items={tabItems} active={tab} onChange={onTab} />

    <div class="tab-body">
      {#if tab === 'overview'}
        <OverviewTab {uuid} {inst} {snapshot} />
      {:else if tab === 'console'}
        <ConsoleTab {uuid} {snapshot} {running} />
      {:else if tab === 'backups'}
        <Card><BackupsPanel {uuid} /></Card>
      {:else if tab === 'files'}
        <Card><FilesTab {uuid} /></Card>
      {:else if tab === 'schedules'}
        <Card><SchedulesPanel {uuid} /></Card>
      {:else if tab === 'settings'}
        <SettingsTab {inst} {template} />
      {/if}
    </div>
  </div>
{/if}

{#if confirmRemove}
  <ConfirmDialog
    title="移除實例"
    message={`確定移除「${displayName}」?此操作不可復原。`}
    confirmLabel="移除"
    danger
    busy={removing}
    onConfirm={doRemove}
    onCancel={() => (confirmRemove = false)}
  >
    {#snippet children()}
      <label class="purge">
        <input type="checkbox" bind:checked={purge} disabled={removing} />
        <span>連同資料與備份一併刪除(purge)</span>
      </label>
    {/snippet}
  </ConfirmDialog>
{/if}

<style>
  .detail {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .loading {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .hd {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    flex-wrap: wrap;
  }
  .left {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
  }
  .back {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 34px;
    height: 34px;
    flex: none;
    font-size: var(--text-lg);
    color: var(--fg-1);
    background-color: var(--bg-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-standard);
  }
  .back:hover {
    background-color: var(--line);
    color: var(--fg-0);
  }
  .back:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .ident {
    min-width: 0;
  }
  .name {
    margin: 0;
    font-size: var(--text-xl);
    font-weight: 700;
    color: var(--fg-0);
    line-height: 1.2;
    word-break: break-word;
  }
  .meta {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-top: 4px;
    font-size: var(--text-sm);
    color: var(--fg-2);
    flex-wrap: wrap;
  }
  .meta .dot {
    color: var(--fg-2);
  }
  .node {
    color: var(--fg-1);
  }
  .mid {
    display: flex;
    align-items: center;
    gap: var(--space-5);
  }
  .kpi {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .k-lbl {
    font-size: var(--text-xs);
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .k-val {
    font-size: var(--text-md);
    font-weight: 600;
    color: var(--fg-0);
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .menu-item {
    display: block;
    width: 100%;
    text-align: left;
    font: inherit;
    font-size: var(--text-sm);
    color: var(--fg-0);
    background: transparent;
    border: none;
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    cursor: pointer;
  }
  .menu-item:hover {
    background-color: var(--bg-3);
  }
  .menu-item.danger {
    color: var(--err);
  }
  .menu-item:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  .purge {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .mono {
    font-family: var(--font-mono);
  }
</style>
