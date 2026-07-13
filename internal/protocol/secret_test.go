package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const secretKeyName = "RCON_PASSWORD"

// TestSecretRef_Redaction 斷言 SecretRef 經 String()/fmt/JSON 一律遮罩,不外洩鍵名,
// 但仍可經匯出欄位 Key 取用(供金鑰庫查詢與 DB 存參照)。
// 對應 T1 驗收:SecretRef redaction 單元測試。
func TestSecretRef_Redaction(t *testing.T) {
	s := NewSecretRef(secretKeyName)

	// Key 仍可取用(供 T2 金鑰庫/DB)
	if s.Key != secretKeyName {
		t.Errorf("Key = %q, 期望 %q", s.Key, secretKeyName)
	}

	// String()
	if got := s.String(); got != secretRedacted {
		t.Errorf("String() = %q, 期望 %q", got, secretRedacted)
	}

	// fmt 動詞:%s / %v / %+v / %#v 皆不得含鍵名
	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q"} {
		out := fmt.Sprintf(verb, s)
		if strings.Contains(out, secretKeyName) {
			t.Errorf("fmt %s 外洩鍵名: %q", verb, out)
		}
	}

	// 直接 json.Marshal
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal(SecretRef) 失敗: %v", err)
	}
	if string(b) != `"`+secretRedacted+`"` {
		t.Errorf("json.Marshal = %s, 期望 %q", b, secretRedacted)
	}
	if strings.Contains(string(b), secretKeyName) {
		t.Errorf("json.Marshal 外洩鍵名: %s", b)
	}
}

// TestSecretRef_RedactionInStruct 斷言 SecretRef 作為巢狀欄位時,父結構的 JSON 序列化
// 與格式化輸出同樣遮罩(避免經 DTO/event/log 間接外洩)。
func TestSecretRef_RedactionInStruct(t *testing.T) {
	type wrapper struct {
		Name   string    `json:"name"`
		Secret SecretRef `json:"secret"`
	}
	w := wrapper{Name: "mc-1", Secret: NewSecretRef(secretKeyName)}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("json.Marshal(wrapper) 失敗: %v", err)
	}
	if strings.Contains(string(b), secretKeyName) {
		t.Errorf("父結構 JSON 外洩鍵名: %s", b)
	}
	if !strings.Contains(string(b), secretRedacted) {
		t.Errorf("父結構 JSON 未見遮罩值: %s", b)
	}

	// %+v 逐欄位輸出亦不得外洩
	if out := fmt.Sprintf("%+v", w); strings.Contains(out, secretKeyName) {
		t.Errorf("父結構 %%+v 外洩鍵名: %q", out)
	}
}
