//go:build windows

package agent

// agent 重啟收養活行程的真機整合測(native-backend T11 / R7/R9/R13):起真行程 → 銷毀 NativeBackend
// (模擬 agent 退出,不殺行程)→ 新建 NativeBackend 收養 → 斷言 Status running、Job 重掛或降級、
// 可 Stop。死行程路徑由跨平台的 TestNativeAdopt_DeadProcessSynthesizesDie 涵蓋(Windows 亦執行)。
//
// 執行:go test -run 'TestNativeAdopt' ./internal/agent/(即為 Windows)。

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNativeAdopt_AliveProcessAdopted(t *testing.T) {
	if testing.Short() {
		t.Skip("真機收養整合測:-short 跳過")
	}
	root := t.TempDir()
	prov := &fakeProv{javaPath: os.Args[0], serverJar: "server.jar"}
	ctx := context.Background()

	// 舊 agent:建立並啟動真行程(助手行程常駐)。
	b1 := newNativeBackendAt(t, root, prov)
	id, err := b1.Create(ctx, nativeHelperSpec("uuid-adopt"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b1.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	b1.mu.Lock()
	h1 := b1.procs["uuid-adopt"]
	b1.mu.Unlock()
	if h1 == nil {
		t.Fatal("啟動後 procHandle 不存在")
	}
	pid := h1.pid

	// 模擬舊 agent 行程結束:釋放其 Job 控制代碼(未設 KILL_ON_JOB_CLOSE,行程續活;Job 物件於最後
	// 一個控制代碼關閉後銷毀,行程即不再屬任何 Job,供收養端乾淨重掛),再關閉 backend(不殺行程,R9)。
	if h1.job != nil {
		_ = h1.job.close()
	}
	_ = b1.Close()

	if alive, _ := adoptedProcessStatus(pid); !alive {
		t.Fatal("agent 退出後行程應續活(R9:不連坐殺伺服器)")
	}

	// 新 agent:建構即收養掃描。
	b2 := newNativeBackendAt(t, root, prov)
	t.Cleanup(func() { _ = b2.Close() })

	st, err := b2.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running {
		t.Fatalf("收養後應 running, 得 %+v", st)
	}
	b2.mu.Lock()
	h2 := b2.procs["uuid-adopt"]
	b2.mu.Unlock()
	if h2 == nil {
		t.Fatal("收養未登記 procHandle")
	}
	if h2.pid != pid {
		t.Fatalf("收養 pid=%d want %d", h2.pid, pid)
	}

	// Job 重掛或降級皆為合格收養結果(design 風險節:行程已在他人 Job 內可能 Assign 失敗→降級)。
	if h2.job != nil {
		t.Logf("收養後 Job 重掛成功")
	} else {
		degraded := false
		for _, ln := range readLogHistory(b2.instanceDataRoot("uuid-adopt"), 0) {
			if ln.Stream == "gsm" {
				degraded = true
				break
			}
		}
		if !degraded {
			t.Fatal("收養未重掛 Job 亦無降級日誌")
		}
		t.Logf("收養後 Job 重掛失敗,降級為僅監督(合格)")
	}

	// 收養行程應可經新 backend 停止。
	if err := b2.Stop(ctx, id, StopOpts{Grace: 150 * time.Millisecond}); err != nil {
		t.Fatalf("Stop 收養行程: %v", err)
	}
	if st2, _ := b2.Status(ctx, id); st2.Running {
		t.Fatal("Stop 後不應 running")
	}
	// 給 OS 一小段收束時間後確認行程確已終止。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := adoptedProcessStatus(pid); !alive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Stop 後行程仍存活")
}
