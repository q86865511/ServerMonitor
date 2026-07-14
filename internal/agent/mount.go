package agent

import (
	"context"
	"errors"
	"io"
	"strings"

	"servermonitor/internal/protocol"
)

// MountWriter 是 RuntimeBackend 之外的可選擴充:把上傳的檔案位元組寫入某實例的具名 mount
// 宿主目錄(R11 手動模組包檔傳輸)。與 BackupLister 同樣以「橫切能力」介面表達,不擴張核心
// RuntimeBackend 介面;節點代理的 PUT /instances/{id}/mounts/{name} 端點在後端實作本介面時啟用。
type MountWriter interface {
	// WriteMountFile 把 r 的內容寫入實例 instanceUUID 的 mount mountName 下 filename 檔(覆寫冪等)。
	// mount 未於實例 spec 宣告 → ErrNotFound;filename 不安全 → ErrInvalidFilename。
	WriteMountFile(ctx context.Context, instanceUUID, mountName, filename string, r io.Reader) error
}

var (
	_ MountWriter = (*MockBackend)(nil)
	_ MountWriter = (*DockerBackend)(nil)
)

// ErrInvalidFilename 表示上傳檔名不安全(空、含路徑分隔/NUL,或為 . / ..)。映射到 ERR_BAD_REQUEST。
var ErrInvalidFilename = errors.New("agent: invalid mount filename")

// validateMountFilename 驗證上傳檔名為單一安全檔名:非空、不含路徑分隔或 NUL、非 . / ..。
// 用於端點(回 400)與後端(防禦性再驗)兩處,杜絕路徑穿越寫出 mount 目錄外。
func validateMountFilename(name string) error {
	if name == "" {
		return ErrInvalidFilename
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return ErrInvalidFilename
	}
	if name == "." || name == ".." {
		return ErrInvalidFilename
	}
	return nil
}

// specHasMount 回報 spec 是否宣告了名為 name 的 mount(未宣告的 mount 上傳視為 404)。
func specHasMount(spec protocol.InstanceSpec, name string) bool {
	for _, mnt := range spec.Mounts {
		if mnt.Name == name {
			return true
		}
	}
	return false
}

// sanitizeMountName 把 mount 名轉為安全的單層宿主目錄名(沿用 sanitizeDataDir 規則)。
func sanitizeMountName(name string) string {
	return sanitizeDataDir(name)
}
