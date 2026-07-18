<script lang="ts">
  // 新增節點對話框(R5 多節點 GUI):兩段式——① 填位址/token 按「測試連線」撥測(TOFU),
  // 顯示憑證指紋供使用者與 agent console 印出值比對;② 使用者核對無誤後按「確認新增」才落地。
  // 任一欄位在測試後又變動,視為測試結果過期(需重新測試),避免帶著舊指紋誤新增。
  import { main } from '../../../../wailsjs/go/models';
  import { AddNode, ProbeNode } from '../../../../wailsjs/go/main/App';
  import { call, errMsg } from '../../api';
  import { pushToast } from '../../stores/toasts';
  import Modal from '../../ui/Modal.svelte';
  import Button from '../../ui/Button.svelte';
  import TextField from '../../ui/TextField.svelte';

  let {
    onClose,
    onAdded,
  }: {
    onClose: () => void;
    onAdded: () => void;
  } = $props();

  let name = $state('');
  let baseURL = $state('https://');
  let token = $state('');
  let insecureHTTP = $state(false);

  let probing = $state(false);
  let probeResult = $state<main.ProbeNodeResultDTO | null>(null);
  let probedFor = $state(''); // 上次測試成功/失敗時的欄位簽章,供偵測「欄位已變更」
  let adding = $state(false);
  let addError = $state('');

  function signature(): string {
    return JSON.stringify([name.trim(), baseURL.trim(), token, insecureHTTP]);
  }

  // 欄位在成功測試後又變動 → 視為過期,不可直接用舊指紋新增。
  const stale = $derived(probeResult !== null && probedFor !== signature());
  const canProbe = $derived(
    name.trim() !== '' && baseURL.trim() !== '' && token.trim() !== '' && !probing,
  );
  const canConfirm = $derived(!!probeResult && probeResult.ok && !stale && !adding);

  async function probe(): Promise<void> {
    if (!canProbe) return;
    probing = true;
    addError = '';
    try {
      const result = await call(() => ProbeNode(baseURL.trim(), token, insecureHTTP), {
        silent: true,
      });
      probeResult = result;
      probedFor = signature();
    } catch (e) {
      // ProbeNode 正常不拋錯(結果的 ok/error 欄位已表達失敗);此分支僅接住非預期例外。
      probeResult = main.ProbeNodeResultDTO.createFrom({ ok: false, error: errMsg(e) });
      probedFor = signature();
    } finally {
      probing = false;
    }
  }

  async function addNode(): Promise<void> {
    if (!canConfirm || !probeResult) return;
    adding = true;
    addError = '';
    try {
      const req = main.AddNodeRequest.createFrom({
        name: name.trim(),
        base_url: baseURL.trim(),
        token,
        fingerprint: probeResult.fingerprint,
        insecure_http: insecureHTTP,
      });
      await call(() => AddNode(req), { silent: true });
      pushToast('success', `已新增節點 ${name.trim()}`);
      onAdded();
      onClose();
    } catch (e) {
      addError = errMsg(e);
    } finally {
      adding = false;
    }
  }
</script>

<Modal title="新增節點" onClose={() => !adding && onClose()}>
  <div class="form">
    <p class="hint">
      新增遠端節點前需先撥測確認身分(TOFU,Trust On First Use):按「測試連線」後,
      請將下方顯示的憑證指紋與該 agent 主控台啟動時印出的指紋比對一致,再按「確認新增」。
    </p>

    <TextField
      label="節點名稱"
      required
      value={name}
      onInput={(v) => (name = v)}
      placeholder="例如 cloud-ubuntu-1"
      hint="限英數與 . _ -,不可與 local 重複。"
      disabled={adding}
    />
    <TextField
      label="節點位址"
      required
      value={baseURL}
      onInput={(v) => (baseURL = v)}
      placeholder="https://host:9444"
      disabled={adding}
    />
    <TextField
      label="Token"
      required
      type="password"
      value={token}
      onInput={(v) => (token = v)}
      placeholder="agent 啟動時印出的 token"
      disabled={adding}
    />

    <label class="checkbox-row">
      <input type="checkbox" bind:checked={insecureHTTP} disabled={adding} />
      <span>允許明文 HTTP 連線</span>
    </label>
    <p class="warn">
      僅限內網/VPN 環境使用:明文連線不加密,token 與流量可能被竊聽或竄改。
    </p>

    <div class="probe-actions">
      <Button variant="secondary" loading={probing} disabled={!canProbe} onclick={probe}>
        測試連線
      </Button>
    </div>

    {#if probeResult && !stale}
      {#if probeResult.ok}
        <div class="probe-box ok">
          <div class="probe-row"><span class="lbl">版本</span><span>{probeResult.version || '—'}</span></div>
          <div class="probe-row">
            <span class="lbl">憑證指紋</span>
            <span class="mono fp">{probeResult.fingerprint || '(明文 HTTP,無憑證指紋)'}</span>
          </div>
          <p class="hint">請核對上方指紋與 agent console 印出值一致,再按「確認新增」。</p>
        </div>
      {:else}
        <div class="probe-box err">{probeResult.error || '連線測試失敗'}</div>
      {/if}
    {:else if stale}
      <p class="hint stale">欄位已變更,請重新測試連線後再新增。</p>
    {/if}

    {#if addError}
      <div class="probe-box err">{addError}</div>
    {/if}
  </div>

  {#snippet footer()}
    <Button variant="ghost" disabled={adding} onclick={onClose}>取消</Button>
    <Button variant="primary" loading={adding} disabled={!canConfirm} onclick={addNode}>
      確認新增
    </Button>
  {/snippet}
</Modal>

<style>
  .form {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .hint {
    margin: 0;
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
  .warn {
    margin: -6px 0 0;
    font-size: var(--text-xs);
    color: var(--err);
  }
  .checkbox-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-base);
    color: var(--fg-0);
    cursor: pointer;
  }
  .probe-actions {
    display: flex;
    justify-content: flex-start;
  }
  .probe-box {
    border-radius: var(--radius-sm);
    padding: 10px 12px;
    font-size: var(--text-sm);
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .probe-box.ok {
    background-color: var(--ok-bg);
    border: 1px solid var(--ok);
  }
  .probe-box.err {
    background-color: var(--err-bg);
    border: 1px solid var(--err);
    color: var(--fg-0);
    white-space: pre-wrap;
  }
  .probe-row {
    display: flex;
    gap: var(--space-2);
  }
  .lbl {
    color: var(--fg-2);
    min-width: 70px;
    flex: none;
  }
  .mono {
    font-family: var(--font-mono, monospace);
  }
  .fp {
    word-break: break-all;
  }
  .stale {
    color: var(--err);
  }
</style>
