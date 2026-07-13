package agent

import (
	"context"

	"servermonitor/internal/protocol"
)

// GameCommandAdapter 送出遊戲內指令並回顯(R7),與 RuntimeBackend 分離:
// core 不需硬編遊戲語意、RuntimeBackend 不需理解遊戲。實作:RconAdapter、
// PalworldRestAdapter(均為 T9)。
type GameCommandAdapter interface {
	Send(ctx context.Context, target CommandTarget, cmd protocol.GameCommand) (protocol.CommandResult, error)
}

// CommandTarget 描述一則指令的送達目標(由範本 command_protocols 與實例解析後的
// 埠/機密組出)。首版最小欄位,T9 實作 rcon/rest 時可擴充。
type CommandTarget struct {
	ProtocolID string             // 對應範本 command_protocols[].protocol_id
	Kind       string             // "rcon" | "rest"
	Host       string             // 連線主機(單機為 127.0.0.1)
	Port       int                // 已解析的宿主埠
	Secret     protocol.SecretRef // 憑證參照(值在金鑰庫,一律 redact)
}

// NopCommandAdapter 是 GameCommandAdapter 的暫位實作,供 T9 前的編譯與接線。
// Send 不做任何事、回傳空結果;T9 以 RconAdapter/PalworldRestAdapter 取代。
type NopCommandAdapter struct{}

// Send 實作 GameCommandAdapter,為 no-op。
func (NopCommandAdapter) Send(_ context.Context, _ CommandTarget, _ protocol.GameCommand) (protocol.CommandResult, error) {
	return protocol.CommandResult{}, nil
}

var _ GameCommandAdapter = NopCommandAdapter{}
