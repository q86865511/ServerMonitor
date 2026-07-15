<script lang="ts">
  import { stateLabel, stateTone } from './format';

  export let state: string;
  export let desired = '';

  $: tone = stateTone(state);
  $: diverged = desired !== '' && desired !== state;
</script>

<span class="state {tone}" title={diverged ? `目標:${stateLabel(desired)}` : ''}>
  <span class="dot"></span>
  <span class="state-copy">{stateLabel(state)}</span>
  {#if diverged}
    <span class="target">/ → {stateLabel(desired)}</span>
  {/if}
</span>

<style>
  .state {
    display: inline-flex;
    flex: none;
    align-items: center;
    gap: 7px;
    color: var(--fg-1);
    font-family: var(--font-mono);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.055em;
    text-transform: uppercase;
  }

  .dot {
    width: 7px;
    height: 7px;
    flex: none;
    background: var(--idle);
    border-radius: 50%;
  }

  .state.ok .dot { background: var(--ok); }
  .state.busy .dot { background: var(--busy); animation: pulse 1.2s steps(2, end) infinite; }
  .state.err .dot { background: var(--err); }
  .state.off .dot { background: var(--off); }

  .target {
    color: var(--fg-3);
    font-size: 9px;
  }

  @keyframes pulse {
    50% { opacity: 0.28; }
  }
</style>
