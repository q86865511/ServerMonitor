//go:build docker

// docker↔native 備份互轉整合測(native-backend R10:「同一實例資料在 docker/native 後端間可透過
// 備份還原互轉」)。需 Docker daemon,以 -tags docker 執行;daemon 不可用時自動 Skip。
//
// 互通的根據是兩後端共用同一套備份產物(backup_archive.go 的 tarDir/untar/backupRecord)與同一
// 備份根佈局(<BackupRoot>/<uuid>/<bid>/{data.tar, meta.json})。本測試以「共用 BackupRoot、各自
// DataRoot」建模使用者切換某實例 runtime 後以備份還原資料的情境,雙向各驗一次:
//   - docker 封存 → native 還原:native Restore 純檔案操作(解 tar + 原子切換 data_dir),無需容器。
//   - native 封存 → docker 還原:docker Restore 依備份 spec 重建真容器(spec 帶 Image),資料落位。
package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// newInteropNativeBackend 建一個 NativeBackend,共用給定 BackupRoot(與 docker 後端互通),
// 供應器為 no-op 假件(互轉測試不觸網、不啟動行程)。
func newInteropNativeBackend(t *testing.T, dataRoot, backupRoot string) *NativeBackend {
	t.Helper()
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   dataRoot,
		BackupRoot: backupRoot,
		CacheRoot:  filepath.Join(t.TempDir(), "cache"),
		Node:       "interop-native",
		Prov:       &fakeProv{},
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestBackupInterop_DockerNative 雙向互轉:docker→native 與 native→docker 各驗資料經備份還原無損。
func TestBackupInterop_DockerNative(t *testing.T) {
	sharedBackupRoot := filepath.Join(t.TempDir(), "backups") // 兩後端共用備份根(互通關鍵)

	dockerBackend := newITestBackend(t) // daemon 不可用即 Skip
	// 讓 docker 後端改用共用備份根:newITestBackend 用獨立 TempDir,故重建一個指定 BackupRoot 的。
	d, err := NewDockerBackend(DockerOptions{
		DataRoot:   filepath.Join(t.TempDir(), "docker-data"),
		BackupRoot: sharedBackupRoot,
		Node:       "interop-docker",
		Schema:     "1",
	})
	if err != nil {
		t.Fatalf("NewDockerBackend(shared backup): %v", err)
	}
	defer d.Close()
	_ = dockerBackend.Close() // 僅用其 Skip 檢查,實際用共用備份根的 d

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// ---- 方向 A:docker 封存 → native 還原 ----
	nativeData := filepath.Join(t.TempDir(), "native-data-A")
	nA := newInteropNativeBackend(t, nativeData, sharedBackupRoot)

	uuidA := "interop-d2n-" + shortRand()
	specA := protocol.InstanceSpec{
		UUID:       uuidA,
		TemplateID: "interop-nginx",
		Image:      itestImage,
		DataDirs:   []string{"/data"},
		Labels:     map[string]string{},
	}
	idA, err := d.Create(ctx, specA)
	if err != nil {
		t.Fatalf("A docker Create: %v", err)
	}
	defer d.Remove(ctx, idA, RemoveOpts{Purge: true})
	if err := d.Start(ctx, idA); err != nil {
		t.Fatalf("A docker Start: %v", err)
	}
	const payloadA = "docker-to-native-payload"
	if res, err := d.ExecProcess(ctx, idA, ExecCmd{Cmd: []string{"sh", "-c", "echo " + payloadA + " > /data/save.txt"}}); err != nil || res.ExitCode != 0 {
		t.Fatalf("A write: err=%v res=%+v", err, res)
	}
	if err := d.Stop(ctx, idA, StopOpts{Grace: 5 * time.Second}); err != nil {
		t.Fatalf("A docker Stop: %v", err)
	}
	bidA, err := d.Archive(ctx, idA)
	if err != nil {
		t.Fatalf("A docker Archive: %v", err)
	}
	// native 還原同一備份(純檔案操作,無需容器)。
	if _, err := nA.Restore(ctx, nativeID(uuidA), bidA); err != nil {
		t.Fatalf("A native Restore(docker 備份): %v", err)
	}
	gotA, err := os.ReadFile(filepath.Join(nativeData, uuidA, "data", "save.txt"))
	if err != nil {
		t.Fatalf("A 還原後讀 native 資料: %v", err)
	}
	if strings.TrimSpace(string(gotA)) != payloadA {
		t.Fatalf("A docker→native 資料不符: 得 %q 期望 %q", strings.TrimSpace(string(gotA)), payloadA)
	}
	t.Logf("方向 A(docker→native)還原成功: %q", strings.TrimSpace(string(gotA)))

	// ---- 方向 B:native 封存 → docker 還原 ----
	nativeDataB := filepath.Join(t.TempDir(), "native-data-B")
	nB := newInteropNativeBackend(t, nativeDataB, sharedBackupRoot)

	uuidB := "interop-n2d-" + shortRand()
	specB := protocol.InstanceSpec{
		UUID:       uuidB,
		TemplateID: "interop-nginx",
		Image:      itestImage, // 帶 docker 映像:docker Restore 據備份 spec 重建容器需之(雙能力範本語意)
		DataDirs:   []string{"/data"},
		Labels:     map[string]string{},
		Runtime:    "native",
		Native: &protocol.NativeSpecPayload{
			Provision: protocol.NativeProvision{Kind: ""}, // 免供應(不觸網)
			Launch:    protocol.NativeLaunch{Command: []string{"noop"}},
		},
	}
	if _, err := nB.Create(ctx, specB); err != nil {
		t.Fatalf("B native Create: %v", err)
	}
	const payloadB = "native-to-docker-payload"
	nativeDataFile := filepath.Join(nativeDataB, uuidB, "data", "save.txt")
	if err := os.WriteFile(nativeDataFile, []byte(payloadB+"\n"), 0o644); err != nil {
		t.Fatalf("B 寫 native 資料: %v", err)
	}
	bidB, err := nB.Archive(ctx, nativeID(uuidB))
	if err != nil {
		t.Fatalf("B native Archive: %v", err)
	}
	// docker 還原同一備份:依備份 spec 重建真容器,資料落位於 docker DataRoot。
	dockerDataB := filepath.Join(t.TempDir(), "docker-data-B")
	dB, err := NewDockerBackend(DockerOptions{
		DataRoot:   dockerDataB,
		BackupRoot: sharedBackupRoot,
		Node:       "interop-docker-B",
		Schema:     "1",
	})
	if err != nil {
		t.Fatalf("B NewDockerBackend: %v", err)
	}
	defer dB.Close()
	// id 傳空:容器尚不存在,findBackup 掃描共用備份根定位 bidB(其 spec 帶 uuidB)。
	newID, err := dB.Restore(ctx, "", bidB)
	if err != nil {
		t.Fatalf("B docker Restore(native 備份): %v", err)
	}
	defer dB.Remove(ctx, newID, RemoveOpts{Purge: true})
	gotB, err := os.ReadFile(filepath.Join(dockerDataB, uuidB, "data", "save.txt"))
	if err != nil {
		t.Fatalf("B 還原後讀 docker 資料: %v", err)
	}
	if strings.TrimSpace(string(gotB)) != payloadB {
		t.Fatalf("B native→docker 資料不符: 得 %q 期望 %q", strings.TrimSpace(string(gotB)), payloadB)
	}
	t.Logf("方向 B(native→docker)還原成功: %q (新容器 %s)", strings.TrimSpace(string(gotB)), newID)
}
