<script lang="ts">
  let {
    value,
    max = 100,
    tone,
    warnAt = 70,
    dangerAt = 90,
    label = '',
  }: {
    /** 目前值(0..max)。 */
    value: number;
    max?: number;
    /** 明確指定色調;不給則依 warnAt/dangerAt 閾值自動判定。 */
    tone?: 'ok' | 'busy' | 'err';
    warnAt?: number;
    dangerAt?: number;
    label?: string;
  } = $props();

  const pct = $derived(Math.max(0, Math.min(100, (value / max) * 100)));
  const autoTone = $derived.by((): 'ok' | 'busy' | 'err' => {
    if (tone) return tone;
    if (pct >= dangerAt) return 'err';
    if (pct >= warnAt) return 'busy';
    return 'ok';
  });
</script>

<div class="progress-wrap">
  {#if label}
    <div class="label-row">
      <span>{label}</span>
      <span class="pct">{pct.toFixed(0)}%</span>
    </div>
  {/if}
  <div
    class="track"
    role="progressbar"
    aria-valuenow={value}
    aria-valuemin={0}
    aria-valuemax={max}
  >
    <div class="fill {autoTone}" style="width: {pct}%"></div>
  </div>
</div>

<style>
  .progress-wrap {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .label-row {
    display: flex;
    justify-content: space-between;
    font-size: var(--text-xs);
    color: var(--fg-1);
  }
  .pct {
    color: var(--fg-2);
  }
  .track {
    height: 6px;
    background-color: var(--bg-3);
    border-radius: var(--radius-full);
    overflow: hidden;
  }
  .fill {
    height: 100%;
    border-radius: var(--radius-full);
    transition: width var(--dur-base) var(--ease-standard);
  }
  .fill.ok {
    background-color: var(--ok);
  }
  .fill.busy {
    background-color: var(--busy);
  }
  .fill.err {
    background-color: var(--err);
  }
</style>
