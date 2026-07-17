<script lang="ts">
  // 未知路徑錯誤態(R3):不存在的路徑不得白屏,顯示 ErrorState 並可返回總覽。
  import { navigate } from '../router';
  import ErrorState from '../ui/ErrorState.svelte';
</script>

<ErrorState message="找不到這個頁面。" retryLabel="返回總覽" onRetry={() => navigate('/')} />
