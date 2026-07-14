package core

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// gsm.* 標籤鍵(R2 對帳/擁有權標記),與 agent 端 docker.go 的常數對齊。定義於此使核心
// 建立 spec 時可打上權威標籤;agent 建容器時亦會疊加,兩處值須一致。
const (
	labelManagedBy = "gsm.managed-by"
	labelUUID      = "gsm.uuid"
	labelNode      = "gsm.node"
	labelSchema    = "gsm.schema"

	managedByValue = "servermonitor"

	// defaultNode 是單機首版的本機節點識別。
	defaultNode = "local"
)

// 建立相關錯誤哨符(R2)。以 errors.Is 判別;細節由對應的具型別錯誤攜帶。
var (
	// ErrTemplateNotFound 表示指定 template ID 未載入。
	ErrTemplateNotFound = errors.New("core: 範本不存在")
	// ErrVariantNotFound 表示指定 variant 不在範本中。
	ErrVariantNotFound = errors.New("core: 變體不存在")
	// ErrMissingParams 表示缺少必填參數(建立前阻擋)。
	ErrMissingParams = errors.New("core: 缺少必填參數")
	// ErrEULANotAccepted 表示必填的同意項(如 Minecraft EULA)未接受。
	ErrEULANotAccepted = errors.New("core: 必填同意項未接受")
	// ErrPortConflict 表示埠衝突(ERR_PORT_CONFLICT 語意);見 PortConflictError.Code。
	ErrPortConflict = errors.New("core: 埠衝突")
	// ErrMissingSecrets 表示「被啟用中的 command_protocols 引用」的必填機密未提供
	// (建立前阻擋;legacy=true 的協定不計入,見 requiredSecretKeys)。
	ErrMissingSecrets = errors.New("core: 缺少必填機密")
)

// ParamError 攜帶被阻擋的參數/機密清單(R2:指出缺項)。
type ParamError struct {
	Missing        []string // 缺少的必填參數
	Unaccepted     []string // 必填但未接受的同意項(必填 bool 未設為 true,如 EULA)
	MissingSecrets []string // 缺少的必填機密(被啟用中的指令協定引用,見 requiredSecretKeys)
}

func (e *ParamError) Error() string {
	var parts []string
	if len(e.Missing) > 0 {
		parts = append(parts, "缺少必填參數: "+strings.Join(e.Missing, ", "))
	}
	if len(e.Unaccepted) > 0 {
		parts = append(parts, "未接受必填同意項: "+strings.Join(e.Unaccepted, ", "))
	}
	if len(e.MissingSecrets) > 0 {
		parts = append(parts, "缺少必填機密: "+strings.Join(e.MissingSecrets, ", "))
	}
	return "core: " + strings.Join(parts, ";")
}

// Is 使 ParamError 可被 errors.Is 對應到 ErrMissingParams / ErrEULANotAccepted / ErrMissingSecrets。
func (e *ParamError) Is(target error) bool {
	switch target {
	case ErrMissingParams:
		return len(e.Missing) > 0
	case ErrEULANotAccepted:
		return len(e.Unaccepted) > 0
	case ErrMissingSecrets:
		return len(e.MissingSecrets) > 0
	}
	return false
}

// PortConflictError 攜帶衝突的埠鍵(R2)。Code() 回 ERR_PORT_CONFLICT 語意。
type PortConflictError struct {
	BindIP       string
	Protocol     string
	HostPort     int
	ExistingUUID string // 已占用該埠鍵的實例(若為既有預留)
}

func (e *PortConflictError) Error() string {
	return fmt.Sprintf("core: 埠衝突 %s/%s:%d(已被 %q 占用)",
		e.BindIP, e.Protocol, e.HostPort, e.ExistingUUID)
}

// Is 使 PortConflictError 對應到 ErrPortConflict。
func (e *PortConflictError) Is(target error) bool { return target == ErrPortConflict }

// Code 回傳統一錯誤碼(供 GUI/上層以 ERR_PORT_CONFLICT 語意呈現)。
func (e *PortConflictError) Code() protocol.ErrorCode { return protocol.ErrPortConflict }

// CreateOptions 是 InstanceService.Create 的輸入。
type CreateOptions struct {
	TemplateID string            // 範本 ID(必填)
	Variant    string            // 變體 ID(可空)
	Params     map[string]string // 非機密參數值(key 即容器 env 變數名,itzg 慣例)
	Secrets    map[string]string // 機密值(範本 [[secrets]].key → 明文);寫入金鑰庫,不落 DB
	Node       string            // 目標節點(可空,預設本機節點)
	Modpack    *ModpackSource    // R11 模組包來源(可空;itzg 原生透傳,見 modpack.go)
}

// InstanceService 實作 R2「一鍵建立」的原子建立骨幹:驗證必填/EULA → 機密入庫 →
// 埠預留(含 wildcard 重疊規則)→ 建立 journal → 經 NodeClient 呼叫代理建容器 →
// 寫 DB 完成 → 清 journal;任一階段失敗回滾。狀態機/啟停編排屬 T8,不在此。
type InstanceService struct {
	store    *Store
	secrets  *SecretStore
	events   *EventLog
	engine   *TemplateEngine
	registry *NodeRegistry
	journal  *Journal
	node     string

	newUUID func() string
	now     func() time.Time

	// reserveMu 序列化「埠重疊檢查 + 預留」臨界區,使兩個併發建立不會同時通過
	// wildcard 重疊檢查(唯一約束只擋完全相同鍵,擋不住 0.0.0.0 vs 具體 IP 的重疊)。
	reserveMu sync.Mutex

	// hookAfterAgentCreate 是測試縫:於代理成功建容器後注入失敗,以驗證「已建容器」
	// 階段失敗的回滾(移除孤兒容器)。生產環境恆為 nil。
	hookAfterAgentCreate func(uuid string) error
}

// InstanceServiceConfig 是 InstanceService 的建構參數。
type InstanceServiceConfig struct {
	Store    *Store
	Secrets  *SecretStore
	Events   *EventLog
	Engine   *TemplateEngine
	Registry *NodeRegistry
	Journal  *Journal
	Node     string        // 預設節點;空字串用 "local"
	NewUUID  func() string // 測試可注入;預設 UUIDv4
	Now      func() time.Time
}

// NewInstanceService 建立 InstanceService。
func NewInstanceService(cfg InstanceServiceConfig) *InstanceService {
	node := cfg.Node
	if node == "" {
		node = defaultNode
	}
	newUUID := cfg.NewUUID
	if newUUID == nil {
		newUUID = newUUIDv4
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &InstanceService{
		store:    cfg.Store,
		secrets:  cfg.Secrets,
		events:   cfg.Events,
		engine:   cfg.Engine,
		registry: cfg.Registry,
		journal:  cfg.Journal,
		node:     node,
		newUUID:  newUUID,
		now:      now,
	}
}

// Create 以原子方式建立一個 Created 狀態實例(R2)。成功回傳登錄的 InstanceRecord;
// 任一階段失敗則完成回滾(釋放埠、刪除已寫機密、移除已建容器、不留 DB 完成紀錄)並記
// INSTANCE_CREATE_FAILED。埠衝突回 *PortConflictError(errors.Is(…, ErrPortConflict))。
func (s *InstanceService) Create(ctx context.Context, opts CreateOptions) (rec InstanceRecord, err error) {
	tmpl, ok := s.engine.Get(opts.TemplateID)
	if !ok {
		return InstanceRecord{}, fmt.Errorf("%w: %s", ErrTemplateNotFound, opts.TemplateID)
	}
	node := opts.Node
	if node == "" {
		node = s.node
	}

	// 1. 驗證變體與必填參數 / 同意項(無副作用,失敗前不動任何狀態)。
	variantEnv, verr := resolveVariant(tmpl, opts.Variant)
	if verr != nil {
		return InstanceRecord{}, verr
	}
	if perr := validateParams(tmpl, opts.Params); perr != nil {
		return InstanceRecord{}, perr
	}
	if serr := validateSecrets(tmpl, opts.Secrets); serr != nil {
		return InstanceRecord{}, serr
	}
	// R11 模組包前置檢查(型別相容/CF_API_KEY/手動檔格式);無副作用,失敗前不動任何狀態。
	if merr := validateModpack(tmpl, opts.Variant, opts); merr != nil {
		return InstanceRecord{}, merr
	}

	uuid := s.newUUID()
	ports := buildPortReservations(tmpl, uuid)

	// 回滾狀態旗標;下方 defer 於 err!=nil 時反序清理已完成的階段。
	var (
		secretsWritten []protocol.SecretRef
		portsReserved  bool
		journalWritten bool
		containermade  bool
		completed      bool
	)
	defer func() {
		if err == nil || completed {
			return
		}
		s.rollback(ctx, node, uuid, rollbackState{
			secrets:        secretsWritten,
			portsReserved:  portsReserved,
			journalWritten: journalWritten,
			containerMade:  containermade,
		})
		s.recordCreateFailed(uuid, tmpl.ID, node, err)
	}()

	// 2. 機密入庫(R12:值寫 OS 金鑰庫,DB 不落明文;金鑰庫鍵以 UUID 命名空間避免跨實例碰撞)。
	for _, sec := range tmpl.Secrets {
		val, provided := opts.Secrets[sec.Key]
		if !provided {
			continue
		}
		ref := protocol.NewSecretRef(instanceSecretKey(uuid, sec.Key))
		if serr := s.secrets.Set(ref, val); serr != nil {
			return InstanceRecord{}, fmt.Errorf("寫入機密 %s 失敗: %w", sec.Key, serr)
		}
		secretsWritten = append(secretsWritten, ref)
	}

	// 3. 埠預留(含 wildcard 重疊規則),臨界區序列化。
	if perr := s.reservePorts(ports); perr != nil {
		portsReserved = true // 可能已部分預留,交回滾統一釋放
		return InstanceRecord{}, perr
	}
	portsReserved = true

	// 4. 寫建立 journal(記錄進行中建立,供崩潰後 R13 對帳清孤兒)。
	if jerr := s.journal.Begin(JournalEntry{
		UUID: uuid, Node: node, TemplateID: tmpl.ID, StartedAt: s.now().UTC(),
	}); jerr != nil {
		return InstanceRecord{}, fmt.Errorf("寫入建立 journal 失敗: %w", jerr)
	}
	journalWritten = true

	// 5. 經 NodeClient 呼叫代理建容器(機密於此邊界注入 Env:R12 runtime 明文例外,loopback 信任域)。
	spec := s.buildSpec(uuid, node, tmpl, opts, variantEnv)
	var runtimeID protocol.RuntimeID
	cerr := s.registry.Call(node, func(c *NodeClient) error {
		resp, e := c.Create(ctx, spec)
		runtimeID = resp.RuntimeID
		return e
	})
	if cerr != nil {
		return InstanceRecord{}, fmt.Errorf("代理建立容器失敗: %w", cerr)
	}
	containermade = true

	// 5b. 手動模組包檔上傳(代理建容器後、寫 DB 前):讀本機 ModpackSource.Ref 送達 agent 掛載目錄。
	//     上傳失敗走既有回滾(移除容器/釋放埠/刪機密/清 journal),不留半套。
	if uerr := s.uploadModpackMount(ctx, node, uuid, tmpl, opts); uerr != nil {
		return InstanceRecord{}, fmt.Errorf("上傳手動模組包檔失敗: %w", uerr)
	}

	// 測試縫:模擬「已建容器、寫 DB 前」失敗,驗證回滾移除孤兒容器。
	if s.hookAfterAgentCreate != nil {
		if herr := s.hookAfterAgentCreate(uuid); herr != nil {
			return InstanceRecord{}, herr
		}
	}

	// 6. 寫 DB 完成(狀態 Created)。
	rec = InstanceRecord{
		UUID:          uuid,
		TemplateID:    tmpl.ID,
		Variant:       opts.Variant,
		ParamsJSON:    marshalParams(opts.Params),
		Node:          node,
		RuntimeID:     runtimeID,
		DesiredState:  protocol.InstanceStateStopped, // 建立後未啟動;desired 停止(啟動由 T8 編排)
		ObservedState: protocol.InstanceStateCreated,
	}
	if uerr := s.store.UpsertInstance(rec); uerr != nil {
		return InstanceRecord{}, fmt.Errorf("寫入實例紀錄失敗: %w", uerr)
	}

	// 7. 清 journal;完成。
	if jerr := s.journal.Complete(uuid); jerr != nil {
		// 已落 DB,journal 殘留只會讓對帳多做一次無害檢查;不視為建立失敗。
		s.recordEvent(protocol.Event{
			Code: protocol.EventInstanceCreated, Severity: protocol.SeverityWarning,
			InstanceUUID: &uuid, TemplateID: &tmpl.ID, Node: &node,
			DetailsJSON: mustJSON(map[string]string{"warning": "journal 清除失敗: " + jerr.Error()}),
		})
	}
	completed = true

	s.recordEvent(protocol.Event{
		Code:         protocol.EventInstanceCreated,
		Severity:     protocol.SeverityInfo,
		InstanceUUID: &uuid,
		TemplateID:   &tmpl.ID,
		Node:         &node,
		DetailsJSON:  mustJSON(map[string]string{"runtime_id": string(runtimeID)}),
	})
	return rec, nil
}

// rollbackState 記錄建立過程已完成、需回滾的階段。
type rollbackState struct {
	secrets        []protocol.SecretRef
	portsReserved  bool
	journalWritten bool
	containerMade  bool
}

// rollback 反序清理已完成的建立階段(R2 原子性)。
//
// journal 的清除有條件:唯有確認「代理端無殘留容器」(移除成功或代理回報查無)才清 journal;
// 若代理不可達而無法確認,保留 journal 供下次啟動的 R13 對帳清理(避免遺漏孤兒)。
func (s *InstanceService) rollback(ctx context.Context, node, uuid string, st rollbackState) {
	orphanGone := true
	if st.journalWritten {
		// 不論本地是否認為容器已建,一律嘗試依 UUID 移除,涵蓋「回應遺失但容器已建」的孤兒。
		rerr := s.removeContainer(ctx, node, uuid)
		orphanGone = rerr == nil || errors.Is(rerr, ErrNodeNotFound)
		if orphanGone {
			_ = s.journal.Complete(uuid)
		}
		_ = st.containerMade // 已由上面的移除涵蓋
	}
	if st.portsReserved {
		_ = s.store.ReleasePortsForInstance(uuid)
	}
	for _, ref := range st.secrets {
		_ = s.secrets.Delete(ref)
	}
}

// removeContainer 經 NodeClient 依 UUID 移除容器(purge);冪等(查無視為成功由呼叫端判定)。
func (s *InstanceService) removeContainer(ctx context.Context, node, uuid string) error {
	return s.registry.Call(node, func(c *NodeClient) error {
		return c.Remove(ctx, uuid, true)
	})
}

// uploadModpackMount 於代理建容器後,把手動模組包本機檔上傳到 agent 的具名掛載目錄(R11)。
// 非手動來源為 no-op;上傳到 modpack mount(檔名取 ref 基礎名,與 applyModpackEnv 的容器路徑對齊)。
func (s *InstanceService) uploadModpackMount(ctx context.Context, node, uuid string, tmpl *protocol.GameTemplate, opts CreateOptions) error {
	if tmpl.Mods == nil || !isManualModpack(opts.Modpack) {
		return nil
	}
	f, err := os.Open(opts.Modpack.Ref)
	if err != nil {
		return fmt.Errorf("開啟模組包檔失敗: %w", err)
	}
	defer f.Close()
	filename := modpackMountFilename(opts.Modpack.Ref)
	return s.registry.Call(node, func(c *NodeClient) error {
		return c.UploadMount(ctx, uuid, modpackMountName, filename, f)
	})
}

// reservePorts 在序列化臨界區內做 wildcard 重疊檢查後逐一預留(R2)。
// 檢查涵蓋「與既有 DB 預留」及「本批範本內部」的重疊;唯一約束為併發下的最終防線。
func (s *InstanceService) reservePorts(ports []PortReservation) error {
	s.reserveMu.Lock()
	defer s.reserveMu.Unlock()

	existing, err := s.store.ListPortReservations()
	if err != nil {
		return fmt.Errorf("查詢既有埠預留失敗: %w", err)
	}

	checked := append([]PortReservation(nil), existing...)
	for _, np := range ports {
		for _, ep := range checked {
			if portsOverlap(np, ep) {
				return &PortConflictError{
					BindIP:       np.BindIP,
					Protocol:     np.Protocol,
					HostPort:     np.HostPort,
					ExistingUUID: ep.InstanceUUID,
				}
			}
		}
		checked = append(checked, np)
	}

	for _, np := range ports {
		if rerr := s.store.ReservePort(np); rerr != nil {
			if errors.Is(rerr, ErrPortReserved) {
				return &PortConflictError{BindIP: np.BindIP, Protocol: np.Protocol, HostPort: np.HostPort}
			}
			return fmt.Errorf("預留埠失敗: %w", rerr)
		}
	}
	return nil
}

// buildSpec 由範本 + 使用者輸入組出 InstanceSpec(env 解析、機密注入、標籤、映像鎖)。
func (s *InstanceService) buildSpec(uuid, node string, tmpl *protocol.GameTemplate, opts CreateOptions, variantEnv map[string]string) protocol.InstanceSpec {
	env := resolveEnv(tmpl, opts.Params, variantEnv)
	// 機密以明文注入 Env(R12 runtime 明文例外;僅在 loopback 信任域內經 NodeClient 傳遞)。
	for _, sec := range tmpl.Secrets {
		if val, ok := opts.Secrets[sec.Key]; ok {
			env[sec.Key] = val
		}
	}
	// R11 模組包 env(itzg 原生透傳):TYPE 由模組包機制接管(覆寫變體 TYPE),故置於機密注入之後。
	applyModpackEnv(tmpl, opts.Modpack, env)

	// 手動模組包檔改走 Mounts 具名掛載(不併入 DataDirs,#3:解除備份汙染);檔案位元組於
	// Create 建容器後經 UploadMount 送達。DataDirs 維持範本原樣。
	dataDirs := append([]string(nil), tmpl.DataDirs...)

	return protocol.InstanceSpec{
		UUID:       uuid,
		TemplateID: tmpl.ID,
		Variant:    opts.Variant,
		Image:      resolveImage(tmpl.Docker),
		Env:        env,
		Ports:      buildPortBindings(tmpl),
		DataDirs:   dataDirs,
		Mounts:     modpackMounts(tmpl, opts.Modpack),
		Labels: map[string]string{
			labelManagedBy: managedByValue,
			labelUUID:      uuid,
			labelNode:      node,
			labelSchema:    strconv.Itoa(tmpl.SchemaVersion),
		},
		Node: node,
	}
}

func (s *InstanceService) recordCreateFailed(uuid, templateID, node string, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	s.recordEvent(protocol.Event{
		Code:         protocol.EventInstanceCreateFailed,
		Severity:     protocol.SeverityError,
		InstanceUUID: &uuid,
		TemplateID:   &templateID,
		Node:         &node,
		DetailsJSON:  mustJSON(map[string]string{"error": msg}),
	})
}

func (s *InstanceService) recordEvent(ev protocol.Event) {
	if s.events == nil {
		return
	}
	_ = s.events.Append(ev)
}

// ---- 純函式輔助(範本解析、埠、env)----

// resolveVariant 回傳選定變體的 env(未選變體回 nil);變體不存在回 ErrVariantNotFound。
func resolveVariant(tmpl *protocol.GameTemplate, variant string) (map[string]string, error) {
	if variant == "" {
		return nil, nil
	}
	for _, v := range tmpl.Variants {
		if v.ID == variant {
			return v.Env, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrVariantNotFound, variant)
}

// validateParams 檢查必填參數齊備(R2:缺項於建立前阻擋);必填 bool 需為 true(涵蓋
// Minecraft EULA「未接受不得建立、不暗中預設」——不硬編遊戲語意,而以「必填同意項」通則表達)。
//
// I 修正:必填參數的值直接查 params(使用者輸入),不經 resolveParamValue 回退範本
// default——否則範本作者若把 required=true 的參數又設了 default=true(如誤寫的 EULA
// 宣告),使用者未明確提供時會被 default 悄悄滿足,架空「必填同意項需主動同意」的防呆
// 用意。非必填參數的 default 回退不受影響,見 resolveParamValue/resolveEnv。
func validateParams(tmpl *protocol.GameTemplate, params map[string]string) error {
	var pe ParamError
	for _, p := range tmpl.Params {
		if !p.Required {
			continue
		}
		val := strings.TrimSpace(params[p.Key])
		if val == "" {
			pe.Missing = append(pe.Missing, p.Key)
			continue
		}
		if strings.EqualFold(p.Type, "bool") {
			b, err := strconv.ParseBool(val)
			if err != nil || !b {
				pe.Unaccepted = append(pe.Unaccepted, p.Key)
			}
		}
	}
	if len(pe.Missing) > 0 || len(pe.Unaccepted) > 0 {
		return &pe
	}
	return nil
}

// requiredSecretKeys 回傳「被啟用中的 command_protocols 引用」的必填機密鍵(E 修正)。
// legacy=true 的協定(如 Palworld RCON)首版不啟用,其引用的 secret 不計入必填,避免
// 使用者被要求填一個用不到的機密。同一鍵可能被協定層與動作層(可覆寫)重複引用,
// 去重後依鍵名排序回傳,使缺項清單順序穩定、可測。
func requiredSecretKeys(tmpl *protocol.GameTemplate) []string {
	seen := make(map[string]bool)
	for _, cp := range tmpl.CommandProtocols {
		if cp.Legacy {
			continue
		}
		if cp.PasswordRef != "" {
			seen[cp.PasswordRef] = true
		}
		for _, act := range cp.Actions {
			if act.PasswordRef != "" {
				seen[act.PasswordRef] = true
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateSecrets 檢查必填機密齊備(E 修正:見 requiredSecretKeys)。純函式、無副作用
// (不觸碰金鑰庫/DB/journal),呼叫時機在 Create 任一寫入動作之前。
func validateSecrets(tmpl *protocol.GameTemplate, secrets map[string]string) error {
	var pe ParamError
	for _, key := range requiredSecretKeys(tmpl) {
		if strings.TrimSpace(secrets[key]) == "" {
			pe.MissingSecrets = append(pe.MissingSecrets, key)
		}
	}
	if len(pe.MissingSecrets) > 0 {
		return &pe
	}
	return nil
}

// resolveParamValue 取參數的有效值:使用者輸入優先,否則用範本 default(字串化);皆無回空字串。
func resolveParamValue(p protocol.ParamSpec, params map[string]string) string {
	if v, ok := params[p.Key]; ok {
		return strings.TrimSpace(v)
	}
	return stringifyDefault(p.Default)
}

// resolveEnv 疊出容器 env:變體 env → 參數(key 即 env 名);空值不注入以免覆蓋映像預設。
func resolveEnv(tmpl *protocol.GameTemplate, params, variantEnv map[string]string) map[string]string {
	env := make(map[string]string)
	for k, v := range variantEnv {
		env[k] = v
	}
	for _, p := range tmpl.Params {
		val := resolveParamValue(p, params)
		if val != "" {
			env[p.Key] = val
		}
	}
	return env
}

// resolveImage 由 [docker] 組出鎖定映像字串:有 digest 用 image@digest,否則用 image(tag)。
func resolveImage(d *protocol.DockerImage) string {
	if d == nil {
		return ""
	}
	if d.ImageDigest != "" {
		if d.Image != "" {
			return d.Image + "@" + d.ImageDigest
		}
		return d.ImageDigest
	}
	return d.Image
}

// buildPortReservations 由範本 ports 造 DB 預留列(只取 host_port>0 的靜態綁定;
// host_port=0 為動態分配,首版不預留、交由後端 OS 分配)。
func buildPortReservations(tmpl *protocol.GameTemplate, uuid string) []PortReservation {
	var out []PortReservation
	for _, p := range tmpl.Ports {
		if p.HostPort <= 0 {
			continue
		}
		out = append(out, PortReservation{
			BindIP:       normalizeBindIP(p.BindIP),
			Protocol:     normalizeProtocol(p.Protocol),
			HostPort:     p.HostPort,
			InstanceUUID: uuid,
		})
	}
	return out
}

// buildPortBindings 由範本 ports 造要傳給代理的 PortBinding(含動態埠)。
func buildPortBindings(tmpl *protocol.GameTemplate) []protocol.PortBinding {
	out := make([]protocol.PortBinding, 0, len(tmpl.Ports))
	for _, p := range tmpl.Ports {
		out = append(out, protocol.PortBinding{
			Name:      p.Name,
			Container: p.Container,
			HostPort:  p.HostPort,
			BindIP:    normalizeBindIP(p.BindIP),
			Protocol:  normalizeProtocol(p.Protocol),
		})
	}
	return out
}

// ---- 埠 wildcard 重疊規則(R2)----

// portsOverlap 回報兩個埠鍵是否衝突:須同協定、同 host_port,且 bind_ip 依 wildcard 規則重疊。
func portsOverlap(a, b PortReservation) bool {
	if a.HostPort != b.HostPort || a.Protocol != b.Protocol {
		return false
	}
	return bindIPsOverlap(a.BindIP, b.BindIP)
}

// bindIPsOverlap 判定兩個 bind_ip 是否重疊(R2):
//   - 完全相同 → 重疊。
//   - 同 IP 家族內,任一方為 wildcard(0.0.0.0 / ::)→ 重疊(wildcard 覆蓋規則)。
//   - IPv4 與 IPv6 各自判定,跨家族不重疊。
func bindIPsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	fa, wa := ipFamilyWildcard(a)
	fb, wb := ipFamilyWildcard(b)
	if fa == 0 || fb == 0 || fa != fb {
		return false
	}
	return wa || wb
}

// ipFamilyWildcard 回傳 (家族, 是否 wildcard)。家族 4=IPv4、6=IPv6、0=無法解析。
// 空字串視為 IPv4 wildcard(等同 0.0.0.0,Docker 對空綁定的預設)。
func ipFamilyWildcard(s string) (family int, wildcard bool) {
	if s == "" {
		return 4, true
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, false
	}
	if ip.To4() != nil {
		return 4, ip.IsUnspecified()
	}
	return 6, ip.IsUnspecified()
}

func normalizeBindIP(ip string) string {
	if ip == "" {
		return "0.0.0.0"
	}
	return ip
}

func normalizeProtocol(p string) string {
	if p == "" {
		return "tcp"
	}
	return strings.ToLower(p)
}

// ---- 其他輔助 ----

// instanceSecretKey 以 UUID 命名空間包住範本機密鍵,避免多實例共用同一金鑰庫鍵而互相覆蓋。
func instanceSecretKey(uuid, key string) string {
	return uuid + ":" + key
}

// marshalParams 把非機密參數序列化為 params_json(機密不入,見 R12)。
func marshalParams(params map[string]string) json.RawMessage {
	if len(params) == 0 {
		return json.RawMessage("{}")
	}
	// 以排序鍵輸出,使 params_json 穩定(便於比對/測試)。
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(params))
	for _, k := range keys {
		ordered[k] = params[k]
	}
	data, err := json.Marshal(ordered)
	if err != nil {
		return json.RawMessage("{}")
	}
	return data
}

func stringifyDefault(def any) string {
	if def == nil {
		return ""
	}
	switch v := def.(type) {
	case string:
		return strings.TrimSpace(v)
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}

// newUUIDv4 產生一個隨機 UUIDv4 字串(免外部依賴,對齊 agent 以 crypto/rand 產 token 的做法)。
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
