<script lang="ts">
  // 實例維度的告警面板(單一實作;T13 全域頁以實例選擇器複用本元件,未來詳細頁亦可掛同一元件)。
  // 功能等價舊 lib/AlertsPanel.svelte:CPU/RAM 門檻讀寫 + webhook 遮罩流程;UI 改用 ui/ 元件,
  // 結構比照 pages/server/BackupsPanel.svelte / SchedulesPanel.svelte(同一 uuid prop + 重載模式)。
  import { main } from '../../../../wailsjs/go/models';
  import { GetAlertSettings, SetAlertSettings } from '../../../../wailsjs/go/main/App';
  import { call } from '../../api';
  import { pushToast } from '../../stores/toasts';
  import Button from '../../ui/Button.svelte';
  import ErrorState from '../../ui/ErrorState.svelte';
  import Skeleton from '../../ui/Skeleton.svelte';
  import TextField from '../../ui/TextField.svelte';

  let { uuid }: { uuid: string } = $props();

  let loading = $state(true);
  let loadError = $state('');
  let saving = $state(false);
  let webhookConfigured = $state(false);

  // 門檻:空字串=停用(送 0)。
  let cpuStr = $state('');
  let memStr = $state('');

  // Webhook:預設不更動(顯示遮罩);勾「變更」才覆寫。留空並勾選=清除既有。
  let updateWebhook = $state(false);
  let webhookUrl = $state('');

  // 遞增序號:load() 可能因 uuid 切換而併發(舊 uuid 慢回應晚到);回應套用前檢查自己仍是
  // 最新一次呼叫,否則丟棄(同 stores/instances.ts 的 refreshSeq)——避免 A 的回應在切到 B 之後
  // 覆寫表單,使用者按「儲存」把 A 的門檻/webhook 誤寫到 B。
  let loadSeq = 0;

  async function load(): Promise<void> {
    const seq = ++loadSeq;
    loading = true;
    loadError = '';
    try {
      const s: main.AlertSettingsDTO = await call(() => GetAlertSettings(uuid), { silent: true });
      if (seq !== loadSeq) return; // 已有更新的 load 在途,丟棄此次舊回應
      webhookConfigured = s.webhook_configured;
      cpuStr = s.cpu_percent > 0 ? String(s.cpu_percent) : '';
      memStr = s.memory_percent > 0 ? String(s.memory_percent) : '';
      updateWebhook = false;
      webhookUrl = '';
    } catch (e) {
      if (seq !== loadSeq) return;
      loadError = e instanceof Error ? e.message : String(e);
    } finally {
      if (seq === loadSeq) loading = false;
    }
  }

  // uuid 變動(全域頁切換實例)時重載;首次掛載也由本 effect 觸發(不另掛 onMount,避免雙重載入)。
  let loadedFor = '';
  $effect(() => {
    if (uuid && uuid !== loadedFor) {
      loadedFor = uuid;
      load();
    }
  });

  async function save(): Promise<void> {
    saving = true;
    try {
      await call(() =>
        SetAlertSettings(
          uuid,
          main.AlertSettingsRequest.createFrom({
            update_webhook: updateWebhook,
            webhook_url: updateWebhook ? webhookUrl.trim() : '',
            cpu_percent: cpuStr.trim() === '' ? 0 : Number(cpuStr),
            memory_percent: memStr.trim() === '' ? 0 : Number(memStr),
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

<div class="panel">
  <div class="head">
    <h3>告警設定</h3>
    <div class="tools">
      <Button size="sm" onclick={load} disabled={loading}>重新整理</Button>
    </div>
  </div>

  {#if loading}
    <div class="skeletons">
      <Skeleton height="38px" />
      <Skeleton height="38px" />
      <Skeleton height="38px" />
    </div>
  {:else if loadError}
    <ErrorState message={`載入告警設定失敗:${loadError}`} onRetry={load} />
  {:else}
    <div class="form">
      <div class="field">
        <span class="lbl">Discord Webhook</span>
        <div class="masked">
          目前:{webhookConfigured ? '已設定(●●●●●●,基於安全不顯示)' : '未設定'}
        </div>
        <label class="check">
          <input type="checkbox" bind:checked={updateWebhook} />
          <span>變更 webhook</span>
        </label>
        {#if updateWebhook}
          <TextField
            type="password"
            bind:value={webhookUrl}
            placeholder="貼上新的 webhook URL(留空=清除既有)"
            hint="存入金鑰庫,不明文落檔。留空並儲存會清除既有 webhook。"
          />
        {/if}
      </div>

      <TextField label="CPU 門檻(%,空=停用)" type="number" bind:value={cpuStr} placeholder="停用" />
      <TextField label="記憶體門檻(%,空=停用)" type="number" bind:value={memStr} placeholder="停用" />

      <div class="actions">
        <Button variant="primary" loading={saving} onclick={save}>儲存</Button>
      </div>
    </div>
  {/if}
</div>

<style>
  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }
  .head h3 {
    margin: 0;
    font-size: var(--text-md);
  }
  .tools {
    display: flex;
    gap: var(--space-2);
  }
  .skeletons {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .form {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    max-width: 440px;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .lbl {
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .masked {
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
  }
</style>
