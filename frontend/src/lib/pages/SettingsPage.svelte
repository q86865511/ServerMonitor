<script lang="ts">
  // 設定頁(R9):CurseForge API 金鑰卡,功能等價舊 lib/CurseForgeSettings.svelte(runes 化+ui 元件化)。
  // 備份/排程/警報已各自獨立成全域頁(BackupsPage/SchedulesPage/AlertsPage),不在此重複。
  // 應用資訊區:目前後端未暴露版本等資訊(app.go 無對應 binding),故僅保留 CurseForge 卡
  // (依任務簡報「沒有就只放 CurseForge 卡」)。
  import { onMount } from 'svelte';
  import {
    CurseForgeEnabled,
    CurseForgeKeyConfigured,
    SetCurseForgeAPIKey,
  } from '../../../wailsjs/go/main/App';
  import { call } from '../api';
  import { pushToast } from '../stores/toasts';
  import Badge from '../ui/Badge.svelte';
  import Button from '../ui/Button.svelte';
  import Card from '../ui/Card.svelte';
  import TextField from '../ui/TextField.svelte';

  let loading = $state(true);
  let saving = $state(false);
  let enabled = $state(false); // CurseForgeEnabled:內嵌或使用者覆蓋任一啟用即 true
  let configured = $state(false); // CurseForgeKeyConfigured:使用者已設定覆蓋金鑰
  let keyInput = $state('');

  // 三態狀態文案:使用者覆蓋 > 內建啟用 > 停用。
  const statusLabel = $derived(
    configured
      ? '已設定使用者金鑰(可清除以回退內建)'
      : enabled
        ? '使用內建金鑰(已啟用)'
        : '未設定(CurseForge 模組包停用)',
  );
  const statusTone = $derived(configured || enabled ? 'ok' : 'idle');

  async function load(): Promise<void> {
    loading = true;
    try {
      [enabled, configured] = await Promise.all([
        call(() => CurseForgeEnabled(), { silent: true }),
        call(() => CurseForgeKeyConfigured(), { silent: true }),
      ]);
    } catch {
      enabled = false;
      configured = false;
    } finally {
      loading = false;
    }
  }

  onMount(load);

  async function save(): Promise<void> {
    const key = keyInput.trim();
    if (key === '' || saving) return;
    saving = true;
    try {
      await call(() => SetCurseForgeAPIKey(key));
      keyInput = '';
      pushToast('success', '已儲存 CurseForge 金鑰(即時生效)');
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      saving = false;
    }
  }

  async function clear(): Promise<void> {
    if (saving) return;
    saving = true;
    try {
      await call(() => SetCurseForgeAPIKey(''));
      keyInput = '';
      pushToast('success', '已清除 CurseForge 使用者金鑰');
      await load();
    } catch {
      /* toast 已呈現 */
    } finally {
      saving = false;
    }
  }
</script>

<Card>
  <div class="cf-head">
    <h3>CurseForge 模組包金鑰</h3>
    <Badge tone={statusTone}>{loading ? '查詢中…' : statusLabel}</Badge>
  </div>
  <p class="hint">
    native 後端的 CurseForge 模組包需 API 金鑰。可於此填入自己的金鑰(比照 Prism
    Launcher)覆蓋建置內嵌值;金鑰存入 OS 金鑰庫,不明文落檔。儲存或清除後即時生效,無需重啟。
  </p>

  <TextField
    label="API 金鑰"
    type="password"
    bind:value={keyInput}
    placeholder={configured ? '已設定(輸入新值以覆寫)' : '貼上 CurseForge API 金鑰'}
    disabled={saving || loading}
  />

  <div class="actions">
    <Button variant="ghost" onclick={clear} disabled={saving || loading || !configured}>清除</Button>
    <Button variant="primary" loading={saving} disabled={loading || keyInput.trim() === ''} onclick={save}>
      儲存
    </Button>
  </div>
</Card>

<style>
  .cf-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    margin-bottom: var(--space-2);
  }
  .cf-head h3 {
    margin: 0;
    font-size: var(--text-md);
    color: var(--fg-0);
  }
  .hint {
    margin: 0 0 var(--space-4);
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-4);
  }
</style>
