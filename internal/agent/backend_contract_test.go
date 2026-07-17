package agent

// backend 契約測試套件(native-backend R1):以同一組「介面行為不變量」跑多個 RuntimeBackend
// 實作,鎖定其可等價替換。本檔涵蓋 MockBackend 與 NativeBackend(假供應器 + go test 助手行程);
// DockerBackend 掛同套件於 backend_contract_docker_test.go(build tag docker,需 Docker daemon)。
//
// 契約項目:
//   - 生命週期狀態機:Create→created/not-running、Start→running、Stop→not-running。
//   - 未知 RuntimeID:Status/Stop/Inspect 回錯(ErrNotFound 語意)。
//   - Archive→Restore:回非空 BackupID/RuntimeID,且(實作 BackupLister 者)備份中繼可列舉、checksum 非空。
//   - Remove:清除後端對該實例的可見性(Status 回錯)。

import (
	"context"
	"testing"

	"servermonitor/internal/protocol"
)

// closeBackend 關閉受測後端(Close 非 RuntimeBackend 介面方法,以型別斷言呼叫)。
func closeBackend(b RuntimeBackend) {
	if c, ok := b.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

// backendContract 描述一個受測後端工廠及其 spec 產生器(各後端 Create 前置差異由此吸收)。
type backendContract struct {
	name       string
	newBackend func(t *testing.T) RuntimeBackend
	makeSpec   func(uuid string) protocol.InstanceSpec
	unknownID  protocol.RuntimeID
}

func TestBackendContract_Mock(t *testing.T) {
	runBackendContract(t, backendContract{
		name:       "mock",
		newBackend: func(t *testing.T) RuntimeBackend { return NewMockBackend() },
		makeSpec: func(uuid string) protocol.InstanceSpec {
			return protocol.InstanceSpec{UUID: uuid, Image: "img:latest", Labels: map[string]string{"gsm.uuid": uuid}}
		},
		unknownID: "mock-does-not-exist",
	})
}

func TestBackendContract_Native(t *testing.T) {
	runBackendContract(t, backendContract{
		name:       "native",
		newBackend: func(t *testing.T) RuntimeBackend { return newTestNativeBackend(t) },
		makeSpec:   nativeHelperSpec,
		unknownID:  nativeID("does-not-exist"),
	})
}

func runBackendContract(t *testing.T, c backendContract) {
	t.Run(c.name+"/lifecycle_states", func(t *testing.T) {
		ctx := context.Background()
		b := c.newBackend(t)
		defer closeBackend(b)

		id, err := b.Create(ctx, c.makeSpec("uuid-life"))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if id == "" {
			t.Fatal("Create 回空 RuntimeID")
		}
		st, err := b.Status(ctx, id)
		if err != nil {
			t.Fatalf("Status(created): %v", err)
		}
		if st.State != protocol.RuntimeStateCreated {
			t.Fatalf("建立後 state=%q want created", st.State)
		}
		if st.Running {
			t.Fatal("建立後不應 running")
		}

		if err := b.Start(ctx, id); err != nil {
			t.Fatalf("Start: %v", err)
		}
		st, err = b.Status(ctx, id)
		if err != nil {
			t.Fatalf("Status(running): %v", err)
		}
		if !st.Running {
			t.Fatalf("Start 後應 running,得 state=%q", st.State)
		}

		if err := b.Stop(ctx, id, StopOpts{Grace: 0}); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		st, err = b.Status(ctx, id)
		if err != nil {
			t.Fatalf("Status(stopped): %v", err)
		}
		if st.Running {
			t.Fatalf("Stop 後不應 running,得 state=%q", st.State)
		}
	})

	t.Run(c.name+"/unknown_id_errors", func(t *testing.T) {
		ctx := context.Background()
		b := c.newBackend(t)
		defer closeBackend(b)

		if _, err := b.Status(ctx, c.unknownID); err == nil {
			t.Error("未知 id 的 Status 應回錯")
		}
		if err := b.Stop(ctx, c.unknownID, StopOpts{}); err != ErrNotFound {
			t.Errorf("未知 id 的 Stop 應回 ErrNotFound,得 %v", err)
		}
		if _, err := b.Inspect(ctx, c.unknownID); err == nil {
			t.Error("未知 id 的 Inspect 應回錯")
		}
	})

	t.Run(c.name+"/archive_restore", func(t *testing.T) {
		ctx := context.Background()
		b := c.newBackend(t)
		defer closeBackend(b)

		id, err := b.Create(ctx, c.makeSpec("uuid-ar"))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		bid, err := b.Archive(ctx, id)
		if err != nil {
			t.Fatalf("Archive: %v", err)
		}
		if bid == "" {
			t.Fatal("Archive 回空 BackupID")
		}
		if bl, ok := b.(BackupLister); ok {
			metas, err := bl.ListBackups(ctx, "uuid-ar")
			if err != nil {
				t.Fatalf("ListBackups: %v", err)
			}
			found := false
			for _, m := range metas {
				if m.BackupID == bid {
					found = true
					if m.Checksum == "" {
						t.Error("備份中繼 checksum 不應為空")
					}
				}
			}
			if !found {
				t.Fatalf("ListBackups 未含剛封存的 %s", bid)
			}
		}
		newID, err := b.Restore(ctx, id, bid)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if newID == "" {
			t.Fatal("Restore 回空 RuntimeID")
		}
	})

	t.Run(c.name+"/remove_hides_instance", func(t *testing.T) {
		ctx := context.Background()
		b := c.newBackend(t)
		defer closeBackend(b)

		id, err := b.Create(ctx, c.makeSpec("uuid-rm"))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := b.Remove(ctx, id, RemoveOpts{}); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, err := b.Status(ctx, id); err == nil {
			t.Error("Remove 後 Status 應回錯(後端不再可見該實例)")
		}
	})
}
