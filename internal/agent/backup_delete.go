package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"servermonitor/internal/protocol"
)

// ErrInvalidBackupID 表示 backupID 不安全(空、含路徑分隔/NUL/.. 片段,或以 . 開頭)。
// 映射到 ERR_BAD_REQUEST。用於 DELETE/restore 端點與後端兩處防禦,杜絕以 backupID 做路徑遍歷。
var ErrInvalidBackupID = errors.New("agent: invalid backup id")

// validateBackupID 驗證 backupID 為單一安全路徑片段:非空、不含路徑分隔或 NUL、不含 ".." 片段、
// 不以 "." 開頭。備份根為 <backupRoot>/<uuid>/<backupID>/,backupID 直接充當目錄名——未消毒時
// Windows 下 "..\..\" 類值可 filepath.Join 逃逸備份根。手法對齊 mount.go 的 validateMountFilename。
func validateBackupID(id protocol.BackupID) error {
	s := string(id)
	if s == "" {
		return ErrInvalidBackupID
	}
	if strings.ContainsAny(s, "/\\\x00") {
		return ErrInvalidBackupID
	}
	if strings.Contains(s, "..") {
		return ErrInvalidBackupID
	}
	if strings.HasPrefix(s, ".") {
		return ErrInvalidBackupID
	}
	return nil
}

// backupDirWithinRoot 斷言 join 後的備份目錄 filepath.Clean 後仍嚴格落在該實例備份根之下
// (非等於根本身)。第二層防禦:即使 validateBackupID 漏網,亦不對備份根外目錄動手。
func backupDirWithinRoot(root, dir string) bool {
	cleanRoot := filepath.Clean(root)
	cleanDir := filepath.Clean(dir)
	return strings.HasPrefix(cleanDir, cleanRoot+string(os.PathSeparator))
}

// BackupDeleter 是 RuntimeBackend 之外的可選備份刪除擴充(比照 BackupLister 的可選介面模式,
// 見 backup_query.go)。保留策略(R9:超過 N 刪最舊)由核心 BackupService 編排,經節點代理
// DELETE /instances/{id}/backups/{backupID} 端點呼叫本介面刪除單一備份;後端不實作此介面時,
// 該端點回 500(視為後端能力缺失)。刪除以 instanceUUID 過濾,拒絕跨實例刪除(對齊
// findBackup 不跨 uuid 掃描的所有權邊界)。
type BackupDeleter interface {
	DeleteBackup(ctx context.Context, instanceUUID string, backupID protocol.BackupID) error
}

var (
	_ BackupDeleter = (*MockBackend)(nil)
	_ BackupDeleter = (*DockerBackend)(nil)
)

// DeleteBackup 移除指定實例的一份記憶體備份;備份不存在或不屬該實例 → ErrNotFound。
func (m *MockBackend) DeleteBackup(ctx context.Context, instanceUUID string, backupID protocol.BackupID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	b, ok := m.backups[backupID]
	if !ok || b.meta.InstanceUUID != instanceUUID {
		return ErrNotFound
	}
	delete(m.backups, backupID)
	return nil
}

// DeleteBackup 刪除 agent 備份根下該實例 UUID 的指定備份目錄(保留策略;R9)。目錄不存在 →
// ErrNotFound。以 instanceUUID 定位(不跨 uuid 掃描),避免以某實例端點刪他實例備份。
func (b *DockerBackend) DeleteBackup(ctx context.Context, instanceUUID string, backupID protocol.BackupID) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := validateBackupID(backupID); err != nil {
		return err
	}
	root := b.backupInstanceRoot(instanceUUID)
	dir := filepath.Join(root, string(backupID))
	if !backupDirWithinRoot(root, dir) {
		return ErrNotFound // 第二層防禦:清理後逃逸備份根 → 當作不存在,絕不 RemoveAll 根外目錄。
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return nil
}
