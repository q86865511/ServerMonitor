<script lang="ts">
  import { onMount } from 'svelte';
  import {
    CurseForgeEnabled,
    CurseForgeKeyConfigured,
    SetCurseForgeAPIKey,
  } from '../../wailsjs/go/main/App';
  import { call } from './api';
  import { pushToast } from './stores';

  // 節點層級的 CurseForge API 金鑰設定(native-backend R14:比照 Prism Launcher,使用者可填自己的
  // 金鑰覆蓋建置內嵌值)。實值只落 OS 金鑰庫,不明文落檔;設定/清除後熱生效(免重啟),建立精靈的
  // CurseForge 選項置灰即時反映。
  let loading = true;
  let saving = false;
  let enabled = false; // CurseForgeEnabled:內嵌或使用者覆蓋任一啟用即 true
  let configured = false; // CurseForgeKeyConfigured:使用者已設定覆蓋金鑰
  let keyInput = '';

  // 三態狀態文案:使用者覆蓋 > 內建啟用 > 停用。
  $: statusLabel = configured
    ? '已設定使用者金鑰(可清除以回退內建)'
    : enabled
      ? '使用內建金鑰(已啟用)'
      : '未設定(CurseForge 模組包停用)';
  $: statusKind = configured || enabled ? 'ok' : 'off';

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

<div class="cf-card">
  <div class="cf-head">
    <h3>CurseForge 模組包金鑰</h3>
    <span class="badge" class:ok={statusKind === 'ok'} class:off={statusKind === 'off'}>
      {loading ? '查詢中…' : statusLabel}
    </span>
  </div>
  <div class="hint">
    native 後端的 CurseForge 模組包需 API 金鑰。可於此填入自己的金鑰(比照 Prism Launcher)覆蓋建置
    內嵌值;金鑰存入 OS 金鑰庫,不明文落檔。儲存或清除後即時生效,無需重啟。
  </div>

  <div class="field">
    <label for="cf-key">API 金鑰</label>
    <input
      id="cf-key"
      type="password"
      bind:value={keyInput}
      autocomplete="off"
      placeholder={configured ? '已設定(輸入新值以覆寫)' : '貼上 CurseForge API 金鑰'}
      disabled={saving || loading}
    />
  </div>

  <div class="actions">
    <button
      class="ghost"
      on:click={clear}
      disabled={saving || loading || !configured}
      title={configured ? '清除使用者金鑰,回退建置內嵌值' : '尚未設定使用者金鑰'}
    >
      清除
    </button>
    <button class="primary" on:click={save} disabled={saving || loading || keyInput.trim() === ''}>
      {saving ? '儲存中…' : '儲存'}
    </button>
  </div>
</div>

<style>
  .cf-card {
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    padding: 16px;
    margin-bottom: 20px;
    background: var(--bg-1);
  }
  .cf-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 10px;
  }
  .cf-head h3 {
    margin: 0;
    font-size: 15px;
    color: var(--fg-0);
  }
  .badge {
    font-size: 12px;
    padding: 3px 10px;
    border-radius: 999px;
    border: 1px solid var(--line);
    color: var(--fg-1);
    white-space: nowrap;
  }
  .badge.ok {
    color: var(--ok, #3fb950);
    border-color: color-mix(in srgb, var(--ok, #3fb950) 45%, transparent);
    background: color-mix(in srgb, var(--ok, #3fb950) 12%, transparent);
  }
  .badge.off {
    color: var(--fg-2);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
    margin-top: 14px;
  }
  .ghost {
    background: transparent;
  }
</style>
