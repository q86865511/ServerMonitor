<script lang="ts">
  // 步驟②選擇範本:圖卡網格(has_icon 用 /tpl-icons/{id}+onerror 佔位)、名稱、runtime 能力標籤、
  // 變體選擇;SearchInput 過濾;選取態高亮。選取範本由父層 onSelect 處理(切換時重置範本相關欄位)。
  import type { main } from '../../../../wailsjs/go/models';
  import SearchInput from '../../ui/SearchInput.svelte';
  import Select from '../../ui/Select.svelte';
  import Badge from '../../ui/Badge.svelte';
  import { tplIconSrc, tplIconColor, tplIconInitial } from '../../tplicon';

  let {
    templates,
    selectedId,
    variant,
    tmpl,
    onSelect,
    onVariant,
  }: {
    templates: main.TemplateDTO[];
    selectedId: string;
    variant: string;
    tmpl: main.TemplateDTO | undefined;
    onSelect: (id: string) => void;
    onVariant: (id: string) => void;
  } = $props();

  let search = $state('');
  // onerror 已觸發的範本 id 集合:改渲染佔位色塊。
  let failed = $state<Set<string>>(new Set());

  const filtered = $derived.by(() => {
    const q = search.trim().toLowerCase();
    if (!q) return templates;
    return templates.filter(
      (t) => t.name.toLowerCase().includes(q) || t.id.toLowerCase().includes(q),
    );
  });

  function markFailed(id: string): void {
    if (failed.has(id)) return;
    const next = new Set(failed);
    next.add(id);
    failed = next;
  }

  function runtimeLabel(rt: string): string {
    return rt === 'native' ? '本機行程' : rt === 'docker' ? 'Docker' : rt;
  }

  const variantOptions = $derived(
    (tmpl?.variants ?? []).map((v) => ({
      value: v.id,
      label: v.loader ? `${v.id}(${v.loader})` : v.id,
    })),
  );
</script>

<div class="step">
  <SearchInput bind:value={search} placeholder="搜尋範本名稱或 id…" />

  {#if filtered.length === 0}
    <div class="empty">無符合的範本。</div>
  {:else}
    <div class="grid" role="listbox" aria-label="遊戲範本">
      {#each filtered as t (t.id)}
        {@const showPlaceholder = !t.has_icon || failed.has(t.id)}
        <button
          type="button"
          class="card"
          class:selected={selectedId === t.id}
          role="option"
          aria-selected={selectedId === t.id}
          onclick={() => onSelect(t.id)}
        >
          <div class="icon-wrap">
            {#if showPlaceholder}
              <div class="placeholder" style={`background:${tplIconColor(t.id)}`}>
                {tplIconInitial(t.name, t.id)}
              </div>
            {:else}
              <img
                class="icon"
                src={tplIconSrc(t.id)}
                alt={t.name}
                onerror={() => markFailed(t.id)}
              />
            {/if}
          </div>
          <div class="meta">
            <div class="name" title={t.name}>{t.name}</div>
            <div class="tags">
              {#each t.runtimes ?? [] as rt}
                <Badge tone="neutral">{runtimeLabel(rt)}</Badge>
              {/each}
              {#if t.modpack}<Badge tone="accent">模組包</Badge>{/if}
            </div>
          </div>
        </button>
      {/each}
    </div>
  {/if}

  {#if tmpl && variantOptions.length > 0}
    <div class="variant">
      <Select
        label="變體"
        required
        value={variant}
        options={variantOptions}
        onChange={onVariant}
      />
    </div>
  {/if}
</div>

<style>
  .step {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
    gap: var(--space-3);
  }
  .card {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    text-align: left;
    background-color: var(--bg-0);
    border: 1px solid var(--line-strong);
    border-radius: var(--radius);
    padding: var(--space-3);
    cursor: pointer;
    transition: border-color var(--dur-fast) var(--ease-standard),
      background-color var(--dur-fast) var(--ease-standard);
  }
  .card:hover {
    border-color: var(--fg-2);
  }
  .card.selected {
    border-color: var(--accent);
    background-color: var(--accent-bg);
  }
  .card:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .icon-wrap {
    flex: none;
    width: 44px;
    height: 44px;
  }
  .icon,
  .placeholder {
    width: 44px;
    height: 44px;
    border-radius: var(--radius-sm);
    object-fit: cover;
  }
  .placeholder {
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--fg-on-accent);
    font-size: var(--text-lg);
    font-weight: 700;
  }
  .meta {
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .name {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-0);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .tags {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .variant {
    max-width: 320px;
  }
  .empty {
    color: var(--fg-2);
    font-size: var(--text-sm);
    padding: var(--space-4);
    text-align: center;
  }
</style>
