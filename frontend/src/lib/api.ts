// 統一的 binding 呼叫層(錯誤狀態矩陣 f)。所有 Wails binding 呼叫經 call() 包裝:
// 失敗時把 Go 回傳的可讀錯誤訊息(埠衝突/缺參數等已是中文)轉成字串,預設彈 toast,並向上拋出
// 讓呼叫端可另做 inline 呈現。訊息原樣顯示,不加工。
import { pushToast } from './stores';

/** 把任意 reject 值轉為可讀字串。Wails 對回傳 error 的方法會以 Go 錯誤字串 reject。 */
export function errMsg(e: unknown): string {
  if (e == null) return '未知錯誤';
  if (typeof e === 'string') return e;
  if (e instanceof Error) return e.message;
  if (typeof e === 'object' && 'message' in e) {
    const m = (e as { message?: unknown }).message;
    if (typeof m === 'string') return m;
  }
  return String(e);
}

/**
 * 包裝一次 binding 呼叫。失敗時預設彈錯誤 toast 並重新拋出(呼叫端可 catch 做 inline 處理)。
 * silent=true 時只拋不彈(供呼叫端完全接管錯誤呈現,如建立精靈的 inline 錯誤)。
 */
export async function call<T>(
  fn: () => Promise<T>,
  opts: { silent?: boolean } = {},
): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    if (!opts.silent) pushToast('error', errMsg(e));
    throw e;
  }
}
