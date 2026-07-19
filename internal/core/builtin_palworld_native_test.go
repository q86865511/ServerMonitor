package core

import (
	"testing"
)

// TestBuiltinPalworldNativeConfig 載入真實內建 palworld 範本,驗證 B14 的 native config 接線正確解析:
// ADMIN_PASSWORD 映射進 ini、REST 啟用/埠以 set 釘住、ServerName/AdminPassword 宣告為需引號的字串鍵。
// 這是「真範本」端到端解析的廉價回歸守衛(有別於用合成範本的 buildspec 測試),避免範本 TOML/欄位接線
// 迴歸只能在 30-40 分鐘的 native Palworld E2E 才被發現。
func TestBuiltinPalworldNativeConfig(t *testing.T) {
	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	eng := NewTemplateEngine(nil, events)
	if _, err := eng.LoadDir("../../templates"); err != nil {
		t.Fatalf("LoadDir templates: %v", err)
	}
	tm, ok := eng.Get("palworld")
	if !ok {
		t.Fatal("內建 palworld 範本未載入")
	}
	if tm.Native == nil || len(tm.Native.Config) == 0 {
		t.Fatal("palworld 範本無 [native.config]")
	}
	cm := tm.Native.Config[0]

	if cm.Map["ADMIN_PASSWORD"] != "AdminPassword" {
		t.Errorf("B14:ADMIN_PASSWORD 應映射為 AdminPassword,實際 Map=%v", cm.Map)
	}
	if cm.Set["RESTAPIEnabled"] != "True" {
		t.Errorf("B14:RESTAPIEnabled 應以 set 釘為 True,實際 Set=%v", cm.Set)
	}
	if cm.Set["RESTAPIPort"] != "{port:rest}" {
		t.Errorf("B14:RESTAPIPort 應以 set 釘為 {port:rest},實際 Set=%v", cm.Set)
	}
	quoted := map[string]bool{}
	for _, q := range cm.Quote {
		quoted[q] = true
	}
	if !quoted["ServerName"] || !quoted["AdminPassword"] {
		t.Errorf("B14:ServerName/AdminPassword 應宣告為需引號的字串鍵,實際 Quote=%v", cm.Quote)
	}
}
