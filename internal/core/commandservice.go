package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"servermonitor/internal/protocol"
)

// ErrCommandNotEnabled 表示範本未定義可用(非 legacy)指令協定,或所引用之協定為 legacy/不存在。
// GUI 據此停用指令輸入;hooks 據此判定 no-op。
var ErrCommandNotEnabled = errors.New("core: 範本未啟用指令協定")

// EventHookFailed 標記生命週期 hook(如 hooks.stop)以 best-effort 執行失敗——僅記錄、不阻擋
// 主流程(R3)。屬補充事件碼(不在 protocol.RequiredEventCodes),與 EventConfigRecovered 同慣例。
const EventHookFailed protocol.EventCode = "HOOK_FAILED"

// CommandService 把「範本指令協定 + 實例埠映射 + 金鑰庫機密」解析為 protocol.CommandTarget,
// 經 NodeClient.Command 送達代理執行(R7)。它是 core 端唯一組裝指令目標的地方:GUI 指令、
// 生命週期 hooks(hooks.stop)、排程公告(hooks.announce)皆走此服務,agent 不再解析範本。
// 機密以 UUID 命名空間自 SecretStore 取實值,循 R12「runtime 明文例外」隨 target 過 loopback。
type CommandService struct {
	store    *Store
	engine   *TemplateEngine
	secrets  *SecretStore
	registry *NodeRegistry
}

// NewCommandService 建立 CommandService。
func NewCommandService(store *Store, engine *TemplateEngine, secrets *SecretStore, registry *NodeRegistry) *CommandService {
	return &CommandService{store: store, engine: engine, secrets: secrets, registry: registry}
}

// Send 以範本「啟用中」協定(第一個非 legacy)送出一則遊戲指令並回顯(R7)。供 GUI 指令輸入。
// 範本無可用協定→ErrCommandNotEnabled。
func (s *CommandService) Send(ctx context.Context, uuid string, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	rec, tmpl, err := s.resolveInstance(uuid)
	if err != nil {
		return protocol.CommandResult{}, err
	}
	cp, err := activeProtocol(tmpl)
	if err != nil {
		return protocol.CommandResult{}, err
	}
	return s.dispatch(ctx, rec, tmpl, cp, cmd)
}

// Announce 經範本 hooks.announce 廣播訊息(供 T11 排程重啟前公告)。範本無 announce hook 或其
// 協定為 legacy/不存在→ErrCommandNotEnabled 之屬。
func (s *CommandService) Announce(ctx context.Context, uuid, msg string) (protocol.CommandResult, error) {
	rec, tmpl, err := s.resolveInstance(uuid)
	if err != nil {
		return protocol.CommandResult{}, err
	}
	hook := tmpl.Hooks.Announce
	if hook == nil {
		return protocol.CommandResult{}, fmt.Errorf("%w: 範本 %q 無 hooks.announce", ErrCommandNotEnabled, tmpl.ID)
	}
	cp, err := protocolByID(tmpl, hook.ProtocolID)
	if err != nil {
		return protocol.CommandResult{}, err
	}
	return s.dispatch(ctx, rec, tmpl, cp, buildHookCommand(cp, hook, msg))
}

// RunStopHook 以 best-effort 執行範本 hooks.stop(供 Orchestrator.Stop 於代理 Stop 之前呼叫,
// 讓遊戲伺服器優雅存檔退出;如 Minecraft RCON "stop"、Palworld REST shutdown)。
// 範本無 hooks.stop→視為 no-op 回 nil;有 hook 但送出失敗→回錯誤,由呼叫端記事件、不阻擋停止。
func (s *CommandService) RunStopHook(ctx context.Context, uuid string) error {
	rec, tmpl, err := s.resolveInstance(uuid)
	if err != nil {
		return err
	}
	hook := tmpl.Hooks.Stop
	if hook == nil {
		return nil // 無 stop hook:no-op
	}
	cp, err := protocolByID(tmpl, hook.ProtocolID)
	if err != nil {
		return err
	}
	_, serr := s.dispatch(ctx, rec, tmpl, cp, buildHookCommand(cp, hook, ""))
	return serr
}

// Capability 回傳實例「啟用中」指令協定(第一個非 legacy),供 GUI 主控台決定渲染方式
// (rcon 自由輸入 / rest 具名動作 / 停用)。範本無可用協定→ErrCommandNotEnabled;實例或範本
// 查無→對應錯誤。不送出任何指令,純解析。
func (s *CommandService) Capability(uuid string) (protocol.CommandProtocol, error) {
	_, tmpl, err := s.resolveInstance(uuid)
	if err != nil {
		return protocol.CommandProtocol{}, err
	}
	return activeProtocol(tmpl)
}

// ---- 內部 ----

// resolveInstance 取實例記錄與其(已載入)範本。
func (s *CommandService) resolveInstance(uuid string) (InstanceRecord, *protocol.GameTemplate, error) {
	rec, err := s.store.GetInstance(uuid)
	if err != nil {
		return InstanceRecord{}, nil, err
	}
	tmpl, ok := s.engine.Get(rec.TemplateID)
	if !ok {
		return rec, nil, fmt.Errorf("實例 %s 的範本 %q 未載入", uuid, rec.TemplateID)
	}
	return rec, tmpl, nil
}

// dispatch 由協定 cp 組出 target,蓋上 protocol_id,經節點送出指令。
func (s *CommandService) dispatch(ctx context.Context, rec InstanceRecord, tmpl *protocol.GameTemplate, cp protocol.CommandProtocol, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	target, err := s.buildTarget(rec, tmpl, cp)
	if err != nil {
		return protocol.CommandResult{}, err
	}
	cmd.ProtocolID = cp.ProtocolID
	var res protocol.CommandResult
	cerr := s.registry.Call(rec.Node, func(c *NodeClient) error {
		var e error
		res, e = c.Command(ctx, rec.UUID, target, cmd)
		return e
	})
	return res, cerr
}

// buildTarget 由範本協定 + 實例 UUID(機密命名空間)+ 埠映射組出線上 CommandTarget。
func (s *CommandService) buildTarget(rec InstanceRecord, tmpl *protocol.GameTemplate, cp protocol.CommandProtocol) (protocol.CommandTarget, error) {
	host, port, err := resolveInstancePort(s.store, tmpl, rec.UUID, cp.HostPortRef)
	if err != nil {
		return protocol.CommandTarget{}, err
	}
	target := protocol.CommandTarget{
		ProtocolID: cp.ProtocolID,
		Kind:       cp.Kind,
		Host:       host,
		Port:       port,
	}
	if cp.PasswordRef != "" {
		ref := protocol.NewSecretRef(instanceSecretKey(rec.UUID, cp.PasswordRef))
		pw, gerr := s.secrets.Get(ref)
		if gerr != nil {
			return protocol.CommandTarget{}, fmt.Errorf("取指令協定 %q 密碼失敗: %w", cp.ProtocolID, gerr)
		}
		target.Password = pw
	}
	if cp.Kind == "rest" {
		if cp.Auth == "basic" {
			username := cp.Username
			if username == "" {
				username = "admin" // 相容回退:未於範本指定 username 時沿用 Palworld 慣例帳號
			}
			target.Username = username
		}
		for _, a := range cp.Actions {
			target.Actions = append(target.Actions, protocol.RestActionSpec{
				ActionID: a.ActionID,
				Method:   a.Method,
				Path:     a.Path,
			})
		}
	}
	return target, nil
}

// activeProtocol 回傳範本第一個非 legacy 指令協定;全 legacy 或無協定→ErrCommandNotEnabled。
func activeProtocol(tmpl *protocol.GameTemplate) (protocol.CommandProtocol, error) {
	for _, cp := range tmpl.CommandProtocols {
		if cp.Legacy {
			continue
		}
		return cp, nil
	}
	return protocol.CommandProtocol{}, fmt.Errorf("%w: 範本 %q 無非 legacy 協定", ErrCommandNotEnabled, tmpl.ID)
}

// protocolByID 依 protocol_id 取協定(供 hooks 引用);legacy 協定不可被選(明確錯誤)。
func protocolByID(tmpl *protocol.GameTemplate, id string) (protocol.CommandProtocol, error) {
	for _, cp := range tmpl.CommandProtocols {
		if cp.ProtocolID != id {
			continue
		}
		if cp.Legacy {
			return protocol.CommandProtocol{}, fmt.Errorf("%w: 協定 %q 為 legacy", ErrCommandNotEnabled, id)
		}
		return cp, nil
	}
	return protocol.CommandProtocol{}, fmt.Errorf("hook 引用的指令協定 %q 於範本不存在", id)
}

// buildHookCommand 由 hook 定義組 GameCommand:rcon 用 Command 字串(替換 {msg});rest 用 ActionID +
// Args——先複製 hook.Args(範本宣告的靜態參數,如 Palworld shutdown 的 waittime),再視需要以
// hook.MessageKey 指定的欄位名(預設 "message",對齊 Palworld REST announce/shutdown)寫入動態
// 訊息 msg。cp 的 kind 為 rcon 時 Args 不適用。
func buildHookCommand(cp protocol.CommandProtocol, hook *protocol.Hook, msg string) protocol.GameCommand {
	cmd := protocol.GameCommand{ProtocolID: cp.ProtocolID}
	switch cp.Kind {
	case "rcon":
		cmd.Raw = strings.ReplaceAll(hook.Command, "{msg}", msg)
	case "rest":
		cmd.ActionID = hook.ActionID
		if len(hook.Args) > 0 || msg != "" {
			cmd.Args = make(map[string]string, len(hook.Args)+1)
			for k, v := range hook.Args {
				cmd.Args[k] = v
			}
			if msg != "" {
				key := hook.MessageKey
				if key == "" {
					key = "message"
				}
				cmd.Args[key] = msg
			}
		}
	}
	return cmd
}

// resolveInstancePort 由 port_ref(指向 [[ports]].name)解析「該實例實際使用」的宿主 host/port,
// 是指令協定(CommandService)與健康探針(HealthProber)共用的唯一解析路徑。
//
// 取值序:該實例在 DB 的 PortReservation(同 Name)→ 範本宣告值。前者才是真值——使用者埠覆寫
// 與核心動態分配後,範本宣告值已不代表實際埠;後者僅為相容路徑(本改動前建立、或建立時未留下
// 具名預留的舊實例)。範本無此 port_ref 一律先報錯,語意不因 DB 有無預留而改變。
func resolveInstancePort(store *Store, tmpl *protocol.GameTemplate, uuid, ref string) (host string, port int, err error) {
	// 先驗範本:port_ref 缺失/無對應 [[ports]] 的錯誤語意不因 DB 有無預留而改變。
	spec, serr := lookupPortSpec(tmpl, ref)
	if serr != nil {
		return "", 0, serr
	}
	if store != nil && uuid != "" {
		res, lerr := store.ListPortReservationsForInstance(uuid)
		if lerr != nil {
			return "", 0, fmt.Errorf("查詢實例 %s 的埠預留失敗: %w", uuid, lerr)
		}
		for _, r := range res {
			if r.Name == ref && r.HostPort > 0 {
				return commandHost(r.BindIP), r.HostPort, nil
			}
		}
	}
	// 相容退路:舊實例無具名預留 → 用範本宣告值。
	if spec.HostPort <= 0 {
		return "", 0, fmt.Errorf("實例 %s 的埠 %q 查無預留、範本亦為動態分配,無法解析宿主埠", uuid, ref)
	}
	return commandHost(spec.BindIP), spec.HostPort, nil
}

// lookupPortSpec 取範本中 name==ref 的 [[ports]] 宣告。
func lookupPortSpec(tmpl *protocol.GameTemplate, ref string) (protocol.PortSpec, error) {
	if ref == "" {
		return protocol.PortSpec{}, fmt.Errorf("指令協定缺少 host_port_ref")
	}
	for _, p := range tmpl.Ports {
		if p.Name == ref {
			return p, nil
		}
	}
	return protocol.PortSpec{}, fmt.Errorf("指令協定埠參照 %q 於範本無對應 [[ports]]", ref)
}

// resolveCommandPort 由 port_ref 解析範本宣告的宿主 host/port(不查實例預留)。
// 僅供「無實例上下文」的場合;有 uuid 時一律走 resolveInstancePort。
// host 取 bind_ip:wildcard(0.0.0.0/::)或空值一律回 127.0.0.1(單機 loopback,且 command
// 埠本即綁 loopback)。
func resolveCommandPort(tmpl *protocol.GameTemplate, ref string) (host string, port int, err error) {
	spec, serr := lookupPortSpec(tmpl, ref)
	if serr != nil {
		return "", 0, serr
	}
	if spec.HostPort <= 0 {
		return "", 0, fmt.Errorf("指令協定埠 %q 於範本為動態分配,無宣告宿主埠可解析", ref)
	}
	return commandHost(spec.BindIP), spec.HostPort, nil
}

// commandHost 把埠的 bind_ip 正規化為代理連線用主機:wildcard/空→loopback。
func commandHost(bindIP string) string {
	switch bindIP {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return bindIP
	}
}
