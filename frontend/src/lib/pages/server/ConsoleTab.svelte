<script lang="ts">
  // 主控台分頁(R7):LogViewer(串流日誌)+ 指令列(rcon/rest/none 三態)+ 右側即時資訊欄。
  // 日誌 buffer 由 logs store 持有(引用計數+30s 延遲釋放),本元件不清 buffer,切分頁不丟失。
  import { protocol, main } from '../../../../wailsjs/go/models';
  import {
    SendCommand,
    GetCommandCapability,
    ListSchedules,
    GetAlertSettings,
  } from '../../../../wailsjs/go/main/App';
  import {
    logRows,
    logConnection,
    acquireLogs,
    releaseLogs,
    clearLogs,
    type LogConn,
  } from '../../stores/logs';
  import { call, errMsg } from '../../api';
  import { fmtCPU, fmtPlayers, memRatio } from '../../format';
  import type { LogRow } from '../../ui/types';
  import LogViewer from '../../ui/LogViewer.svelte';
  import Button from '../../ui/Button.svelte';
  import Select from '../../ui/Select.svelte';

  let { uuid, snapshot }: { uuid: string; snapshot: main.SnapshotDTO | null } = $props();

  // ---- 日誌訂閱(引用計數;uuid 變動接手,unmount 釋放;buffer 不清)----
  let rows = $state<LogRow[]>([]);
  let conn = $state<LogConn>({ state: 'idle' });
  $effect(() => {
    // 捕捉當輪 uuid:cleanup 讀反應式 uuid 會拿到「已前進」的新值(同 OverviewTab),
    // 用區域副本確保 releaseLogs 釋放的是本輪 acquireLogs 的同一實例,否則舊訂閱洩漏。
    const id = uuid;
    acquireLogs(id);
    const u1 = logRows(id).subscribe((v) => (rows = v));
    const u2 = logConnection(id).subscribe((v) => (conn = v));
    return () => {
      u1();
      u2();
      releaseLogs(id);
    };
  });

  // ---- 指令能力(rcon 自由輸入 / rest 具名動作 / none 停用)----
  let capKind = $state(''); // '' = 載入中
  let capReason = $state('');
  let actions = $state<main.CommandActionDTO[]>([]);
  let selectedAction = $state('');

  interface ArgRow {
    id: number;
    key: string;
    value: string;
  }
  let argRows = $state<ArgRow[]>([]);
  let argSeq = 0;

  function addArgRow(): void {
    argRows = [...argRows, { id: ++argSeq, key: '', value: '' }];
  }
  function removeArgRow(id: number): void {
    argRows = argRows.filter((r) => r.id !== id);
  }

  $effect(() => {
    const id = uuid;
    capKind = '';
    capReason = '';
    actions = [];
    selectedAction = '';
    argRows = [];
    GetCommandCapability(id)
      .then((c: main.CommandCapabilityDTO) => {
        if (id !== uuid) return;
        capKind = c.kind || 'none';
        capReason = c.reason || '';
        actions = c.actions ?? [];
        if (capKind === 'rest') {
          if (actions.length > 0) selectedAction = actions[0].action_id;
          if (argRows.length === 0) addArgRow();
        }
      })
      .catch((e) => {
        if (id !== uuid) return;
        capKind = 'none';
        capReason = errMsg(e);
      });
  });

  // ---- 指令回顯(本地;不寫入 store buffer)----
  interface FeedbackRow {
    id: number;
    kind: 'echo' | 'out' | 'err';
    text: string;
  }
  let feedback = $state<FeedbackRow[]>([]);
  let fbSeq = 0;
  function pushFb(kind: FeedbackRow['kind'], text: string): void {
    feedback = [...feedback, { id: ++fbSeq, kind, text }];
    if (feedback.length > 200) feedback = feedback.slice(feedback.length - 200);
  }

  let cmd = $state('');
  let sending = $state(false);

  async function sendRcon(): Promise<void> {
    const text = cmd.trim();
    if (!text || sending || capKind !== 'rcon') return;
    sending = true;
    pushFb('echo', `> ${text}`);
    try {
      const res: protocol.CommandResult = await SendCommand(
        uuid,
        protocol.GameCommand.createFrom({ raw: text }),
      );
      if (res.output) pushFb('out', res.output);
      if (!res.success) pushFb('err', '(指令回報未成功)');
      cmd = '';
    } catch (e) {
      pushFb('err', `(錯誤:${errMsg(e)})`);
    } finally {
      sending = false;
    }
  }

  async function sendRest(): Promise<void> {
    if (!selectedAction || sending || capKind !== 'rest') return;
    const args: Record<string, string> = {};
    for (const r of argRows) {
      const k = r.key.trim();
      if (k) args[k] = r.value;
    }
    sending = true;
    const argsDesc = Object.keys(args).length ? ` ${JSON.stringify(args)}` : '';
    pushFb('echo', `> [${selectedAction}]${argsDesc}`);
    try {
      const res: protocol.CommandResult = await SendCommand(
        uuid,
        protocol.GameCommand.createFrom({ action_id: selectedAction, args }),
      );
      if (res.output) pushFb('out', res.output);
      if (!res.success) pushFb('err', '(指令回報未成功)');
    } catch (e) {
      pushFb('err', `(錯誤:${errMsg(e)})`);
    } finally {
      sending = false;
    }
  }

  function onRconKey(e: KeyboardEvent): void {
    if (e.key === 'Enter') sendRcon();
  }

  const actionOptions = $derived(
    actions.map((a) => ({
      value: a.action_id,
      label: a.method ? `${a.action_id} (${a.method})` : a.action_id,
    })),
  );

  // ---- 右側即時資訊 ----
  const stats = $derived(snapshot?.stats ?? null);
  const cpuNow = $derived(stats ? fmtCPU(stats.cpu_percent) : '不適用');
  const ramRatio = $derived(stats ? memRatio(stats.memory_bytes, stats.memory_limit) : null);
  const ramNow = $derived(ramRatio == null ? '不適用' : `${(ramRatio * 100).toFixed(1)}%`);
  const playersNow = $derived(fmtPlayers(snapshot?.player_count));

  // 下次排程重啟倒數。
  let schedules = $state<main.ScheduleDTO[]>([]);
  let alerts = $state<main.AlertSettingsDTO | null>(null);
  $effect(() => {
    const id = uuid;
    ListSchedules(id)
      .then((s) => {
        if (id === uuid) schedules = s;
      })
      .catch(() => {
        if (id === uuid) schedules = [];
      });
    GetAlertSettings(id)
      .then((a) => {
        if (id === uuid) alerts = a;
      })
      .catch(() => {
        if (id === uuid) alerts = null;
      });
  });

  // 每秒 tick 供倒數。
  let now = $state(Date.now());
  $effect(() => {
    const t = setInterval(() => (now = Date.now()), 1000);
    return () => clearInterval(t);
  });

  function nextRestartMs(list: main.ScheduleDTO[], from: number): number | null {
    const base = new Date(from);
    let best: number | null = null;
    for (const s of list) {
      if (!s.enabled || s.kind !== 'restart' || !s.at) continue;
      const [hh, mm] = s.at.split(':').map((x) => Number(x));
      if (Number.isNaN(hh) || Number.isNaN(mm)) continue;
      const days = s.weekdays && s.weekdays.length ? s.weekdays : [0, 1, 2, 3, 4, 5, 6];
      for (let i = 0; i < 8; i++) {
        const d = new Date(
          Date.UTC(base.getUTCFullYear(), base.getUTCMonth(), base.getUTCDate() + i, hh, mm, 0),
        );
        if (d.getTime() <= from) continue;
        if (days.includes(d.getUTCDay())) {
          if (best === null || d.getTime() < best) best = d.getTime();
          break;
        }
      }
    }
    return best;
  }

  const nextRestart = $derived(nextRestartMs(schedules, now));
  const countdown = $derived.by(() => {
    if (nextRestart == null) return null;
    let rem = Math.max(0, Math.floor((nextRestart - now) / 1000));
    const d = Math.floor(rem / 86400);
    rem -= d * 86400;
    const h = Math.floor(rem / 3600);
    rem -= h * 3600;
    const m = Math.floor(rem / 60);
    const s = rem - m * 60;
    const p = (n: number): string => String(n).padStart(2, '0');
    return d > 0 ? `${d}天 ${p(h)}:${p(m)}:${p(s)}` : `${p(h)}:${p(m)}:${p(s)}`;
  });

  const alertSummary = $derived.by(() => {
    if (!alerts) return null;
    const cpu = alerts.cpu_percent > 0 ? `CPU ≥ ${alerts.cpu_percent}%` : 'CPU 未設';
    const mem = alerts.memory_percent > 0 ? `RAM ≥ ${alerts.memory_percent}%` : 'RAM 未設';
    return { cpu, mem, webhook: alerts.webhook_configured };
  });
</script>

<div class="console-tab">
  <div class="log-col">
    <LogViewer {rows} connected={conn.state === 'connected'} onClear={() => clearLogs(uuid)}>
      {#snippet commandBar()}
        <div class="cmd">
          {#if feedback.length > 0}
            <div class="feedback mono">
              {#each feedback as f (f.id)}
                <div class="fb {f.kind}">{f.text}</div>
              {/each}
            </div>
          {/if}

          {#if capKind === ''}
            <div class="note">載入指令能力…</div>
          {:else if capKind === 'none'}
            <div class="note disabled">
              此範本未啟用指令協定,無法送出指令。{capReason ? `(${capReason})` : ''}
            </div>
          {:else if capKind === 'rcon'}
            <div class="rcon-row">
              <input
                class="rcon-input mono"
                type="text"
                bind:value={cmd}
                onkeydown={onRconKey}
                placeholder="輸入指令後 Enter 送出…"
                disabled={sending}
              />
              <Button variant="primary" loading={sending} disabled={cmd.trim() === ''} onclick={sendRcon}>
                送出
              </Button>
            </div>
          {:else if capKind === 'rest'}
            <div class="rest">
              <div class="rest-row">
                <div class="rest-select">
                  <Select bind:value={selectedAction} options={actionOptions} disabled={sending || actions.length === 0} />
                </div>
                <Button variant="primary" loading={sending} disabled={!selectedAction} onclick={sendRest}>
                  送出動作
                </Button>
              </div>
              <div class="args">
                <div class="args-head">
                  <span class="muted">參數(選填,key = value)</span>
                  <Button size="sm" variant="ghost" onclick={addArgRow} disabled={sending}>＋ 參數</Button>
                </div>
                {#each argRows as r (r.id)}
                  <div class="arg-row">
                    <input class="mono" type="text" placeholder="key" bind:value={r.key} disabled={sending} />
                    <span class="eq">=</span>
                    <input class="mono" type="text" placeholder="value" bind:value={r.value} disabled={sending} />
                    <Button size="sm" variant="ghost" onclick={() => removeArgRow(r.id)} disabled={sending}>移除</Button>
                  </div>
                {/each}
              </div>
            </div>
          {/if}
        </div>
      {/snippet}
    </LogViewer>
  </div>

  <aside class="info-col">
    <div class="info-card">
      <div class="info-title">即時資源</div>
      <div class="stat">
        <span class="s-lbl">CPU</span><span class="s-val">{cpuNow}</span>
      </div>
      <div class="stat">
        <span class="s-lbl">記憶體</span><span class="s-val">{ramNow}</span>
      </div>
      <div class="stat">
        <span class="s-lbl">玩家</span><span class="s-val">{playersNow}</span>
      </div>
    </div>

    {#if countdown != null}
      <div class="info-card">
        <div class="info-title">下次排程重啟</div>
        <div class="countdown mono">{countdown}</div>
        <div class="cd-sub">距下一次自動重啟</div>
      </div>
    {/if}

    {#if alertSummary}
      <div class="info-card">
        <div class="info-title">告警門檻</div>
        <div class="stat"><span class="s-lbl">{alertSummary.cpu}</span></div>
        <div class="stat"><span class="s-lbl">{alertSummary.mem}</span></div>
        <div class="stat">
          <span class="s-lbl">Webhook</span>
          <span class="s-val" class:on={alertSummary.webhook}>
            {alertSummary.webhook ? '已設定' : '未設定'}
          </span>
        </div>
      </div>
    {/if}
  </aside>
</div>

<style>
  .console-tab {
    display: grid;
    grid-template-columns: 1fr 260px;
    gap: var(--space-4);
    height: calc(100vh - 240px);
    min-height: 420px;
  }
  .log-col {
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  @media (max-width: 1000px) {
    .console-tab {
      grid-template-columns: 1fr;
      height: auto;
    }
    .log-col {
      height: 60vh;
    }
  }

  .cmd {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin-top: var(--space-2);
  }
  .feedback {
    max-height: 120px;
    overflow-y: auto;
    background: var(--bg-0);
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    padding: 6px 8px;
    font-size: 12px;
    line-height: 1.5;
  }
  .fb {
    white-space: pre-wrap;
    word-break: break-word;
  }
  .fb.echo {
    color: var(--accent);
  }
  .fb.out {
    color: var(--fg-1);
  }
  .fb.err {
    color: var(--err);
  }
  .note {
    font-size: var(--text-sm);
    color: var(--fg-2);
  }
  .note.disabled {
    color: var(--busy);
    background: var(--busy-bg);
    border: 1px solid var(--busy);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
  }
  .rcon-row {
    display: flex;
    gap: var(--space-2);
  }
  .rcon-input {
    flex: 1;
    color: var(--fg-0);
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 6px 9px;
  }
  .rcon-input:focus-visible {
    outline: none;
    border-color: var(--accent);
  }
  .rest {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .rest-row {
    display: flex;
    gap: var(--space-2);
    align-items: flex-start;
  }
  .rest-select {
    flex: 1;
  }
  .args {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .args-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }
  .arg-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .arg-row input {
    flex: 1;
    color: var(--fg-0);
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 5px 8px;
  }
  .arg-row input:focus-visible {
    outline: none;
    border-color: var(--accent);
  }
  .arg-row .eq {
    color: var(--fg-2);
  }

  .info-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-width: 0;
  }
  .info-card {
    background-color: var(--bg-1);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: var(--space-3) var(--space-4);
  }
  .info-title {
    font-size: var(--text-xs);
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    margin-bottom: var(--space-2);
  }
  .stat {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
    padding: 3px 0;
    font-size: var(--text-sm);
  }
  .s-lbl {
    color: var(--fg-1);
  }
  .s-val {
    color: var(--fg-0);
    font-weight: 600;
  }
  .s-val.on {
    color: var(--ok);
  }
  .countdown {
    font-size: var(--text-xl);
    font-weight: 700;
    color: var(--fg-0);
  }
  .cd-sub {
    font-size: var(--text-xs);
    color: var(--fg-2);
    margin-top: 2px;
  }
  .muted {
    color: var(--fg-2);
    font-size: var(--text-sm);
  }
  .mono {
    font-family: var(--font-mono);
  }
</style>
