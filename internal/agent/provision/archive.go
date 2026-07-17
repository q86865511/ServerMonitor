package provision

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafeZipEntry 表示 zip 內含路徑穿越/絕對路徑條目(zip-slip),整包拒絕解壓。以 errors.Is 判別。
var ErrUnsafeZipEntry = errors.New("provision: zip 條目路徑不安全")

// extractZip 將 src 解壓至 destDir,對每個條目做 zip-slip 防禦(參照 internal/core/modpack.go
// 既有 .. / 絕對路徑檢查慣例,並額外以 filepath.Rel 確認落點在 destDir 之內)。任一條目不安全
// 即整包拒絕(回 ErrUnsafeZipEntry),不留部分解壓內容由呼叫端負責清理(見 java.go 的 staging)。
func extractZip(src, destDir, stage string, progress ProgressFunc) error {
	rc, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("provision: 無法以 zip 開啟 %q: %w", src, err)
	}
	defer rc.Close()

	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("provision: 解析解壓目的路徑失敗: %w", err)
	}
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return fmt.Errorf("provision: 建立解壓目的目錄失敗: %w", err)
	}

	total := len(rc.File)
	for i, f := range rc.File {
		if err := extractZipEntry(f, destAbs); err != nil {
			return err
		}
		if progress != nil && total > 0 {
			progress.report(ProvisionProgress{
				Stage:   stage,
				Percent: int((i + 1) * 100 / total),
				Detail:  fmt.Sprintf("%d/%d 檔", i+1, total),
			})
		}
	}
	return nil
}

// extractZipEntry 解壓單一條目至 destAbs 下,先做路徑安全檢查再落地。
func extractZipEntry(f *zip.File, destAbs string) error {
	// 防線一:比照 modpack.go——條目名不得為絕對路徑或含 .. 元素。
	if strings.HasPrefix(f.Name, "/") || strings.HasPrefix(f.Name, "\\") {
		return fmt.Errorf("%w: 絕對路徑 %q", ErrUnsafeZipEntry, f.Name)
	}
	if zipEntryHasDotDot(f.Name) {
		return fmt.Errorf("%w: 路徑穿越 %q", ErrUnsafeZipEntry, f.Name)
	}

	target := filepath.Join(destAbs, filepath.FromSlash(f.Name))
	// 防線二:確認清理後的落點仍在 destAbs 之內(防符號穿越/邊界繞過)。
	rel, err := filepath.Rel(destAbs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%w: 落點越界 %q", ErrUnsafeZipEntry, f.Name)
	}

	if f.FileInfo().IsDir() {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("provision: 建立解壓子目錄失敗: %w", err)
	}

	in, err := f.Open()
	if err != nil {
		return fmt.Errorf("provision: 開啟 zip 條目 %q 失敗: %w", f.Name, err)
	}
	defer in.Close()

	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return fmt.Errorf("provision: 建立解壓檔 %q 失敗: %w", target, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("provision: 寫入解壓檔 %q 失敗: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("provision: 關閉解壓檔 %q 失敗: %w", target, err)
	}
	return nil
}

// zipEntryHasDotDot 回報 zip 條目名是否含 .. 路徑元素(以 / 或 \ 分隔皆檢查)。
// 與 internal/core/modpack.go 同名輔助同義,本套件獨立持有以維持零跨套件相依。
func zipEntryHasDotDot(name string) bool {
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}
