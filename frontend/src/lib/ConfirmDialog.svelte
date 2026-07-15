<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import Modal from './Modal.svelte';

  export let title = '確認';
  export let message = '';
  export let confirmLabel = '確認';
  export let danger = false;
  export let busy = false;

  const dispatch = createEventDispatcher<{ confirm: void; cancel: void }>();
</script>

<Modal {title} on:close={() => dispatch('cancel')}>
  <p class="msg">{message}</p>
  <div class="actions">
    <button on:click={() => dispatch('cancel')} disabled={busy}>取消</button>
    <button
      class={danger ? 'danger' : 'primary'}
      on:click={() => dispatch('confirm')}
      disabled={busy}
    >
      {busy ? '處理中…' : confirmLabel}
    </button>
  </div>
</Modal>

<style>
  .msg {
    margin: 0 0 18px;
    color: var(--fg-1);
    white-space: pre-wrap;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
  }
</style>
