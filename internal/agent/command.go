package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"servermonitor/internal/protocol"
)

// GameCommandAdapter 送出遊戲內指令並回顯(R7),與 RuntimeBackend 分離:
// core 不需硬編遊戲語意、RuntimeBackend 不需理解遊戲。目標(host/port/機密/rest 動作定義)
// 由 core 解析範本後以 protocol.CommandTarget 越過 loopback 帶入;adapter 只負責協定執行。
// 實作:RconAdapter(Source RCON)、PalworldRestAdapter(REST 具名動作)。
type GameCommandAdapter interface {
	Send(ctx context.Context, target protocol.CommandTarget, cmd protocol.GameCommand) (protocol.CommandResult, error)
}

// NopCommandAdapter 是 GameCommandAdapter 的 no-op 實作,供「未配置指令轉接器」時的安全預設
// (Send 不做任何事、回空結果)。生產路徑用 NewCommandDispatcher 依 Kind 選真 adapter。
type NopCommandAdapter struct{}

// Send 實作 GameCommandAdapter,為 no-op。
func (NopCommandAdapter) Send(_ context.Context, _ protocol.CommandTarget, _ protocol.GameCommand) (protocol.CommandResult, error) {
	return protocol.CommandResult{}, nil
}

var _ GameCommandAdapter = NopCommandAdapter{}

// commandDispatcher 依 CommandTarget.Kind 選底層 adapter(rcon→RconAdapter、rest→
// PalworldRestAdapter),取代 T9 前的 NopCommandAdapter。legacy 協定由 core 端不選(不會出現在
// 送達的 target),此處對未知/空 Kind 亦回明確錯誤,作為第二道防線。
type commandDispatcher struct {
	rcon GameCommandAdapter
	rest GameCommandAdapter
}

// NewCommandDispatcher 建立依 Kind 分派的 GameCommandAdapter(供 Server 預設與 app 組裝使用)。
func NewCommandDispatcher() GameCommandAdapter {
	return commandDispatcher{rcon: RconAdapter{}, rest: PalworldRestAdapter{}}
}

// Send 依 target.Kind 分派。
func (d commandDispatcher) Send(ctx context.Context, target protocol.CommandTarget, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	switch target.Kind {
	case "rcon":
		return d.rcon.Send(ctx, target, cmd)
	case "rest":
		return d.rest.Send(ctx, target, cmd)
	default:
		return protocol.CommandResult{}, fmt.Errorf("agent: 未知或未啟用的指令協定 kind %q", target.Kind)
	}
}

var _ GameCommandAdapter = commandDispatcher{}

// ---- RCON(Source RCON 協定,標準庫實作)----

// defaultCommandTimeout 是連線與讀寫的預設逾時(避免對無回應的伺服器掛死)。
const defaultCommandTimeout = 5 * time.Second

// Source RCON 封包型別。
const (
	rconTypeAuth         int32 = 3 // SERVERDATA_AUTH
	rconTypeAuthResponse int32 = 2 // SERVERDATA_AUTH_RESPONSE
	rconTypeExecCommand  int32 = 2 // SERVERDATA_EXECCOMMAND
	rconTypeResponse     int32 = 0 // SERVERDATA_RESPONSE_VALUE

	rconAuthFailID int32 = -1      // 認證失敗時 AUTH_RESPONSE 的 id
	rconMaxPayload int32 = 4 << 10 // 單封包 payload 上限(防惡意/損壞長度)

	// rconExecID、rconSentinelID:exec 用一個 id,哨兵用另一個(必須不同)。哨兵法靠讀到
	// 哨兵 id 的回顯判定 exec 多封包回應已送完。
	rconExecID     int32 = 2
	rconSentinelID int32 = 3

	// rconMaxAggregate 是 exec 多封包回應「串接後」的總上限(防惡意無限流)。單封包仍受
	// rconMaxPayload 約束;此值只限制哨兵法聚合的總量。
	rconMaxAggregate = 1 << 20 // 1 MiB
)

// RconAdapter 以 Source RCON 協定(TCP)送出 raw 指令並回顯(R7)。Timeout 可設定;連線失敗/
// 認證失敗/逾時皆回明確錯誤而不掛死(全程設 deadline)。
type RconAdapter struct {
	// Timeout 是撥號與每次讀寫的逾時;<=0 用 defaultCommandTimeout。
	Timeout time.Duration
}

// Send 連線→認證→執行 cmd.Raw→回顯回應主體。
func (a RconAdapter) Send(ctx context.Context, target protocol.CommandTarget, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	host := target.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(target.Port))

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return protocol.CommandResult{}, fmt.Errorf("rcon 連線 %s 失敗: %w", addr, err)
	}
	defer conn.Close()

	// 全程 deadline:無回應時讀取會逾時而非阻塞。ctx 取消亦透過 deadline 生效。
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if err := rconAuthenticate(conn, target.Password); err != nil {
		return protocol.CommandResult{}, err
	}

	if err := writeRconPacket(conn, rconExecID, rconTypeExecCommand, cmd.Raw); err != nil {
		return protocol.CommandResult{}, fmt.Errorf("rcon 送出指令失敗: %w", err)
	}
	// 多封包聚合(循序哨兵法):先讀 exec 的第一個回應封包,**之後**才送空 body 的 EXECCOMMAND
	// 哨兵(id=rconSentinelID≠rconExecID),再聚合讀取直到收到帶哨兵 id 的回應——其到達標誌
	// exec 的多封包回應已全數送完(單一大回應會被伺服器拆成多個 ≤4096 的封包,同 rconExecID)。
	// ⚠ 不可管線化(exec+哨兵連寫後才開讀):vanilla Minecraft 的 RCON 讀到 socket 緩衝裡的
	// 第二個請求封包會直接斷線(實測 1.21.1:auth 成功後任何指令都 EOF;循序送則正常,對空
	// 指令哨兵回「Unknown or incomplete command」)。Paper 容忍管線化,故舊寫法只在 vanilla 炸,
	// 造成 vanilla 的就緒探針/指令/stop hook 全數失敗。哨兵同樣必須用 type 2(空指令),
	// 不可用 Source 慣例的 type-0 RESPONSE_VALUE 請求。
	first, err := readExecResponse(conn)
	if err != nil {
		return protocol.CommandResult{}, err
	}
	var out strings.Builder
	out.WriteString(first)
	if err := writeRconPacket(conn, rconSentinelID, rconTypeExecCommand, ""); err != nil {
		return protocol.CommandResult{}, fmt.Errorf("rcon 送出哨兵封包失敗: %w", err)
	}
	for {
		id, typ, body, err := readRconPacket(conn)
		if err != nil {
			return protocol.CommandResult{}, fmt.Errorf("rcon 讀取回應失敗: %w", err)
		}
		switch {
		case id == rconSentinelID:
			// 哨兵回應 → exec 回應已完結。
			return protocol.CommandResult{Success: true, Output: out.String()}, nil
		case id == rconExecID && typ == rconTypeResponse:
			if out.Len()+len(body) > rconMaxAggregate {
				return protocol.CommandResult{}, fmt.Errorf("rcon 回應過長(超過聚合上限 %d bytes)", rconMaxAggregate)
			}
			out.WriteString(body)
		default:
			// 其他 id/type:忽略(防前一連線殘包;本實作每指令新連線,理論不會出現,防禦性處理)。
		}
	}
}

// readExecResponse 讀 exec 的第一個回應封包(哨兵送出前的循序步驟,見 Send 內註解)。
// 忽略非 exec id 的封包(防禦性;每指令新連線理論不會出現)。
func readExecResponse(conn net.Conn) (string, error) {
	for {
		id, typ, body, err := readRconPacket(conn)
		if err != nil {
			return "", fmt.Errorf("rcon 讀取回應失敗: %w", err)
		}
		if id == rconExecID && typ == rconTypeResponse {
			return body, nil
		}
	}
}

var _ GameCommandAdapter = RconAdapter{}

// rconAuthenticate 送出 AUTH 封包並驗證回應。伺服器可能先回一個空的 RESPONSE_VALUE,再回
// AUTH_RESPONSE;故跳過型別為 RESPONSE_VALUE 的前導封包。核對回應:必須為 AUTH_RESPONSE(type 2)
// 且 id 為 authID(成功)或 -1(認證失敗);其餘視為協定錯誤。
func rconAuthenticate(conn net.Conn, password string) error {
	const authID int32 = 1
	if err := writeRconPacket(conn, authID, rconTypeAuth, password); err != nil {
		return fmt.Errorf("rcon 送出認證失敗: %w", err)
	}
	for {
		id, typ, _, err := readRconPacket(conn)
		if err != nil {
			return fmt.Errorf("rcon 讀取認證回應失敗: %w", err)
		}
		if typ == rconTypeResponse {
			continue // 前導空 RESPONSE_VALUE(type 0),續讀 AUTH_RESPONSE
		}
		// 此處應為 AUTH_RESPONSE。注意 rconTypeAuthResponse 與 rconTypeExecCommand 常數同為 2,
		// 但語意方向相反:前者是 server→client 的認證結果,後者是 client→server 的執行指令;
		// 認證階段收到的 type-2 封包語意上即 AUTH_RESPONSE。
		if typ != rconTypeAuthResponse {
			return fmt.Errorf("rcon 認證回應型別異常: type=%d", typ)
		}
		switch id {
		case rconAuthFailID:
			return fmt.Errorf("rcon 認證失敗:密碼不正確")
		case authID:
			return nil
		default:
			return fmt.Errorf("rcon 認證回應 id 不符: 期望 %d 得 %d", authID, id)
		}
	}
}

// writeRconPacket 依 Source RCON 格式(小端)寫一個封包:size(int32)+id(int32)+type(int32)+
// body(ASCII)+兩個 0x00 終止位元組。
func writeRconPacket(w io.Writer, id, typ int32, body string) error {
	size := int32(4 + 4 + len(body) + 2)
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, size)
	_ = binary.Write(&buf, binary.LittleEndian, id)
	_ = binary.Write(&buf, binary.LittleEndian, typ)
	buf.WriteString(body)
	buf.WriteByte(0)
	buf.WriteByte(0)
	_, err := w.Write(buf.Bytes())
	return err
}

// readRconPacket 讀一個封包,回傳 id、type、body(已剝除兩個終止位元組)。
func readRconPacket(r io.Reader) (id, typ int32, body string, err error) {
	var size int32
	if err = binary.Read(r, binary.LittleEndian, &size); err != nil {
		return 0, 0, "", err
	}
	if size < 10 || size > rconMaxPayload {
		return 0, 0, "", fmt.Errorf("rcon 封包長度異常: %d", size)
	}
	payload := make([]byte, size)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ = int32(binary.LittleEndian.Uint32(payload[4:8]))
	// payload = id(4)+type(4)+body+0x00+0x00;body 為 payload[8:size-2]。
	body = string(payload[8 : size-2])
	return id, typ, body, nil
}

// ---- Palworld REST(具名動作)----

// restMaxBody 是 REST 回應主體讀取上限(1 MiB)。players/metrics 等回應可能超過舊的 4KB 上限
// 而遭截斷;此值獨立於 rconMaxPayload(RCON 單封包上限,語意不同)。讀取超過此上限即視為異常。
const restMaxBody int64 = 1 << 20

// PalworldRestAdapter 依 target.Actions 執行具名 REST 動作(R7):以 action_id 選動作,組
// method/path,POST/PUT 時把 GameCommand.Args 序列化為 JSON body,帶 HTTP Basic Auth
// (帳號預設 "admin",密碼取自 target.Password)。動作不存在→明確錯誤。
type PalworldRestAdapter struct {
	// Timeout 是 HTTP 呼叫逾時;<=0 用 defaultCommandTimeout。
	Timeout time.Duration
}

// Send 執行具名動作並回顯回應主體。
func (a PalworldRestAdapter) Send(ctx context.Context, target protocol.CommandTarget, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	action, ok := findRestAction(target.Actions, cmd.ActionID)
	if !ok {
		return protocol.CommandResult{}, fmt.Errorf("palworld rest: 動作 %q 不存在", cmd.ActionID)
	}
	host := target.Host
	if host == "" {
		host = "127.0.0.1"
	}
	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(target.Port)) + action.Path

	method := strings.ToUpper(action.Method)
	if method == "" {
		method = http.MethodGet
	}
	var reqBody io.Reader
	writesBody := method != http.MethodGet && method != http.MethodHead && len(cmd.Args) > 0
	if writesBody {
		payload, merr := json.Marshal(cmd.Args)
		if merr != nil {
			return protocol.CommandResult{}, fmt.Errorf("palworld rest: 序列化參數失敗: %w", merr)
		}
		reqBody = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return protocol.CommandResult{}, fmt.Errorf("palworld rest: 建立請求失敗: %w", err)
	}
	username := target.Username
	if username == "" {
		username = "admin"
	}
	req.SetBasicAuth(username, target.Password)
	if writesBody {
		req.Header.Set("Content-Type", "application/json")
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return protocol.CommandResult{}, fmt.Errorf("palworld rest: 呼叫 %s 失敗: %w", action.ActionID, err)
	}
	defer resp.Body.Close()
	// 多讀 1 byte:若讀滿 restMaxBody+1 表示回應超過上限而遭截斷,回明確錯誤(不回半截 JSON)。
	respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, restMaxBody+1))
	if rerr != nil {
		return protocol.CommandResult{}, fmt.Errorf("palworld rest: 讀取動作 %q 回應失敗: %w", action.ActionID, rerr)
	}
	if int64(len(respBody)) > restMaxBody {
		return protocol.CommandResult{}, fmt.Errorf("palworld rest: 動作 %q 回應過長(超過 %d bytes)", action.ActionID, restMaxBody)
	}
	if resp.StatusCode/100 != 2 {
		return protocol.CommandResult{}, fmt.Errorf("palworld rest: 動作 %q 回應 %d: %s",
			action.ActionID, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return protocol.CommandResult{Success: true, Output: string(respBody)}, nil
}

var _ GameCommandAdapter = PalworldRestAdapter{}

// findRestAction 依 action_id 找動作定義。
func findRestAction(actions []protocol.RestActionSpec, id string) (protocol.RestActionSpec, bool) {
	for _, a := range actions {
		if a.ActionID == id {
			return a, true
		}
	}
	return protocol.RestActionSpec{}, false
}
