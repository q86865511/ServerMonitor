package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"servermonitor/internal/protocol"
)

// ---- NativeBackend.DeleteBackup(B8;免 Docker，只驗檔案系統邏輯與路徑防護)----

// NativeBackend.DeleteBackup 移除備份目錄；不存在 → ErrNotFound。
func TestNativeBackend_DeleteBackup_RemovesDir(t *testing.T) {
	tmp := t.TempDir()
	b := &NativeBackend{backupRoot: tmp}
	uuid := "u-ndel"
	bid := protocol.BackupID("bk-1")
	dir := filepath.Join(b.backupInstanceRoot(uuid), string(bid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.tar"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := b.DeleteBackup(context.Background(), uuid, bid); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("備份目錄應已刪除, stat err=%v", err)
	}
	// 不存在 → ErrNotFound。
	if err := b.DeleteBackup(context.Background(), uuid, bid); err != ErrNotFound {
		t.Fatalf("missing delete want ErrNotFound, got %v", err)
	}
}

// NativeBackend.DeleteBackup 對路徑遍歷 backupID → ErrInvalidBackupID，且不刪備份根外目錄。
func TestNativeBackend_DeleteBackup_RejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	b := &NativeBackend{backupRoot: filepath.Join(tmp, "backups")}
	uuid := "u-nesc"
	secret := filepath.Join(tmp, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatalf("mkdir secret: %v", err)
	}

	for _, bid := range []protocol.BackupID{"..\\..\\secret", "../../secret", "..", ".hidden", "a/b", ""} {
		if err := b.DeleteBackup(context.Background(), uuid, bid); err != ErrInvalidBackupID {
			t.Fatalf("DeleteBackup(%q) want ErrInvalidBackupID, got %v", bid, err)
		}
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("備份根外目錄不應被動到, stat err=%v", err)
	}
}

// ---- 每實例磁碟用量(dirSizeBytes / InstanceDiskUsage;免 Docker）----

// InstanceDiskUsage 遞迴加總資料根與備份根的檔案位元組（含子目錄）；目錄不存在計 0。
func TestNativeBackend_InstanceDiskUsage_ComputesSize(t *testing.T) {
	tmp := t.TempDir()
	b := &NativeBackend{dataRoot: filepath.Join(tmp, "data"), backupRoot: filepath.Join(tmp, "backups")}
	uuid := "u-usage"

	// 資料根：頂層 10 bytes + 子目錄 20 bytes = 30。
	dataRoot := b.instanceDataRoot(uuid)
	if err := os.MkdirAll(filepath.Join(dataRoot, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "a.txt"), make([]byte, 10), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "sub", "b.txt"), make([]byte, 20), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	// 備份根：單檔 100 bytes。
	backupRoot := b.backupInstanceRoot(uuid)
	if err := os.MkdirAll(filepath.Join(backupRoot, "bk1"), 0o755); err != nil {
		t.Fatalf("mkdir backup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupRoot, "bk1", "data.tar"), make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	du, err := b.InstanceDiskUsage(context.Background(), uuid)
	if err != nil {
		t.Fatalf("InstanceDiskUsage: %v", err)
	}
	if du.DataBytes != 30 {
		t.Fatalf("DataBytes want 30, got %d", du.DataBytes)
	}
	if du.BackupBytes != 100 {
		t.Fatalf("BackupBytes want 100, got %d", du.BackupBytes)
	}

	// 未知 uuid（目錄不存在）→ 0，不報錯。
	du2, err := b.InstanceDiskUsage(context.Background(), "no-such")
	if err != nil {
		t.Fatalf("InstanceDiskUsage(missing): %v", err)
	}
	if du2.DataBytes != 0 || du2.BackupBytes != 0 {
		t.Fatalf("missing uuid want 0/0, got %d/%d", du2.DataBytes, du2.BackupBytes)
	}
}

// ---- dispatchBackend：映像/容器管理路由與 native-only 不支援 ----

// fakeResourceBackend 是實作 ImageManager/ContainerManager/instanceDiskUsager 的 docker 替身。
type fakeResourceBackend struct {
	*fakeBackend
	images     []protocol.ImageSummary
	containers []protocol.ContainerSummary
	pruneRes   protocol.PruneImagesResult
	diskUsage  protocol.InstanceDiskUsage
	removedImg string
	removedCtr string
}

func (f *fakeResourceBackend) ListImages(context.Context) ([]protocol.ImageSummary, error) {
	return f.images, nil
}
func (f *fakeResourceBackend) RemoveImage(_ context.Context, id string, _ bool) error {
	f.removedImg = id
	return nil
}
func (f *fakeResourceBackend) PruneImages(context.Context) (protocol.PruneImagesResult, error) {
	return f.pruneRes, nil
}
func (f *fakeResourceBackend) ListContainers(context.Context) ([]protocol.ContainerSummary, error) {
	return f.containers, nil
}
func (f *fakeResourceBackend) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.removedCtr = id
	return nil
}
func (f *fakeResourceBackend) InstanceDiskUsage(context.Context, string) (protocol.InstanceDiskUsage, error) {
	return f.diskUsage, nil
}

// native-only 節點（docker=nil）：映像/容器管理一律回 ErrUnsupported。
func TestDispatch_ResourceManagement_UnsupportedNativeOnly(t *testing.T) {
	native := newFakeBackend("native") // 不實作 ImageManager/ContainerManager
	d, err := NewDispatchBackend(native, nil)
	if err != nil {
		t.Fatalf("NewDispatchBackend: %v", err)
	}
	defer d.Close()
	ctx := context.Background()

	if _, err := d.ListImages(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ListImages native-only want ErrUnsupported, got %v", err)
	}
	if _, err := d.ListContainers(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ListContainers native-only want ErrUnsupported, got %v", err)
	}
	if err := d.RemoveImage(ctx, "sha256:x", false); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("RemoveImage native-only want ErrUnsupported, got %v", err)
	}
	if err := d.RemoveContainer(ctx, "c1", true); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("RemoveContainer native-only want ErrUnsupported, got %v", err)
	}
	if _, err := d.PruneImages(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("PruneImages native-only want ErrUnsupported, got %v", err)
	}
}

// docker 子後端存在時：映像/容器管理路由至 docker；磁碟用量走首個實作者。
func TestDispatch_ResourceManagement_RoutesToDocker(t *testing.T) {
	native := newFakeBackend("native")
	docker := &fakeResourceBackend{
		fakeBackend: newFakeBackend("docker"),
		images:      []protocol.ImageSummary{{ID: "sha256:img1"}},
		containers:  []protocol.ContainerSummary{{ID: "ctr1"}},
		pruneRes:    protocol.PruneImagesResult{ReclaimedBytes: 42, Deleted: []string{"sha256:d"}},
		diskUsage:   protocol.InstanceDiskUsage{DataBytes: 7, BackupBytes: 9},
	}
	d, err := NewDispatchBackend(native, docker)
	if err != nil {
		t.Fatalf("NewDispatchBackend: %v", err)
	}
	defer d.Close()
	ctx := context.Background()

	imgs, err := d.ListImages(ctx)
	if err != nil || len(imgs) != 1 || imgs[0].ID != "sha256:img1" {
		t.Fatalf("ListImages route want docker images, got %v err=%v", imgs, err)
	}
	if err := d.RemoveImage(ctx, "sha256:img1", true); err != nil || docker.removedImg != "sha256:img1" {
		t.Fatalf("RemoveImage route: removedImg=%q err=%v", docker.removedImg, err)
	}
	pr, err := d.PruneImages(ctx)
	if err != nil || pr.ReclaimedBytes != 42 {
		t.Fatalf("PruneImages route: %+v err=%v", pr, err)
	}
	cs, err := d.ListContainers(ctx)
	if err != nil || len(cs) != 1 || cs[0].ID != "ctr1" {
		t.Fatalf("ListContainers route want docker containers, got %v err=%v", cs, err)
	}
	if err := d.RemoveContainer(ctx, "ctr1", false); err != nil || docker.removedCtr != "ctr1" {
		t.Fatalf("RemoveContainer route: removedCtr=%q err=%v", docker.removedCtr, err)
	}
	// 磁碟用量走首個實作者（native 不實作 → docker）。
	du, err := d.InstanceDiskUsage(ctx, "u-1")
	if err != nil || du.DataBytes != 7 || du.BackupBytes != 9 {
		t.Fatalf("InstanceDiskUsage route: %+v err=%v", du, err)
	}
}
