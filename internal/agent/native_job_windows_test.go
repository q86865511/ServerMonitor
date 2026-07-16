//go:build windows

package agent

// Windows Job Objects 真機整合測(native-backend T7 / R9):記憶體超限終止+oom 事件、CPU rate cap
// 實測、agent 退出(關 Job handle)伺服器續活(KILL_ON_JOB_CLOSE 未設)。本檔需真 Windows 執行,
// 以助手行程(env 觸發)提供記憶體膨脹/CPU 忙碌/常駐三種負載,語意對齊 native_test.go 的助手模式。
//
// 執行:go test -run 'TestNativeJob' ./internal/agent/(即為 Windows,故不另設 build tag native)。

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"servermonitor/internal/protocol"
)

// ---- 助手行程:三種負載,僅在對應 env 設定時進入,否則為 no-op(當一般測試通過)。----

// TestNativeMemHogProcess 持續配置並觸碰記憶體,直到撞上 Job 記憶體上限被 runtime 判 OOM 而退出。
func TestNativeMemHogProcess(t *testing.T) {
	if os.Getenv("GSM_NATIVE_MEMHOG") != "1" {
		return
	}
	var hold [][]byte
	deadline := time.Now().Add(30 * time.Second) // 保護:上限異常未生效時不無限膨脹
	for time.Now().Before(deadline) {
		chunk := make([]byte, 16*1024*1024)
		for i := 0; i < len(chunk); i += 4096 { // 逐頁觸碰以強制提交(commit)
			chunk[i] = byte(i)
		}
		hold = append(hold, chunk)
		runtime.KeepAlive(hold)
	}
	os.Exit(0)
}

var cpuSink uint64

// TestNativeCPUHogProcess 以 NumCPU 條 goroutine 全力燒 CPU(無上限時可吃滿全部核心)。
func TestNativeCPUHogProcess(t *testing.T) {
	if os.Getenv("GSM_NATIVE_CPUHOG") != "1" {
		return
	}
	for i := 0; i < runtime.NumCPU(); i++ {
		go func() {
			var x uint64
			for {
				x = x*1664525 + 1013904223
				atomic.StoreUint64(&cpuSink, x) // 具副作用,避免迴圈被最佳化消除
			}
		}()
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	os.Exit(0)
}

// nativeLoadSpec 產生一個啟動指定助手負載的 native InstanceSpec(帶資源上限)。
func nativeLoadSpec(uuid, runName string, env map[string]string, res *protocol.ResourceLimits) protocol.InstanceSpec {
	e := map[string]string{}
	for k, v := range env {
		e[k] = v
	}
	return protocol.InstanceSpec{
		UUID:      uuid,
		Runtime:   "native",
		Variant:   "vanilla",
		DataDirs:  []string{"/data"},
		Env:       e,
		Labels:    map[string]string{"gsm.uuid": uuid},
		Resources: res,
		Native: &protocol.NativeSpecPayload{
			Provision: protocol.NativeProvision{Kind: "java", JavaMajor: 21, Loader: "vanilla"},
			Launch:    protocol.NativeLaunch{Command: []string{"{java}", "-test.run=" + runName}},
		},
	}
}

// collectUntilDie 讀事件串流,收集本 id 的事件 kind,直到收到 die 或逾時。
func collectUntilDie(t *testing.T, es EventStream, id protocol.RuntimeID, timeout time.Duration) map[protocol.RuntimeEventKind]bool {
	t.Helper()
	kinds := map[protocol.RuntimeEventKind]bool{}
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-es.Events():
			if !ok {
				return kinds
			}
			if ev.ID != id {
				continue
			}
			kinds[ev.Kind] = true
			if ev.Kind == RuntimeEventDie {
				return kinds
			}
		case <-deadline:
			return kinds
		}
	}
}

// TestNativeJob_OOMTerminatesAndEmitsEvent:記憶體超限 → 行程被終止 → 合成 oom 事件(R9 驗收)。
func TestNativeJob_OOMTerminatesAndEmitsEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("真機記憶體超限測:-short 跳過")
	}
	b := newTestNativeBackend(t)
	ctx := context.Background()
	spec := nativeLoadSpec("uuid-oom", "TestNativeMemHogProcess",
		map[string]string{"GSM_NATIVE_MEMHOG": "1"},
		&protocol.ResourceLimits{MemoryMB: 256})
	id, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	es, err := b.Events(ctx, "")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer es.Close()

	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	kinds := collectUntilDie(t, es, id, 25*time.Second)
	if !kinds[RuntimeEventDie] {
		t.Fatal("未在時限內收到 die(記憶體上限未終止行程?)")
	}
	if !kinds[RuntimeEventOOM] {
		t.Fatal("記憶體超限退出未合成 oom 事件(告警無法標『超出記憶體上限』)")
	}
	if st, _ := b.Status(ctx, id); st.Running {
		t.Fatal("OOM 後不應仍 running")
	}
}

// TestNativeJob_CPURateCapEnforced:CPU hard cap 生效,實測占用不超過設定值+容忍誤差(R9 驗收)。
//
// 量測方法:hard cap 以排程averaging interval近似強制,故短窗(如 Stats 的 200ms)可見瞬時越界。
// R9「不超過設定值」指的是持續占用,須以較長窗量測方穩定收斂。此處直接取行程樹 accounting 於一個
// 2s 窗的 CPU 累計差(與 Stats 同一 calcNativeCPUPercent 正規化,只是窗更長),排除短窗噪音。
// 容忍 +15(占全機核心的百分比),留機器負載餘裕。
func TestNativeJob_CPURateCapEnforced(t *testing.T) {
	if testing.Short() {
		t.Skip("真機 CPU cap 測:-short 跳過")
	}
	const capPercent = 25
	const tolerance = 15.0
	const window = 2 * time.Second

	b := newTestNativeBackend(t)
	ctx := context.Background()
	spec := nativeLoadSpec("uuid-cpucap", "TestNativeCPUHogProcess",
		map[string]string{"GSM_NATIVE_CPUHOG": "1"},
		&protocol.ResourceLimits{CPUPercent: capPercent})
	id, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = b.Stop(ctx, id, StopOpts{}) }()

	b.mu.Lock()
	h := b.procs["uuid-cpucap"]
	b.mu.Unlock()
	if h == nil || h.job == nil {
		t.Fatal("行程或 Job 未就緒(CPU cap 未套用?)")
	}

	time.Sleep(700 * time.Millisecond) // 暖身:讓 goroutine 全速並讓 cap 收斂
	s1, err := h.job.stats()
	if err != nil {
		t.Fatalf("job.stats: %v", err)
	}
	t0 := time.Now()
	time.Sleep(window)
	s2, err := h.job.stats()
	if err != nil {
		t.Fatalf("job.stats: %v", err)
	}
	pct := calcNativeCPUPercent(s2.CPUTime-s1.CPUTime, time.Since(t0), runtime.NumCPU())
	if pct > capPercent+tolerance {
		t.Fatalf("CPU 持續占用 %.1f%% 超過 cap %d%%+容忍 %.0f,hard cap 未生效", pct, capPercent, tolerance)
	}
	t.Logf("CPU cap=%d%% 實測(2s 窗持續占用)=%.1f%%", capPercent, pct)
}

// TestNativeJob_AgentExitKeepsServerAlive:關閉 Job handle(模擬 agent 退出)後行程仍存活
// ——驗 KILL_ON_JOB_CLOSE 未設(R9 驗收:Job 設定不使 agent 退出連坐殺伺服器,與 R7 收養共存)。
func TestNativeJob_AgentExitKeepsServerAlive(t *testing.T) {
	// 直接以 jobObject + os/exec 測,聚焦 Job handle 關閉語意(不經 backend reap 干擾)。
	cmd := exec.Command(os.Args[0], "-test.run=TestNativeHelperProcess")
	cmd.Env = append(os.Environ(), "GSM_NATIVE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("啟動助手行程: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	job, err := newJobObject(jobLimits{MemoryBytes: 512 * 1024 * 1024})
	if err != nil {
		t.Fatalf("newJobObject: %v", err)
	}
	if err := job.assign(pid); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// 關閉 Job handle:未設 KILL_ON_JOB_CLOSE,行程應續活。
	if err := job.close(); err != nil {
		t.Fatalf("close job: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	if !isProcessAlive(t, pid) {
		t.Fatal("關閉 Job handle 後行程被連坐終止(KILL_ON_JOB_CLOSE 意外生效)")
	}
}

// isProcessAlive 以 GetExitCodeProcess 判 pid 是否仍執行(STILL_ACTIVE)。
func isProcessAlive(t *testing.T, pid int) bool {
	t.Helper()
	const stillActive = 259
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
