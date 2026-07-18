<script lang="ts">
  // 步驟①基本資訊:顯示名稱(選填,空→後端存空,由卡片 fallback「範本名 #uuid 前 8 碼」)
  // 與節點(單節點顯示 local 不可選)。
  import TextField from '../../ui/TextField.svelte';

  interface WizardForm {
    name: string;
    [k: string]: unknown;
  }

  let {
    form,
    nodeLabel,
  }: {
    form: WizardForm;
    nodeLabel: string;
  } = $props();
</script>

<div class="step">
  <TextField
    label="伺服器名稱(選填)"
    value={form.name}
    onInput={(v) => (form.name = v)}
    placeholder="留空自動以「範本名 #短碼」命名"
    hint="用於清單、卡片與詳細頁顯示;可留空,建立後仍可辨識。"
  />

  <div class="field">
    <span class="lbl">節點</span>
    <div class="node-val">
      <span class="dot" aria-hidden="true"></span>
      <span class="node-name">{nodeLabel}</span>
      <span class="muted">單一節點,不可變更</span>
    </div>
    <div class="hint">目前為單機部署;多節點為預留能力。</div>
  </div>
</div>

<style>
  .step {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
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
  .node-val {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 7px 10px;
    color: var(--fg-0);
    font-size: var(--text-base);
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: var(--radius-full);
    background-color: var(--ok);
    flex: none;
  }
  .node-name {
    font-weight: 600;
  }
  .muted {
    color: var(--fg-2);
    font-size: var(--text-xs);
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
</style>
