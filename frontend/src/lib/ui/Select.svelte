<script lang="ts">
  export interface SelectOption {
    value: string;
    label: string;
    disabled?: boolean;
  }

  let {
    value = $bindable(''),
    options,
    label = '',
    error = '',
    required = false,
    disabled = false,
    id,
    onChange,
  }: {
    value?: string;
    options: SelectOption[];
    label?: string;
    error?: string;
    required?: boolean;
    disabled?: boolean;
    id?: string;
    onChange?: (value: string) => void;
  } = $props();

  const generatedId = `select-${Math.random().toString(36).slice(2, 9)}`;
  const fieldId = $derived(id ?? generatedId);

  function onInput(e: Event): void {
    value = (e.currentTarget as HTMLSelectElement).value;
    onChange?.(value);
  }
</script>

<div class="field">
  {#if label}
    <label for={fieldId}>{label}{#if required}<span class="req">*</span>{/if}</label>
  {/if}
  <select id={fieldId} class:err={!!error} {value} {disabled} required={required} oninput={onInput}>
    {#each options as opt (opt.value)}
      <option value={opt.value} disabled={opt.disabled}>{opt.label}</option>
    {/each}
  </select>
  {#if error}
    <div class="error-msg">{error}</div>
  {/if}
</div>

<style>
  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  label {
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
  .req {
    color: var(--err);
    margin-left: 3px;
  }
  select {
    font: inherit;
    color: var(--fg-0);
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 6px 9px;
    width: 100%;
    transition: border-color var(--dur-fast) var(--ease-standard);
  }
  select:hover:not(:disabled) {
    border-color: var(--fg-2);
  }
  select:focus-visible {
    outline: none;
    border-color: var(--accent);
  }
  select:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  select.err {
    border-color: var(--err);
  }
  .error-msg {
    font-size: var(--text-xs);
    color: var(--err);
  }
</style>
