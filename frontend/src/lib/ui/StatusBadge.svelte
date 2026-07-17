<script lang="ts">
  import { stateLabel, stateTone } from '../format';

  let {
    state,
    desired = '',
  }: {
    /** 實例狀態原始值(Running/Stopped/... ),未知值顯示「未知」。 */
    state: string;
    /** 對照顯示 desired(選填,空字串=不比對):與 observed 不同時以副標提示。 */
    desired?: string;
  } = $props();

  const tone = $derived(stateTone(state));
  const diverged = $derived(desired !== '' && desired !== state);
</script>

<span class="status-badge" title={diverged ? `目標:${stateLabel(desired)}` : ''}>
  <span class="dot {tone}"></span>
  <span class="text">{stateLabel(state)}</span>
  {#if diverged}
    <span class="target">→ {stateLabel(desired)}</span>
  {/if}
</span>

<style>
  .status-badge {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: var(--text-sm);
    color: var(--fg-0);
  }
  .dot {
    width: 9px;
    height: 9px;
    border-radius: 50%;
    flex: none;
  }
  .dot.ok {
    background: var(--ok);
    box-shadow: 0 0 6px var(--ok);
  }
  .dot.busy {
    background: var(--busy);
    animation: pulse 1.2s ease-in-out infinite;
  }
  .dot.idle {
    background: var(--idle);
  }
  .dot.err {
    background: var(--err);
    box-shadow: 0 0 6px var(--err);
  }
  .dot.off {
    background: var(--off);
  }
  .target {
    color: var(--fg-2);
    font-size: var(--text-xs);
  }
  @keyframes pulse {
    0%,
    100% {
      opacity: 1;
    }
    50% {
      opacity: 0.35;
    }
  }
</style>
