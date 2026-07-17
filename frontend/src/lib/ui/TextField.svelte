<script lang="ts">
  let {
    value = $bindable(''),
    label = '',
    type = 'text',
    placeholder = '',
    hint = '',
    error = '',
    required = false,
    disabled = false,
    id,
    onInput,
  }: {
    value?: string;
    label?: string;
    type?: 'text' | 'password' | 'number' | 'email' | 'url';
    placeholder?: string;
    hint?: string;
    error?: string;
    required?: boolean;
    disabled?: boolean;
    id?: string;
    onInput?: (value: string) => void;
  } = $props();

  const generatedId = `field-${Math.random().toString(36).slice(2, 9)}`;
  const fieldId = $derived(id ?? generatedId);

  function handleInput(e: Event): void {
    value = (e.currentTarget as HTMLInputElement).value;
    onInput?.(value);
  }
</script>

<div class="field">
  {#if label}
    <label for={fieldId}>{label}{#if required}<span class="req">*</span>{/if}</label>
  {/if}
  <input
    id={fieldId}
    {type}
    {placeholder}
    {disabled}
    required={required}
    class:err={!!error}
    {value}
    oninput={handleInput}
    aria-invalid={!!error}
  />
  {#if error}
    <div class="error-msg">{error}</div>
  {:else if hint}
    <div class="hint">{hint}</div>
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
  input {
    font: inherit;
    color: var(--fg-0);
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    padding: 6px 9px;
    width: 100%;
    transition: border-color var(--dur-fast) var(--ease-standard);
  }
  input:hover:not(:disabled) {
    border-color: var(--fg-2);
  }
  input:focus-visible {
    outline: none;
    border-color: var(--accent);
  }
  input:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  input.err {
    border-color: var(--err);
  }
  .error-msg {
    font-size: var(--text-xs);
    color: var(--err);
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
</style>
