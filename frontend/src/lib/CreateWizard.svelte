<script lang="ts">
  import { onMount, createEventDispatcher } from 'svelte';
  import { main } from '../../wailsjs/go/models';
  import { ListTemplates, CreateInstance } from '../../wailsjs/go/main/App';
  import { call } from './api';
  import { pushToast } from './stores';
  import Modal from './Modal.svelte';
  import Icon from './Icon.svelte';

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
    <div class="empty">正在載入遊戲範本…</div>
  {:else if templates.length === 0}
    <div class="empty">無可用範本。</div>
  {:else}
    <div class="wizard">
      <aside class="wizard-rail">
        <div class="rail-code mono">DEPLOYMENT PROFILE</div>
        <div class="rail-title">建立新實例</div>
        <p class="rail-copy">依範本配置遊戲、執行參數與存取機密。</p>

        <div class="rail-steps" aria-label="建立內容摘要">
          <div class:active={!tmpl} class:done={!!tmpl}>
            <span class="step-icon"><Icon name="server" size={15} /></span>
            <span><strong>遊戲設定</strong><small>範本與變體</small></span>
          </div>
          <div class:active={!!tmpl}>
            <span class="step-icon"><Icon name="settings" size={15} /></span>
            <span><strong>執行配置</strong><small>參數與模組包</small></span>
          </div>
          <div>
            <span class="step-icon"><Icon name="shield" size={15} /></span>
            <span><strong>安全資料</strong><small>OS 金鑰庫</small></span>
          </div>
        </div>

        {#if tmpl}
          <div class="profile-summary">
            <span class="summary-label mono">SELECTED PROFILE</span>
            <strong>{tmpl.name}</strong>
            <span class="mono">{tmpl.id} / {variant || 'default'}</span>
          </div>
        {/if}

        <div class="secure-note">
          <Icon name="shield" size={16} />
          <span>密碼與 API 金鑰只寫入 Windows 金鑰庫，不會明文儲存。</span>
        </div>
      </aside>

      <div class="wizard-form">
        <section class="form-section">
          <div class="section-heading">
            <span class="section-index mono">01</span>
            <div><h4>遊戲範本</h4><p>選擇要部署的伺服器類型。</p></div>
          </div>
          <div class="field">
            <label for="tmpl">遊戲範本<span class="req">*</span></label>
            <select id="tmpl" bind:value={templateId} on:change={onTemplateChange} data-autofocus>
              <option value="" disabled>請選擇範本…</option>
              {#each templates as t}
                <option value={t.id}>{t.name} ({t.id})</option>
              {/each}
            </select>
          </div>

          {#if tmpl && tmpl.variants.length > 0}
            <div class="field">
              <label for="variant">變體<span class="req">*</span></label>
              <select id="variant" bind:value={variant}>
                {#each tmpl.variants as v}
                  <option value={v.id}>{v.id}{v.loader ? ` (${v.loader})` : ''}</option>
                {/each}
              </select>
            </div>
          {/if}
        </section>

        {#if tmpl}
          {#if tmpl.params.length > 0}
            <section class="form-section">
              <div class="section-heading">
                <span class="section-index mono">02</span>
                <div><h4>執行參數</h4><p>套用範本需要的遊戲設定。</p></div>
              </div>
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
                      <label for={`p-${p.key}`}>
                        {p.label || p.key}{#if p.required}<span class="req">*</span>{/if}
                      </label>
                    </div>
                    {#if p.required}<div class="hint">此為必要同意項目，未勾選前無法建立。</div>{/if}
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
            </section>
          {/if}

          {#if tmpl.secrets.length > 0}
            <section class="form-section">
              <div class="section-heading">
                <span class="section-index mono">03</span>
                <div><h4>機密資料</h4><p>安全寫入作業系統金鑰庫。</p></div>
              </div>
              {#each tmpl.secrets as s}
                <div class="field">
                  <label for={`s-${s.key}`}>{s.label || s.key}<span class="req">*</span></label>
                  <input id={`s-${s.key}`} type="password" bind:value={secretValues[s.key]} autocomplete="off" />
                </div>
              {/each}
            </section>
          {/if}

          {#if tmpl.modpack}
            <section class="form-section">
              <div class="section-heading">
                <span class="section-index mono">04</span>
                <div><h4>模組包</h4><p>選填，可使用遠端來源或本機檔案。</p></div>
              </div>
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
                    <div class="hint">CurseForge 來源需 API 金鑰；此值會存入金鑰庫。</div>
                  </div>
                {/if}
              {/if}
            </section>
          {/if}

          {#if error}
            <div class="err-box" role="alert"><Icon name="alert" size={16} /><span>{error}</span></div>
          {/if}

          <div class="wizard-actions">
            <div class="validation-copy">
              {#if canSubmit}<span class="ready"><Icon name="check" size={13} />配置完整</span>{:else}<span>請完成所有必要欄位</span>{/if}
            </div>
            <button on:click={() => dispatch('close')} disabled={submitting}>取消</button>
            <button class="primary" on:click={submit} disabled={!canSubmit}>
              <Icon name="server" size={14} />
              <span>{submitting ? '建立中…' : '建立實例'}</span>
            </button>
          </div>
        {:else}
          <div class="select-prompt">
            <Icon name="server" size={25} />
            <p>選擇範本後顯示可用參數與部署選項。</p>
          </div>
        {/if}
      </div>
    </div>
  {/if}
</Modal>

<style>
  .wizard {
    display: grid;
    min-height: 560px;
    grid-template-columns: 230px minmax(0, 1fr);
    margin: -20px;
  }

  .wizard-rail {
    display: flex;
    flex-direction: column;
    padding: 24px 20px;
    background: var(--bg-inset);
    border-right: 1px solid var(--line);
  }

  .rail-code {
    color: var(--accent);
    font-size: 8px;
    font-weight: 700;
    letter-spacing: 0.13em;
  }

  .rail-title {
    margin-top: 5px;
    font-size: 18px;
    font-weight: 700;
  }

  .rail-copy {
    margin-top: 7px;
    color: var(--fg-2);
    font-size: 11px;
    line-height: 1.55;
  }

  .rail-steps {
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin-top: 28px;
  }

  .rail-steps > div {
    display: grid;
    grid-template-columns: 31px 1fr;
    align-items: center;
    gap: 9px;
    padding: 8px;
    color: var(--fg-3);
    border-left: 2px solid transparent;
  }

  .rail-steps > div.active {
    color: var(--fg-0);
    background: var(--bg-1);
    border-left-color: var(--accent);
  }

  .rail-steps > div.done .step-icon { color: var(--ok); }

  .step-icon {
    display: grid;
    width: 29px;
    height: 29px;
    place-items: center;
    border: 1px solid var(--line);
  }

  .rail-steps strong,
  .rail-steps small {
    display: block;
  }

  .rail-steps strong { font-size: 11px; }
  .rail-steps small { margin-top: 1px; color: var(--fg-3); font-size: 9px; }

  .profile-summary {
    display: flex;
    flex-direction: column;
    gap: 3px;
    margin-top: 20px;
    padding: 11px;
    background: var(--bg-1);
    border: 1px solid var(--line);
  }

  .profile-summary strong { font-size: 12px; }
  .profile-summary > span:last-child { color: var(--fg-2); font-size: 9px; }
  .summary-label { color: var(--fg-3); font-size: 8px; letter-spacing: .1em; }

  .secure-note {
    display: flex;
    align-items: flex-start;
    gap: 9px;
    margin-top: auto;
    padding-top: 18px;
    color: var(--fg-3);
    border-top: 1px solid var(--line);
    font-size: 10px;
    line-height: 1.5;
  }

  .secure-note :global(svg) { flex: none; color: var(--ok); }

  .wizard-form {
    min-width: 0;
    padding: 22px 24px;
    background: var(--bg-1);
  }

  .form-section + .form-section {
    margin-top: 24px;
    padding-top: 21px;
    border-top: 1px solid var(--line);
  }

  .section-heading {
    display: grid;
    grid-template-columns: 28px 1fr;
    gap: 9px;
    margin-bottom: 16px;
  }

  .section-index {
    color: var(--accent);
    font-size: 10px;
    font-weight: 700;
  }

  .section-heading h4 { color: var(--fg-0); font-size: 13px; }
  .section-heading p { margin-top: 2px; color: var(--fg-3); font-size: 10px; }

  .err-box {
    display: flex;
    align-items: flex-start;
    gap: 9px;
    margin: 18px 0;
    padding: 10px 12px;
    color: #f0aaa3;
    background: rgba(217, 108, 98, 0.08);
    border: 1px solid rgba(217, 108, 98, 0.6);
    border-radius: var(--radius-sm);
    font-size: 12px;
    white-space: pre-wrap;
  }

  .wizard-actions {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 8px;
    margin-top: 25px;
    padding-top: 16px;
    border-top: 1px solid var(--line);
  }

  .wizard-actions button,
  .ready {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }

  .validation-copy {
    margin-right: auto;
    color: var(--fg-3);
    font-size: 10px;
  }

  .validation-copy .ready { color: var(--ok); }

  .select-prompt {
    display: grid;
    min-height: 240px;
    place-items: center;
    align-content: center;
    gap: 12px;
    color: var(--fg-3);
    border: 1px dashed var(--line-strong);
    font-size: 12px;
    text-align: center;
  }

  @media (max-width: 720px) {
    .wizard {
      grid-template-columns: 1fr;
      margin: -16px;
    }
    .wizard-rail {
      display: none;
    }
    .wizard-form {
      padding: 18px;
    }
    .wizard-actions {
      align-items: stretch;
      flex-wrap: wrap;
    }
    .validation-copy {
      width: 100%;
    }
  }
</style>
