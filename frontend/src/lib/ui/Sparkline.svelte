<script lang="ts">
  import type { TrendPoint } from './types';

  let {
    points,
    colorVar = '--accent',
    yMax,
    width = 100,
    height = 28,
    area = true,
  }: {
    /** 資料點;value 為 null 畫缺口(斷線,不補 0)。 */
    points: TrendPoint[];
    /** 線色 CSS 變數名。 */
    colorVar?: string;
    /** 選配 y 上限(如百分比 100);未給依資料最大值。 */
    yMax?: number;
    /** viewBox 寬(隨容器縮放,實際尺寸由 CSS 控制)。 */
    width?: number;
    height?: number;
    area?: boolean;
  } = $props();

  const PAD = 2;
  const gid = `sp-${Math.random().toString(36).slice(2, 9)}`;

  const model = $derived.by(() => {
    const valued = points.filter((p): p is TrendPoint & { value: number } => p.value != null);
    const hasData = valued.length > 0;
    const tsVals = points.map((p) => p.ts);
    const minTs = tsVals.length ? Math.min(...tsVals) : 0;
    const maxTs = tsVals.length ? Math.max(...tsVals) : 1;
    let effMax = yMax ?? 0;
    if (!yMax) {
      const dataMax = hasData ? Math.max(...valued.map((p) => p.value)) : 1;
      effMax = dataMax > 0 ? dataMax * 1.1 : 1;
    }
    if (effMax <= 0) effMax = 1;

    const plotW = Math.max(1, width - PAD * 2);
    const plotH = Math.max(1, height - PAD * 2);
    const xOf = (ts: number): number =>
      PAD + (maxTs === minTs ? plotW / 2 : ((ts - minTs) / (maxTs - minTs)) * plotW);
    const yOf = (v: number): number => PAD + plotH - (Math.min(v, effMax) / effMax) * plotH;
    const baseY = (PAD + plotH).toFixed(1);

    let line = '';
    let pen = false;
    for (const p of points) {
      if (p.value == null) {
        pen = false;
        continue;
      }
      line += `${pen ? 'L' : 'M'}${xOf(p.ts).toFixed(1)} ${yOf(p.value).toFixed(1)} `;
      pen = true;
    }

    let fill = '';
    let run: TrendPoint[] = [];
    const flush = (): void => {
      if (run.length === 0) return;
      const f = run[0];
      const l = run[run.length - 1];
      fill += `M${xOf(f.ts).toFixed(1)} ${baseY} L${xOf(f.ts).toFixed(1)} ${yOf(f.value as number).toFixed(1)} `;
      for (const p of run) fill += `L${xOf(p.ts).toFixed(1)} ${yOf(p.value as number).toFixed(1)} `;
      fill += `L${xOf(l.ts).toFixed(1)} ${baseY} Z `;
      run = [];
    };
    for (const p of points) {
      if (p.value == null) flush();
      else run.push(p);
    }
    flush();

    return { hasData, line: line.trim(), fill: fill.trim() };
  });
</script>

{#if model.hasData}
  <svg
    class="sparkline"
    viewBox="0 0 {width} {height}"
    width="100%"
    height={height}
    preserveAspectRatio="none"
    role="img"
    aria-hidden="true"
  >
    <defs>
      <linearGradient id={gid} x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="var({colorVar})" stop-opacity="0.25" />
        <stop offset="100%" stop-color="var({colorVar})" stop-opacity="0" />
      </linearGradient>
    </defs>
    {#if area}
      <path d={model.fill} fill="url(#{gid})" stroke="none" />
    {/if}
    <path d={model.line} fill="none" stroke="var({colorVar})" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke" />
  </svg>
{:else}
  <span class="na">—</span>
{/if}

<style>
  .sparkline {
    display: block;
    width: 100%;
    overflow: visible;
  }
  .na {
    color: var(--fg-2);
    font-size: var(--text-sm);
  }
</style>
