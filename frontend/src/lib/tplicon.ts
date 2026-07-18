// 範本圖示 helper(R14):has_icon 者用 AssetServer 路徑 /tpl-icons/{id};無圖或載入失敗時
// 以「遊戲名首字 + 依 id 生成的 HSL 色塊」佔位。零依賴、無硬編碼色值(色相由 id hash 決定)。
// 註:此檔亦可能由並行任務 T10 建立同名 helper;若衝突以先存在者為準(本檔讓位)。

/** 範本 icon 的 AssetServer 路徑;無圖時後端回 404,由 <img onerror> 換佔位。 */
export function tplIconSrc(id: string): string {
  return `/tpl-icons/${id}`;
}

/** 依範本 id 生成穩定 hash(djb2 變體),供佔位色相取值。 */
function hashId(id: string): number {
  let h = 5381;
  for (let i = 0; i < id.length; i++) {
    h = ((h << 5) + h + id.charCodeAt(i)) >>> 0;
  }
  return h;
}

/** 依 id 生成穩定的 HSL 佔位背景色(色相取自 hash,飽和/亮度固定以確保可讀對比)。 */
export function tplIconColor(id: string): string {
  const hue = hashId(id) % 360;
  return `hsl(${hue}, 46%, 40%)`;
}

/** 佔位顯示的首字:取名稱(或退回 id)第一個非空白字元並大寫。 */
export function tplIconInitial(name: string, id: string): string {
  const src = (name || id || '?').trim();
  return src.length > 0 ? src[0].toUpperCase() : '?';
}
