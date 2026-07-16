package agent

// 備份封存的 package 內共用核心:tar 打包(排除中繼/具名掛載/暫存)、解包(含路徑穿越與
// symlink 逃逸防護)、sha256 checksum、Restore 原子切換,以及備份記錄讀寫。DockerBackend 與
// NativeBackend 皆復用此處(native-backend R10:兩後端備份格式互通)。docker/native 後端各自的
// Archive/Restore 只負責取 uuid、斷言停機、組 BackupMeta 與(docker)重建容器,打包/解包/切換一律
// 委由本檔,確保兩後端產出的 data.tar 位元組級一致、可跨後端還原。

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"servermonitor/internal/protocol"
)

// 備份檔名(於 <backupRoot>/<uuid>/<backupID>/ 內)。
const (
	backupDataFile = "data.tar"
	backupMetaFile = "meta.json"
)

// backupRecord 是落檔的備份中繼:BackupMeta + spec 快照(供 Restore 自包含重建實例)。
type backupRecord struct {
	Meta protocol.BackupMeta   `json:"meta"`
	Spec protocol.InstanceSpec `json:"spec"`
}

// tarDir 打包實例資料根 root 下各 data_dir 子目錄,邊寫邊算 sha256,回傳十六進位 checksum。
// 排除項(不入備份、跨後端一致):
//   - 頂層中繼檔:instance.json(spec 快照)、native.json(native 中繼)、proc.json(PID 中繼)、
//     server.log*(native 滾動日誌)——皆為「執行環境」而非「實例資料」,不隨備份漂移。
//   - mounts/ 具名掛載(R11 手動模組包檔:屬建立時輸入,跨還原保持原樣)。
//   - .gsm-* 暫存(還原 staging/換出暫存)。
// docker 後端無 native.json/proc.json/server.log,對其為無害的 no-op 排除。
func tarDir(root, tarPath string) (string, error) {
	f, err := os.Create(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(f, h))

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		top := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			top = rel[:i]
		}
		if isTopLevelMeta(rel) {
			return nil
		}
		// mounts/ 與 .gsm-* 整棵排除。
		if top == mountsSubdir || strings.HasPrefix(top, ".gsm-") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		var link string
		if info.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return herr
		}
		hdr.Name = rel
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.Type().IsRegular() {
			src, oerr := os.Open(p)
			if oerr != nil {
				return oerr
			}
			_, cerr := io.Copy(tw, src)
			src.Close()
			if cerr != nil {
				return cerr
			}
		}
		return nil
	})
	if walkErr != nil {
		tw.Close()
		return "", walkErr
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isTopLevelMeta 判定 rel(已 ToSlash)是否為應排除於備份的頂層中繼檔(非資料)。
func isTopLevelMeta(rel string) bool {
	if strings.Contains(rel, "/") {
		return false // 僅頂層檔案為中繼;子目錄下同名檔屬實例資料
	}
	switch rel {
	case instanceSpecFile, nativeMetaFile, procMetaFile:
		return true
	}
	return strings.HasPrefix(rel, nativeLogFile) // server.log / server.log.1 ...
}

// untar 解包 tar 到 dest,含路徑穿越與 symlink 逃逸防護。
func untar(tarPath, dest string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	cleanDest := filepath.Clean(dest)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(cleanDest, filepath.FromSlash(hdr.Name))
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("備份含非法路徑: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !linkWithinDest(cleanDest, target, hdr.Linkname) {
				return fmt.Errorf("備份含逃逸 symlink: %s -> %s", hdr.Name, hdr.Linkname)
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Symlink(hdr.Linkname, target) // Windows 可能需權限;盡力而為
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, oerr := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if oerr != nil {
				return oerr
			}
			if _, cerr := io.Copy(out, tr); cerr != nil {
				out.Close()
				return cerr
			}
			out.Close()
		}
	}
	return nil
}

// linkWithinDest 判定一個 symlink(target 位置、指向 linkname)解析後是否仍落在 dest 內。
// 絕對 linkname 直接以 Clean 判定;相對 linkname 以 target 所在目錄 join 後 Clean。空 linkname 視為非法。
func linkWithinDest(cleanDest, target, linkname string) bool {
	if linkname == "" {
		return false
	}
	ln := filepath.FromSlash(linkname)
	var resolved string
	if filepath.IsAbs(ln) {
		resolved = filepath.Clean(ln)
	} else {
		resolved = filepath.Clean(filepath.Join(filepath.Dir(target), ln))
	}
	return resolved == cleanDest || strings.HasPrefix(resolved, cleanDest+string(os.PathSeparator))
}

// fileSHA256 回傳檔案內容的十六進位 sha256。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// restoreSwap 記錄一個 data_dir 子目錄的原子切換:live 為最終目標(root/name),
// oldPath 為原資料被移開的暫存位置(原本不存在則為空)。
type restoreSwap struct {
	name    string
	live    string
	oldPath string
}

// applyRestoreSwap 把 staging 下各子目錄以 rename-aside 原子換入 root:每目錄先把現行資料
// 移到 .gsm-old-*(記入 swap),再把 staging 的還原資料換入。任一 rename 失敗即反向回滾
// (把已換入的移回 staging、把 .gsm-old 移回原位),回錯時 root 與呼叫前完全一致、無殘留。
// 全部成功時回傳 commit(永久刪除換出的舊資料)與 rollback(供之後步驟如建容器失敗時反悔)。
// rename 參數供測試注入失敗;回滾一律用 os.Rename 實際還原。
func applyRestoreSwap(root, staging string, rename func(oldpath, newpath string) error) (commit func(), rollback func(), err error) {
	entries, rerr := os.ReadDir(staging)
	if rerr != nil {
		return nil, nil, fmt.Errorf("讀取 staging 失敗: %w", rerr)
	}
	var swaps []restoreSwap
	rollback = func() {
		for i := len(swaps) - 1; i >= 0; i-- {
			s := swaps[i]
			_ = os.Rename(s.live, filepath.Join(staging, s.name)) // 換入的移回 staging
			if s.oldPath != "" {
				_ = os.Rename(s.oldPath, s.live) // 舊資料移回原位
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		sw := restoreSwap{name: name, live: filepath.Join(root, name)}
		if _, serr := os.Stat(sw.live); serr == nil {
			old := filepath.Join(root, ".gsm-old-"+shortRand()+"-"+name)
			if rnErr := rename(sw.live, old); rnErr != nil {
				rollback()
				return nil, nil, fmt.Errorf("原子切換(移開現行)失敗: %w", rnErr)
			}
			sw.oldPath = old
		}
		if rnErr := rename(filepath.Join(staging, name), sw.live); rnErr != nil {
			if sw.oldPath != "" {
				_ = os.Rename(sw.oldPath, sw.live)
			}
			rollback()
			return nil, nil, fmt.Errorf("原子切換(換入還原)失敗: %w", rnErr)
		}
		swaps = append(swaps, sw)
	}
	commit = func() {
		for _, s := range swaps {
			if s.oldPath != "" {
				_ = os.RemoveAll(s.oldPath)
			}
		}
	}
	return commit, rollback, nil
}

func writeBackupRecord(dir string, rec backupRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化備份記錄失敗: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupMetaFile), data, 0o644); err != nil {
		return fmt.Errorf("寫入備份記錄失敗: %w", err)
	}
	return nil
}

func readBackupRecord(dir string) (backupRecord, error) {
	var rec backupRecord
	data, err := os.ReadFile(filepath.Join(dir, backupMetaFile))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	return rec, nil
}
