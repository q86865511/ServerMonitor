package agent

// Job Objects 相關的跨平台單元測(T7):CPU 正規化語意、資源上限/記憶體欄位傳遞、Job 降級路徑。
// 需真 Windows 的強制/統計/OOM/續活測見 native_job_windows_test.go(build tag windows)。

import (
	"context"
	"errors"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

func TestCalcNativeCPUPercent(t *testing.T) {
	cases := []struct {
		name     string
		cpuDelta time.Duration
		wall     time.Duration
		numCPU   int
		want     float64
	}{
		// 4 核、牆鐘 1s:用滿 1 核(1s CPU)= 25%(占全機容量)。
		{"one-core-of-four", time.Second, time.Second, 4, 25},
		// 4 核、牆鐘 1s:用滿全部 4 核(4s CPU)= 100%。
		{"all-cores", 4 * time.Second, time.Second, 4, 100},
		// 單核、牆鐘 1s、0.5s CPU = 50%。
		{"half-single-core", 500 * time.Millisecond, time.Second, 1, 50},
		{"zero-wall", time.Second, 0, 4, 0},
		{"zero-numcpu", time.Second, time.Second, 0, 0},
		{"negative-delta", -time.Second, time.Second, 4, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := calcNativeCPUPercent(c.cpuDelta, c.wall, c.numCPU)
			if diff := got - c.want; diff > 0.001 || diff < -0.001 {
				t.Fatalf("calcNativeCPUPercent(%v,%v,%d)=%v want %v", c.cpuDelta, c.wall, c.numCPU, got, c.want)
			}
		})
	}
}

func TestResolveJobLimits(t *testing.T) {
	// 無 Resources:全零(不限額)。
	if lim := resolveJobLimits(protocol.InstanceSpec{}); lim.MemoryBytes != 0 || lim.CPUPercent != 0 {
		t.Fatalf("無 Resources 應全零,得 %+v", lim)
	}
	// 有 Resources:MB→bytes、CPU 直傳。
	spec := protocol.InstanceSpec{Resources: &protocol.ResourceLimits{MemoryMB: 512, CPUPercent: 40}}
	lim := resolveJobLimits(spec)
	if lim.MemoryBytes != 512*1024*1024 {
		t.Fatalf("MemoryBytes=%d want %d", lim.MemoryBytes, 512*1024*1024)
	}
	if lim.CPUPercent != 40 {
		t.Fatalf("CPUPercent=%d want 40", lim.CPUPercent)
	}
}

func TestResolveMemoryMB_PrefersResources(t *testing.T) {
	// Resources.MemoryMB 優先於 Env。
	spec := protocol.InstanceSpec{
		Resources: &protocol.ResourceLimits{MemoryMB: 3072},
		Env:       map[string]string{"MEMORY_MB": "1024"},
	}
	if got := resolveMemoryMB(spec); got != 3072 {
		t.Fatalf("resolveMemoryMB=%d want 3072(Resources 優先)", got)
	}
	// 無 Resources 時退回 Env。
	if got := resolveMemoryMB(protocol.InstanceSpec{Env: map[string]string{"MEMORY": "1500"}}); got != 1500 {
		t.Fatalf("resolveMemoryMB=%d want 1500(Env 退回)", got)
	}
	// 皆缺時採預設。
	if got := resolveMemoryMB(protocol.InstanceSpec{}); got != 2048 {
		t.Fatalf("resolveMemoryMB=%d want 2048(預設)", got)
	}
}

// TestNativeStart_JobDegradeOnFailure 驗:Job 建立失敗時 Start 仍成功(降級為僅監督),procHandle.job
// 為 nil,且實例日誌留下可辨識的降級訊息(design 風險節:納管失敗降級不阻斷啟動)。以 newJob 注入
// 失敗模擬(assign 失敗與 create 失敗同走此降級分支)。
func TestNativeStart_JobDegradeOnFailure(t *testing.T) {
	orig := newJob
	newJob = func(jobLimits) (*jobObject, error) { return nil, errors.New("模擬 Job 建立失敗") }
	defer func() { newJob = orig }()

	b := newTestNativeBackend(t)
	ctx := context.Background()
	spec := nativeHelperSpec("uuid-degrade")
	spec.Resources = &protocol.ResourceLimits{MemoryMB: 256, CPUPercent: 50}
	id, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start 應在 Job 降級下仍成功: %v", err)
	}
	defer func() { _ = b.Stop(ctx, id, StopOpts{}) }()

	b.mu.Lock()
	h := b.procs["uuid-degrade"]
	b.mu.Unlock()
	if h == nil {
		t.Fatal("procHandle 不存在")
	}
	if h.job != nil {
		t.Fatal("降級後 procHandle.job 應為 nil")
	}

	// 日誌應含一則 stream=="gsm" 的降級訊息。
	found := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !found {
		for _, ln := range readLogHistory(b.instanceDataRoot("uuid-degrade"), 0) {
			if ln.Stream == "gsm" {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("未在日誌找到降級訊息(stream=gsm)")
	}
}
