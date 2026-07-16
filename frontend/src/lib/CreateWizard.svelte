<script lang="ts">
  import { onMount, onDestroy, createEventDispatcher } from 'svelte';
  import { main } from '../../wailsjs/go/models';
  import {
    ListTemplates,
    CreateInstance,
    DockerAvailable,
    CurseForgeEnabled,
  } from '../../wailsjs/go/main/App';
  import { EventsOn, EventsOff, BrowserOpenURL } from '../../wailsjs/runtime/runtime';
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

  // Runtime 選擇(native-backend R12):runtime 為選定後端;dockerAvailable 決定 docker 選項是否置灰。
  let runtime = '';
  let dockerAvailable = true;
  // native 資源上限(選填;R9)。空字串=不限。
  let memoryMB = '';
  let cpuPercent = '';

  // 供應進度(建立期間;R12)。stage 空=尚無進度;percent<0=不確定態(如查版本/跑 installer)。
  type Provision = { stage?: string; percent?: number; detail?: string };
  let provStage = '';
  let provPercent = -1;
  let provDetail = '';
  let unlistenProv: (() => void) | null = null;

  // 模組包(R11):type 空=無;docker/itzg 路徑的 curseforge/manual-cfzip 需使用者填 CF_API_KEY。
  const CF_KEY = 'CF_API_KEY';
  let modpackType = '';
  let modpackRef = '';
  let cfApiKey = '';

  // CurseForge 於 native 模式(R14):由建置內嵌/設定覆蓋的專案 key 啟用,使用者不另填 key。
  // cfEnabled=false 時 native 路徑的 CurseForge 選項置灰並提示。
  let cfEnabled = false;

  // 被擋模組(R14):CurseForge 作者停用第三方散布(downloadUrl=null)時,provision 事件
  // stage="blocked-mods" 攜 JSON 清單;解析後彈出對話框引導使用者手動下載。
  type BlockedMod = {
    project_id: number;
    file_id: number;
    file_name: string;
    url: string;
  };
  let blockedMods: BlockedMod[] = [];

  $: tmpl = templates.find((t) => t.id === templateId);
  $: isCFSource = modpackType === 'curseforge' || modpackType === 'manual-cfzip';
  // 使用者填 key 的 CF_API_KEY 欄位僅 docker/itzg 路徑需要;native 用內嵌/設定 key,不另填。
  $: needsCFKey = isCFSource && runtime !== 'native';
  // native + CF 但未啟用(無內嵌/設定 key):不可送出,提示改用其他來源或於設定填 key。
  $: cfNativeUnavailable = isCFSource && runtime === 'native' && !cfEnabled;
  // native 模式且 CF 未啟用時,下拉的 CurseForge 選項置灰。
  $: cfDisabled = runtime === 'native' && !cfEnabled;
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
    }
    try {
      dockerAvailable = await call(() => DockerAvailable(), { silent: true });
    } catch {
      dockerAvailable = false; // 查詢失敗一律視為不可用(保守置灰)
    }
    try {
      cfEnabled = await call(() => CurseForgeEnabled(), { silent: true });
    } catch {
      cfEnabled = false; // 查詢失敗視為未啟用(保守隱藏 native CF)
    }
    loading = false;
  });

  onDestroy(() => {
    if (unlistenProv) unlistenProv();
    EventsOff('provision');
  });

  function onTemplateChange(): void {
    error = '';
    paramValues = {};
    secretValues = {};
    modpackType = '';
    modpackRef = '';
    cfApiKey = '';
    memoryMB = '';
    cpuPercent = '';
    // 預選範本的預設 runtime;若預設為 docker 但不可用,退回其他可用能力(避免預選一個置灰項)。
    runtime = tmpl?.runtime ?? '';
    if (runtime === 'docker' && !dockerAvailable) {
      runtime = (tmpl?.runtimes ?? []).find((r) => r !== 'docker') ?? runtime;
    }
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
    !cfKeyMissing &&
    !cfNativeUnavailable;

  async function submit(): Promise<void> {
    if (!tmpl || !canSubmit) return;
    error = '';
    submitting = true;
    resetProvision();

    // 訂閱供應進度:須在 CreateInstance 之前註冊(供應事件於建立同步期間送達)。
    // stage="blocked-mods" 為 R14 特例:detail 為被擋模組 JSON 清單,解析後彈對話框引導手動下載。
    unlistenProv = EventsOn('provision', (p: Provision) => {
      if (p.stage === 'blocked-mods') {
        try {
          blockedMods = JSON.parse(p.detail ?? '[]') as BlockedMod[];
        } catch {
          blockedMods = [];
        }
        return;
      }
      provStage = p.stage ?? '';
      provPercent = typeof p.percent === 'number' ? p.percent : -1;
      provDetail = p.detail ?? '';
    });

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
      runtime,
      memory_mb: runtime === 'native' ? parseIntOr0(memoryMB) : 0,
      cpu_percent: runtime === 'native' ? parseIntOr0(cpuPercent) : 0,
      modpack:
        modpackType !== '' ? { type: modpackType, ref: modpackRef.trim() } : undefined,
    });

    try {
      const uuid = await call(() => CreateInstance(req), { silent: true });
      pushToast('success', `已建立實例 ${uuid}`);
      dispatch('created', uuid);
    } catch (e) {
      // 失敗訊息帶最後的供應階段,便於指出卡在哪個供應步驟(R12)。
      const base = e == null ? '建立失敗' : typeof e === 'string' ? e : String((e as Error).message ?? e);
      error = provStage ? `${base}(供應階段:${provStage})` : base;
    } finally {
      submitting = false;
      if (unlistenProv) unlistenProv();
      EventsOff('provision');
      unlistenProv = null;
    }
  }

  function resetProvision(): void {
    provStage = '';
    provPercent = -1;
    provDetail = '';
    blockedMods = [];
  }

  // parseIntOr0 把選填數字輸入轉為非負整數;空白或非法值視為 0(不限)。
  function parseIntOr0(v: string): number {
    const n = parseInt(v.trim(), 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  }

  // openBlockedModPage 以系統瀏覽器開啟被擋模組的手動下載頁(R14 降級方案:使用者下載後重試建立)。
  function openBlockedModPage(url: string): void {
    if (url) BrowserOpenURL(url);
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

      {#if (tmpl.runtimes ?? []).length > 0}
        <div class="field">
          <label>執行後端<span class="req">*</span></label>
          <div class="runtime-opts">
            {#each tmpl.runtimes as rt}
              {@const disabled = rt === 'docker' && !dockerAvailable}
              <label class="runtime-opt" class:disabled>
                <input
                  type="radio"
                  name="runtime"
                  value={rt}
                  checked={runtime === rt}
                  {disabled}
                  on:change={() => (runtime = rt)}
                />
                <span class="rt-name">{rt === 'native' ? '本機行程(native)' : 'Docker 容器'}</span>
                {#if disabled}<span class="rt-note">未偵測到 Docker</span>{/if}
              </label>
            {/each}
          </div>
          <div class="hint">
            {runtime === 'native'
              ? '本機行程:直接在本機執行,無容器隔離;自動供應 Java／伺服器檔案,免安裝 Docker。'
              : runtime === 'docker'
                ? 'Docker 容器:需 Docker Desktop,提供檔案系統與網路隔離。'
                : '選擇此實例的執行方式。'}
          </div>
        </div>

        {#if runtime === 'native'}
          <div class="field grid2">
            <div>
              <label for="mem-mb">記憶體上限 MB(選填)</label>
              <input
                id="mem-mb"
                type="number"
                min="0"
                bind:value={memoryMB}
                placeholder="不限"
              />
            </div>
            <div>
              <label for="cpu-pct">CPU 上限 %(選填)</label>
              <input
                id="cpu-pct"
                type="number"
                min="0"
                max="100"
                bind:value={cpuPercent}
                placeholder="不限"
              />
            </div>
          </div>
          <div class="hint">留空表示不限額;native 以 Windows Job Objects 強制上限。</div>
        {/if}
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
            <option value="curseforge" disabled={cfDisabled}
              >CurseForge{cfDisabled ? '(未啟用)' : ''}</option
            >
            <option value="manual-mrpack">手動 .mrpack 檔</option>
            <option value="manual-cfzip" disabled={cfDisabled}
              >手動 CurseForge zip 檔{cfDisabled ? '(未啟用)' : ''}</option
            >
          </select>
          {#if cfDisabled}
            <div class="hint">
              native 模式的 CurseForge 需建置內嵌 API 金鑰,或於設定填入自己的金鑰(比照 Prism
              Launcher);目前未啟用。可改用 Modrinth,或改以 Docker 後端(使用者自填 CF_API_KEY)。
            </div>
          {/if}
        </div>
        {#if cfNativeUnavailable}
          <div class="err-box">
            已選 CurseForge 但 native 模式未啟用 CurseForge(無內嵌/設定金鑰)。請改選其他來源或後端。
          </div>
        {/if}
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

      {#if submitting && (provStage || runtime === 'native')}
        <div class="prov-box">
          <div class="prov-head">
            <span class="prov-stage">{provStage || '準備供應…'}</span>
            {#if provPercent >= 0}<span class="prov-pct">{Math.round(provPercent)}%</span>{/if}
          </div>
          <div class="bar" class:indet={provPercent < 0}>
            <div class="fill" style={provPercent >= 0 ? `width:${Math.min(100, provPercent)}%` : ''}></div>
          </div>
          {#if provDetail}<div class="prov-detail muted">{provDetail}</div>{/if}
        </div>
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

{#if blockedMods.length > 0}
  <Modal title="需手動下載的模組(CurseForge)" on:close={() => (blockedMods = [])}>
    <div class="hint" style="margin-bottom:12px">
      以下模組的作者已停用第三方 API 散布,無法自動下載。請點各項「開啟下載頁」以瀏覽器手動下載
      對應檔案,放入實例的匯入資料夾後重新建立(系統會以檔名比對自動匯入續裝)。其餘可下載的模組
      與設定已安裝完成。
    </div>
    <ul class="blocked-list">
      {#each blockedMods as m}
        <li class="blocked-item">
          <div class="blocked-name" title={m.file_name}>{m.file_name || `檔案 #${m.file_id}`}</div>
          <button class="link-btn" on:click={() => openBlockedModPage(m.url)}>開啟下載頁</button>
        </li>
      {/each}
    </ul>
    <div class="actions">
      <button class="primary" on:click={() => (blockedMods = [])}>知道了</button>
    </div>
  </Modal>
{/if}

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
  .runtime-opts {
    display: flex;
    flex-wrap: wrap;
    gap: 10px;
  }
  .runtime-opt {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 8px 12px;
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    cursor: pointer;
    font-size: 13px;
  }
  .runtime-opt.disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .runtime-opt .rt-note {
    font-size: 11px;
    color: var(--fg-2);
  }
  .grid2 {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }
  .prov-box {
    background: var(--bg-2, rgba(127, 127, 127, 0.08));
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    padding: 12px;
    margin: 14px 0;
  }
  .prov-head {
    display: flex;
    justify-content: space-between;
    font-size: 13px;
    margin-bottom: 8px;
  }
  .prov-pct {
    font-variant-numeric: tabular-nums;
    color: var(--fg-1);
  }
  .prov-detail {
    font-size: 12px;
    margin-top: 6px;
    word-break: break-all;
  }
  .bar {
    height: 6px;
    background: var(--bg-3);
    border-radius: 3px;
    overflow: hidden;
  }
  .bar .fill {
    height: 100%;
    background: var(--accent);
    transition: width 0.2s ease;
  }
  /* 不確定態:百分比未知(percent<0),以動畫條表達進行中。 */
  .bar.indet .fill {
    width: 40%;
    animation: indet 1.1s ease-in-out infinite;
  }
  @keyframes indet {
    0% {
      margin-left: -40%;
    }
    100% {
      margin-left: 100%;
    }
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
    margin-top: 20px;
  }
  .blocked-list {
    list-style: none;
    margin: 0;
    padding: 0;
    max-height: 320px;
    overflow-y: auto;
  }
  .blocked-item {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 8px 0;
    border-bottom: 1px solid var(--line);
  }
  .blocked-name {
    font-size: 13px;
    color: var(--fg-0);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .link-btn {
    flex: 0 0 auto;
    font-size: 12px;
  }
</style>
