package core

// 免外部依賴的單元測試,鎖定機密必填判定的兩個真源合流(協定引用 ∪ required 旗標)
// 與「空白值視同未提供」的驗證語意。

import (
	"testing"

	"servermonitor/internal/protocol"
)

func secretsUnionTemplate() *protocol.GameTemplate {
	return &protocol.GameTemplate{
		ID: "union-test",
		Secrets: []protocol.SecretSpec{
			{Key: "FLAG_ONLY", Required: true},   // 只標旗標、無協定引用
			{Key: "REF_ONLY"},                    // 只被協定引用、未標旗標
			{Key: "OPTIONAL"},                    // 兩者皆無 → 選填
		},
		CommandProtocols: []protocol.CommandProtocol{
			{ProtocolID: "rcon", Kind: "rcon", PasswordRef: "REF_ONLY"},
			{ProtocolID: "legacy-rcon", Kind: "rcon", PasswordRef: "OPTIONAL", Legacy: true},
		},
	}
}

// TestRequiredSecretKeys_Union 驗證必填鍵為「非 legacy 協定引用 ∪ required=true」,
// legacy 協定引用與未標旗標者不計入。
func TestRequiredSecretKeys_Union(t *testing.T) {
	got := requiredSecretKeys(secretsUnionTemplate())
	want := []string{"FLAG_ONLY", "REF_ONLY"}
	if len(got) != len(want) {
		t.Fatalf("必填鍵 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("必填鍵 = %v, 期望 %v", got, want)
		}
	}
}

// TestValidateSecrets_RequiredFlagEnforcedAndBlankIsMissing 驗證 required=true 的機密未提供
// 或僅提供空白值時,validateSecrets 都回缺項;齊備時通過。
func TestValidateSecrets_RequiredFlagEnforcedAndBlankIsMissing(t *testing.T) {
	tmpl := secretsUnionTemplate()

	if err := validateSecrets(tmpl, map[string]string{"REF_ONLY": "x"}); err == nil {
		t.Fatal("FLAG_ONLY 未提供時應回缺項")
	}
	if err := validateSecrets(tmpl, map[string]string{"FLAG_ONLY": "  ", "REF_ONLY": "x"}); err == nil {
		t.Fatal("FLAG_ONLY 僅空白值時應回缺項")
	}
	if err := validateSecrets(tmpl, map[string]string{"FLAG_ONLY": "a", "REF_ONLY": "b"}); err != nil {
		t.Fatalf("必填齊備時應通過,實際: %v", err)
	}
}
