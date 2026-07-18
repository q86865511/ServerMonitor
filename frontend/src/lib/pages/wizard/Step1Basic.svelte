<script lang="ts">
  // 步驟①基本資訊:顯示名稱(選填,空→後端存空,由卡片 fallback「範本名 #uuid 前 8 碼」)
  // 與節點選擇(R5 多節點:下拉來源為 nodeStatuses 輪詢快照,離線節點標示並禁選)。
  import type { main } from '../../../../wailsjs/go/models';
  import TextField from '../../ui/TextField.svelte';
  import Select from '../../ui/Select.svelte';

  interface WizardForm {
    name: string;
    node: string;
    [k: string]: unknown;
  }

  let {
    form,
    nodeStatuses,
  }: {
    form: WizardForm;
    nodeStatuses: main.NodeStatusDTO[];
  } = $props();

  // 未輪詢到任何節點(如首次載入尚未拉到)時仍保底提供 local,避免下拉空白。
  const nodeOptions = $derived(
    nodeStatuses.length > 0
      ? nodeStatuses.map((n) => ({
          value: n.node,
          label: n.online ? n.node : `${n.node}(離線)`,
          disabled: !n.online,
        }))
      : [{ value: 'local', label: 'local' }],
  );
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
    <Select
      label="節點"
      value={form.node}
      options={nodeOptions}
      onChange={(v) => (form.node = v)}
    />
    <div class="hint">離線節點無法選取;新增節點請至「節點」頁管理。</div>
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
  .hint {
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
</style>
