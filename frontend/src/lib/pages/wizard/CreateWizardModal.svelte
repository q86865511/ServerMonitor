<script lang="ts">
  // 四步驟建立精靈(R8):①基本資訊 →②選擇範本 →③資源與參數 →④確認建立。
  // 全部邏輯自舊 lib/CreateWizard.svelte 功能等價搬移:runtime 選擇/DockerAvailable 置灰/
  // memory_mb+cpu_percent 僅 native/CF 金鑰三態/provision 進度/blocked-mods 對話框/BrowserOpenURL。
  // provision 為全域事件、僅精靈消費 —— 依 R8 為「元件不自行 EventsOn」規則的明訂例外,
  // 隨精靈生命週期 EventsOn/EventsOff(關閉任何路徑皆解除,見 onDestroy)。
  import { onMount, onDestroy } from 'svelte';
  import { main } from '../../../../wailsjs/go/models';
  import {
    ListTemplates,
    CreateInstance,
    DockerAvailable,
    CurseForgeEnabled,
  } from '../../../../wailsjs/go/main/App';
  import { EventsOn, EventsOff, BrowserOpenURL } from '../../../../wailsjs/runtime/runtime';
  import { call, errMsg } from '../../api';
  import { pushToast } from '../../stores';
  import { nodeStatuses } from '../../stores/instances';
  import Modal from '../../ui/Modal.svelte';
  import Button from '../../ui/Button.svelte';
  import ConfirmDialog from '../../ui/ConfirmDialog.svelte';
  import ErrorState from '../../ui/ErrorState.svelte';
  import Step1Basic from './Step1Basic.svelte';
  import Step2Template from './Step2Template.svelte';
  import Step3Config from './Step3Config.svelte';
  import Step4Confirm from './Step4Confirm.svelte';

  let {
    onClose,
    onCreated,
  }: {
    onClose: () => void;
    onCreated: (uuid: string) => void;
  } = $props();

  const CF_KEY = 'CF_API_KEY';

  type Provision = { stage?: string; percent?: number; detail?: string };
  interface BlockedMod {
    project_id: number;
    file_id: number;
    file_name: string;
    url: string;
  }

  interface WizardForm {
    name: string;
    templateId: string;
    variant: string;
    paramValues: Record<string, string>;
    secretValues: Record<string, string>;
    runtime: string;
    memoryMB: string;
    cpuPercent: string;
    modpackType: string;
    modpackRef: string;
    cfApiKey: string;
    // 與各步子元件的 WizardForm 介面結構相容(其宣告了 index signature)。
    [k: string]: unknown;
  }

  const STEPS = [
    { n: 1, label: '基本資訊' },
    { n: 2, label: '選擇範本' },
    { n: 3, label: '資源與參數' },
    { n: 4, label: '確認建立' },
  ];

  let templates = $state<main.TemplateDTO[]>([]);
  let loading = $state(true);
  let dockerAvailable = $state(true);
  let cfEnabled = $state(false);

  let step = $state(1);
  let submitting = $state(false);
  let error = $state('');
  let showCloseConfirm = $state(false);

  let form = $state<WizardForm>({
    name: '',
    templateId: '',
    variant: '',
    paramValues: {},
    secretValues: {},
    runtime: '',
    memoryMB: '',
    cpuPercent: '',
    modpackType: '',
    modpackRef: '',
    cfApiKey: '',
  });

  // 供應進度(建立期間);stage 空=尚無進度;percent<0=不確定態。
  let provStage = $state('');
  let provPercent = $state(-1);
  let provDetail = $state('');
  let blockedMods = $state<BlockedMod[]>([]);
  let unlistenProv: (() => void) | null = null;

  const nodeLabel = $derived($nodeStatuses[0]?.node || 'local');
  const tmpl = $derived(templates.find((t) => t.id === form.templateId));

  const isCFSource = $derived(form.modpackType === 'curseforge' || form.modpackType === 'manual-cfzip');
  const needsCFKey = $derived(isCFSource && form.runtime !== 'native');
  const cfNativeUnavailable = $derived(isCFSource && form.runtime === 'native' && !cfEnabled);
  const cfDisabled = $derived(form.runtime === 'native' && !cfEnabled);
  const templateHasCFSecret = $derived((tmpl?.secrets ?? []).some((s) => s.key === CF_KEY));
  const modpackRefPlaceholder = $derived(
    form.modpackType === 'modrinth'
      ? 'Modrinth slug 或專案 URL'
      : form.modpackType === 'curseforge'
        ? form.runtime === 'native'
          ? 'projectID:fileID 或 cfzip 直接下載連結(https)'
          : 'CurseForge slug 或整合頁 URL'
        : form.modpackType.startsWith('manual')
          ? '本機檔案完整路徑'
          : '',
  );

  // docker-only 範本 + Docker 不可用 → 無可用執行後端(阻擋前進)。
  const noRuntimeAvailable = $derived.by(() => {
    const rts = tmpl?.runtimes ?? [];
    if (rts.length === 0) return false;
    return rts.every((r) => r === 'docker') && !dockerAvailable;
  });

  const missingParams = $derived(
    (tmpl?.params ?? []).filter((p) => {
      if (!p.required) return false;
      const v = form.paramValues[p.key] ?? '';
      return p.type === 'bool' ? v !== 'true' : v.trim() === '';
    }),
  );
  const missingSecrets = $derived(
    (tmpl?.secrets ?? []).filter((s) => (form.secretValues[s.key] ?? '').trim() === ''),
  );
  const modpackIncomplete = $derived(form.modpackType !== '' && form.modpackRef.trim() === '');
  const cfKeyMissing = $derived(needsCFKey && !templateHasCFSecret && form.cfApiKey.trim() === '');

  const canStep3 = $derived(
    !!tmpl &&
      !noRuntimeAvailable &&
      missingParams.length === 0 &&
      missingSecrets.length === 0 &&
      !modpackIncomplete &&
      !cfKeyMissing &&
      !cfNativeUnavailable,
  );
  const canSubmit = $derived(canStep3 && !submitting);

  const missingItems = $derived.by(() => {
    const items: string[] = [];
    if (noRuntimeAvailable)
      items.push('無可用執行後端(此範本僅支援 Docker,但未偵測到可用 Docker)');
    for (const p of missingParams) items.push(`必填參數:${p.label || p.key}`);
    for (const s of missingSecrets) items.push(`必填機密:${s.label || s.key}`);
    if (modpackIncomplete) items.push('模組包來源位置未填');
    if (cfKeyMissing) items.push('CF_API_KEY 未填');
    if (cfNativeUnavailable) items.push('native 模式未啟用 CurseForge,請改用其他來源或後端');
    return items;
  });

  const dirty = $derived(form.name.trim() !== '' || form.templateId !== '');

  function canNext(n: number): boolean {
    if (n === 1) return true;
    if (n === 2) return !!tmpl;
    if (n === 3) return canStep3;
    return false;
  }

  // 範本載入失敗與「真的沒有範本」分開呈現(R16):失敗顯 ErrorState 可原地重試,不誤導成空。
  let tplError = $state('');
  async function loadTemplates(): Promise<void> {
    loading = true;
    tplError = '';
    try {
      templates = await call(() => ListTemplates(), { silent: true });
    } catch (err) {
      tplError = errMsg(err);
    }
    loading = false;
  }

  onMount(async () => {
    await loadTemplates();
    try {
      dockerAvailable = await call(() => DockerAvailable(), { silent: true });
    } catch {
      dockerAvailable = false; // 查詢失敗保守置灰
    }
    try {
      cfEnabled = await call(() => CurseForgeEnabled(), { silent: true });
    } catch {
      cfEnabled = false; // 查詢失敗保守視為未啟用
    }
  });

  onDestroy(() => {
    // 精靈關閉(任何路徑,含成功 navigate 後卸載)一律解除 provision 訂閱。
    if (unlistenProv) unlistenProv();
    EventsOff('provision');
    unlistenProv = null;
  });

  // 切換範本:重置範本相關欄位(等價舊版 onTemplateChange);同一範本重選不重置(保留輸入)。
  function selectTemplate(id: string): void {
    if (id === form.templateId) return;
    form.templateId = id;
    error = '';
    const t = templates.find((x) => x.id === id);
    form.modpackType = '';
    form.modpackRef = '';
    form.cfApiKey = '';
    form.memoryMB = '';
    form.cpuPercent = '';
    let rt = t?.runtime ?? '';
    if (rt === 'docker' && !dockerAvailable) {
      rt = (t?.runtimes ?? []).find((r) => r !== 'docker') ?? rt;
    }
    form.runtime = rt;
    form.variant = t && t.variants.length > 0 ? t.variants[0].id : '';
    const pv: Record<string, string> = {};
    for (const p of t?.params ?? []) {
      pv[p.key] = p.default != null ? String(p.default) : p.type === 'bool' ? 'false' : '';
    }
    const sv: Record<string, string> = {};
    for (const s of t?.secrets ?? []) sv[s.key] = '';
    form.paramValues = pv;
    form.secretValues = sv;
  }

  function next(): void {
    if (step < 4 && canNext(step)) {
      error = '';
      step += 1;
    }
  }
  function back(): void {
    if (step > 1) step -= 1;
  }
  function gotoStep(n: number): void {
    if (n < step) step = n; // 僅允許回到較早步驟(保留輸入);前進須經「下一步」驗證
  }

  function requestClose(): void {
    if (submitting) return; // 建立中不可關閉(防重複/孤兒送出)
    if (dirty) {
      showCloseConfirm = true;
      return;
    }
    onClose();
  }
  function confirmClose(): void {
    showCloseConfirm = false;
    onClose();
  }

  function resetProvision(): void {
    provStage = '';
    provPercent = -1;
    provDetail = '';
    blockedMods = [];
  }

  // 選填數字 → 非負整數;空白/非法視為 0(不限)。
  function parseIntOr0(v: string): number {
    const n = parseInt(v.trim(), 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  }

  function openBlockedModPage(url: string): void {
    if (url) BrowserOpenURL(url);
  }

  async function submit(): Promise<void> {
    if (!tmpl || !canSubmit) return;
    error = '';
    submitting = true;
    resetProvision();

    // provision 訂閱須在 CreateInstance 之前註冊(供應事件於建立同步期間送達)。
    // stage="blocked-mods":detail 為被擋模組 JSON,解析後彈對話框引導手動下載。
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

    const secrets: Record<string, string> = { ...form.secretValues };
    if (needsCFKey && !templateHasCFSecret && form.cfApiKey.trim() !== '') {
      secrets[CF_KEY] = form.cfApiKey.trim();
    }
    const req = main.CreateInstanceRequest.createFrom({
      name: form.name.trim(),
      template_id: tmpl.id,
      variant: form.variant,
      params: { ...form.paramValues },
      secrets,
      node: '',
      runtime: form.runtime,
      memory_mb: form.runtime === 'native' ? parseIntOr0(form.memoryMB) : 0,
      cpu_percent: form.runtime === 'native' ? parseIntOr0(form.cpuPercent) : 0,
      modpack:
        form.modpackType !== ''
          ? { type: form.modpackType, ref: form.modpackRef.trim() }
          : undefined,
    });

    try {
      const uuid = await call(() => CreateInstance(req), { silent: true });
      pushToast('success', `已建立實例 ${uuid}`);
      onCreated(uuid); // 父層負責關閉精靈 + navigate + refresh
    } catch (e) {
      // 失敗訊息帶最後供應階段,便於指出卡在哪一步。
      const base = errMsg(e);
      error = provStage ? `${base}(供應階段:${provStage})` : base;
    } finally {
      submitting = false;
      if (unlistenProv) unlistenProv();
      EventsOff('provision');
      unlistenProv = null;
    }
  }
</script>

<Modal title="建立伺服器" wide onClose={requestClose}>
  {#if loading}
    <div class="msg">載入範本中…</div>
  {:else if tplError}
    <ErrorState message={`載入範本失敗:${tplError}`} onRetry={loadTemplates} />
  {:else if templates.length === 0}
    <div class="msg">無可用範本。</div>
  {:else}
    <div class="wizard">
      <nav class="rail" aria-label="建立步驟">
        {#each STEPS as s}
          <button
            type="button"
            class="rail-item"
            class:current={step === s.n}
            class:done={step > s.n}
            disabled={s.n >= step}
            onclick={() => gotoStep(s.n)}
          >
            <span class="badge">{step > s.n ? '✓' : s.n}</span>
            <span class="rail-label">{s.label}</span>
          </button>
        {/each}
      </nav>

      <section class="content">
        {#if step === 1}
          <Step1Basic {form} {nodeLabel} />
        {:else if step === 2}
          <Step2Template
            {templates}
            selectedId={form.templateId}
            variant={form.variant}
            {tmpl}
            onSelect={selectTemplate}
            onVariant={(v) => (form.variant = v)}
          />
          {#if !tmpl}
            <div class="foot-hint">請先選擇一個範本以繼續。</div>
          {/if}
        {:else if step === 3 && tmpl}
          <Step3Config
            {form}
            {tmpl}
            {dockerAvailable}
            {cfDisabled}
            {cfNativeUnavailable}
            {needsCFKey}
            {templateHasCFSecret}
            {modpackRefPlaceholder}
            {noRuntimeAvailable}
          />
          {#if missingItems.length > 0}
            <div class="missing">
              <div class="missing-title">尚需完成才能繼續:</div>
              <ul>
                {#each missingItems as m}<li>{m}</li>{/each}
              </ul>
            </div>
          {/if}
        {:else if step === 4 && tmpl}
          <Step4Confirm
            {form}
            {tmpl}
            {nodeLabel}
            {submitting}
            {provStage}
            {provPercent}
            {provDetail}
            {error}
          />
        {/if}
      </section>
    </div>
  {/if}

  {#snippet footer()}
    <div class="nav">
      <div class="nav-left">
        {#if step > 1}
          <Button variant="secondary" disabled={submitting} onclick={back}>上一步</Button>
        {/if}
      </div>
      <div class="nav-right">
        <Button variant="ghost" disabled={submitting} onclick={requestClose}>取消</Button>
        {#if step < 4}
          <Button variant="primary" disabled={!canNext(step)} onclick={next}>下一步</Button>
        {:else}
          <Button variant="primary" loading={submitting} disabled={!canSubmit} onclick={submit}>
            建立
          </Button>
        {/if}
      </div>
    </div>
  {/snippet}
</Modal>

{#if blockedMods.length > 0}
  <Modal title="需手動下載的模組(CurseForge)" onClose={() => (blockedMods = [])}>
    <div class="hint hint-block">
      以下模組的作者已停用第三方 API 散布,無法自動下載。請點各項「開啟下載頁」以瀏覽器手動下載
      對應檔案,放入實例的匯入資料夾後重新建立(系統以檔名比對自動匯入續裝)。其餘可下載的模組
      與設定已安裝完成。
    </div>
    <ul class="blocked-list">
      {#each blockedMods as m}
        <li class="blocked-item">
          <div class="blocked-name" title={m.file_name}>{m.file_name || `檔案 #${m.file_id}`}</div>
          <Button size="sm" variant="secondary" onclick={() => openBlockedModPage(m.url)}>
            開啟下載頁
          </Button>
        </li>
      {/each}
    </ul>
    <div class="blocked-foot">
      <Button variant="primary" onclick={() => (blockedMods = [])}>知道了</Button>
    </div>
  </Modal>
{/if}

{#if showCloseConfirm}
  <ConfirmDialog
    title="放棄建立?"
    message="已填入的內容尚未送出,關閉後將遺失。確定要關閉精靈嗎?"
    confirmLabel="關閉"
    danger
    onConfirm={confirmClose}
    onCancel={() => (showCloseConfirm = false)}
  />
{/if}

<style>
  .msg {
    padding: var(--space-6);
    text-align: center;
    color: var(--fg-2);
  }
  .wizard {
    display: grid;
    grid-template-columns: 180px 1fr;
    gap: var(--space-5);
    min-height: 340px;
  }
  .rail {
    display: flex;
    flex-direction: column;
    gap: 4px;
    border-right: 1px solid var(--line);
    padding-right: var(--space-4);
  }
  .rail-item {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    background: transparent;
    border: none;
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    text-align: left;
    color: var(--fg-2);
    cursor: pointer;
    font-size: var(--text-sm);
    transition: background-color var(--dur-fast) var(--ease-standard),
      color var(--dur-fast) var(--ease-standard);
  }
  .rail-item:not(:disabled):hover {
    background-color: var(--bg-3);
    color: var(--fg-0);
  }
  .rail-item.current {
    color: var(--fg-0);
    font-weight: 600;
  }
  .rail-item.done {
    color: var(--fg-1);
  }
  .rail-item:disabled {
    cursor: default;
  }
  .rail-item:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 22px;
    height: 22px;
    flex: none;
    border-radius: var(--radius-full);
    background-color: var(--bg-3);
    color: var(--fg-1);
    font-size: var(--text-xs);
    font-weight: 700;
  }
  .rail-item.current .badge {
    background-color: var(--accent);
    color: var(--fg-on-accent);
  }
  .rail-item.done .badge {
    background-color: var(--ok);
    color: var(--fg-on-accent);
  }
  .content {
    min-width: 0;
  }
  .foot-hint {
    margin-top: var(--space-3);
    font-size: var(--text-sm);
    color: var(--fg-2);
  }
  .missing {
    margin-top: var(--space-4);
    background-color: var(--err-bg);
    border: 1px solid var(--err);
    border-radius: var(--radius-sm);
    padding: 10px 12px;
  }
  .missing-title {
    font-size: var(--text-sm);
    color: var(--fg-0);
    font-weight: 600;
    margin-bottom: 4px;
  }
  .missing ul {
    margin: 0;
    padding-left: 18px;
    color: var(--fg-1);
    font-size: var(--text-sm);
  }
  .nav {
    display: flex;
    align-items: center;
    justify-content: space-between;
    width: 100%;
  }
  .nav-right {
    display: flex;
    gap: var(--space-2);
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
  .hint-block {
    margin-bottom: 12px;
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
    gap: var(--space-3);
    padding: 8px 0;
    border-bottom: 1px solid var(--line);
  }
  .blocked-name {
    font-size: var(--text-sm);
    color: var(--fg-0);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .blocked-foot {
    display: flex;
    justify-content: flex-end;
    margin-top: var(--space-4);
  }
</style>
