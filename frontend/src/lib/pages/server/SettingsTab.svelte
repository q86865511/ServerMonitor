<script lang="ts">
  // 設定分頁(R6):唯讀組態檢視。範本/變體/runtime/埠/參數宣告(secret 類遮罩不顯值)。
  // 不提供任何編輯控件——上位規格 game-server-manager 不支援線上 update,修改參數需重建實例。
  import type { main } from '../../../../wailsjs/go/models';
  import Card from '../../ui/Card.svelte';
  import Badge from '../../ui/Badge.svelte';

  let {
    inst,
    template,
  }: {
    inst: main.InstanceDTO;
    template: main.TemplateDTO | null;
  } = $props();

  const templateName = $derived(template?.name || inst.template_id);
  const ports = $derived(inst.ports ?? []);

  // 範本宣告的參數;secret 類(type 含 secret/password 或列於 secrets)遮罩顯示,值永不進 DOM。
  function isSecretType(type: string): boolean {
    const t = (type || '').toLowerCase();
    return t.includes('secret') || t.includes('password');
  }

  interface ConfigField {
    key: string;
    label: string;
    type: string;
    required: boolean;
    secret: boolean;
  }

  const params = $derived<ConfigField[]>(
    (template?.params ?? []).map((p) => ({
      key: p.key,
      label: p.label || p.key,
      type: p.type,
      required: p.required,
      secret: isSecretType(p.type),
    })),
  );

  const secrets = $derived<ConfigField[]>(
    (template?.secrets ?? []).map((s) => ({
      key: s.key,
      label: s.label || s.key,
      type: 'SecretRef',
      required: false,
      secret: true,
    })),
  );

  const allFields = $derived<ConfigField[]>([...params, ...secrets]);

  function fmtIp(ip: string): string {
    return !ip || ip === '0.0.0.0' ? '全介面' : ip;
  }
</script>

<div class="settings-tab">
  <div class="hint">
    <span class="hint-icon" aria-hidden="true">ℹ</span>
    此為唯讀組態檢視。修改參數需重建實例(移除後以新參數重新建立);本頁不提供線上編輯。
  </div>

  <Card>
    <div class="sec-head">基本組態</div>
    <dl class="kv">
      <dt>範本</dt>
      <dd>{templateName} <span class="muted mono">({inst.template_id})</span></dd>
      <dt>變體</dt>
      <dd>{inst.variant || '(預設)'}</dd>
      <dt>執行後端</dt>
      <dd><Badge tone={inst.runtime === 'docker' ? 'accent' : 'off'}>{inst.runtime || '未知'}</Badge></dd>
      <dt>節點</dt>
      <dd>{inst.node || 'local'}</dd>
    </dl>
  </Card>

  <Card>
    <div class="sec-head">連接埠</div>
    {#if ports.length === 0}
      <div class="na">尚無保留連接埠</div>
    {:else}
      <div class="ports">
        {#each ports as p (p.name + p.protocol + p.host_port)}
          <div class="port-row">
            <span class="p-name">{p.name || '(未命名)'}</span>
            <span class="p-addr mono">{fmtIp(p.bind_ip)}:{p.host_port}</span>
            <span class="p-proto">{(p.protocol || 'tcp').toUpperCase()}</span>
          </div>
        {/each}
      </div>
    {/if}
  </Card>

  <Card>
    <div class="sec-head">參數宣告</div>
    {#if allFields.length === 0}
      <div class="na">此範本無宣告參數。</div>
    {:else}
      <div class="fields">
        {#each allFields as f (f.key)}
          <div class="field-row">
            <span class="f-label">
              {f.label}
              {#if f.required}<span class="req">*</span>{/if}
            </span>
            <span class="f-key mono muted">{f.key}</span>
            <span class="f-type">
              {#if f.secret}
                <Badge tone="busy">機密</Badge>
                <span class="masked mono" aria-label="機密值不顯示">••••••••</span>
              {:else}
                <Badge tone="neutral">{f.type}</Badge>
              {/if}
            </span>
          </div>
        {/each}
      </div>
      <div class="foot-note">機密欄位(密碼/金鑰參照)不顯示實際值。</div>
    {/if}
  </Card>
</div>

<style>
  .settings-tab {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .hint {
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--fg-1);
    background: var(--accent-bg);
    border: 1px solid var(--accent);
    border-radius: var(--radius-sm);
    padding: 10px 12px;
  }
  .hint-icon {
    color: var(--accent);
    font-weight: 700;
    flex: none;
  }
  .sec-head {
    font-size: var(--text-sm);
    color: var(--fg-2);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    margin-bottom: var(--space-3);
  }
  .kv {
    display: grid;
    grid-template-columns: 120px 1fr;
    gap: var(--space-2) var(--space-4);
    margin: 0;
  }
  .kv dt {
    color: var(--fg-2);
    font-size: var(--text-sm);
  }
  .kv dd {
    margin: 0;
    color: var(--fg-0);
    font-size: var(--text-sm);
  }
  .ports {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .port-row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }
  .p-name {
    color: var(--fg-1);
    min-width: 88px;
  }
  .p-addr {
    color: var(--fg-0);
    flex: 1;
  }
  .p-proto {
    color: var(--fg-2);
    font-size: var(--text-xs);
  }
  .fields {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .field-row {
    display: grid;
    grid-template-columns: 1fr 1fr auto;
    align-items: center;
    gap: var(--space-3);
    font-size: var(--text-sm);
    padding: 4px 0;
    border-bottom: 1px solid var(--line);
  }
  .field-row:last-child {
    border-bottom: none;
  }
  .f-label {
    color: var(--fg-0);
  }
  .req {
    color: var(--err);
    margin-left: 3px;
  }
  .f-key {
    color: var(--fg-2);
  }
  .f-type {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    justify-content: flex-end;
  }
  .masked {
    color: var(--fg-2);
    letter-spacing: 2px;
  }
  .foot-note {
    margin-top: var(--space-3);
    font-size: var(--text-xs);
    color: var(--fg-2);
  }
  .na {
    color: var(--fg-2);
    font-size: var(--text-sm);
  }
  .muted {
    color: var(--fg-2);
  }
  .mono {
    font-family: var(--font-mono);
  }
</style>
