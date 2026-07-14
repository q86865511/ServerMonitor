package agent

// 免 Docker 的單元測試,鎖定雙審修正的資料安全語意:
//   B  Restore 全有或全無回滾(applyRestoreSwap)
//   D  跨實例還原防護(findBackup 不跨 uuid fallback)
//   H  untar symlink Linkname 逃逸防護
// 真 Docker 整合路徑另見 docker_test.go(-tags docker)。

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"servermonitor/internal/protocol"
)

// ---- 測試輔助 ----

// mkTree 在 base 下建立子目錄 name,內含 file 檔(內容 content)。
func mkTree(t *testing.T, base, name, file, content string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

// readFile 讀回 base/name/file 的內容(不存在回空字串 + ok=false)。
func readFile(t *testing.T, base, name, file string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base, name, file))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// hasOldResidue 檢查 root 下是否殘留 .gsm-old 換出暫存(applyRestoreSwap 負責清掉的部分;
// staging .gsm-restore 的生命週期由呼叫端 defer 管理,不在此函式職責內)。
func hasOldResidue(t *testing.T, root string) bool {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir %s: %v", root, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gsm-old-") {
			return true
		}
	}
	return false
}

// ---- B:Restore 回滾 ----

// applyRestoreSwap 成功後呼叫 rollback(模擬「建容器失敗」),資料應回到還原前、且無 .gsm 殘留。
func TestApplyRestoreSwap_RollbackRestoresOriginal(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".gsm-restore-x")
	// 現行(還原前)資料。
	mkTree(t, root, "data", "f.txt", "ORIGINAL")
	mkTree(t, root, "world", "w.txt", "ORIG-WORLD")
	// staging 內的還原資料。
	mkTree(t, staging, "data", "f.txt", "RESTORED")
	mkTree(t, staging, "world", "w.txt", "REST-WORLD")

	commit, rollback, err := applyRestoreSwap(root, staging, os.Rename)
	if err != nil {
		t.Fatalf("applyRestoreSwap: %v", err)
	}
	// 換入後應為還原內容。
	if got, _ := readFile(t, root, "data", "f.txt"); got != "RESTORED" {
		t.Fatalf("after swap data=%q want RESTORED", got)
	}
	// 模擬 Create 失敗 → 回滾。
	rollback()
	if got, ok := readFile(t, root, "data", "f.txt"); !ok || got != "ORIGINAL" {
		t.Fatalf("after rollback data=%q ok=%v want ORIGINAL", got, ok)
	}
	if got, ok := readFile(t, root, "world", "w.txt"); !ok || got != "ORIG-WORLD" {
		t.Fatalf("after rollback world=%q ok=%v want ORIG-WORLD", got, ok)
	}
	if hasOldResidue(t, root) {
		t.Fatal("rollback 後不應殘留 .gsm-old")
	}
	_ = commit // 回滾路徑不呼叫 commit
	_ = os.RemoveAll(staging)
}

// 多 data_dir 中途 rename 失敗 → 完整反向回滾至還原前,無殘留。
func TestApplyRestoreSwap_MidFailureFullRollback(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".gsm-restore-y")
	mkTree(t, root, "aaa", "f.txt", "ORIG-A")
	mkTree(t, root, "bbb", "f.txt", "ORIG-B")
	mkTree(t, staging, "aaa", "f.txt", "NEW-A")
	mkTree(t, staging, "bbb", "f.txt", "NEW-B")

	// 注入:第 3 次 rename 失敗(第 1、2 次為 aaa 的移開+換入,第 3 次為 bbb 的移開)。
	// os.ReadDir 依名稱排序,故 aaa 先於 bbb。
	var calls int
	failing := func(oldpath, newpath string) error {
		calls++
		if calls == 3 {
			return errors.New("injected rename failure")
		}
		return os.Rename(oldpath, newpath)
	}

	_, _, err := applyRestoreSwap(root, staging, failing)
	if err == nil {
		t.Fatal("expected applyRestoreSwap to fail on injected rename")
	}
	// 兩個目錄都應回到原內容。
	if got, ok := readFile(t, root, "aaa", "f.txt"); !ok || got != "ORIG-A" {
		t.Fatalf("aaa after rollback=%q ok=%v want ORIG-A", got, ok)
	}
	if got, ok := readFile(t, root, "bbb", "f.txt"); !ok || got != "ORIG-B" {
		t.Fatalf("bbb after rollback=%q ok=%v want ORIG-B", got, ok)
	}
	if hasOldResidue(t, root) {
		t.Fatal("中途失敗回滾後不應殘留 .gsm-old")
	}
}

// commit 於成功路徑永久刪除換出的舊資料,且不殘留 .gsm。
func TestApplyRestoreSwap_CommitDropsOldData(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".gsm-restore-z")
	mkTree(t, root, "data", "f.txt", "ORIGINAL")
	mkTree(t, staging, "data", "f.txt", "RESTORED")

	commit, _, err := applyRestoreSwap(root, staging, os.Rename)
	if err != nil {
		t.Fatalf("applyRestoreSwap: %v", err)
	}
	commit()
	if got, ok := readFile(t, root, "data", "f.txt"); !ok || got != "RESTORED" {
		t.Fatalf("after commit data=%q ok=%v want RESTORED", got, ok)
	}
	if hasOldResidue(t, root) {
		t.Fatal("commit 後不應殘留 .gsm-old")
	}
}

// ---- D:findBackup 不跨 uuid fallback ----

func TestFindBackup_NoCrossUUIDFallback(t *testing.T) {
	tmp := t.TempDir()
	b := &DockerBackend{backupRoot: tmp}
	writeRec := func(uuid string, bid protocol.BackupID) {
		dir := filepath.Join(b.backupInstanceRoot(uuid), string(bid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := writeBackupRecord(dir, backupRecord{Meta: protocol.BackupMeta{BackupID: bid, InstanceUUID: uuid}}); err != nil {
			t.Fatalf("writeBackupRecord: %v", err)
		}
	}
	writeRec("uuid-A", "bk-A")
	writeRec("uuid-B", "bk-B")

	// 對已知 uuid A 提交 B 的 BackupID → ErrNotFound(不 fallback 掃到 B)。
	if _, _, err := b.findBackup("uuid-A", "bk-B"); err != ErrNotFound {
		t.Fatalf("cross-uuid want ErrNotFound, got %v", err)
	}
	// 正確配對可找到。
	if _, rec, err := b.findBackup("uuid-B", "bk-B"); err != nil || rec.Meta.InstanceUUID != "uuid-B" {
		t.Fatalf("same-uuid find: err=%v rec=%+v", err, rec)
	}
	// uuid 為空(舊容器已不存在)才容許掃描全部 uuid。
	if _, rec, err := b.findBackup("", "bk-B"); err != nil || rec.Meta.InstanceUUID != "uuid-B" {
		t.Fatalf("empty-uuid scan: err=%v rec=%+v", err, rec)
	}
	// B 的備份記錄未被動到(findBackup 唯讀)。
	if rec, err := readBackupRecord(filepath.Join(b.backupInstanceRoot("uuid-B"), "bk-B")); err != nil || rec.Meta.InstanceUUID != "uuid-B" {
		t.Fatalf("B backup mutated: err=%v rec=%+v", err, rec)
	}
}

// ---- #3:mounts/ 具名掛載排除於備份範圍 ----

// tarInstanceData 應打包 data_dir 子目錄,但排除 instance.json、mounts/ 與 .gsm-* 暫存。
func TestTarInstanceData_ExcludesMountsNamespace(t *testing.T) {
	dataRoot := t.TempDir()
	b := &DockerBackend{dataRoot: dataRoot}
	uuid := "u-excl"
	root := b.instanceDataRoot(uuid)

	// data_dir 子目錄(應入備份)。
	mkTree(t, root, "data", "world.dat", "WORLD")
	// mounts/ 具名掛載(不應入備份)。
	mkTree(t, filepath.Join(root, mountsSubdir), "modpack", "pack.mrpack", "MODPACK-BYTES")
	// spec 快照與 .gsm 暫存(既有排除)。
	if err := os.WriteFile(filepath.Join(root, instanceSpecFile), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "data.tar")
	if _, err := b.tarInstanceData(uuid, tarPath); err != nil {
		t.Fatalf("tarInstanceData: %v", err)
	}

	names := tarEntryNames(t, tarPath)
	if !containsPrefix(names, "data/") {
		t.Errorf("備份應含 data/ 子目錄, 得 %v", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, mountsSubdir+"/") || n == mountsSubdir+"/" {
			t.Errorf("備份不應含 mounts/ 具名掛載, 得條目 %q (全部=%v)", n, names)
		}
		if n == instanceSpecFile {
			t.Errorf("備份不應含 instance.json, 得 %v", names)
		}
	}
}

// tarEntryNames 讀回 tar 內所有條目名。
func tarEntryNames(t *testing.T, tarPath string) []string {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tar: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var names []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	return names
}

func containsPrefix(ss []string, prefix string) bool {
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// ---- H:untar symlink Linkname 逃逸防護 ----

func writeTar(t *testing.T, path string, build func(*tar.Writer)) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	build(tw)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
}

func TestUntar_RejectsEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "b.tar")
	dest := filepath.Join(dir, "dest")
	writeTar(t, tarPath, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "../../../secret"})
	})
	if err := untar(tarPath, dest); err == nil {
		t.Fatal("untar 應拒絕 ../ 逃逸 symlink")
	}
}

func TestUntar_RejectsAbsoluteSymlink(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "b.tar")
	dest := filepath.Join(dir, "dest")
	outside, err := filepath.Abs(filepath.Join(dir, "outside-secret"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	writeTar(t, tarPath, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "abslink", Typeflag: tar.TypeSymlink, Linkname: outside})
	})
	if err := untar(tarPath, dest); err == nil {
		t.Fatal("untar 應拒絕逸出 dest 的絕對 symlink")
	}
}

func TestUntar_AllowsInDestSymlink(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "b.tar")
	dest := filepath.Join(dir, "dest")
	writeTar(t, tarPath, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "data", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
		_, _ = tw.Write([]byte("hey"))
		_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "data"})
	})
	// dest 內的 symlink 應被接受;symlink 建立本身為盡力而為(Windows 無權限時忽略),
	// 故 untar 回 nil。
	if err := untar(tarPath, dest); err != nil {
		t.Fatalf("dest 內 symlink 應被接受: %v", err)
	}
}
