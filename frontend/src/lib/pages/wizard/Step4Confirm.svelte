<script lang="ts">
  // 步驟④確認:摘要清單 + 供應進度列(stage/percent/detail;percent<0=不確定態)+ inline 失敗錯誤。
  // blocked-mods 對話框由父層(CreateWizardModal)渲染。
  import type { main } from '../../../../wailsjs/go/models';
  import ProgressBar from '../../ui/ProgressBar.svelte';

  interface WizardForm {
    name: string;
    variant: string;
    paramValues: Record<string, string>;
    secretValues: Record<string, string>;
    runtime: string;
    memoryMB: string;
    cpuPercent: string;
    modpackType: string;
    modpackRef: string;
    [k: string]: unknown;
  }

  let {
    form,
    tmpl,
    nodeLabel,
    submitting,
    provStage,
    provPercent,
    provDetail,
    error,
  }: {
    form: WizardForm;
    tmpl: main.TemplateDTO;
    nodeLabel: string;
    submitting: boolean;
    provStage: string;
    provPercent: number;
    provDetail: string;
    error: string;
  } = $props();

  function runtimeName(rt: string): string {
    return rt === 'native' ? '本機行程(native)' : rt === 'docker' ? 'Docker 容器' : rt;
  }

  const variantLabel = $derived(
    (tmpl.variants ?? []).find((v) => v.id === form.variant)?.id ?? '',
  );

  const paramRows = $derived(
    (tmpl.params ?? []).map((p) => {
      const raw = form.paramValues[p.key] ?? '';
      const val = p.type === 'bool' ? (raw === 'true' ? '是' : '否') : raw === '' ? '—' : raw;
      return { label: p.label || p.key, value: val };
    }),
  );

  const secretKeys = $derived(
    (tmpl.secrets ?? []).map((s) => s.label || s.key),
  );

  const showProvision = $derived(submitting && (provStage !== '' || form.runtime === 'native'));
</script>

<div class="step">
  <dl class="summary">
    <div class="row">
      <dt>名稱</dt>
      <dd>{form.name.trim() !== '' ? form.name.trim() : '(自動命名:範本名 #短碼)'}</dd>
    </div>
    <div class="row">
      <dt>範本</dt>
      <dd>{tmpl.name}<span class="muted"> ({tmpl.id})</span></dd>
    </div>
    {#if variantLabel !== ''}
      <div class="row"><dt>變體</dt><dd>{variantLabel}</dd></div>
    {/if}
    <div class="row"><dt>節點</dt><dd>{nodeLabel}</dd></div>
    <div class="row"><dt>執行後端</dt><dd>{runtimeName(form.runtime)}</dd></div>
    {#if form.runtime === 'native'}
      <div class="row">
        <dt>資源上限</dt>
        <dd>
          記憶體 {form.memoryMB.trim() !== '' ? `${form.memoryMB} MB` : '不限'} ／ CPU
          {form.cpuPercent.trim() !== '' ? `${form.cpuPercent}%` : '不限'}
        </dd>
      </div>
    {/if}
    {#each paramRows as r}
      <div class="row"><dt>{r.label}</dt><dd>{r.value}</dd></div>
    {/each}
    {#each secretKeys as k}
      <div class="row"><dt>{k}</dt><dd class="muted">••••••</dd></div>
    {/each}
    {#if form.modpackType !== ''}
      <div class="row">
        <dt>模組包</dt>
        <dd>{form.modpackType}<span class="muted"> — {form.modpackRef.trim() || '(未填)'}</span></dd>
      </div>
    {/if}
  </dl>

  {#if showProvision}
    <div class="prov-box">
      <div class="prov-head">
        <span class="prov-stage">{provStage || '準備供應…'}</span>
        {#if provPercent >= 0}<span class="prov-pct">{Math.round(provPercent)}%</span>{/if}
      </div>
      {#if provPercent >= 0}
        <ProgressBar value={Math.min(100, provPercent)} tone="busy" />
      {:else}
        <div class="indet-track"><div class="indet-fill"></div></div>
      {/if}
      {#if provDetail}<div class="prov-detail muted">{provDetail}</div>{/if}
    </div>
  {/if}

  {#if error}
    <div class="err-box">{error}</div>
  {/if}
</div>

<style>
  .step {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .summary {
    margin: 0;
    display: flex;
    flex-direction: column;
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }
  .row {
    display: grid;
    grid-template-columns: 140px 1fr;
    gap: var(--space-3);
    padding: 8px 12px;
    font-size: var(--text-sm);
  }
  .row:nth-child(odd) {
    background-color: var(--bg-2);
  }
  dt {
    color: var(--fg-2);
  }
  dd {
    margin: 0;
    color: var(--fg-0);
    word-break: break-word;
  }
  .muted {
    color: var(--fg-2);
  }
  .prov-box {
    background-color: var(--bg-2);
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .prov-head {
    display: flex;
    justify-content: space-between;
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .prov-pct {
    font-variant-numeric: tabular-nums;
    color: var(--fg-1);
  }
  .prov-detail {
    font-size: var(--text-xs);
    word-break: break-all;
  }
  /* 不確定態(percent<0):以動畫條表達進行中。 */
  .indet-track {
    height: 6px;
    background-color: var(--bg-3);
    border-radius: var(--radius-full);
    overflow: hidden;
  }
  .indet-fill {
    height: 100%;
    width: 40%;
    background-color: var(--busy);
    border-radius: var(--radius-full);
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
