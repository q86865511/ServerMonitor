package agent

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
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
	_ MountWriter = (*NativeBackend)(nil)
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

// WriteMountFile 把上傳的檔案位元組寫入某實例的具名 mount 宿主目錄(R11 手動模組包檔傳輸,
// 語意同 DockerBackend.WriteMountFile)。native 額外行為:寫入完成後,若該檔案是可辨識的模組包
// 封存檔(mrpack/cfzip,見 detectModpackArchiveType)且範本宣告了 native mods 落位目錄,立即解進
// mods——手動上傳無法在 Create 時處理(Create 早於本次上傳完成,見 native.go Create 的說明與
// instance_service.go:295 上傳時序),此處是它抵達後的唯一安裝時機。非模組包封存檔的一般掛載
// 檔案不受影響(no-op)。
func (b *NativeBackend) WriteMountFile(ctx context.Context, instanceUUID, mountName, filename string, r io.Reader) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := validateMountFilename(filename); err != nil {
		return err
	}
	spec, err := b.readSpec(instanceUUID)
	if err != nil || !specHasMount(spec, mountName) {
		return ErrNotFound
	}
	dir := b.hostDirForMount(instanceUUID, mountName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("建立掛載目錄失敗: %w", err)
	}
	dst := filepath.Join(dir, filename)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("開啟掛載檔失敗: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("寫入掛載檔失敗: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("關閉掛載檔失敗: %w", err)
	}
	return b.maybeInstallManualModpack(ctx, instanceUUID, spec, dst)
}

// maybeInstallManualModpack 於手動掛載檔上傳完成後,依封存檔內容判別模組包型別(mrpack/cfzip)
// 並安裝至 native mods 目錄(R11)。無法辨識為模組包封存檔(如一般設定檔掛載)時 no-op、不報錯
// ——mounts 具名掛載本非模組包專用機制(見 protocol.MountSpec 註解)。範本未宣告 native mods 目錄
// 則回明確錯誤(不臆測落位路徑);cfzip 依 provisionRunner.InstallModpack 的約定回「尚未支援」。
func (b *NativeBackend) maybeInstallManualModpack(ctx context.Context, uuid string, spec protocol.InstanceSpec, archivePath string) error {
	kind, ok := detectModpackArchiveType(archivePath)
	if !ok {
		return nil
	}
	// #2×#7(複審 A):TargetDir 傳 workDir(伺服器工作目錄=遊戲根),而非實例根——provider 的
	// files[].path 自帶 mods/ 前綴、overrides 相對遊戲根,伺服器以 cwd=workDir 讀取,落位必須跟隨
	// workDir(見 native.go Create 與 modprovider.go)。modsInstallDir 於此僅作「範本有宣告 mods_dir」
	// 的前置檢查,未宣告即拒安裝、不臆測落位。
	root := b.instanceDataRoot(uuid)
	if _, derr := b.modsInstallDir(root, spec); derr != nil {
		return derr
	}
	workDir := b.workingDir(root, spec.Native.Launch.WorkingDir)
	var mcVersion, loader string
	if spec.Native != nil {
		mcVersion = spec.Native.Provision.MCVersion
		loader = spec.Native.Provision.Loader
	}
	return b.prov.InstallModpack(ctx, ModpackInstallRequest{
		Type:        kind,
		ArchivePath: archivePath,
		TargetDir:   workDir,
		MCVersion:   mcVersion,
		Loader:      loader,
	}, b.progressEmitter(uuid))
}

// detectModpackArchiveType 開啟 zip 並依頂層索引檔判別模組包封存檔型別,同
// internal/core/modpack.go validateZipHasEntry 的辨識慣例:含 modrinth.index.json → "modrinth";
// 含 manifest.json → "curseforge"(索引檔內容是否合法 JSON 已由 core 端 validateModpack 於上傳前
// 把關,此處只需判別型別)。非 zip 或無索引檔 → ok=false。
func detectModpackArchiveType(archivePath string) (kind string, ok bool) {
	rc, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", false
	}
	defer rc.Close()
	for _, f := range rc.File {
		switch path.Clean(f.Name) {
		case "modrinth.index.json":
			return "modrinth", true
		case "manifest.json":
			return "curseforge", true
		}
	}
	return "", false
}
