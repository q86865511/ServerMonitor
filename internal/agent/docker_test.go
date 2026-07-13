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
		t.Fatalf("NewDockerBackend: %v", err)
	}
	// daemon 可達性檢查:不可達則跳過。
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

	// List 應含兩者且皆 managed。
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
	if !seenOld || !seenNew {
		t.Fatalf("List missing containers: old=%v new=%v", seenOld, seenNew)
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
