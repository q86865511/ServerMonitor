<script lang="ts">
  import type { TrendSeries, TrendPoint } from './types';

  let {
    series,
    yMax,
    yUnit = '',
    height = 200,
    area = true,
    emptyText = '尚無歷史資料',
  }: {
    /** 1-2 條線;各含 label / colorVar / points。 */
    series: TrendSeries[];
    /** 選配 y 軸上限(百分比圖固定傳 100);未給則依資料最大值自動。 */
    yMax?: number;
    /** y 軸刻度後綴(如 '%')。 */
    yUnit?: string;
    /** 繪圖高度(px);寬度隨容器。 */
    height?: number;
    /** 是否繪製面積漸層填充。 */
    area?: boolean;
    emptyText?: string;
  } = $props();

  const PAD = { top: 10, right: 12, bottom: 22, left: 46 } as const;

  // ---- 響應容器寬度 ----
  let wrap = $state<HTMLDivElement | null>(null);
  let width = $state(600);
  $effect(() => {
    const el = wrap;
    if (!el) return;
    const ro = new ResizeObserver((entries) => {
      for (const e of entries) width = Math.max(120, Math.floor(e.contentRect.width));
    });
    ro.observe(el);
    return () => ro.disconnect();
  });

  // 唯一的 gradient id 前綴,避免同頁多圖 id 衝突。
  const gid = `tc-${Math.random().toString(36).slice(2, 9)}`;

  interface Geo {
    hasData: boolean;
    plotW: number;
    plotH: number;
    minTs: number;
    maxTs: number;
    effMax: number;
    ticks: number[];
    xOf: (ts: number) => number;
    yOf: (v: number) => number;
  }

  const geo = $derived.by<Geo>(() => {
    const plotW = Math.max(1, width - PAD.left - PAD.right);
    const plotH = Math.max(1, height - PAD.top - PAD.bottom);
    const valued = series.flatMap((s) => s.points).filter((p): p is TrendPoint & { value: number } => p.value != null);
    const hasData = valued.length > 0;

    const tsVals = series.flatMap((s) => s.points).map((p) => p.ts);
    const minTs = tsVals.length ? Math.min(...tsVals) : 0;
    const maxTs = tsVals.length ? Math.max(...tsVals) : 1;

    let effMax = yMax ?? 0;
    if (!yMax) {
      const dataMax = hasData ? Math.max(...valued.map((p) => p.value)) : 1;
      effMax = dataMax > 0 ? dataMax * 1.15 : 1;
    }
    if (effMax <= 0) effMax = 1;

    const xOf = (ts: number): number =>
      PAD.left + (maxTs === minTs ? plotW / 2 : ((ts - minTs) / (maxTs - minTs)) * plotW);
    const yOf = (v: number): number => PAD.top + plotH - (Math.min(v, effMax) / effMax) * plotH;

    // y 軸刻度:0..effMax 分 4 段。
    const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => effMax * f);

    return { hasData, plotW, plotH, minTs, maxTs, effMax, ticks, xOf, yOf };
  });

  /** 依 null 斷開的折線 path(缺口不連線)。 */
  function linePath(points: TrendPoint[]): string {
    let d = '';
    let pen = false;
    for (const p of points) {
      if (p.value == null) {
        pen = false;
        continue;
      }
      const x = geo.xOf(p.ts).toFixed(1);
      const y = geo.yOf(p.value).toFixed(1);
      d += `${pen ? 'L' : 'M'}${x} ${y} `;
      pen = true;
    }
    return d.trim();
  }

  /** 面積填充:每段連續(非 null)資料各自成一個封閉子路徑,缺口不填。 */
  function areaPath(points: TrendPoint[]): string {
    const baseY = (geo.yOf(0)).toFixed(1);
    let d = '';
    let run: TrendPoint[] = [];
    const flush = (): void => {
      if (run.length === 0) return;
      const first = run[0];
      const last = run[run.length - 1];
      const fv = first.value as number;
      d += `M${geo.xOf(first.ts).toFixed(1)} ${baseY} `;
      d += `L${geo.xOf(first.ts).toFixed(1)} ${geo.yOf(fv).toFixed(1)} `;
      for (const p of run) {
        d += `L${geo.xOf(p.ts).toFixed(1)} ${geo.yOf(p.value as number).toFixed(1)} `;
      }
      d += `L${geo.xOf(last.ts).toFixed(1)} ${baseY} Z `;
      run = [];
    };
    for (const p of points) {
      if (p.value == null) flush();
      else run.push(p);
    }
    flush();
    return d.trim();
  }

  // ---- Hover ----
  let svgEl = $state<SVGSVGElement | null>(null);
  let hoverTs = $state<number | null>(null);

  // 合併去重的時間軸(升冪),供 hover 就近吸附。
  const timeline = $derived.by<number[]>(() => {
    const set = new Set<number>();
    for (const s of series) for (const p of s.points) set.add(p.ts);
    return [...set].sort((a, b) => a - b);
  });

  function onMove(e: MouseEvent): void {
    const el = svgEl;
    if (!el || !geo.hasData || timeline.length === 0) return;
    const rect = el.getBoundingClientRect();
    // client px → viewBox 座標(svg 以 width:100% 縮放時仍正確)。
    const vbX = ((e.clientX - rect.left) / rect.width) * width;
    const target = geo.minTs + (geo.maxTs === geo.minTs ? 0 : ((vbX - PAD.left) / geo.plotW) * (geo.maxTs - geo.minTs));
    // 就近吸附。
    let best = timeline[0];
    let bestD = Math.abs(best - target);
    for (const t of timeline) {
      const d = Math.abs(t - target);
      if (d < bestD) {
        bestD = d;
        best = t;
      }
    }
    hoverTs = best;
  }
  function onLeave(): void {
    hoverTs = null;
  }

  interface HoverEntry {
    label: string;
    colorVar: string;
    value: number | null;
    unit: string;
  }
  const hover = $derived.by(() => {
    if (hoverTs == null) return null;
    const entries: HoverEntry[] = series.map((s) => {
      const pt = s.points.find((p) => p.ts === hoverTs);
      return { label: s.label, colorVar: s.colorVar, value: pt ? pt.value : null, unit: s.unit ?? '' };
    });
    return { ts: hoverTs, x: geo.xOf(hoverTs), entries };
  });

  function fmtHms(ts: number): string {
    const d = new Date(ts);
    const p = (n: number): string => String(n).padStart(2, '0');
    return `${p(d.getHours())}:${p(d.getMinutes())}`;
  }
  function fmtTick(v: number): string {
    // 大數值以整數,小數值保留一位。
    const s = v >= 100 || Number.isInteger(v) ? v.toFixed(0) : v.toFixed(1);
    return `${s}${yUnit}`;
  }
  // x 軸標籤:起 / 中 / 末三點時間。
  const xLabels = $derived.by(() => {
    if (!geo.hasData) return [] as { x: number; text: string }[];
    const mid = (geo.minTs + geo.maxTs) / 2;
    return [geo.minTs, mid, geo.maxTs].map((t) => ({ x: geo.xOf(t), text: fmtHms(t) }));
  });
</script>

<div class="trend" bind:this={wrap} style="height:{height}px">
  {#if !geo.hasData}
    <div class="empty">{emptyText}</div>
  {:else}
    <svg
      bind:this={svgEl}
      viewBox="0 0 {width} {height}"
      width="100%"
      height={height}
      preserveAspectRatio="none"
      role="img"
      aria-label={series.map((s) => s.label).join('、') + ' 趨勢圖'}
      onmousemove={onMove}
      onmouseleave={onLeave}
    >
      <defs>
        {#each series as s, i (s.label)}
          <linearGradient id="{gid}-{i}" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stop-color="var({s.colorVar})" stop-opacity="0.28" />
            <stop offset="100%" stop-color="var({s.colorVar})" stop-opacity="0" />
          </linearGradient>
        {/each}
      </defs>

      <!-- 水平格線 + y 軸標籤 -->
      {#each geo.ticks as t (t)}
        <line class="grid" x1={PAD.left} y1={geo.yOf(t)} x2={width - PAD.right} y2={geo.yOf(t)} />
        <text class="y-lbl" x={PAD.left - 6} y={geo.yOf(t)} text-anchor="end" dominant-baseline="middle">
          {fmtTick(t)}
        </text>
      {/each}

      <!-- x 軸標籤 -->
      {#each xLabels as l (l.text + l.x)}
        <text class="x-lbl" x={l.x} y={height - 6} text-anchor="middle">{l.text}</text>
      {/each}

      <!-- 面積 + 折線(null 缺口斷開) -->
      {#each series as s, i (s.label)}
        {#if area}
          <path class="area" d={areaPath(s.points)} fill="url(#{gid}-{i})" />
        {/if}
        <path class="line" d={linePath(s.points)} stroke="var({s.colorVar})" />
      {/each}

      <!-- Hover 十字線 + 最近點 -->
      {#if hover}
        <line class="crosshair" x1={hover.x} y1={PAD.top} x2={hover.x} y2={height - PAD.bottom} />
        {#each hover.entries as en, i (en.label)}
          {#if en.value != null}
            <circle class="pt" cx={hover.x} cy={geo.yOf(en.value)} r="3.5" stroke="var({en.colorVar})" />
          {/if}
        {/each}
      {/if}
    </svg>

    {#if hover}
      <div
        class="tip"
        class:right={hover.x > width * 0.6}
        style="left:{(hover.x / width) * 100}%"
      >
        <div class="tip-ts">{fmtHms(hover.ts)}</div>
        {#each hover.entries as en (en.label)}
          <div class="tip-row">
            <span class="sw" style="background:var({en.colorVar})"></span>
            <span class="tip-lbl">{en.label}</span>
            <span class="tip-val">{en.value == null ? '—' : en.value.toFixed(1) + en.unit}</span>
          </div>
        {/each}
      </div>
    {/if}
  {/if}
</div>

<style>
  .trend {
    position: relative;
    width: 100%;
  }
  .empty {
    height: 100%;
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--fg-2);
    font-size: var(--text-sm);
    border: 1px dashed var(--line);
    border-radius: var(--radius-sm);
  }
  svg {
    display: block;
    overflow: visible;
  }
  .grid {
    stroke: var(--chart-grid);
    stroke-width: 1;
  }
  .y-lbl,
  .x-lbl {
    fill: var(--fg-2);
    font-size: 10px;
    font-family: var(--font-sans);
  }
  .area {
    stroke: none;
  }
  .line {
    fill: none;
    stroke-width: 1.75;
    stroke-linejoin: round;
    stroke-linecap: round;
    vector-effect: non-scaling-stroke;
  }
  .crosshair {
    stroke: var(--line-strong);
    stroke-width: 1;
    stroke-dasharray: 3 3;
  }
  .pt {
    fill: var(--bg-0);
    stroke-width: 2;
  }
  .tip {
    position: absolute;
    top: 4px;
    transform: translateX(8px);
    pointer-events: none;
    background: var(--bg-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-sm);
    box-shadow: var(--shadow-md);
    padding: 6px 9px;
    font-size: var(--text-xs);
    white-space: nowrap;
    z-index: 1;
  }
  .tip.right {
    transform: translateX(-100%) translateX(-8px);
  }
  .tip-ts {
    color: var(--fg-2);
    margin-bottom: 3px;
  }
  .tip-row {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .sw {
    width: 8px;
    height: 8px;
    border-radius: 2px;
    flex: none;
  }
  .tip-lbl {
    color: var(--fg-1);
  }
  .tip-val {
    color: var(--fg-0);
    font-weight: 600;
    margin-left: auto;
  }
</style>
