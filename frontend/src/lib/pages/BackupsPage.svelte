<script lang="ts">
  // T9 佔位;全域備份頁(R9)由後續任務實作(複用詳細頁 T11 產出的 BackupsPanel)。
  import Card from '../ui/Card.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
</script>

<Card>
  <EmptyState title="備份頁建置中" description="含實例選擇器的全域備份列表將於後續任務完成。" />
</Card>
