package agent

import (
	"context"
	"net/http"
	"testing"

	"servermonitor/internal/protocol"
)

// TestValidateInstanceUUID_RejectsTraversal 驗證階段 4 安全修正:以 uuid 定位宿主目錄的端點
// (diskusage/delete backup/purge)先驗 uuid 格式,拒絕路徑遍歷(對稱 validateBackupID)。
func TestValidateInstanceUUID_RejectsTraversal(t *testing.T) {
	for _, u := range []string{"550e8400-e29b-41d4-a716-446655440000", "abc123", "local"} {
		if err := validateInstanceUUID(u); err != nil {
			t.Errorf("合法 uuid %q 不應被拒: %v", u, err)
		}
	}
	for _, u := range []string{"", "..", "../x", "..\\x", "a/b", "a\\b", ".hidden", "x\x00y"} {
		if err := validateInstanceUUID(u); err == nil {
			t.Errorf("不安全 uuid %q 應被拒", u)
		}
	}
}

// purgeRecordingBackend 嵌入 MockBackend 並記錄 PurgeInstanceData 呼叫,供 B7 handler 測試。
type purgeRecordingBackend struct {
	*MockBackend
	purged []string
}

func (b *purgeRecordingBackend) PurgeInstanceData(_ context.Context, uuid string) error {
	b.purged = append(b.purged, uuid)
	return nil
}

// TestServer_DeletePurgesDataWhenContainerGone_B7 驗證 B7:容器已 out-of-band 移除(resolve 查無)時,
// 帶 purge 的 DELETE 仍以 uuid 直接清宿主資料/備份(否則 resolve 短路使 backend.Remove 的 purge 清理
// 被跳過,而 core 把 404 視為已移除續刪 DB,造成資料+備份永久孤兒卻回報成功)。
func TestServer_DeletePurgesDataWhenContainerGone_B7(t *testing.T) {
	backend := &purgeRecordingBackend{MockBackend: NewMockBackend()}
	hs := newTestServer(t, backend)

	// 容器不存在(從未建立)→ resolve 回 ErrNotFound → 404;purge=true → 應仍以 uuid 清資料。
	status, _ := request(t, hs, http.MethodDelete, apiBase+"/instances/gone-uuid", testToken, "",
		protocol.RemoveInstanceRequest{Purge: true})
	if status != http.StatusNotFound {
		t.Fatalf("容器不存在應回 404, 得 %d", status)
	}
	if len(backend.purged) != 1 || backend.purged[0] != "gone-uuid" {
		t.Errorf("B7:容器已不存在時 purge 應以 uuid 清資料, 實際 purged=%v", backend.purged)
	}
}

// TestServer_DeleteNoPurgeWhenContainerGoneSkipsData_B7 驗證 B7 邊界:容器已不存在且**未**帶 purge 時,
// 不觸發資料清理(僅回 404,保留資料)。
func TestServer_DeleteNoPurgeWhenContainerGoneSkipsData_B7(t *testing.T) {
	backend := &purgeRecordingBackend{MockBackend: NewMockBackend()}
	hs := newTestServer(t, backend)

	status, _ := request(t, hs, http.MethodDelete, apiBase+"/instances/gone-uuid", testToken, "",
		protocol.RemoveInstanceRequest{Purge: false})
	if status != http.StatusNotFound {
		t.Fatalf("容器不存在應回 404, 得 %d", status)
	}
	if len(backend.purged) != 0 {
		t.Errorf("B7:未帶 purge 不應清資料, 實際 purged=%v", backend.purged)
	}
}
