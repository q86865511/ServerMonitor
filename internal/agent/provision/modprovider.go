package provision

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ErrModpackIncompatible 表示模組包宣告的 Minecraft 版本或 loader 依賴與請求不符,拒絕安裝
// (native-backend R11)。以 errors.Is 判別。
var ErrModpackIncompatible = errors.New("provision: 模組包版本/loader 不相容")

// maxMrpackIndexBytes 是讀取 modrinth.index.json 的上限,防禦高壓縮比條目(zip bomb);沿用
// internal/core/modpack.go 的索引檔讀取上限慣例(該檔獨立持有,不跨套件相依)。
const maxMrpackIndexBytes = 32 << 20

// ModpackRef 描述一個遠端模組包來源(對映 protocol.ModpackRef,本套件刻意不依賴 protocol,
// 由呼叫端 adapter 轉譯,同 ProvisionProgress 的解耦慣例)。
type ModpackRef struct {
	Type string // "modrinth" | "curseforge"(T14)
	Ref  string // slug / project id / 版本 id
}

// ModpackInstallRequest 是一次模組包安裝請求。Ref 與 ArchivePath 擇一有值:
//   - Ref 非 nil:由 ModProvider 解析遠端來源(查 API、下載)取得封存檔。
//   - ArchivePath 非空:直接安裝既有本機封存檔(手動上傳路徑),略過解析/下載。
type ModpackInstallRequest struct {
	Ref         *ModpackRef
	ArchivePath string
	TargetDir   string // 模組落位目錄(mods)絕對路徑
	MCVersion   string // 相容性檢查用(index.json dependencies.minecraft)
	Loader      string // 相容性檢查用(fabric/forge/neoforge/quilt 才有對應依賴鍵可查)
}

// ModProvider 依模組包來源解析並安裝模組/模組包至 TargetDir(native-backend R11/R14)。
// 各實作只做「取得封存檔並安全落地」,不碰 Job Object / PID(supervisor 職責)。
type ModProvider interface {
	InstallModpack(ctx context.Context, req ModpackInstallRequest, progress ProgressFunc) error
}

// ---- mrpack(Modrinth modpack)格式 ----
//
// 查證(2026-07-16,https://support.modrinth.com/en/articles/8802351-modrinth-modpack-format-mrpack):
//   - modrinth.index.json 必要欄位:formatVersion、game(僅支援 "minecraft")、versionId、name、
//     files、dependencies;summary 為選用。
//   - files[]:path(相對 Minecraft 實例根,如 "mods/Foo.jar")、hashes(官方保證至少含 sha1、
//     sha512)、env(選用,client/server 屬性,值為 required/optional/unsupported)、downloads
//     (HTTPS URL 陣列)、fileSize。
//   - dependencies 鍵名:minecraft、forge、neoforge、fabric-loader、quilt-loader。
//   - overrides/ 複製到實例根;server-overrides/ 其後套用並覆寫 overrides 內容(分層應用);
//     client-overrides/ 對伺服器無意義,不處理。

// mrpackIndex 對映 modrinth.index.json。
type mrpackIndex struct {
	FormatVersion int               `json:"formatVersion"`
	Game          string            `json:"game"`
	VersionID     string            `json:"versionId"`
	Name          string            `json:"name"`
	Files         []mrpackFile      `json:"files"`
	Dependencies  map[string]string `json:"dependencies"`
}

// mrpackFile 是 modrinth.index.json 的 files[] 元素。
type mrpackFile struct {
	Path      string            `json:"path"`
	Hashes    map[string]string `json:"hashes"`
	Env       map[string]string `json:"env,omitempty"`
	Downloads []string          `json:"downloads"`
	FileSize  int64             `json:"fileSize"`
}

// installMrpackArchive 是 mrpack 安裝的共用核心邏輯(解析 index → 相容性檢查 → 逐檔下載校驗 →
// 解 overrides),供 ModrinthProvider 於「已取得本機封存檔」後呼叫——不論該檔是遠端解析下載而來,
// 抑或使用者手動上傳(見 native.go WriteMountFile)。
func installMrpackArchive(ctx context.Context, client *http.Client, archivePath, targetDir, mcVersion, loader string, progress ProgressFunc) error {
	idx, err := readMrpackIndex(archivePath)
	if err != nil {
		return err
	}
	if err := checkMrpackCompatibility(idx, mcVersion, loader); err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("provision: 建立模組落位目錄失敗: %w", err)
	}

	total := len(idx.Files)
	for i, f := range idx.Files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if modpackFileSkipped(f) {
			continue
		}
		if len(f.Downloads) == 0 {
			return fmt.Errorf("provision: 模組包檔案 %q 無下載連結", f.Path)
		}
		dest, err := safeModpackFilePath(targetDir, f.Path)
		if err != nil {
			return err
		}
		if err := download(ctx, client, downloadRequest{
			URL:      f.Downloads[0],
			DestPath: dest,
			Checksum: mrpackChecksum(f.Hashes),
			Stage:    fmt.Sprintf("下載模組 %d/%d", i+1, total),
			Progress: progress,
		}); err != nil {
			return fmt.Errorf("provision: 下載模組檔 %q 失敗: %w", f.Path, err)
		}
	}

	if err := extractMrpackOverrides(archivePath, targetDir); err != nil {
		return err
	}
	progress.report(ProvisionProgress{Stage: "modpack", Percent: 100, Detail: "安裝完成"})
	return nil
}

// readMrpackIndex 於 archivePath 內尋找頂層 modrinth.index.json 並解析。
func readMrpackIndex(archivePath string) (mrpackIndex, error) {
	rc, err := zip.OpenReader(archivePath)
	if err != nil {
		return mrpackIndex{}, fmt.Errorf("provision: 無法以 zip 開啟模組包 %q: %w", archivePath, err)
	}
	defer rc.Close()
	for _, f := range rc.File {
		if f.Name != "modrinth.index.json" {
			continue
		}
		data, err := readZipEntryLimited(f, maxMrpackIndexBytes)
		if err != nil {
			return mrpackIndex{}, err
		}
		var idx mrpackIndex
		if err := json.Unmarshal(data, &idx); err != nil {
			return mrpackIndex{}, fmt.Errorf("provision: 解析 modrinth.index.json 失敗: %w", err)
		}
		if idx.Game != "" && idx.Game != "minecraft" {
			return mrpackIndex{}, fmt.Errorf("provision: 不支援的模組包遊戲類型 %q", idx.Game)
		}
		return idx, nil
	}
	return mrpackIndex{}, fmt.Errorf("provision: 模組包 %q 缺少 modrinth.index.json", archivePath)
}

// readZipEntryLimited 讀出 zip 條目內容,以 LimitReader 限制讀取量防禦高壓縮比條目。
func readZipEntryLimited(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("provision: 開啟 zip 條目 %q 失敗: %w", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit))
	if err != nil {
		return nil, fmt.Errorf("provision: 讀取 zip 條目 %q 失敗: %w", f.Name, err)
	}
	return data, nil
}

// checkMrpackCompatibility 檢查 index 宣告的 Minecraft 版本與 loader 依賴是否與請求相符;
// 不符回 ErrModpackIncompatible(可 errors.Is 判別)。mcVersion/loader 為空時略過對應檢查
// (呼叫端未提供,不誤擋)。
func checkMrpackCompatibility(idx mrpackIndex, mcVersion, loader string) error {
	if want := idx.Dependencies["minecraft"]; want != "" && mcVersion != "" && want != mcVersion {
		return fmt.Errorf("%w: 模組包需要 Minecraft %s,實際 %s", ErrModpackIncompatible, want, mcVersion)
	}
	if loader == "" {
		return nil
	}
	key, checked := mrpackLoaderDependencyKey(loader)
	if !checked {
		return nil // 未知 loader 家族(型別層級相容已由 core 端 validateModpack 把關)。
	}
	if _, has := idx.Dependencies[key]; !has {
		return fmt.Errorf("%w: 模組包未宣告 loader %q 依賴(dependencies=%v)", ErrModpackIncompatible, loader, idx.Dependencies)
	}
	return nil
}

// mrpackLoaderDependencyKey 把 loader 家族名對映到 modrinth.index.json dependencies 的鍵名
// (查證來源同套件頂註)。
func mrpackLoaderDependencyKey(loader string) (key string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(loader)) {
	case "fabric":
		return "fabric-loader", true
	case "forge":
		return "forge", true
	case "neoforge":
		return "neoforge", true
	case "quilt":
		return "quilt-loader", true
	default:
		return "", false
	}
}

// modpackFileSkipped 回報 mrpack 檔案是否因 env.server=="unsupported" 而應跳過(R11)。
func modpackFileSkipped(f mrpackFile) bool {
	if f.Env == nil {
		return false
	}
	return strings.EqualFold(f.Env["server"], "unsupported")
}

// mrpackChecksum 由 mrpack 官方保證的 hashes(至少含 sha1、sha512)取校驗規格,優先 sha512。
func mrpackChecksum(hashes map[string]string) checksumSpec {
	if v := strings.TrimSpace(hashes["sha512"]); v != "" {
		return checksumSpec{Algo: "sha512", Value: v}
	}
	if v := strings.TrimSpace(hashes["sha1"]); v != "" {
		return checksumSpec{Algo: "sha1", Value: v}
	}
	return checksumSpec{}
}

// safeModpackFilePath 把 mrpack 相對路徑(index.json 的 files[].path,或 overrides/ 條目名去除
// 前綴後的相對路徑)安全地落於 targetDir 之內,防路徑穿越/絕對路徑(zip-slip 同構風險——來源雖非
// zip 條目名,而是 JSON 欄位值,但可信度同樣不可假設,故套用與 archive.go extractZipEntry 相同的
// 兩道防線)。
func safeModpackFilePath(targetDir, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("provision: 模組包檔案路徑為空")
	}
	if strings.HasPrefix(relPath, "/") || strings.HasPrefix(relPath, "\\") {
		return "", fmt.Errorf("%w: 絕對路徑 %q", ErrUnsafeZipEntry, relPath)
	}
	if zipEntryHasDotDot(relPath) {
		return "", fmt.Errorf("%w: 路徑穿越 %q", ErrUnsafeZipEntry, relPath)
	}
	target := filepath.Join(targetDir, filepath.FromSlash(relPath))
	rel, err := filepath.Rel(targetDir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: 落點越界 %q", ErrUnsafeZipEntry, relPath)
	}
	return target, nil
}

// extractMrpackOverrides 解 overrides/ 與 server-overrides/(依序套用,後者覆寫前者,官方優先權;
// client-overrides/ 對伺服器無意義,略過)。
func extractMrpackOverrides(archivePath, targetDir string) error {
	rc, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("provision: 無法以 zip 開啟模組包 %q: %w", archivePath, err)
	}
	defer rc.Close()
	for _, prefix := range []string{"overrides/", "server-overrides/"} {
		for _, f := range rc.File {
			rel, ok := strings.CutPrefix(f.Name, prefix)
			if !ok || rel == "" {
				continue
			}
			if err := extractMrpackOverrideEntry(f, rel, targetDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// extractMrpackOverrideEntry 落地單一 overrides 條目(zip-slip 防禦同 safeModpackFilePath)。
func extractMrpackOverrideEntry(f *zip.File, rel, targetDir string) error {
	dest, err := safeModpackFilePath(targetDir, rel)
	if err != nil {
		return err
	}
	if f.FileInfo().IsDir() {
		return os.MkdirAll(dest, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("provision: 建立 overrides 子目錄失敗: %w", err)
	}
	in, err := f.Open()
	if err != nil {
		return fmt.Errorf("provision: 開啟 overrides 條目 %q 失敗: %w", f.Name, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return fmt.Errorf("provision: 建立 overrides 檔 %q 失敗: %w", dest, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("provision: 寫入 overrides 檔 %q 失敗: %w", dest, err)
	}
	return out.Close()
}
