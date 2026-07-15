<script lang="ts">
  import { onMount, createEventDispatcher } from 'svelte';
  import { main } from '../../wailsjs/go/models';
  import { ListTemplates, CreateInstance } from '../../wailsjs/go/main/App';
  import { call } from './api';
  import { pushToast } from './stores';
  import Modal from './Modal.svelte';

  const dispatch = createEventDispatcher<{ close: void; created: string }>();

  let templates: main.TemplateDTO[] = [];
  let loading = true;
  let submitting = false;
  let error = ''; // inline 錯誤(建立失敗時原樣顯示)

  let templateId = '';
  let variant = '';
  let paramValues: Record<string, string> = {};
  let secretValues: Record<string, string> = {};

  // 模組包(R11):type 空=無;curseforge/manual-cfzip 需 CF_API_KEY。
  const CF_KEY = 'CF_API_KEY';
  let modpackType = '';
  let modpackRef = '';
  let cfApiKey = '';

  $: tmpl = templates.find((t) => t.id === templateId);
  $: needsCFKey = modpackType === 'curseforge' || modpackType === 'manual-cfzip';
  // 範本自身已宣告 CF_API_KEY 機密時,不再另立欄位(避免重複)。
  $: templateHasCFSecret = (tmpl?.secrets ?? []).some((s) => s.key === CF_KEY);
  $: modpackRefPlaceholder =
    modpackType === 'modrinth'
      ? 'Modrinth slug 或專案 URL'
      : modpackType === 'curseforge'
        ? 'CurseForge slug 或整合頁 URL'
        : modpackType.startsWith('manual')
          ? '本機檔案完整路徑'
          : '';

  onMount(async () => {
    try {
      templates = await call(() => ListTemplates());
    } catch {
      /* toast 已呈現 */
    } finally {
      loading = false;
    }
  });

  function onTemplateChange(): void {
    error = '';
    paramValues = {};
    secretValues = {};
    modpackType = '';
    modpackRef = '';
    cfApiKey = '';
    variant = tmpl && tmpl.variants.length > 0 ? tmpl.variants[0].id : '';
    for (const p of tmpl?.params ?? []) {
      paramValues[p.key] = p.default != null ? String(p.default) : p.type === 'bool' ? 'false' : '';
    }
    for (const s of tmpl?.secrets ?? []) {
      secretValues[s.key] = '';
    }
    paramValues = paramValues;
    secretValues = secretValues;
  }

  function setBool(key: string, checked: boolean): void {
    paramValues[key] = checked ? 'true' : 'false';
    paramValues = paramValues;
  }

  // 送出條件:必填 text 非空;必填 bool(如 EULA 同意項)須為 true;模組包來源需填 ref;CF 需金鑰。
  $: missingParams = (tmpl?.params ?? []).filter((p) => {
    if (!p.required) return false;
    const v = paramValues[p.key] ?? '';
    return p.type === 'bool' ? v !== 'true' : (v ?? '').trim() === '';
  });
  $: missingSecrets = (tmpl?.secrets ?? []).filter((s) => (secretValues[s.key] ?? '').trim() === '');
  $: modpackIncomplete = modpackType !== '' && modpackRef.trim() === '';
  $: cfKeyMissing =
    needsCFKey && !templateHasCFSecret && cfApiKey.trim() === '';
  $: canSubmit =
    !!tmpl &&
    !submitting &&
    missingParams.length === 0 &&
    missingSecrets.length === 0 &&
    !modpackIncomplete &&
    !cfKeyMissing;

  async function submit(): Promise<void> {
    if (!tmpl || !canSubmit) return;
    error = '';
    submitting = true;

    const secrets = { ...secretValues };
    if (needsCFKey && !templateHasCFSecret && cfApiKey.trim() !== '') {
      secrets[CF_KEY] = cfApiKey.trim();
    }
    const req = main.CreateInstanceRequest.createFrom({
      template_id: tmpl.id,
      variant,
      params: paramValues,
      secrets,
      node: '',
      modpack:
        modpackType !== '' ? { type: modpackType, ref: modpackRef.trim() } : undefined,
    });

    try {
      const uuid = await call(() => CreateInstance(req), { silent: true });
      pushToast('success', `已建立實例 ${uuid}`);
      dispatch('created', uuid);
    } catch (e) {
      error = e == null ? '建立失敗' : typeof e === 'string' ? e : String((e as Error).message ?? e);
    } finally {
      submitting = false;
    }
  }
</script>

<Modal title="建立伺服器" wide on:close={() => dispatch('close')}>
  {#if loading}
    <div class="empty">載入範本中…</div>
  {:else if templates.length === 0}
    <div class="empty">無可用範本。</div>
  {:else}
    <div class="field">
      <label for="tmpl">遊戲範本<span class="req">*</span></label>
      <select id="tmpl" bind:value={templateId} on:change={onTemplateChange}>
        <option value="" disabled>請選擇範本…</option>
        {#each templates as t}
          <option value={t.id}>{t.name}({t.id})</option>
        {/each}
      </select>
    </div>

    {#if tmpl}
      {#if tmpl.variants.length > 0}
        <div class="field">
          <label for="variant">變體<span class="req">*</span></label>
          <select id="variant" bind:value={variant}>
            {#each tmpl.variants as v}
              <option value={v.id}>{v.id}{v.loader ? `(${v.loader})` : ''}</option>
            {/each}
          </select>
        </div>
      {/if}

      {#if tmpl.params.length > 0}
        <h4 class="sect">參數</h4>
        {#each tmpl.params as p}
          <div class="field">
            {#if p.type === 'bool'}
              <div class="checkbox-row">
                <input
                  id={`p-${p.key}`}
                  type="checkbox"
                  checked={paramValues[p.key] === 'true'}
                  on:change={(e) => setBool(p.key, e.currentTarget.checked)}
                />
                <label for={`p-${p.key}`} style="margin:0">
                  {p.label || p.key}{#if p.required}<span class="req">*</span>{/if}
                </label>
              </div>
              {#if p.required}
                <div class="hint">必填同意項:未勾選不可送出(如 EULA)。</div>
              {/if}
            {:else}
              <label for={`p-${p.key}`}>
                {p.label || p.key}{#if p.required}<span class="req">*</span>{/if}
              </label>
              <input
                id={`p-${p.key}`}
                type={p.type === 'int' || p.type === 'number' ? 'number' : 'text'}
                value={paramValues[p.key] ?? ''}
                on:input={(e) => (paramValues[p.key] = e.currentTarget.value)}
                placeholder={p.key}
              />
            {/if}
          </div>
        {/each}
      {/if}

      {#if tmpl.secrets.length > 0}
        <h4 class="sect">機密</h4>
        <div class="hint" style="margin-bottom:10px">以下欄位存入 OS 金鑰庫,不明文落檔。</div>
        {#each tmpl.secrets as s}
          <div class="field">
            <label for={`s-${s.key}`}>{s.label || s.key}<span class="req">*</span></label>
            <input id={`s-${s.key}`} type="password" bind:value={secretValues[s.key]} autocomplete="off" />
          </div>
        {/each}
      {/if}

      {#if tmpl.modpack}
        <h4 class="sect">模組包(選填)</h4>
        <div class="field">
          <label for="mp-type">來源</label>
          <select id="mp-type" bind:value={modpackType}>
            <option value="">無</option>
            <option value="modrinth">Modrinth</option>
            <option value="curseforge">CurseForge</option>
            <option value="manual-mrpack">手動 .mrpack 檔</option>
            <option value="manual-cfzip">手動 CurseForge zip 檔</option>
          </select>
        </div>
        {#if modpackType !== ''}
          <div class="field">
            <label for="mp-ref">來源位置<span class="req">*</span></label>
            <input id="mp-ref" type="text" bind:value={modpackRef} placeholder={modpackRefPlaceholder} />
          </div>
          {#if needsCFKey && !templateHasCFSecret}
            <div class="field">
              <label for="mp-cf">CF_API_KEY<span class="req">*</span></label>
              <input id="mp-cf" type="password" bind:value={cfApiKey} autocomplete="off" />
              <div class="hint">CurseForge 來源需 API 金鑰;存入金鑰庫。</div>
            </div>
          {/if}
        {/if}
      {/if}

      {#if error}
        <div class="err-box">{error}</div>
      {/if}

      <div class="actions">
        <button on:click={() => dispatch('close')} disabled={submitting}>取消</button>
        <button class="primary" on:click={submit} disabled={!canSubmit}>
          {submitting ? '建立中…' : '建立'}
        </button>
      </div>
    {/if}
  {/if}
</Modal>

<style>
  .sect {
    margin: 20px 0 12px;
    padding-bottom: 6px;
    border-bottom: 1px solid var(--line);
    color: var(--fg-1);
  }
  .err-box {
    background: rgba(248, 81, 73, 0.12);
    border: 1px solid var(--err);
    border-radius: var(--radius-sm);
    padding: 10px 12px;
    margin: 14px 0;
    color: var(--fg-0);
    white-space: pre-wrap;
    font-size: 13px;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
    margin-top: 20px;
  }
</style>
