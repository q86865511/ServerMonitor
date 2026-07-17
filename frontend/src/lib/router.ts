// 自製 hash 路由(R3):監聽 hashchange,將目前 hash 解析為 {page, params} readable store;
// navigate() 為切換路徑的 helper。零依賴,免 svelte-spa-router。
import { readable, type Readable } from 'svelte/store';

/** 全部可到達頁面(對齊側欄 9 項 + 伺服器詳細 + 未知路徑)。 */
export type Page =
  | 'dashboard'
  | 'servers'
  | 'server-detail'
  | 'templates'
  | 'nodes'
  | 'backups'
  | 'schedules'
  | 'alerts'
  | 'events'
  | 'settings'
  | 'not-found';

/** 伺服器詳細頁分頁;非法值一律視為 overview。 */
export type ServerTab = 'overview' | 'console' | 'backups' | 'schedules' | 'settings';

const SERVER_TABS: readonly ServerTab[] = ['overview', 'console', 'backups', 'schedules', 'settings'];

export interface Route {
  page: Page;
  params: { uuid?: string; tab?: ServerTab };
}

/**
 * 純函式:hash 字串 → Route。不依賴 window,方便單元驗證。
 * 空 hash / `#/` 視為根路徑(dashboard);未知第一段 → not-found。
 */
export function parseHash(hash: string): Route {
  const path = hash.replace(/^#/, '') || '/';
  const segments = path.split('/').filter((s) => s.length > 0);

  if (segments.length === 0) {
    return { page: 'dashboard', params: {} };
  }

  const [head, ...rest] = segments;

  switch (head) {
    case 'servers': {
      if (rest.length === 0) return { page: 'servers', params: {} };
      const [uuid, tabRaw] = rest;
      const tab = SERVER_TABS.includes(tabRaw as ServerTab) ? (tabRaw as ServerTab) : 'overview';
      return { page: 'server-detail', params: { uuid, tab } };
    }
    case 'templates':
      return { page: 'templates', params: {} };
    case 'nodes':
      return { page: 'nodes', params: {} };
    case 'backups':
      return { page: 'backups', params: {} };
    case 'schedules':
      return { page: 'schedules', params: {} };
    case 'alerts':
      return { page: 'alerts', params: {} };
    case 'events':
      return { page: 'events', params: {} };
    case 'settings':
      return { page: 'settings', params: {} };
    default:
      return { page: 'not-found', params: {} };
  }
}

/** 目前路由(響應式);初始值取當前 window.location.hash,之後隨 hashchange 更新。 */
export const route: Readable<Route> = readable(parseHash(window.location.hash), (set) => {
  const onHashChange = (): void => set(parseHash(window.location.hash));
  window.addEventListener('hashchange', onHashChange);
  return () => window.removeEventListener('hashchange', onHashChange);
});

/** 切換路徑,如 navigate('/servers') 或 navigate('/servers/abc/console')。自動補 `#` 前綴。 */
export function navigate(path: string): void {
  window.location.hash = path.startsWith('#') ? path : `#${path}`;
}
