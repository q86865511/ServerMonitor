<script lang="ts">
  // 伺服器清單頁(T9 過渡期):包舊 InstanceList 保持功能;建立/主控台仍走舊 CreateWizard/Console
  // modal(頁面自持狀態,T12 精靈與 T11 詳細頁主控台落地後汰換)。
  import { refresh } from '../stores/instances';
  import InstanceList from '../InstanceList.svelte';
  import CreateWizard from '../CreateWizard.svelte';
  import Console from '../Console.svelte';

  let showCreate = $state(false);
  let consoleUuid = $state<string | null>(null);

  function onCreated(): void {
    showCreate = false;
    refresh(false);
  }
</script>

<InstanceList
  on:create={() => (showCreate = true)}
  on:console={(e) => (consoleUuid = e.detail)}
  on:changed={() => refresh(false)}
/>

{#if showCreate}
  <CreateWizard on:close={() => (showCreate = false)} on:created={onCreated} />
{/if}

{#if consoleUuid}
  <Console uuid={consoleUuid} on:close={() => (consoleUuid = null)} />
{/if}
