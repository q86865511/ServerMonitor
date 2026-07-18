// 範本中繼資料快取:ListTemplates 惰性載入一次,供實例標籤/選擇器共用。
// 失敗時留空(標籤退化為 template_id)並允許下次呼叫重試;範本為輔助中繼資料,不顯示錯誤態。
import { writable } from 'svelte/store';
import type { Readable } from 'svelte/store';
import type { main } from '../../../wailsjs/go/models';
import { ListTemplates } from '../../../wailsjs/go/main/App';

const store = writable<main.TemplateDTO[]>([]);
let loaded = false;

/** 範本清單(響應式;首次訂閱觸發惰性載入)。 */
export function templates(): Readable<main.TemplateDTO[]> {
  if (!loaded) {
    loaded = true;
    ListTemplates()
      .then((t) => store.set(t))
      .catch(() => {
        loaded = false; // 失敗允許之後重試
      });
  }
  return store;
}

/** 實例顯示標籤(R10 fallback):自訂名稱 →「範本名 #uuid8」;範本查無時退 template_id。 */
export function instanceLabel(inst: main.InstanceDTO, tpls: main.TemplateDTO[]): string {
  if (inst.name?.trim()) return inst.name;
  const tplName = tpls.find((t) => t.id === inst.template_id)?.name || inst.template_id;
  return `${tplName} #${inst.uuid.slice(0, 8)}`;
}
