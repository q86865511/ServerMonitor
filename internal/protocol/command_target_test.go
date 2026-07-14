package protocol_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"servermonitor/internal/protocol"
)

// TestCommandTarget_RedactsPasswordInFormatting:%v/%s/%+v/%#v 皆不得洩漏 Password 明文,
// 而 json.Marshal 仍須保留明文(wire 需要;R12 loopback 例外)。
func TestCommandTarget_RedactsPasswordInFormatting(t *testing.T) {
	const secret = "s3cr3t-pw"
	tg := protocol.CommandTarget{
		ProtocolID: "svc-rcon", Kind: "rcon", Host: "127.0.0.1", Port: 25575,
		Username: "admin", Password: secret,
	}

	for _, verb := range []string{"%v", "%s", "%+v", "%#v"} {
		if out := fmt.Sprintf(verb, tg); strings.Contains(out, secret) {
			t.Errorf("%s 洩漏 Password: %s", verb, out)
		}
		// 指標亦不得洩漏(fmt 對 *T 亦調用 value receiver 的 Stringer/GoStringer)。
		if out := fmt.Sprintf(verb, &tg); strings.Contains(out, secret) {
			t.Errorf("%s(*T) 洩漏 Password: %s", verb, out)
		}
	}

	// 遮罩後應能看出「有密碼」但不透露值。
	if out := tg.String(); !strings.Contains(out, "[REDACTED]") {
		t.Errorf("非空密碼應顯示 [REDACTED],得 %s", out)
	}

	// 巢狀於 CommandRequest 時亦不得洩漏。
	req := protocol.CommandRequest{Target: tg, Command: protocol.GameCommand{Raw: "list"}}
	if out := fmt.Sprintf("%v", req); strings.Contains(out, secret) {
		t.Errorf("CommandRequest %%v 洩漏 Password: %s", out)
	}

	// wire 契約:JSON 必須保留明文。
	b, err := json.Marshal(tg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), secret) {
		t.Errorf("JSON 應保留 Password 明文(wire 需要),得 %s", b)
	}
}

// TestCommandTarget_EmptyPasswordNoRedactedMarker:密碼為空時不應標 [REDACTED]。
func TestCommandTarget_EmptyPasswordNoRedactedMarker(t *testing.T) {
	tg := protocol.CommandTarget{Kind: "rest", Host: "127.0.0.1", Port: 8212, Username: "admin"}
	if out := tg.String(); strings.Contains(out, "[REDACTED]") {
		t.Errorf("空密碼不應顯示 [REDACTED],得 %s", out)
	}
}
