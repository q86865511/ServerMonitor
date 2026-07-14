package agent

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"

	"servermonitor/internal/protocol"
)

// 備份檔名(於 <backupRoot>/<uuid>/<backupID>/ 內)。
const (
	backupDataFile = "data.tar"
	backupMetaFile = "meta.json"
)

// backupRecord 是落檔的備份中繼:BackupMeta + spec 快照(供 Restore 自包含重建容器)。
type backupRecord struct {
	Meta protocol.BackupMeta   `json:"meta"`
	Spec protocol.InstanceSpec `json:"spec"`
}

// Archive 對(呼叫端已 planned-stop 的)實例做一致快照:tar host bind mount 資料 + sha256
// checksum,連同 spec 快照落 agent 擁有的備份根,回傳對外 opaque 的 BackupID(R9)。
func (b *DockerBackend) Archive(ctx context.Context, id protocol.RuntimeID) (protocol.BackupID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	j, err := b.cli.ContainerInspect(ctx, string(id))
	if err != nil {
		return "", mapDockerErr(err)
	}
	if j.Config == nil {
		return "", fmt.Errorf("agent: 容器 %s 無 Config,無法封存", id)
	}
	uuid := j.Config.Labels[labelUUID]
	if uuid == "" {
		return "", fmt.Errorf("agent: 容器 %s 缺 gsm.uuid 標籤,無法封存", id)
	}
	// spec 以資料根的快照為準;缺檔時由 inspect 盡力重建(TemplateID 可能遺失)。
	spec, serr := b.readInstanceSpec(uuid)
	if serr != nil {
		spec = specFromInspect(j)
	}

	backupID := protocol.BackupID(strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + shortRand())
	bkpDir := filepath.Join(b.backupInstanceRoot(uuid), string(backupID))
	if err := os.MkdirAll(bkpDir, 0o755); err != nil {
		return "", fmt.Errorf("建立備份目錄失敗: %w", err)
	}

	checksum, err := b.tarInstanceData(uuid, filepath.Join(bkpDir, backupDataFile))
	if err != nil {
		_ = os.RemoveAll(bkpDir)
		return "", fmt.Errorf("封存資料失敗: %w", err)
	}

	rec := backupRecord{
		Meta: protocol.BackupMeta{
			BackupID:     backupID,
			InstanceUUID: uuid,
			Game:         spec.TemplateID,
			TsUTC:        time.Now().UTC(),
			Checksum:     checksum,
		},
		Spec: spec,
	}
	if err := writeBackupRecord(bkpDir, rec); err != nil {
		_ = os.RemoveAll(bkpDir)
		return "", err
	}
	return backupID, nil
}

// Restore 以備份還原:驗 checksum → 解包 staging → rename-aside 原子切換資料 →
// 依備份 spec 建「新」容器回傳新 RuntimeID(R9;舊容器留給 core 切換)。
func (b *DockerBackend) Restore(ctx context.Context, id protocol.RuntimeID, bid protocol.BackupID) (protocol.RuntimeID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	// 先由舊容器標籤取 uuid(盡力);備份自包含,uuid 最終以備份記錄為準。
	inspectedUUID := ""
	if j, ierr := b.cli.ContainerInspect(ctx, string(id)); ierr == nil && j.Config != nil {
		inspectedUUID = j.Config.Labels[labelUUID]
	}
	bkpDir, rec, err := b.findBackup(inspectedUUID, bid)
	if err != nil {
		return "", err
	}
	uuid := rec.Meta.InstanceUUID
	if uuid == "" {
		return "", fmt.Errorf("agent: 備份 %s 缺 InstanceUUID", bid)
	}
	// 防跨實例還原:目標容器 uuid 已知且與備份記錄不符時拒絕(避免以 A 的端點提交 B 的
	// BackupID 而改到 B)。inspectedUUID 為空(舊容器已不存在)才容許純以備份記錄重建。
	if inspectedUUID != "" && inspectedUUID != uuid {
		return "", fmt.Errorf("agent: 備份 %s 屬實例 %s,與目標實例 %s 不符,拒絕還原", bid, uuid, inspectedUUID)
	}

	// staging 驗證:重算 tar checksum 與備份記錄比對。
	tarPath := filepath.Join(bkpDir, backupDataFile)
	sum, err := fileSHA256(tarPath)
	if err != nil {
		return "", fmt.Errorf("讀取備份資料失敗: %w", err)
	}
	if sum != rec.Meta.Checksum {
		return "", fmt.Errorf("agent: 備份 %s checksum 不符,拒絕還原", bid)
	}

	root := b.instanceDataRoot(uuid)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("建立實例資料根失敗: %w", err)
	}
	staging := filepath.Join(root, ".gsm-restore-"+shortRand())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", fmt.Errorf("建立還原 staging 失敗: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := untar(tarPath, staging); err != nil {
		return "", fmt.Errorf("解包備份失敗: %w", err)
	}

	// 逐 data_dir 子目錄 rename-aside 原子切換(Windows 不能 rename 覆蓋既有目錄,見 spike);
	// 全有或全無:任一步失敗即反向回滾至還原前狀態。
	commit, rollback, err := applyRestoreSwap(root, staging, os.Rename)
	if err != nil {
		return "", err
	}

	// 依備份 spec 建新容器(同 uuid、同 bind),回傳新 RuntimeID;不自動啟動。
	// Create 失敗時回滾資料(換回舊資料),使實例資料與還原前完全一致。
	newID, err := b.Create(ctx, rec.Spec)
	if err != nil {
		rollback()
		return "", fmt.Errorf("還原後建立新容器失敗: %w", err)
	}
	// Create 成功後才永久刪除換出的舊資料(trash),確保刪除前新容器已就緒。
	commit()
	return newID, nil
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
			// 本目錄換入失敗:先把剛移開的舊資料還原,再回滾先前各目錄。
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

// findBackup 定位備份目錄與記錄。uuid 已知時只在該 uuid 目錄下找,找不到即回 ErrNotFound
// (不跨 uuid 掃描——避免以某實例端點提交他實例 BackupID 時誤配到別的實例);僅 uuid 為空
// (舊容器已不存在)才容許掃描所有 uuid 目錄。
func (b *DockerBackend) findBackup(uuid string, bid protocol.BackupID) (string, backupRecord, error) {
	tryDir := func(dir string) (string, backupRecord, bool) {
		rec, err := readBackupRecord(dir)
		if err != nil {
			return "", backupRecord{}, false
		}
		return dir, rec, true
	}
	if uuid != "" {
		dir := filepath.Join(b.backupInstanceRoot(uuid), string(bid))
		if d, rec, ok := tryDir(dir); ok {
			return d, rec, nil
		}
		return "", backupRecord{}, ErrNotFound // 不 fallback 跨 uuid 掃描
	}
	// uuid 為空:掃描所有 uuid 目錄。
	uuids, err := os.ReadDir(b.backupRoot)
	if err == nil {
		for _, u := range uuids {
			if !u.IsDir() {
				continue
			}
			dir := filepath.Join(b.backupRoot, u.Name(), string(bid))
			if d, rec, ok := tryDir(dir); ok {
				return d, rec, nil
			}
		}
	}
	return "", backupRecord{}, ErrNotFound
}

// tarInstanceData 打包實例資料根下各 data_dir(排除 spec 快照與 .gsm-* 暫存),
// 邊寫邊算 sha256,回傳十六進位 checksum。
func (b *DockerBackend) tarInstanceData(uuid, tarPath string) (string, error) {
	root := b.instanceDataRoot(uuid)
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
		if rel == instanceSpecFile {
			return nil
		}
		if strings.HasPrefix(top, ".gsm-") {
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

// untar 解包 tar 到 dest,含路徑穿越防護。
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
			// 驗 Linkname 解析後仍落在 dest 內,拒絕逃逸(絕對路徑或 ../ 逸出)。
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
// 絕對 linkname 直接以 Clean 判定;相對 linkname 以 target 所在目錄 join 後 Clean。
// 空 linkname 視為非法。
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

// specFromInspect 由容器 inspect 盡力重建 InstanceSpec(缺 instance.json 時的 fallback;
// TemplateID/Variant 不可從 Docker 還原,留空)。
func specFromInspect(j types.ContainerJSON) protocol.InstanceSpec {
	spec := protocol.InstanceSpec{}
	if j.Config != nil {
		spec.Image = j.Config.Image
		spec.Labels = cloneStringMap(j.Config.Labels)
		spec.UUID = j.Config.Labels[labelUUID]
		spec.Node = j.Config.Labels[labelNode]
		spec.Env = map[string]string{}
		for _, kv := range j.Config.Env {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				spec.Env[kv[:i]] = kv[i+1:]
			}
		}
	}
	for _, m := range j.Mounts {
		if m.Type == "bind" && m.Destination != "" {
			spec.DataDirs = append(spec.DataDirs, m.Destination)
		}
	}
	spec.Ports = portsFromInspect(j)
	return spec
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
