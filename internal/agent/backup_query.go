package agent

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"servermonitor/internal/protocol"
)

// BackupLister 是 RuntimeBackend 之外的可選備份查詢擴充。RuntimeBackend 的 Archive 只回
// opaque BackupID、無列舉能力(封存/還原是核心編排的動作,查詢則否),故不擴張核心介面;
// 後端若實作本介面,節點代理的 GET /instances/{id}/backups 即回實際清單、POST /backup
// 亦能回完整中繼(以 instance UUID 過濾,對齊 BackupMeta.InstanceUUID)。
//
// 方法置於本檔(而非 mock.go / docker_backup.go)以集中「備份查詢」這個橫切能力,
// 並使既有後端檔案維持不動;實作仍為各後端型別的方法(同套件)。
type BackupLister interface {
	ListBackups(ctx context.Context, instanceUUID string) ([]protocol.BackupMeta, error)
}

var (
	_ BackupLister = (*MockBackend)(nil)
	_ BackupLister = (*DockerBackend)(nil)
)

// ListBackups 回傳指定 instance UUID 的記憶體備份中繼(依 BackupID 排序,輸出穩定)。
func (m *MockBackend) ListBackups(ctx context.Context, instanceUUID string) ([]protocol.BackupMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	out := make([]protocol.BackupMeta, 0)
	for _, b := range m.backups {
		if b.meta.InstanceUUID == instanceUUID {
			out = append(out, b.meta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BackupID < out[j].BackupID })
	return out, nil
}

// ListBackups 掃描 agent 備份根下該 instance UUID 的備份目錄,讀回各筆備份記錄的中繼
// (依時間排序);無備份目錄視為空清單而非錯誤。
func (b *DockerBackend) ListBackups(ctx context.Context, instanceUUID string) ([]protocol.BackupMeta, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	dir := b.backupInstanceRoot(instanceUUID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []protocol.BackupMeta{}, nil
		}
		return nil, err
	}
	out := make([]protocol.BackupMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, rerr := readBackupRecord(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue // 略過殘缺/非備份目錄
		}
		out = append(out, rec.Meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TsUTC.Before(out[j].TsUTC) })
	return out, nil
}
