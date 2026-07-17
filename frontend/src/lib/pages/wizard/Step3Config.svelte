<script lang="ts">
  // 步驟③資源與參數:runtime 選擇(docker 置灰/僅 1 項不顯示/docker-only 不可用→阻擋態)、
  // 資源上限(僅 native)、params/secrets/EULA 動態渲染、模組包來源與 CF 三態。
  // 全部語意自舊 lib/CreateWizard.svelte 等價搬移;所有驗證/派生旗標由父層算好經 props 傳入。
  import type { main } from '../../../../wailsjs/go/models';
  import TextField from '../../ui/TextField.svelte';
  import Select from '../../ui/Select.svelte';

  interface WizardForm {
    paramValues: Record<string, string>;
    secretValues: Record<string, string>;
    runtime: string;
    memoryMB: string;
    cpuPercent: string;
    modpackType: string;
    modpackRef: string;
    cfApiKey: string;
    [k: string]: unknown;
  }

  let {
    form,
    tmpl,
    dockerAvailable,
    cfDisabled,
    cfNativeUnavailable,
    needsCFKey,
    templateHasCFSecret,
    modpackRefPlaceholder,
    noRuntimeAvailable,
  }: {
    form: WizardForm;
    tmpl: main.TemplateDTO;
    dockerAvailable: boolean;
    cfDisabled: boolean;
    cfNativeUnavailable: boolean;
    needsCFKey: boolean;
    templateHasCFSecret: boolean;
    modpackRefPlaceholder: string;
    noRuntimeAvailable: boolean;
  } = $props();

  const runtimes = $derived(tmpl.runtimes ?? []);
  const showRuntimePicker = $derived(runtimes.length > 1);

  function runtimeName(rt: string): string {
    return rt === 'native' ? '本機行程(native)' : rt === 'docker' ? 'Docker 容器' : rt;
  }

  const modpackOptions = $derived([
    { value: '', label: '無' },
    { value: 'modrinth', label: 'Modrinth' },
    { value: 'curseforge', label: `CurseForge${cfDisabled ? '(未啟用)' : ''}`, disabled: cfDisabled },
    { value: 'manual-mrpack', label: '手動 .mrpack 檔' },
    {
      value: 'manual-cfzip',
      label: `手動 CurseForge zip 檔${cfDisabled ? '(未啟用)' : ''}`,
      disabled: cfDisabled,
    },
  ]);
</script>

<div class="step">
  <!-- 執行後端 -->
  {#if runtimes.length > 0}
    {#if noRuntimeAvailable}
      <div class="err-box">
        此範本僅支援 Docker 執行,但未偵測到可用的 Docker。請啟動 Docker Desktop 後重試,
        或改用支援本機行程的範本。<strong>無可用執行後端</strong>,無法建立。
      </div>
    {:else if showRuntimePicker}
      <div class="field">
        <span class="lbl">執行後端<span class="req">*</span></span>
        <div class="runtime-opts">
          {#each runtimes as rt}
            {@const disabled = rt === 'docker' && !dockerAvailable}
            <label class="runtime-opt" class:disabled class:on={form.runtime === rt}>
              <input
                type="radio"
                name="runtime"
                value={rt}
                checked={form.runtime === rt}
                {disabled}
                onchange={() => (form.runtime = rt)}
              />
              <span class="rt-name">{runtimeName(rt)}</span>
              {#if disabled}<span class="rt-note">未偵測到 Docker</span>{/if}
            </label>
          {/each}
        </div>
        <div class="hint">
          {form.runtime === 'native'
            ? '本機行程:直接在本機執行,無容器隔離;自動供應 Java／伺服器檔案,免安裝 Docker。'
            : form.runtime === 'docker'
              ? 'Docker 容器:需 Docker Desktop,提供檔案系統與網路隔離。'
              : '選擇此實例的執行方式。'}
        </div>
      </div>
    {:else}
      <div class="field">
        <span class="lbl">執行後端</span>
        <div class="single-rt">{runtimeName(form.runtime || runtimes[0])}</div>
      </div>
    {/if}
  {/if}

  <!-- 資源上限:僅 native -->
  {#if form.runtime === 'native' && !noRuntimeAvailable}
    <div class="grid2">
      <TextField
        label="記憶體上限 MB(選填)"
        type="number"
        value={form.memoryMB}
        onInput={(v) => (form.memoryMB = v)}
        placeholder="不限"
      />
      <TextField
        label="CPU 上限 %(選填)"
        type="number"
        value={form.cpuPercent}
        onInput={(v) => (form.cpuPercent = v)}
        placeholder="不限"
      />
    </div>
    <div class="hint">留空表示不限額;native 以 Windows Job Objects 強制上限。</div>
  {/if}

  <!-- 參數 -->
  {#if tmpl.params.length > 0}
    <h4 class="sect">參數</h4>
    {#each tmpl.params as p}
      <div class="field">
        {#if p.type === 'bool'}
          <label class="checkbox-row">
            <input
              type="checkbox"
              checked={form.paramValues[p.key] === 'true'}
              onchange={(e) => (form.paramValues[p.key] = e.currentTarget.checked ? 'true' : 'false')}
            />
            <span>{p.label || p.key}{#if p.required}<span class="req">*</span>{/if}</span>
          </label>
          {#if p.required}
            <div class="hint">必填同意項:未勾選不可送出(如 EULA)。</div>
          {/if}
        {:else}
          <TextField
            label={p.label || p.key}
            required={p.required}
            type={p.type === 'int' || p.type === 'number' ? 'number' : 'text'}
            value={form.paramValues[p.key] ?? ''}
            onInput={(v) => (form.paramValues[p.key] = v)}
            placeholder={p.key}
          />
        {/if}
      </div>
    {/each}
  {/if}

  <!-- 機密 -->
  {#if tmpl.secrets.length > 0}
    <h4 class="sect">機密</h4>
    <div class="hint hint-block">以下欄位存入 OS 金鑰庫,不明文落檔。</div>
    {#each tmpl.secrets as s}
      <div class="field">
        <TextField
          label={s.label || s.key}
          required
          type="password"
          value={form.secretValues[s.key] ?? ''}
          onInput={(v) => (form.secretValues[s.key] = v)}
        />
      </div>
    {/each}
  {/if}

  <!-- 模組包 -->
  {#if tmpl.modpack}
    <h4 class="sect">模組包(選填)</h4>
    <div class="field mp-src">
      <Select
        label="來源"
        value={form.modpackType}
        options={modpackOptions}
        onChange={(v) => (form.modpackType = v)}
      />
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

    {#if form.modpackType !== ''}
      <div class="field">
        <TextField
          label="來源位置"
          required
          value={form.modpackRef}
          onInput={(v) => (form.modpackRef = v)}
          placeholder={modpackRefPlaceholder}
        />
      </div>
      {#if needsCFKey && !templateHasCFSecret}
        <div class="field">
          <TextField
            label="CF_API_KEY"
            required
            type="password"
            value={form.cfApiKey}
            onInput={(v) => (form.cfApiKey = v)}
            hint="CurseForge 來源需 API 金鑰;存入金鑰庫。"
          />
        </div>
      {/if}
    {/if}
  {/if}
</div>

<style>
  .step {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .lbl {
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .req {
    color: var(--err);
    margin-left: 3px;
  }
  .sect {
    margin: var(--space-3) 0 4px;
    padding-bottom: 6px;
    border-bottom: 1px solid var(--line);
    color: var(--fg-1);
    font-size: var(--text-md);
  }
  .single-rt {
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 7px 10px;
    color: var(--fg-0);
    font-size: var(--text-base);
  }
  .runtime-opts {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .runtime-opt {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 8px 12px;
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    cursor: pointer;
    font-size: var(--text-sm);
    color: var(--fg-0);
  }
  .runtime-opt.on {
    border-color: var(--accent);
    background-color: var(--accent-bg);
  }
  .runtime-opt.disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .runtime-opt .rt-note {
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
  .checkbox-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-base);
    color: var(--fg-0);
    cursor: pointer;
  }
  .grid2 {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-3);
  }
  .mp-src {
    max-width: 360px;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
  .hint-block {
    margin-bottom: 6px;
  }
  .err-box {
    background-color: var(--err-bg);
    border: 1px solid var(--err);
    border-radius: var(--radius-sm);
    padding: 10px 12px;
    color: var(--fg-0);
    white-space: pre-wrap;
    font-size: var(--text-sm);
  }
</style>
