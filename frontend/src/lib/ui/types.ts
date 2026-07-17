// ui/ 元件共用型別。純呈現層契約(資料由呼叫端 / store 供給),不直接依賴 wailsjs 生成型別。

/** 日誌等級:由 store 於入 buffer 時解析一次(stderr/ERROR/SEVERE/FATAL→error;WARN→warn;其餘 info)。 */
export type LogLevel = 'info' | 'warn' | 'error';

/** 單行日誌。stream 保留原始來源(stdout/stderr/gsm-monitor/echo/echo-out)供樣式細分。 */
export interface LogRow {
  /** 穩定遞增鍵,供 keyed each。 */
  id: number;
  /** 原始串流來源。 */
  stream: string;
  /** 行內容(已去尾換行)。 */
  line: string;
  /** 已解析等級,驅動標色與篩選。 */
  level: LogLevel;
  /** 選配:RFC3339 或本地時間字串,供複製 / 匯出時保留(目前不強制顯示)。 */
  ts?: string;
}

/** 趨勢圖單一資料點;value 為 null 代表該時刻不可採集(畫缺口,不補 0)。 */
export interface TrendPoint {
  /** epoch 毫秒。 */
  ts: number;
  value: number | null;
}

/** 趨勢圖 / Sparkline 的一條線。 */
export interface TrendSeries {
  /** 圖例與 tooltip 顯示名。 */
  label: string;
  /** 色彩 CSS 變數名(如 '--chart-cpu'),元件以 var() 引用,不硬編碼色值。 */
  colorVar: string;
  points: TrendPoint[];
  /** 選配:tooltip 數值後綴(如 '%'、' MB')。 */
  unit?: string;
}
