<script lang="ts">
  // T9 佔位;伺服器詳細頁(R6,含五分頁)由後續任務(T10)實作。
  // 目前僅顯示路由帶入的 uuid/tab 供除錯,與返回伺服器清單鈕。
  import { route, navigate } from '../../router';
  import Card from '../../ui/Card.svelte';
  import Button from '../../ui/Button.svelte';

  const uuid = $derived($route.params.uuid ?? '');
  const tab = $derived($route.params.tab ?? 'overview');
</script>

<Card>
  <div class="placeholder">
    <div class="title">伺服器詳細頁建置中</div>
    <div class="meta">uuid: <span class="mono">{uuid}</span></div>
    <div class="meta">分頁: <span class="mono">{tab}</span></div>
    <Button onclick={() => navigate('/servers')}>← 返回伺服器清單</Button>
  </div>
</Card>

<style>
  .placeholder {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-3);
    padding: var(--space-4);
  }
  .title {
    font-size: var(--text-md);
    font-weight: 600;
  }
  .meta {
    font-size: var(--text-sm);
    color: var(--fg-1);
  }
</style>
