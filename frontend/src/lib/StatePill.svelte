<script lang="ts">
  import { stateLabel, stateTone } from './format';

  export let state: string;
  /** 對照顯示 desired(選填,空字串=不比對):與 observed 不同時以副標提示。 */
  export let desired = '';

  $: tone = stateTone(state);
  $: diverged = desired !== '' && desired !== state;
</script>

<span class="pill" title={diverged ? `目標:${stateLabel(desired)}` : ''}>
  <span class="dot {tone}"></span>
  <span>{stateLabel(state)}</span>
  {#if diverged}
    <span class="target">→ {stateLabel(desired)}</span>
  {/if}
</span>

<style>
  .pill {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 13px;
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
    font-size: 12px;
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
