package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadAppConfig_Missing 驗證檔案不存在時回預設值(首次啟動視為正常,非 recovered)。
func TestLoadAppConfig_Missing(t *testing.T) {
	cfg, recovered, err := LoadAppConfig(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatalf("不存在不應報錯: %v", err)
	}
	if recovered {
		t.Errorf("不存在不應標記 recovered")
	}
	if cfg.Node != DefaultAppConfig().Node {
		t.Errorf("應回預設: %+v", cfg)
	}
}

// TestLoadAppConfig_Valid 驗證合法 JSON 正確載入。
func TestLoadAppConfig_Valid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(path, []byte(`{"data_root":"/data","node":"n1","event_retention_max":42}`), 0o644); err != nil {
		t.Fatalf("寫入: %v", err)
	}
	cfg, recovered, err := LoadAppConfig(path)
	if err != nil {
		t.Fatalf("LoadAppConfig: %v", err)
	}
	if recovered {
		t.Errorf("合法檔不應 recovered")
	}
	if cfg.DataRoot != "/data" || cfg.Node != "n1" || cfg.EventRetentionMax != 42 {
		t.Errorf("載入不符: %+v", cfg)
	}
}

// TestLoadAppConfig_Corrupt 驗證一般設定檔毀損時以預設值降級啟動並回報 recovered(R12)。
func TestLoadAppConfig_Corrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(path, []byte("{這不是合法 JSON,,,,"), 0o644); err != nil {
		t.Fatalf("寫入: %v", err)
	}
	cfg, recovered, err := LoadAppConfig(path)
	if err != nil {
		t.Fatalf("毀損不應報錯(應降級): %v", err)
	}
	if !recovered {
		t.Errorf("毀損應標記 recovered=true")
	}
	if cfg != DefaultAppConfig() {
		t.Errorf("毀損應回預設值, got %+v", cfg)
	}
}

// TestResolveCurseForgeOverrideKey_Migrates 驗證舊版 config.json 明文金鑰於載入時遷入金鑰庫、
// 自設定檔清除,並回報 migrated(#11)。
func TestResolveCurseForgeOverrideKey_Migrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := SaveAppConfig(path, AppConfig{Node: "local", CurseForgeAPIKey: "cf-plain-123"}); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}
	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())

	cfg, _, err := LoadAppConfig(path)
	if err != nil {
		t.Fatalf("LoadAppConfig: %v", err)
	}
	key, migrated, err := ResolveCurseForgeOverrideKey(secrets, path, &cfg)
	if err != nil {
		t.Fatalf("ResolveCurseForgeOverrideKey: %v", err)
	}
	if key != "cf-plain-123" || !migrated {
		t.Fatalf("key=%q migrated=%v, 期望 cf-plain-123/true", key, migrated)
	}
	// 就地清空且金鑰庫已存入。
	if cfg.CurseForgeAPIKey != "" {
		t.Errorf("遷移後 cfg 欄位應清空, 得 %q", cfg.CurseForgeAPIKey)
	}
	if v, gerr := secrets.Get(curseForgeOverrideRef); gerr != nil || v != "cf-plain-123" {
		t.Errorf("金鑰庫值 = %q, err=%v, 期望 cf-plain-123", v, gerr)
	}
	// 回寫的 config.json 不再含明文(omitempty)。
	reloaded, _, _ := LoadAppConfig(path)
	if reloaded.CurseForgeAPIKey != "" {
		t.Errorf("回寫的 config.json 仍含明文金鑰: %q", reloaded.CurseForgeAPIKey)
	}
	data, _ := os.ReadFile(path)
	if indexOf(string(data), "curseforge_api_key") >= 0 {
		t.Errorf("config.json 不應再有 curseforge_api_key 欄位: %s", data)
	}
}

// TestResolveCurseForgeOverrideKey_FromKeyring 驗證無明文時自金鑰庫取回既存覆蓋值(不遷移)。
func TestResolveCurseForgeOverrideKey_FromKeyring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(curseForgeOverrideRef, "cf-stored-key"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cfg := AppConfig{Node: "local"} // 無明文
	key, migrated, err := ResolveCurseForgeOverrideKey(secrets, path, &cfg)
	if err != nil {
		t.Fatalf("ResolveCurseForgeOverrideKey: %v", err)
	}
	if key != "cf-stored-key" || migrated {
		t.Fatalf("key=%q migrated=%v, 期望 cf-stored-key/false", key, migrated)
	}
}

// TestResolveCurseForgeOverrideKey_Absent 驗證明文與金鑰庫皆無時回空字串、不遷移、不報錯。
func TestResolveCurseForgeOverrideKey_Absent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	cfg := AppConfig{Node: "local"}
	key, migrated, err := ResolveCurseForgeOverrideKey(secrets, path, &cfg)
	if err != nil || key != "" || migrated {
		t.Fatalf("key=%q migrated=%v err=%v, 期望 \"\"/false/nil", key, migrated, err)
	}
}

// TestSaveLoadAppConfig_RoundTrip 驗證存/讀往返一致。
func TestSaveLoadAppConfig_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	want := AppConfig{DataRoot: "C:/gsm/data", Node: "local", EventRetentionMax: 5000}
	if err := SaveAppConfig(path, want); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}
	got, recovered, err := LoadAppConfig(path)
	if err != nil || recovered {
		t.Fatalf("LoadAppConfig: err=%v recovered=%v", err, recovered)
	}
	if got != want {
		t.Errorf("往返不符: got=%+v want=%+v", got, want)
	}
}
