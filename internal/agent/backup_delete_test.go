package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"servermonitor/internal/protocol"
)

// ---- 後端 DeleteBackup 單元(免 Docker)----

// MockBackend.DeleteBackup 刪除該實例備份;不存在 / 跨實例 → ErrNotFound。
func TestMockBackend_DeleteBackup(t *testing.T) {
	m := NewMockBackend()
	ctx := context.Background()
	id, err := m.Create(ctx, protocol.InstanceSpec{UUID: "u-1", TemplateID: "life"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bid, err := m.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// 跨實例刪除拒絕(uuid 不符)。
	if err := m.DeleteBackup(ctx, "other", bid); err != ErrNotFound {
		t.Fatalf("cross-uuid delete want ErrNotFound, got %v", err)
	}
	// 正確刪除。
	if err := m.DeleteBackup(ctx, "u-1", bid); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	metas, _ := m.ListBackups(ctx, "u-1")
	if len(metas) != 0 {
		t.Fatalf("刪除後清單應為空, 得 %d", len(metas))
	}
	// 重複刪除 → ErrNotFound。
	if err := m.DeleteBackup(ctx, "u-1", bid); err != ErrNotFound {
		t.Fatalf("double delete want ErrNotFound, got %v", err)
	}
}

// DockerBackend.DeleteBackup 移除備份目錄;不存在 → ErrNotFound(免 Docker,只驗檔案系統邏輯)。
func TestDockerBackend_DeleteBackup_RemovesDir(t *testing.T) {
	tmp := t.TempDir()
	b := &DockerBackend{backupRoot: tmp}
	uuid := "u-del"
	bid := protocol.BackupID("bk-1")
	dir := filepath.Join(b.backupInstanceRoot(uuid), string(bid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeBackupRecord(dir, backupRecord{Meta: protocol.BackupMeta{BackupID: bid, InstanceUUID: uuid}}); err != nil {
		t.Fatalf("writeBackupRecord: %v", err)
	}

	if err := b.DeleteBackup(context.Background(), uuid, bid); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("備份目錄應已刪除, stat err=%v", err)
	}
	// 不存在 → ErrNotFound。
	if err := b.DeleteBackup(context.Background(), uuid, bid); err != ErrNotFound {
		t.Fatalf("missing delete want ErrNotFound, got %v", err)
	}
}

// DockerBackend.DeleteBackup 對路徑遍歷 backupID → ErrInvalidBackupID,且不刪備份根外目錄。
func TestDockerBackend_DeleteBackup_RejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	b := &DockerBackend{backupRoot: filepath.Join(tmp, "backups")}
	uuid := "u-esc"
	// 在備份根外建一個「機密」目錄,遍歷 backupID 若得逞會刪到它。
	secret := filepath.Join(tmp, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatalf("mkdir secret: %v", err)
	}

	for _, bid := range []protocol.BackupID{"..\\..\\secret", "../../secret", "..", ".hidden", "a/b", ""} {
		if err := b.DeleteBackup(context.Background(), uuid, bid); err != ErrInvalidBackupID {
			t.Fatalf("DeleteBackup(%q) want ErrInvalidBackupID, got %v", bid, err)
		}
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("備份根外目錄不應被動到, stat err=%v", err)
	}
}

// findBackup(還原定位)對路徑遍歷 bid 同樣拒絕。
func TestDockerBackend_findBackup_RejectsTraversal(t *testing.T) {
	b := &DockerBackend{backupRoot: t.TempDir()}
	if _, _, err := b.findBackup("u-esc", "..\\..\\meta"); err != ErrInvalidBackupID {
		t.Fatalf("findBackup traversal want ErrInvalidBackupID, got %v", err)
	}
	if _, _, err := b.findBackup("", "../../meta"); err != ErrInvalidBackupID {
		t.Fatalf("findBackup(empty uuid) traversal want ErrInvalidBackupID, got %v", err)
	}
}

// ---- DELETE 端點(401 / 404 / 成功)----

func TestServer_DeleteBackup(t *testing.T) {
	hs := newTestServer(t, NewMockBackend())
	const uuid = "dddddddd-dddd-dddd-dddd-dddddddddddd"

	// 建立實例 + 產一份備份。
	if status, data := request(t, hs, http.MethodPost, apiBase+"/instances", testToken, "", protocol.CreateInstanceRequest{Spec: specWithUUID(uuid)}); status != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", status, data)
	}
	status, data := request(t, hs, http.MethodPost, apiBase+"/instances/"+uuid+"/backup", testToken, "", nil)
	if status != http.StatusCreated {
		t.Fatalf("backup status=%d body=%s", status, data)
	}
	var br protocol.BackupResponse
	if err := json.Unmarshal(data, &br); err != nil {
		t.Fatalf("unmarshal backup: %v", err)
	}
	bid := br.Backup.BackupID
	delPath := apiBase + "/instances/" + uuid + "/backups/" + string(bid)

	// 401:未帶 token。
	if status, _ := request(t, hs, http.MethodDelete, delPath, "", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("no-token delete status=%d want 401", status)
	}

	// 404:刪不存在的備份。
	if status, body := request(t, hs, http.MethodDelete, apiBase+"/instances/"+uuid+"/backups/no-such-backup", testToken, "", nil); status != http.StatusNotFound {
		t.Fatalf("missing backup delete status=%d body=%s want 404", status, body)
	}

	// 路徑遍歷 backupID(URL 編碼 "..\..\secret")→ 400,備份與檔案系統無變化。
	travPath := apiBase + "/instances/" + uuid + "/backups/%2e%2e%5c%2e%2e%5csecret"
	if status, body := request(t, hs, http.MethodDelete, travPath, testToken, "", nil); status != http.StatusBadRequest {
		t.Fatalf("traversal delete status=%d body=%s want 400", status, body)
	}

	// 成功:204 且清單變空。
	if status, body := request(t, hs, http.MethodDelete, delPath, testToken, "", nil); status != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s want 204", status, body)
	}
	status, data = request(t, hs, http.MethodGet, apiBase+"/instances/"+uuid+"/backups", testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	var lr protocol.ListBackupsResponse
	if err := json.Unmarshal(data, &lr); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(lr.Backups) != 0 {
		t.Fatalf("刪除後備份清單應為空, 得 %d", len(lr.Backups))
	}
}
