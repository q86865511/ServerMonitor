package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"servermonitor/internal/protocol"
)

// SupportedSchemaVersion 是本二進位支援的範本 schema 版本(R1)。載入時版本不符即拒載。
const SupportedSchemaVersion = 1

// AdapterRegistry 記錄「已知存在」的 adapter kind(R1),供範本引擎在載入時驗證範本
// 引用的 runtime / command_protocol / health kind 皆有對應實作(或至少已登記其種類)。
//
// 首版登記:runtime=docker、command_protocol=rcon/rest、health=tcp/rcon/rest/docker。
// rcon/rest 的實際指令 adapter 於 T9 實作,但此處先把其 kind 登記為「已知」,使引用它們的
// 範本驗證通過並標示 adapter 存在;真正的 adapter 綁定於 T9 補上。
type AdapterRegistry struct {
	runtimes         map[string]bool
	commandProtocols map[string]bool
	healthKinds      map[string]bool
}

// DefaultAdapterRegistry 回傳首版預設註冊表。
func DefaultAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{
		runtimes:         map[string]bool{"docker": true},
		commandProtocols: map[string]bool{"rcon": true, "rest": true},
		healthKinds:      map[string]bool{"tcp": true, "rcon": true, "rest": true, "docker": true},
	}
}

// HasRuntime 回報是否登記了某 runtime kind。
func (a *AdapterRegistry) HasRuntime(kind string) bool { return a.runtimes[kind] }

// HasCommandProtocol 回報是否登記了某 command_protocol kind。
func (a *AdapterRegistry) HasCommandProtocol(kind string) bool { return a.commandProtocols[kind] }

// HasHealthKind 回報是否登記了某 health kind。
func (a *AdapterRegistry) HasHealthKind(kind string) bool { return a.healthKinds[kind] }

// templateError 是範本驗證失敗的原因(供 TEMPLATE_LOAD_FAILED 事件的 reason/field)。
type templateError struct {
	field  string
	reason string
}

// TemplateEngine 載入、驗證並保存宣告式遊戲範本(R1)。載入一個目錄下所有 *.toml:
// 每份範本經語法解析(protocol.ParseTemplate)與語意驗證(schema 版本、必填欄位、
// template ID 不重複、引用的 adapter 存在);任一項不符則「拒載該範本」並寫入
// TEMPLATE_LOAD_FAILED 事件(含原因與欄位),其餘範本不受影響。併發安全。
type TemplateEngine struct {
	mu        sync.RWMutex
	registry  *AdapterRegistry
	events    *EventLog
	templates map[string]*protocol.GameTemplate
}

// NewTemplateEngine 建立範本引擎。registry 為 nil 時採 DefaultAdapterRegistry;
// events 可為 nil(則載入失敗不記事件,僅略過)。
func NewTemplateEngine(registry *AdapterRegistry, events *EventLog) *TemplateEngine {
	if registry == nil {
		registry = DefaultAdapterRegistry()
	}
	return &TemplateEngine{
		registry:  registry,
		events:    events,
		templates: make(map[string]*protocol.GameTemplate),
	}
}

// LoadDir 載入 dir 下所有 *.toml 範本(依檔名排序,使 ID 重複時「先者勝」有決定性)。
// 個別範本無效(解析失敗/驗證不過/ID 重複)只略過該檔並記事件,不影響其餘。
// 僅在無法讀取目錄本身時回錯。回傳成功載入的份數。
func (e *TemplateEngine) LoadDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("讀取範本目錄失敗: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".toml" {
			continue
		}
		names = append(names, ent.Name())
	}
	sort.Strings(names)

	loaded := 0
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			e.recordLoadFailed(name, "", templateError{field: "file", reason: rerr.Error()})
			continue
		}
		if e.loadOne(name, data) {
			loaded++
		}
	}
	return loaded, nil
}

// loadOne 解析並登錄單一範本;成功回 true。無效範本記 TEMPLATE_LOAD_FAILED 並回 false。
func (e *TemplateEngine) loadOne(file string, data []byte) bool {
	tmpl, err := protocol.ParseTemplate(data)
	if err != nil {
		e.recordLoadFailed(file, "", templateError{field: "toml", reason: err.Error()})
		return false
	}
	if terr, ok := e.validate(tmpl); !ok {
		e.recordLoadFailed(file, tmpl.ID, terr)
		return false
	}

	e.mu.Lock()
	if _, dup := e.templates[tmpl.ID]; dup {
		e.mu.Unlock()
		e.recordLoadFailed(file, tmpl.ID, templateError{
			field:  "id",
			reason: fmt.Sprintf("template ID %q 重複", tmpl.ID),
		})
		return false
	}
	e.templates[tmpl.ID] = tmpl
	e.mu.Unlock()
	return true
}

// validate 執行語意驗證。回傳第一個違規(field+reason)與是否通過。
func (e *TemplateEngine) validate(t *protocol.GameTemplate) (templateError, bool) {
	if t.SchemaVersion != SupportedSchemaVersion {
		return templateError{
			field:  "schema_version",
			reason: fmt.Sprintf("不支援的 schema_version %d(支援 %d)", t.SchemaVersion, SupportedSchemaVersion),
		}, false
	}
	if t.ID == "" {
		return templateError{field: "id", reason: "缺少 id"}, false
	}
	if t.Name == "" {
		return templateError{field: "name", reason: "缺少 name"}, false
	}
	if t.Runtime == "" {
		return templateError{field: "runtime", reason: "缺少 runtime"}, false
	}
	if !e.registry.HasRuntime(t.Runtime) {
		return templateError{
			field:  "runtime",
			reason: fmt.Sprintf("runtime adapter %q 不存在", t.Runtime),
		}, false
	}
	// runtime=docker 需鎖定映像(tag 或 digest)。
	if t.Runtime == "docker" {
		if t.Docker == nil || (t.Docker.Image == "" && t.Docker.ImageDigest == "") {
			return templateError{field: "docker.image", reason: "docker runtime 缺少映像鎖定(image 或 image_digest)"}, false
		}
	}
	for i, cp := range t.CommandProtocols {
		if cp.ProtocolID == "" {
			return templateError{
				field:  fmt.Sprintf("command_protocols[%d].protocol_id", i),
				reason: "缺少 protocol_id",
			}, false
		}
		if !e.registry.HasCommandProtocol(cp.Kind) {
			return templateError{
				field:  fmt.Sprintf("command_protocols[%d].kind", i),
				reason: fmt.Sprintf("command_protocol adapter %q 不存在", cp.Kind),
			}, false
		}
	}
	if t.Health != nil && !e.registry.HasHealthKind(t.Health.Kind) {
		return templateError{
			field:  "health.kind",
			reason: fmt.Sprintf("health adapter %q 不存在", t.Health.Kind),
		}, false
	}
	return templateError{}, true
}

// Get 依 ID 取範本;第二回傳值表示是否存在。
func (e *TemplateEngine) Get(id string) (*protocol.GameTemplate, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	t, ok := e.templates[id]
	return t, ok
}

// List 回傳所有已載入範本,依 ID 排序(供 GUI 列出可建立的遊戲類型;R1)。
func (e *TemplateEngine) List() []*protocol.GameTemplate {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*protocol.GameTemplate, 0, len(e.templates))
	for _, t := range e.templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// recordLoadFailed 寫入一筆 TEMPLATE_LOAD_FAILED 事件(R1:含原因與欄位)。
func (e *TemplateEngine) recordLoadFailed(file, templateID string, terr templateError) {
	if e.events == nil {
		return
	}
	details, _ := json.Marshal(map[string]string{
		"file":   file,
		"field":  terr.field,
		"reason": terr.reason,
	})
	ev := protocol.Event{
		Code:        protocol.EventTemplateLoadFailed,
		Severity:    protocol.SeverityError,
		DetailsJSON: details,
	}
	if templateID != "" {
		tid := templateID
		ev.TemplateID = &tid
	}
	_ = e.events.Append(ev)
}
