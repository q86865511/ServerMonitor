// 顯示格式化與狀態對照。「不適用」語意(R6):null/undefined 一律顯示「不適用」,絕不當 0。

const NA = '不適用';

/** 位元組人類可讀;undefined/null → 不適用;0 → 0 B。 */
export function fmtBytes(n: number | undefined | null): string {
  if (n == null) return NA;
  if (n <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i > 0 && v < 100 ? 1 : 0)} ${units[i]}`;
}

/** CPU 百分比;undefined/null → 不適用。 */
export function fmtCPU(p: number | undefined | null): string {
  return fmtPercent(p);
}

/** 泛用百分比(1 位小數;null/undefined → 不適用)。CPU/RAM 等百分比欄位共用。 */
export function fmtPercent(p: number | undefined | null): string {
  if (p == null) return NA;
  return `${p.toFixed(1)}%`;
}

/** 記憶體用量比例(0..1);limit<=0 時回 null(無上限,不畫比例)。 */
export function memRatio(used: number, limit: number): number | null {
  if (!limit || limit <= 0 || !used || used <= 0) return null;
  return Math.min(1, used / limit);
}

/** 玩家數;null/undefined → 不適用。 */
export function fmtPlayers(n: number | undefined | null): string {
  if (n == null) return NA;
  return String(n);
}

/** 線上狀態;null/undefined → 不適用。 */
export function fmtOnline(v: boolean | undefined | null): string {
  if (v == null) return NA;
  return v ? '線上' : '離線';
}

/** RFC3339 字串 → 本地時間顯示;空/無效 → '—'。 */
export function fmtTime(ts: string | undefined | null): string {
  if (!ts) return '—';
  const d = new Date(ts);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleString();
}

/**
 * 運行時間(R11):RFC3339 起始時間 → 至 nowMs 的精簡經過時間顯示。
 * 空/無效 → '—'(不適用語意同 R15);nowMs 由呼叫端傳入一個會遞增的時鐘值,驅動畫面本地遞增。
 */
export function fmtUptime(startedAt: string | undefined | null, nowMs: number = Date.now()): string {
  if (!startedAt) return '—';
  const start = Date.parse(startedAt);
  if (isNaN(start)) return '—';
  const diffSec = Math.max(0, Math.floor((nowMs - start) / 1000));
  const d = Math.floor(diffSec / 86400);
  const h = Math.floor((diffSec % 86400) / 3600);
  const m = Math.floor((diffSec % 3600) / 60);
  const s = diffSec % 60;
  if (d > 0) return `${d} 天 ${h} 時`;
  if (h > 0) return `${h} 時 ${m} 分`;
  if (m > 0) return `${m} 分 ${s} 秒`;
  return `${s} 秒`;
}

/** 實例狀態 → 中文標籤。 */
const STATE_LABELS: Record<string, string> = {
  Created: '已建立',
  Starting: '啟動中',
  Running: '執行中',
  Stopping: '停止中',
  Stopped: '已停止',
  BackingUp: '備份中',
  Restoring: '還原中',
  Crashed: '已當機',
  Error: '錯誤',
  Offline: '節點離線',
};
export function stateLabel(state: string): string {
  return STATE_LABELS[state] ?? state ?? '未知';
}

/** 實例狀態 → 色點語意類別(對應 CSS 變數 --ok/--busy/--idle/--err/--off)。 */
export function stateTone(state: string): 'ok' | 'busy' | 'idle' | 'err' | 'off' {
  switch (state) {
    case 'Running':
      return 'ok';
    case 'Starting':
    case 'Stopping':
    case 'BackingUp':
    case 'Restoring':
      return 'busy';
    case 'Crashed':
    case 'Error':
      return 'err';
    case 'Offline':
      return 'off';
    default:
      return 'idle';
  }
}

/** 事件 severity → 色調。 */
export function severityTone(sev: string): 'ok' | 'busy' | 'err' {
  switch (sev) {
    case 'error':
      return 'err';
    case 'warning':
      return 'busy';
    default:
      return 'ok';
  }
}

/** 週日索引(0=日..6=六)→ 中文短標。 */
export const WEEKDAY_LABELS = ['日', '一', '二', '三', '四', '五', '六'];

/** weekdays 陣列 → 顯示字串;空=每天。 */
export function fmtWeekdays(days: number[] | undefined | null): string {
  if (!days || days.length === 0) return '每天';
  return days
    .slice()
    .sort((a, b) => a - b)
    .map((d) => WEEKDAY_LABELS[d] ?? String(d))
    .join('、');
}
