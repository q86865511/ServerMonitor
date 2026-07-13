package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"servermonitor/internal/protocol"
)

// TestSecretStore_SetGetDelete 驗證以記憶體金鑰庫的存/取/刪與「查無」映射。
func TestSecretStore_SetGetDelete(t *testing.T) {
	kr := newMemoryKeyring()
	ss := NewSecretStoreWithKeyring(DefaultKeyringService, kr)
	ref := protocol.NewSecretRef("RCON_PASSWORD")

	const value = "hunter2-plaintext"
	if err := ss.Set(ref, value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := ss.Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != value {
		t.Errorf("Get=%q 期望 %q", got, value)
	}

	if err := ss.Delete(ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ss.Get(ref); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("刪除後 Get 應回 ErrSecretNotFound, got %v", err)
	}
	// 刪除為冪等。
	if err := ss.Delete(ref); err != nil {
		t.Errorf("重複刪除應冪等: %v", err)
	}
}

// TestSecretStore_EmptyKeyRejected 驗證空鍵被拒。
func TestSecretStore_EmptyKeyRejected(t *testing.T) {
	ss := NewSecretStoreWithKeyring("", newMemoryKeyring())
	if err := ss.Set(protocol.SecretRef{}, "x"); err == nil {
		t.Errorf("空 Key 的 Set 應報錯")
	}
}

// TestSecretStore_DBHasNoPlaintext 驗證:實值只入金鑰庫,DB 僅存 SecretRef.Key 參照,
// DB 檔位元組不含明文(R12)。
func TestSecretStore_DBHasNoPlaintext(t *testing.T) {
	st, dbPath := newTempStore(t)
	kr := newMemoryKeyring()
	ss := NewSecretStoreWithKeyring(DefaultKeyringService, kr)

	ref := protocol.NewSecretRef("RCON_PASSWORD")
	const secretValue = "S3cr3t-PL41NT3XT-do-not-persist-XYZ987"

	// 實值寫金鑰庫。
	if err := ss.Set(ref, secretValue); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// DB 只存「參照」(鍵名),絕不存實值:此處以實例 params 存鍵名參照。
	inst := InstanceRecord{
		UUID:       "u1",
		TemplateID: "minecraft",
		ParamsJSON: json.RawMessage(`{"secret_ref":"` + ref.Key + `"}`),
	}
	if err := st.UpsertInstance(inst); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}
	// 亦以 settings 存一筆參照。
	if err := st.SetSetting("rcon_secret_ref", ref.Key); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 讀原始 DB 位元組:不得含明文;鍵名參照可存在。
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("讀 DB 位元組: %v", err)
	}
	if bytes.Contains(raw, []byte(secretValue)) {
		t.Errorf("DB 檔含明文祕密(不應發生)")
	}
	if !bytes.Contains(raw, []byte(ref.Key)) {
		t.Errorf("DB 應含鍵名參照 %q(供日後查金鑰庫)", ref.Key)
	}
}
