//go:build docker

// 真 Docker 整合測試(需 daemon;以 -tags docker 執行)。以輕量前景常駐映像
// nginx:alpine 覆蓋 create→start→stats→logs→exec→events(stop→die)→archive→restore
// 全鏈路。預設(無 tag)建置不含本檔。
//
// 註:InstanceSpec 無 command/entrypoint 覆寫欄位,busybox/alpine 的預設 CMD 不會長駐,
// 故採用預設即前景常駐的 nginx:alpine;實例資料由已實作的 ExecProcess 寫入。
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

const itestImage = "nginx:alpine"

func newITestBackend(t *testing.T) *DockerBackend {
	t.Helper()
	b, err := NewDockerBackend(DockerOptions{
		DataRoot:   filepath.Join(t.TempDir(), "data"),
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		Node:       "itest-node",
		Schema:     "1",
	})
	if err != nil {
		// NewDockerBackend 內建 ping;daemon 不可用時建構本身即失敗,同樣視為跳過而非測試失敗。
		t.Skipf("Docker daemon 不可用,跳過: %v", err)
	}
	// 額外以 List 驗證 API 可正常互動(ping 只保證連線,不保證後續呼叫皆正常)。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.List(ctx); err != nil {
		b.Close()
		t.Skipf("Docker daemon 不可用,跳過: %v", err)
	}
	return b
}

func itestSpec(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:       uuid,
		TemplateID: "itest-nginx",
		Image:      itestImage,
		DataDirs:   []string{"/data"},
		Labels:     map[string]string{},
	}
}

func recvEventKind(t *testing.T, s EventStream, want RuntimeEventKind, d time.Duration) RuntimeEvent {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("event stream closed before %s event", want)
			}
			if ev.Kind == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s event", want)
		}
	}
}

// 全鏈路整合測試。
// TestDockerBackend_MountUpload 端到端(真 Docker):建立帶具名 mount 的容器,經 WriteMountFile
// 上傳手動模組包檔,啟動後在容器內讀到該檔(bind mount 生效);且該檔排除於備份範圍(Archive
// 後 backup tar 不含 mounts/)。對映 R11 手動模組包檔傳輸 seam。
func TestDockerBackend_MountUpload(t *testing.T) {
	b := newITestBackend(t)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	uuid := "itest-mnt-" + shortRand()
	spec := itestSpec(uuid)
	spec.Mounts = []protocol.MountSpec{{Name: "modpack", ContainerPath: "/modpacks"}}

	id, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Remove(ctx, id, RemoveOpts{Purge: true})

	// 上傳手動模組包檔到 mount。
	const payload = "MRPACK-CONTENT-XYZ"
	if err := b.WriteMountFile(ctx, uuid, "modpack", "world.mrpack", strings.NewReader(payload)); err != nil {
		t.Fatalf("WriteMountFile: %v", err)
	}

	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 容器內應讀得到上傳的檔(bind mount 生效)。
	res, err := b.ExecProcess(ctx, id, ExecCmd{Cmd: []string{"cat", "/modpacks/world.mrpack"}})
	if err != nil {
		t.Fatalf("ExecProcess cat: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, payload) {
		t.Fatalf("容器內未讀到上傳檔: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}

	// 未宣告的 mount 上傳 → ErrNotFound。
	if err := b.WriteMountFile(ctx, uuid, "nope", "x.bin", strings.NewReader("x")); err != ErrNotFound {
		t.Fatalf("未宣告 mount 上傳 want ErrNotFound, got %v", err)
	}

	// mounts/ 排除於備份:Archive 後 backup tar 不含 mounts 條目。
	_ = b.Stop(ctx, id, StopOpts{Grace: 5 * time.Second})
	bid, err := b.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	bkpDir := filepath.Join(b.backupInstanceRoot(uuid), string(bid))
	names := tarEntryNames(t, filepath.Join(bkpDir, backupDataFile))
	for _, n := range names {
		if strings.HasPrefix(n, mountsSubdir+"/") {
			t.Errorf("備份不應含 mounts/ 條目, 得 %q (全部=%v)", n, names)
		}
	}
}

func TestDockerBackend_Integration(t *testing.T) {
	b := newITestBackend(t)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	uuid := "itest-" + shortRand()
	spec := itestSpec(uuid)

	// Create(拉映像 + 建容器)。
	oldID, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if oldID == "" {
		t.Fatal("Create returned empty RuntimeID")
	}
	defer b.Remove(ctx, oldID, RemoveOpts{Purge: true})

	// Create 後應為 created、未執行。
	if st, err := b.Status(ctx, oldID); err != nil {
		t.Fatalf("Status after Create: %v", err)
	} else if st.State != protocol.RuntimeStateCreated || st.Running {
		t.Fatalf("after Create want created/not-running, got %s running=%v", st.State, st.Running)
	}

	// Start → running。
	if err := b.Start(ctx, oldID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st, _ := b.Status(ctx, oldID); st.State != protocol.RuntimeStateRunning || !st.Running {
		t.Fatalf("after Start want running, got %s running=%v", st.State, st.Running)
	}

	// ExecProcess:寫入實例資料(供 archive/restore 驗證)。
	res, err := b.ExecProcess(ctx, oldID, ExecCmd{Cmd: []string{"sh", "-c", "echo hello-restore > /data/save.txt"}})
	if err != nil {
		t.Fatalf("ExecProcess write: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExecProcess exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Stats:記憶體 > 0、CPU >= 0、資料磁碟 > 0。
	stats, err := b.Stats(ctx, oldID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.MemoryBytes == 0 {
		t.Fatalf("Stats MemoryBytes should be > 0, got %+v", stats)
	}
	if stats.CPUPercent < 0 {
		t.Fatalf("Stats CPUPercent should be >= 0, got %v", stats.CPUPercent)
	}
	if stats.DataDiskBytes == nil || *stats.DataDiskBytes == 0 {
		t.Fatalf("Stats DataDiskBytes should be > 0 after write, got %v", stats.DataDiskBytes)
	}
	t.Logf("stats: cpu=%.3f%% mem=%d limit=%d disk=%d", stats.CPUPercent, stats.MemoryBytes, stats.MemoryLimit, *stats.DataDiskBytes)

	// Logs:讀到啟動輸出至少一行。
	ls, err := b.Logs(ctx, oldID, LogOpts{Tail: 200})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	var logCount int
	logDeadline := time.After(15 * time.Second)
logLoop:
	for {
		select {
		case ln, ok := <-ls.Lines():
			if !ok {
				break logLoop
			}
			logCount++
			_ = ln
		case <-logDeadline:
			break logLoop
		}
	}
	ls.Close()
	if logCount == 0 {
		t.Fatal("Logs produced no lines")
	}
	t.Logf("logs: %d lines", logCount)

	// Events:訂閱後 Stop → 收到 die 事件。
	es, err := b.Events(ctx, "")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer es.Close()

	if err := b.Stop(ctx, oldID, StopOpts{Grace: 5 * time.Second}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	die := recvEventKind(t, es, RuntimeEventDie, 20*time.Second)
	if die.ID != oldID {
		t.Fatalf("die event id want %s, got %s", oldID, die.ID)
	}
	dieExit := -1
	if die.ExitCode != nil {
		dieExit = *die.ExitCode
	}
	t.Logf("die event: cursor=%s exit=%d", die.Cursor, dieExit)

	// Stop 後應為 exited、有退出碼。
	if st, _ := b.Status(ctx, oldID); st.State != protocol.RuntimeStateExited {
		t.Fatalf("after Stop want exited, got %s", st.State)
	} else if st.ExitCode == nil {
		t.Fatal("after Stop ExitCode should be set")
	}

	// Archive(容器已停)→ opaque BackupID。
	bid, err := b.Archive(ctx, oldID)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if bid == "" {
		t.Fatal("Archive returned empty BackupID")
	}
	t.Logf("backup id: %s", bid)

	// 先破壞現行資料,以確認 Restore 真的還原。
	livePath := filepath.Join(b.instanceDataRoot(uuid), "data", "save.txt")
	if err := os.WriteFile(livePath, []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatalf("corrupt live data: %v", err)
	}

	// Restore → 新 RuntimeID(!= 舊)。
	newID, err := b.Restore(ctx, oldID, bid)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if newID == "" || newID == oldID {
		t.Fatalf("Restore want new RuntimeID != %s, got %q", oldID, newID)
	}
	defer b.Remove(ctx, newID, RemoveOpts{Purge: true})
	t.Logf("restored runtime id: %s", newID)

	// 還原後 host 端資料應回到 archive 時的內容。
	restored, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatalf("read restored data: %v", err)
	}
	if strings.TrimSpace(string(restored)) != "hello-restore" {
		t.Fatalf("restored content mismatch: got %q", string(restored))
	}

	// 新容器應為 created、承載同 uuid。
	info, err := b.Inspect(ctx, newID)
	if err != nil {
		t.Fatalf("Inspect restored: %v", err)
	}
	if info.State != protocol.RuntimeStateCreated {
		t.Fatalf("restored container want created, got %s", info.State)
	}
	if info.Labels[labelUUID] != uuid {
		t.Fatalf("restored container uuid label mismatch: %v", info.Labels)
	}

	// List 應只含新容器;舊容器已於 Restore 內被移除(B9:見 docker_backup.go 還原後清理舊容器的
	// 註解——其 bind 資料已於 swap 換出,是帶相同 gsm.uuid 的無用停止殼,不清除會每次還原累積孤兒)。
	refs, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seenOld, seenNew := false, false
	for _, r := range refs {
		if r.Labels[labelManagedBy] != managedByValue {
			t.Fatalf("List returned non-managed container: %+v", r)
		}
		if r.ID == oldID {
			seenOld = true
		}
		if r.ID == newID {
			seenNew = true
		}
	}
	if seenOld {
		t.Fatalf("List should not contain removed old container: old=%v", oldID)
	}
	if !seenNew {
		t.Fatalf("List missing new container: new=%v", newID)
	}
}

// TestDockerBackend_BackupRetentionRestore 端到端(真 Docker):backup→retention(刪最舊)→restore
// 迴圈一輪。驗 DeleteBackup 真的移除 agent 端備份目錄(保留策略的能力鏈),且刪除後仍可用剩下的
// 備份還原。對映 R9 保留策略 + 原子還原。
func TestDockerBackend_BackupRetentionRestore(t *testing.T) {
	b := newITestBackend(t)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	uuid := "itest-ret-" + shortRand()
	spec := itestSpec(uuid)
	id, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer b.Remove(ctx, id, RemoveOpts{Purge: true})
	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 寫入可辨識資料,停機取一致快照(第一份備份)。
	if res, err := b.ExecProcess(ctx, id, ExecCmd{Cmd: []string{"sh", "-c", "echo v1 > /data/save.txt"}}); err != nil || res.ExitCode != 0 {
		t.Fatalf("ExecProcess v1: err=%v res=%+v", err, res)
	}
	if err := b.Stop(ctx, id, StopOpts{Grace: 5 * time.Second}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	bid1, err := b.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive #1: %v", err)
	}

	// 第二份備份(改資料後再存)。
	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start2: %v", err)
	}
	if res, err := b.ExecProcess(ctx, id, ExecCmd{Cmd: []string{"sh", "-c", "echo v2 > /data/save.txt"}}); err != nil || res.ExitCode != 0 {
		t.Fatalf("ExecProcess v2: err=%v res=%+v", err, res)
	}
	if err := b.Stop(ctx, id, StopOpts{Grace: 5 * time.Second}); err != nil {
		t.Fatalf("Stop2: %v", err)
	}
	bid2, err := b.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive #2: %v", err)
	}

	if metas, err := b.ListBackups(ctx, uuid); err != nil || len(metas) != 2 {
		t.Fatalf("Archive 後應有 2 份, err=%v metas=%d", err, len(metas))
	}

	// 保留策略能力:刪最舊(bid1)→ 目錄真的消失、清單剩 bid2。
	if err := b.DeleteBackup(ctx, uuid, bid1); err != nil {
		t.Fatalf("DeleteBackup bid1: %v", err)
	}
	bkp1Dir := filepath.Join(b.backupInstanceRoot(uuid), string(bid1))
	if _, err := os.Stat(bkp1Dir); !os.IsNotExist(err) {
		t.Fatalf("刪除後備份目錄應消失, stat err=%v", err)
	}
	metas, err := b.ListBackups(ctx, uuid)
	if err != nil || len(metas) != 1 || metas[0].BackupID != bid2 {
		t.Fatalf("刪最舊後應剩 bid2, err=%v metas=%+v", err, metas)
	}

	// 以剩下的 bid2 還原 → 新 runtime ID,資料為 v2。
	newID, err := b.Restore(ctx, id, bid2)
	if err != nil {
		t.Fatalf("Restore bid2: %v", err)
	}
	defer b.Remove(ctx, newID, RemoveOpts{Purge: true})
	livePath := filepath.Join(b.instanceDataRoot(uuid), "data", "save.txt")
	restored, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if strings.TrimSpace(string(restored)) != "v2" {
		t.Fatalf("還原內容應為 v2, 得 %q", string(restored))
	}
}

// Status/Inspect 對不存在的容器回 ErrNotFound(驗 mapDockerErr)。
func TestDockerBackend_NotFound(t *testing.T) {
	b := newITestBackend(t)
	defer b.Close()
	ctx := context.Background()

	if _, err := b.Status(ctx, "no-such-container-xyz"); err != ErrNotFound {
		t.Fatalf("Status missing want ErrNotFound, got %v", err)
	}
	if _, err := b.Inspect(ctx, "no-such-container-xyz"); err != ErrNotFound {
		t.Fatalf("Inspect missing want ErrNotFound, got %v", err)
	}
}
