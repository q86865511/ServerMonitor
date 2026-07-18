<script lang="ts">
  // 範本頁(R9):範本圖卡總覽。純資訊展示,不跨頁開建立精靈(建立入口沿用 Topbar「新增伺服器」,
  // 詳見 tasks.md T13 對此頁的裁決)。icon 規則同 ui/ServerCard(R5/R14):has_icon 才嘗試載入
  // /tpl-icons/{id},onerror 或無圖時以「首字+id-hash 色塊」佔位(lib/tplicon.ts)。
  import { onMount } from 'svelte';
  import type { main } from '../../../wailsjs/go/models';
  import { ListTemplates } from '../../../wailsjs/go/main/App';
  import { call } from '../api';
  import { tplIconColor, tplIconInitial, tplIconSrc } from '../tplicon';
  import Badge from '../ui/Badge.svelte';
  import Card from '../ui/Card.svelte';
  import EmptyState from '../ui/EmptyState.svelte';
  import ErrorState from '../ui/ErrorState.svelte';
  import Skeleton from '../ui/Skeleton.svelte';

  let templates = $state<main.TemplateDTO[]>([]);
  let loading = $state(true);
  let loadError = $state('');
  let failedIcons = $state<Record<string, boolean>>({});

  async function load(): Promise<void> {
    loading = true;
    loadError = '';
    try {
      templates = await call(() => ListTemplates(), { silent: true });
    } catch (e) {
      loadError = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  onMount(load);

  /** 埠摘要:name + container/protocol,必要埠加註記(範本層無 host_port,動態分配於建立時決定)。 */
  function portSummary(t: main.TemplateDTO): string {
    if (!t.ports || t.ports.length === 0) return '無宣告連接埠';
    return t.ports
      .map(
        (p) =>
          `${p.name || '(未命名)'} ${p.container}/${(p.protocol || 'tcp').toUpperCase()}${p.required ? ' · 必要' : ''}`,
      )
      .join('、');
  }
</script>

<div class="head">
  <p class="hint">範本資訊展示;如要以範本建立伺服器,請至「伺服器」頁使用右上角「新增伺服器」。</p>
</div>

{#if loading}
  <div class="grid">
    <Card><Skeleton variant="block" height="160px" /></Card>
    <Card><Skeleton variant="block" height="160px" /></Card>
    <Card><Skeleton variant="block" height="160px" /></Card>
  </div>
{:else if loadError}
  <Card>
    <ErrorState message={`載入範本失敗:${loadError}`} onRetry={load} />
  </Card>
{:else if templates.length === 0}
  <Card>
    <EmptyState title="尚無任何範本" description="請於 templates/ 目錄新增範本設定檔(*.toml)。" />
  </Card>
{:else}
  <div class="grid">
    {#each templates as t (t.id)}
      <Card>
        <div class="tpl-card">
          <div class="tpl-head">
            <div class="icon-wrap">
              {#if !t.has_icon || failedIcons[t.id]}
                <div class="placeholder" style={`background:${tplIconColor(t.id)}`}>
                  {tplIconInitial(t.name, t.id)}
                </div>
              {:else}
                <img
                  class="icon"
                  src={tplIconSrc(t.id)}
                  alt=""
                  onerror={() => (failedIcons = { ...failedIcons, [t.id]: true })}
                />
              {/if}
            </div>
            <div class="ident">
              <div class="name" title={t.name}>{t.name}</div>
              <div class="id mono">{t.id}</div>
            </div>
            {#if t.modpack}
              <Badge tone="accent">模組包</Badge>
            {/if}
          </div>

          <div class="runtimes">
            {#each t.runtimes as rt (rt)}
              <Badge tone={rt === 'docker' ? 'accent' : 'off'}>{rt === 'docker' ? 'Docker' : 'Native'}</Badge>
            {/each}
            {#if t.runtimes.length === 0}
              <Badge tone="err">無可用執行後端</Badge>
            {/if}
          </div>

          <dl class="stats">
            <dt>變體</dt>
            <dd>{t.variants.length > 0 ? `${t.variants.length} 個` : '(僅預設)'}</dd>
            <dt>連接埠</dt>
            <dd>{portSummary(t)}</dd>
          </dl>
        </div>
      </Card>
    {/each}
  </div>
{/if}

<style>
  .head {
    margin-bottom: var(--space-4);
  }
  .hint {
    margin: 0;
    font-size: var(--text-sm);
    color: var(--fg-2);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
    gap: var(--space-4);
  }
  .tpl-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .tpl-head {
    display: flex;
    align-items: center;
    gap: var(--space-3);
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
  .ident {
    flex: 1;
    min-width: 0;
  }
  .name {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-0);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .id {
    font-size: var(--text-xs);
    color: var(--fg-2);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .runtimes {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .stats {
    display: grid;
    grid-template-columns: 64px 1fr;
    gap: var(--space-2) var(--space-3);
    margin: 0;
    padding-top: var(--space-2);
    border-top: 1px solid var(--line);
  }
  .stats dt {
    font-size: var(--text-xs);
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .stats dd {
    margin: 0;
    font-size: var(--text-sm);
    color: var(--fg-0);
  }
  .mono {
    font-family: var(--font-mono);
  }
</style>
