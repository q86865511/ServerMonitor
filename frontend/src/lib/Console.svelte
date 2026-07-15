<script lang="ts">
  import { onMount, onDestroy, afterUpdate, createEventDispatcher } from 'svelte';
  import { protocol, main } from '../../wailsjs/go/models';
  import { SubscribeLogs, UnsubscribeLogs, SendCommand, GetCommandCapability } from '../../wailsjs/go/main/App';
  import { EventsOn, EventsOff } from '../../wailsjs/runtime/runtime';
  import { errMsg } from './api';
  import Modal from './Modal.svelte';
  import Icon from './Icon.svelte';

  export let uuid: string;

  const dispatch = createEventDispatcher<{ close: void }>();

  // 監控注入提示行(如「已丟棄 N 行」)的 stream 值,與真實 stdout/stderr 區分樣式。
  const MONITOR_STREAM = 'gsm-monitor';
  const MAX_LINES = 2000;

  interface Row {
    id: number;
    stream: string; // stdout | stderr | gsm-monitor | echo | echo-out
    text: string;
  }
  let rows: Row[] = [];
  let seq = 0;

  // 指令能力:由後端依範本啟用中協定決定渲染方式(rcon 自由輸入 / rest 具名動作 / none 停用)。
  let capKind = ''; // '' = 載入中;'rcon' | 'rest' | 'none'
  let capReason = '';
  let actions: main.CommandActionDTO[] = [];

  // rcon 自由輸入。
  let cmd = '';
  let sending = false;

  // rest 具名動作 + 動態 args(key=value 列)。
  let selectedAction = '';
  interface ArgRow {
    id: number;
    key: string;
    value: string;
  }
  let argRows: ArgRow[] = [];
  let argSeq = 0;

  let autoScroll = true;
  let scrollBox: HTMLDivElement;

  const evName = `logs:${uuid}`;
  let unlisten: (() => void) | null = null;
  // 訂閱 promise 追蹤(#7):onDestroy 必須等 subscribe resolve 後再 Unsubscribe,避免
  // 訂閱尚未建立就取消而留下孤兒訂閱。
  let subscribePromise: Promise<void> | null = null;

  function push(stream: string, text: string): void {
    rows = [...rows, { id: ++seq, stream, text }];
    if (rows.length > MAX_LINES) rows = rows.slice(rows.length - MAX_LINES);
  }

  function addArgRow(): void {
    argRows = [...argRows, { id: ++argSeq, key: '', value: '' }];
  }

  function removeArgRow(id: number): void {
    argRows = argRows.filter((r) => r.id !== id);
  }

  onMount(() => {
    unlisten = EventsOn(evName, (ln: { stream?: string; line?: string }) => {
      push(ln.stream ?? 'stdout', ln.line ?? '');
    });
    // 訂閱 log(記住 promise 供 onDestroy 等待)。
    subscribePromise = SubscribeLogs(uuid)
      .catch((e) => {
        push(MONITOR_STREAM, `(無法訂閱 log:${errMsg(e)})`);
      })
      .then(() => undefined);
    // 查詢指令能力,決定輸入列渲染。
    GetCommandCapability(uuid)
      .then((c: main.CommandCapabilityDTO) => {
        capKind = c.kind || 'none';
        capReason = c.reason || '';
        actions = c.actions ?? [];
        if (capKind === 'rest') {
          if (actions.length > 0) selectedAction = actions[0].action_id;
          if (argRows.length === 0) addArgRow();
        }
      })
      .catch((e) => {
        // 保守降級為停用態(不影響 log 觀看)。
        capKind = 'none';
        capReason = errMsg(e);
      });
  });

  onDestroy(async () => {
    if (unlisten) unlisten();
    EventsOff(evName);
    try {
      // 等訂閱確實建立後再取消,避免孤兒訂閱(#7)。
      if (subscribePromise) await subscribePromise;
      await UnsubscribeLogs(uuid);
    } catch {
      /* 忽略卸載期錯誤 */
    }
  });

  afterUpdate(() => {
    if (autoScroll && scrollBox) {
      scrollBox.scrollTop = scrollBox.scrollHeight;
    }
  });

  function onScroll(): void {
    if (!scrollBox) return;
    // 使用者往上捲動即暫停自動捲底;捲回底部自動恢復。
    const atBottom = scrollBox.scrollHeight - scrollBox.scrollTop - scrollBox.clientHeight < 24;
    autoScroll = atBottom;
  }

  // sendRcon 送出自由輸入指令(rcon 協定)。
  async function sendRcon(): Promise<void> {
    const text = cmd.trim();
    if (!text || sending || capKind !== 'rcon') return;
    sending = true;
    push('echo', `> ${text}`);
    try {
      const res: protocol.CommandResult = await SendCommand(
        uuid,
        protocol.GameCommand.createFrom({ raw: text }),
      );
      if (res.output) push('echo-out', res.output);
      if (!res.success) push('stderr', '(指令回報未成功)');
      cmd = '';
    } catch (e) {
      push('stderr', `(錯誤:${errMsg(e)})`);
    } finally {
      sending = false;
    }
  }

  // sendRest 送出具名動作(rest 協定):組 action_id + 非空 args。
  async function sendRest(): Promise<void> {
    if (!selectedAction || sending || capKind !== 'rest') return;
    const args: Record<string, string> = {};
    for (const r of argRows) {
      const k = r.key.trim();
      if (k) args[k] = r.value;
    }
    sending = true;
    const argsDesc = Object.keys(args).length ? ` ${JSON.stringify(args)}` : '';
    push('echo', `> [${selectedAction}]${argsDesc}`);
    try {
      const res: protocol.CommandResult = await SendCommand(
        uuid,
        protocol.GameCommand.createFrom({ action_id: selectedAction, args }),
      );
      if (res.output) push('echo-out', res.output);
      if (!res.success) push('stderr', '(指令回報未成功)');
    } catch (e) {
      push('stderr', `(錯誤:${errMsg(e)})`);
    } finally {
      sending = false;
    }
  }

  function onKey(e: KeyboardEvent): void {
    if (e.key === 'Enter') sendRcon();
  }
</script>

<Modal title={`主控台 · ${uuid}`} wide on:close={() => dispatch('close')}>
  <div class="console">
    <div class="terminal-bar">
      <div class="terminal-meta">
        <span class="stream-light"></span>
        <span class="mono">LOG STREAM</span>
        <span class="divider"></span>
        <span class="mono muted">{rows.length} LINES</span>
      </div>
      <label class="checkbox-row auto-scroll">
        <input type="checkbox" bind:checked={autoScroll} />
        <span>自動捲底</span>
      </label>
    </div>

    <div class="log mono" bind:this={scrollBox} on:scroll={onScroll} role="log" aria-label="伺服器日誌">
      {#each rows as r (r.id)}
        <div class="line {r.stream}">{r.text}</div>
      {/each}
      {#if rows.length === 0}
        <div class="muted">等待輸出…</div>
      {/if}
    </div>

    {#if capKind === '' }
      <div class="cap-note"><span class="stream-light busy"></span>載入指令能力…</div>
    {:else if capKind === 'none'}
      <div class="disabled-note">
        此範本未啟用指令協定,無法送出指令。{capReason ? `(${capReason})` : ''}
      </div>
    {:else if capKind === 'rcon'}
      <div class="input-row">
        <input
          class="mono"
          type="text"
          bind:value={cmd}
          on:keydown={onKey}
          placeholder="輸入指令後 Enter 送出…"
          disabled={sending}
        />
        <button class="primary send" on:click={sendRcon} disabled={sending || cmd.trim() === ''}>
          <Icon name="terminal" size={14} /><span>{sending ? '送出中…' : '送出'}</span>
        </button>
      </div>
    {:else if capKind === 'rest'}
      <div class="rest-panel">
        <div class="input-row">
          <select bind:value={selectedAction} disabled={sending || actions.length === 0}>
            {#each actions as act (act.action_id)}
              <option value={act.action_id}>{act.action_id}{act.method ? ` (${act.method})` : ''}</option>
            {/each}
          </select>
          <button
            class="primary send"
            on:click={sendRest}
            disabled={sending || !selectedAction}
          >
            <Icon name="terminal" size={14} /><span>{sending ? '送出中…' : '送出動作'}</span>
          </button>
        </div>
        <div class="args">
          <div class="args-head">
            <span class="muted">參數(選填,key = value)</span>
            <button class="ghost sm add-arg" on:click={addArgRow} disabled={sending}><Icon name="plus" size={13} /><span>新增參數</span></button>
          </div>
          {#each argRows as r (r.id)}
            <div class="arg-row">
              <input class="mono" type="text" placeholder="key" bind:value={r.key} disabled={sending} />
              <span class="eq">=</span>
              <input class="mono" type="text" placeholder="value" bind:value={r.value} disabled={sending} />
              <button class="ghost icon-button" on:click={() => removeArgRow(r.id)} disabled={sending} aria-label="移除參數">
                <Icon name="trash" size={14} />
              </button>
            </div>
          {/each}
        </div>
      </div>
    {/if}
  </div>
</Modal>

<style>
  .console {
    display: flex;
    flex-direction: column;
    gap: 0;
    height: 68vh;
    min-height: 460px;
    margin: -20px;
    background: var(--bg-inset);
  }
  .terminal-bar {
    flex: none;
    display: flex;
    align-items: center;
    justify-content: space-between;
    min-height: 39px;
    padding: 7px 12px;
    background: var(--bg-2);
    border-bottom: 1px solid var(--line);
  }
  .terminal-meta {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--fg-1);
    font-size: 9px;
    font-weight: 700;
    letter-spacing: .08em;
  }
  .stream-light {
    width: 6px;
    height: 6px;
    background: var(--ok);
    border-radius: 50%;
  }
  .stream-light.busy { background: var(--busy); }
  .divider { width: 1px; height: 12px; background: var(--line-strong); }
  .auto-scroll {
    color: var(--fg-2);
    font-size: 10px;
  }
  .log {
    flex: 1;
    overflow-y: auto;
    padding: 14px 16px;
    color: #c8c9be;
    background: #090a08;
    border-bottom: 1px solid var(--line);
    font-size: 12px;
    line-height: 1.62;
  }
  .line {
    white-space: pre-wrap;
    word-break: break-word;
  }
  .line.stderr {
    color: #e8887f;
  }
  .line.gsm-monitor {
    color: var(--busy);
    font-style: italic;
  }
  .line.echo {
    color: var(--accent);
  }
  .line.echo-out {
    color: var(--fg-1);
  }
  .disabled-note {
    flex: none;
    margin: 10px 12px;
    font-size: 11px;
    color: var(--busy);
    background: rgba(214, 162, 74, 0.07);
    border: 1px solid rgba(214, 162, 74, 0.5);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
  }
  .cap-note {
    display: flex;
    flex: none;
    align-items: center;
    gap: 8px;
    padding: 9px 12px;
    color: var(--fg-2);
    background: var(--bg-1);
    font-size: 11px;
  }
  .input-row {
    flex: none;
    display: flex;
    gap: 7px;
    padding: 10px 12px;
    background: var(--bg-1);
  }
  .input-row input,
  .input-row select {
    flex: 1;
  }
  .send,
  .add-arg {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .rest-panel {
    flex: none;
    display: flex;
    flex-direction: column;
    gap: 0;
    background: var(--bg-1);
  }
  .args {
    display: flex;
    flex-direction: column;
    gap: 7px;
    padding: 0 12px 11px;
  }
  .args-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    min-height: 31px;
  }
  .arg-row {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .arg-row input {
    flex: 1;
  }
  .arg-row .eq {
    color: var(--fg-2);
  }
  @media (max-width: 820px) {
    .console {
      height: calc(100vh - 112px);
      min-height: 0;
      margin: -16px;
    }
  }
  @media (max-width: 600px) {
    .arg-row { flex-wrap: wrap; }
    .arg-row input { min-width: 120px; }
  }
</style>
