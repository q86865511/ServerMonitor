<script lang="ts">
  let {
    variant = 'line',
    width = '100%',
    height,
    count = 1,
  }: {
    /** block=矩形區塊(卡片/圖片佔位);line=行內文字佔位。 */
    variant?: 'block' | 'line';
    width?: string;
    height?: string;
    count?: number;
  } = $props();

  const resolvedHeight = $derived(height ?? (variant === 'block' ? '80px' : '14px'));
</script>

{#each { length: count } as _, i (i)}
  <div
    class="skeleton {variant}"
    style="width: {width}; height: {resolvedHeight};"
    aria-hidden="true"
  ></div>
{/each}

<style>
  .skeleton {
    background: linear-gradient(90deg, var(--bg-2) 25%, var(--bg-3) 37%, var(--bg-2) 63%);
    background-size: 400% 100%;
    animation: shimmer 1.4s ease infinite;
    border-radius: var(--radius-sm);
  }
  .skeleton.line {
    margin-bottom: var(--space-2);
  }
  .skeleton.line:last-child {
    margin-bottom: 0;
  }
  .skeleton.block {
    border-radius: var(--radius);
  }
  @keyframes shimmer {
    0% {
      background-position: 100% 50%;
    }
    100% {
      background-position: 0 50%;
    }
  }
</style>
