<script lang="ts">
  import { onMount } from 'svelte';
  import { main } from '../../wailsjs/go/models';
  import { GetAlertSettings, SetAlertSettings } from '../../wailsjs/go/main/App';
  import { call } from './api';
  import { pushToast } from './stores';

  export let uuid: string;

  let loading = true;
  let saving = false;
  let webhookConfigured = false;

  // 門檻:空字串=停用(送 0)。
  let cpuStr = '';
  let memStr = '';

  // Webhook:預設不更動(顯示遮罩);勾「變更」才覆寫。留空並勾選=清除既有。
  let updateWebhook = false;
  let webhookUrl = '';

  async function load(): Promise<void> {
    loading = true;
    try {
      const s: main.AlertSettingsDTO = await call(() => GetAlertSettings(uuid));
      webhookConfigured = s.webhook_configured;
      cpuStr = s.cpu_percent > 0 ? String(s.cpu_percent) : '';
      memStr = s.memory_percent > 0 ? String(s.memory_percent) : '';
      updateWebhook = false;
      webhookUrl = '';
    } catch {
      /* toast 已呈現 */
    } finally {
      loading = false;
    }
  }

  onMount(load);

  async function save(): Promise<void> {
    saving = true;
    try {
      await call(() =>
        SetAlertSettings(
          uuid,
          main.AlertSettingsRequest.createFrom({
            update_webhook: updateWebhook,
            webhook_url: updateWebhook ? webhookUrl.trim() : '',
            cpu_percent: (cpuStr ?? '').trim() === '' ? 0 : Number(cpuStr),
            memory_percent: (memStr ?? '').trim() === '' ? 0 : Number(memStr),
          }),
        ),
      );
      pushToast('success', '已儲存告警設定');
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      saving = false;
    }
  }
</script>

<div class="section-head head">
  <div>
    <div class="section-code mono">ALERTS / DISPATCH</div>
    <h3>告警設定</h3>
  </div>
</div>

{#if loading}
  <div class="empty">載入中…</div>
{:else}
  <div class="form">
    <div class="field">
      <div class="field-label">Discord Webhook</div>
      <div class="masked">
        目前:{webhookConfigured ? '已設定(●●●●●●,基於安全不顯示)' : '未設定'}
      </div>
      <div class="checkbox-row" style="margin-top:8px">
        <input id="a-upd" type="checkbox" bind:checked={updateWebhook} />
        <label for="a-upd" style="margin:0">變更 webhook</label>
      </div>
      {#if updateWebhook}
        <input
          aria-label="新的 Discord Webhook URL"
          type="password"
          bind:value={webhookUrl}
          placeholder="貼上新的 webhook URL(留空=清除既有)"
          autocomplete="off"
          style="margin-top:8px"
        />
        <div class="hint">存入金鑰庫,不明文落檔。留空並儲存會清除既有 webhook。</div>
      {/if}
    </div>

    <div class="field">
      <label for="a-cpu">CPU 門檻(%,空=停用)</label>
      <input
        id="a-cpu"
        type="number"
        min="0"
        max="100"
        value={cpuStr}
        on:input={(e) => (cpuStr = e.currentTarget.value)}
        placeholder="停用"
      />
    </div>
    <div class="field">
      <label for="a-mem">記憶體門檻(%,空=停用)</label>
      <input
        id="a-mem"
        type="number"
        min="0"
        max="100"
        value={memStr}
        on:input={(e) => (memStr = e.currentTarget.value)}
        placeholder="停用"
      />
    </div>

    <div class="actions">
      <button class="primary" on:click={save} disabled={saving}>
        {saving ? '儲存中…' : '儲存'}
      </button>
    </div>
  </div>
{/if}

<style>
  .head {
    margin-bottom: 14px;
  }
  .form {
    max-width: 520px;
  }
  .section-code { margin-bottom: 2px; color: var(--fg-3); font-size: 8px; font-weight: 700; letter-spacing: .12em; }
  .masked {
    padding: 9px 10px;
    color: var(--fg-1);
    background: var(--bg-inset);
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
    font-size: 11px;
  }
  .actions {
    margin-top: 8px;
  }
</style>
