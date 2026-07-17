package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
		runtimes:         map[string]bool{"docker": true, "native": true}, // native-backend R2/R3
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
	sources   map[string]string // template ID → 其來源目錄(供 IconPath 解析範本相對 icon 路徑;R14)
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
		sources:   make(map[string]string),
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
		if e.loadOne(dir, name, data) {
			loaded++
		}
	}
	return loaded, nil
}

// loadOne 解析並登錄單一範本;成功回 true。無效範本記 TEMPLATE_LOAD_FAILED 並回 false。
// dir 為範本來源目錄,登錄以供 IconPath 解析範本相對的 icon 路徑(R14)。
func (e *TemplateEngine) loadOne(dir, file string, data []byte) bool {
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
	e.sources[tmpl.ID] = dir
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
	// [docker]/[native] 至少存在一個;runtime 欄位指定的預設 runtime 必須有對應區段
	// (native-backend R3)。缺對應區段時報明確欄位路徑(風格同 docker.image)。
	if t.Docker == nil && t.Native == nil {
		return templateError{field: "docker/native", reason: "至少需宣告 [docker] 或 [native] 其一"}, false
	}
	// runtime=docker 需鎖定映像(tag 或 digest)。
	if t.Runtime == "docker" {
		if t.Docker == nil || (t.Docker.Image == "" && t.Docker.ImageDigest == "") {
			return templateError{field: "docker.image", reason: "docker runtime 缺少映像鎖定(image 或 image_digest)"}, false
		}
	}
	// runtime=native 需宣告 [native] 區段。
	if t.Runtime == "native" && t.Native == nil {
		return templateError{field: "native", reason: "native runtime 缺少 [native] 區段"}, false
	}
	// [native] 區段自身驗證(不論是否為預設 runtime,只要宣告就驗)。
	if t.Native != nil {
		if terr, ok := validateNative(t.Native); !ok {
			return terr, false
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

// validateNative 驗證 [native] 區段(native-backend R3):供應類型與其必填參數、啟動命令、
// 設定映射格式。欄位路徑風格對齊上方 docker.image。
func validateNative(n *protocol.NativeSpec) (templateError, bool) {
	switch n.Provision.Kind {
	case "java":
		if n.Provision.JavaMajor <= 0 {
			return templateError{field: "native.provision.java_major", reason: "kind=java 需正整數 java_major"}, false
		}
	case "steamcmd":
		if n.Provision.SteamAppID == "" {
			return templateError{field: "native.provision.steam_app_id", reason: "kind=steamcmd 需 steam_app_id"}, false
		}
	case "":
		return templateError{field: "native.provision.kind", reason: "缺少 provision kind"}, false
	default:
		return templateError{
			field:  "native.provision.kind",
			reason: fmt.Sprintf("不支援的 provision kind %q(支援 java|steamcmd)", n.Provision.Kind),
		}, false
	}
	if len(n.Launch.Command) == 0 {
		return templateError{field: "native.launch.command", reason: "缺少啟動命令模板"}, false
	}
	for i, cm := range n.Config {
		switch cm.Format {
		case "properties", "palworld-ini":
		default:
			return templateError{
				field:  fmt.Sprintf("native.config[%d].format", i),
				reason: fmt.Sprintf("不支援的設定格式 %q(支援 properties|palworld-ini)", cm.Format),
			}, false
		}
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

// IconPath 回傳範本 id 的 icon 絕對(解析後)檔案路徑與是否可服務(R14)。以下任一不成立即回
// ("", false)——由 AssetServer handler 轉 404:範本不存在、未宣告 icon、路徑逃逸範本目錄、
// 目標不存在或非一般檔案(拒目錄/裝置)、或經 symlink/junction 指向範本目錄之外。
//
// 路徑拘束刻意用 filepath.Rel(範本目錄, 目標) 判定「不以 .. 開頭」,而非字串前綴檢查:後者有
// sibling-prefix 漏洞——`templates-secret` 亦以 `templates` 為前綴,字串前綴法會誤放行。解析
// symlink(filepath.EvalSymlinks)後再次做邊界判定,使「icon 或其路徑組件為 symlink 指向目錄外」
// 也被擋下(單純 os.Stat 會跟隨 symlink 而漏放)。
func (e *TemplateEngine) IconPath(id string) (string, bool) {
	e.mu.RLock()
	tmpl, ok := e.templates[id]
	dir, hasDir := e.sources[id]
	e.mu.RUnlock()
	if !ok || !hasDir || tmpl.Icon == "" {
		return "", false
	}

	// 1) 宣告層邊界:清理後的目標須落在範本目錄內(擋 `../` 與 sibling-prefix)。
	target := filepath.Join(dir, tmpl.Icon)
	if !withinDir(dir, target) {
		return "", false
	}
	// 2) 解析 symlink 後再次邊界判定:擋「symlink/junction 指向目錄外」。EvalSymlinks 亦要求存在。
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", false
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	if !withinDir(realDir, realTarget) {
		return "", false
	}
	// 3) 須為一般檔案(拒目錄/裝置/具名管道等)。
	info, err := os.Stat(realTarget)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return realTarget, true
}

// withinDir 回報 target 是否落在 dir 之內(含 dir 本身以下),以 filepath.Rel 的相對結果不以 `..`
// 起頭判定。刻意不用 strings.HasPrefix(target, dir):那會把 sibling 目錄(如 `templates-secret`
// 之於 `templates`)誤判為在內。跨磁碟或無法求相對路徑時(Rel 回錯)視為在外。
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
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
