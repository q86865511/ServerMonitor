package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"servermonitor/internal/protocol"
)

// TestRemove_PortReleaseFailureAborts 驗證 T15 雙審 #6:埠釋放失敗→Remove 回錯、DB 列仍在
// (供重試),且不繼續刪機密/DB。
func TestRemove_PortReleaseFailureAborts(t *testing.T) {
	h := newSvcHarness(t)
	rec, err := h.svc.Create(context.Background(), h.validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 注入埠釋放失敗。
	wantErr := errors.New("boom: 埠釋放")
	h.svc.releasePortsHook = func(string) error { return wantErr }

	err = h.svc.Remove(context.Background(), rec.UUID, true)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Remove 應因埠釋放失敗中止並回錯,得 %v", err)
	}

	// DB 列仍在(可重試)。
	if _, gerr := h.store.GetInstance(rec.UUID); gerr != nil {
		t.Fatalf("Remove 中止後 DB 列應保留,GetInstance 得 %v", gerr)
	}
	// 機密未被刪(中止在刪機密之前)。
	if _, kerr := h.keyring.Get("test", instanceSecretKey(rec.UUID, "RCON_PASSWORD")); kerr != nil {
		t.Fatalf("中止前不應刪機密,金鑰庫得 %v", kerr)
	}
}

// TestRemove_SecretCleanupIndependentOfTemplate 驗證 T15 雙審 #6:範本已從引擎移除,
// Remove 仍能依建立時持久化的鍵清單清除機密。
func TestRemove_SecretCleanupIndependentOfTemplate(t *testing.T) {
	h := newSvcHarness(t)
	rec, err := h.svc.Create(context.Background(), h.validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	secretKey := instanceSecretKey(rec.UUID, "RCON_PASSWORD")
	if _, kerr := h.keyring.Get("test", secretKey); kerr != nil {
		t.Fatalf("建立後機密應在庫: %v", kerr)
	}

	// 模擬使用者刪除範本檔並重載引擎(範本不再載入)。
	if rerr := os.Remove(filepath.Join(h.dir, "svc.toml")); rerr != nil {
		t.Fatalf("刪範本檔: %v", rerr)
	}
	h.eng = NewTemplateEngine(nil, h.events)
	// 以新(空)引擎替換 service 的引擎:模擬範本不再可得。
	h.svc.engine = h.eng
	if _, ok := h.svc.engine.Get("svc"); ok {
		t.Fatal("範本應已不載入")
	}

	if rerr := h.svc.Remove(context.Background(), rec.UUID, true); rerr != nil {
		t.Fatalf("Remove: %v", rerr)
	}
	// 機密仍被清除(依持久化清單,而非範本推導)。
	if _, kerr := h.keyring.Get("test", secretKey); !errors.Is(kerr, keyring.ErrNotFound) {
		t.Fatalf("範本已刪仍應依持久化鍵清單清機密,得 %v", kerr)
	}
	// 持久化鍵清單設定亦被清除。
	if _, ok, _ := h.store.GetSetting(secretKeysSettingKey(rec.UUID)); ok {
		t.Fatal("Remove 後應清除機密鍵清單設定")
	}
	// DB 列已刪。
	if _, gerr := h.store.GetInstance(rec.UUID); gerr == nil {
		t.Fatal("Remove 成功後 DB 列應已刪")
	}
}

// TestRemove_SecretDeleteFailureRecordsWarningNotAbort 驗證 T15 雙審 #6:機密刪除失敗
// 記警告事件但不中止(DB 列仍被刪)。
func TestRemove_SecretDeleteFailureRecordsWarningNotAbort(t *testing.T) {
	h := newSvcHarness(t)
	rec, err := h.svc.Create(context.Background(), h.validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 讓金鑰庫刪除回非 not-found 的暫時性錯誤。
	h.keyring.deleteErr = errors.New("keyring transient")

	if rerr := h.svc.Remove(context.Background(), rec.UUID, false); rerr != nil {
		t.Fatalf("機密刪除失敗不應使 Remove 失敗: %v", rerr)
	}
	if _, gerr := h.store.GetInstance(rec.UUID); gerr == nil {
		t.Fatal("Remove 應已刪 DB 列")
	}
	evs := queryEvents(t, h.events, EventInstanceRemoved)
	if len(evs) == 0 {
		t.Fatal("應記 INSTANCE_REMOVED 事件")
	}
	warned := false
	for _, e := range evs {
		if e.Severity == protocol.SeverityWarning {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("機密殘留時 INSTANCE_REMOVED 應為警告,得 %+v", evs)
	}
}
