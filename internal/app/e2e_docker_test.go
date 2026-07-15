//go:build docker

// 端到端整合測試(T16;需 Docker daemon,以 -tags docker 執行)。經 app.Runtime API 走完最接近
// GUI 的程式化路徑:Bootstrap(真 DockerBackend)→ 建立 → 啟動(等就緒)→ 監控/指令/備份/還原 →
// 停止/移除。另含重啟後對帳(R12/R13)與效能基準(閒置/每實例監控開銷)。
//
// 執行(擇一):
//
//	go test -tags docker -run TestE2E ./internal/app/            # 全部 E2E + 基準
//	go test -tags docker -run TestE2E_MinecraftFullLifecycle ./internal/app/
//	go test -tags docker -run TestE2E_Palworld ./internal/app/   # Palworld(需先快取映像)
//
// 清理:所有測試建立的容器/資料/備份於結束(含失敗路徑,經 t.Cleanup)移除,不留 gsm 殘留。
// 效能數字回填 specs/game-server-manager/design.md「效能基準」節。
package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"servermonitor/internal/agent"
	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

const (
	// e2eTemplatesDir 是 repo 根 templates/ 目錄相對於本套件(internal/app)的路徑(內建 minecraft/palworld)。
	e2eTemplatesDir = "../../templates"
	mcRconPassword  = "e2e-rcon-secret-pw"
)

// e2eFastTemplate 是 E2E 用的輕量長駐範本:nginx 前景常駐、health 預設(docker,容器 running=就緒),
// 秒級就緒。供重啟對帳與每實例監控開銷量測用(監控機制與遊戲無關,以輕量真容器忠實量測核心開銷)。
const e2eFastTemplate = `schema_version = 1
id = "e2e-longrun"
name = "E2E Long Running"
runtime = "docker"
data_dirs = ["/data"]

[docker]
image = "nginx:alpine"
`

// bootstrapE2E 建立一個以真 DockerBackend 為後端的 Runtime;Docker 節點離線則跳過整個測試。
func bootstrapE2E(t *testing.T, dataRoot, builtinDir string, ready time.Duration) *Runtime {
	t.Helper()
	rt, err := Bootstrap(Options{DataRoot: dataRoot, BuiltinTemplateDir: builtinDir, ReadyTimeout: ready})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if st, ok := rt.registry.Status(rt.node); !ok || !st.Online {
		rt.Shutdown()
		t.Skipf("Docker 節點離線,跳過 E2E: %+v", st)
	}
	return rt
}

// TestE2E_MinecraftFullLifecycle 走完 Minecraft 全流程(GUI 走完一輪的程式化等價):
// 建立(Paper/EULA/RCON 密碼)→ 啟動等就緒 → 快照有 CPU/RAM → log 訂閱收到行 → 指令回顯 →
// 備份(停機→備份→拉回)→ 列備份 → 還原(新 runtime ID、回 Running)→ 停止 → 移除。
func TestE2E_MinecraftFullLifecycle(t *testing.T) {
	dataRoot := t.TempDir()
	rt := bootstrapE2E(t, dataRoot, e2eTemplatesDir, 8*time.Minute)
	defer rt.Shutdown()

	// ListTemplates:內建 minecraft 範本應載入。
	if !hasTemplate(rt.Templates(), "minecraft") {
		t.Fatal("minecraft 範本未載入")
	}

	ctx := context.Background()

	// Create(先於 Start,避免建立 journal 與啟動對帳競態,對齊 t15/bootstrap 測試慣例)。
	rec, err := rt.Create(ctx, core.CreateOptions{
		TemplateID: "minecraft",
		Variant:    "paper",
		Params:     map[string]string{"EULA": "true"}, // VERSION 沿用範本 default(1.21.1)
		Secrets:    map[string]string{"RCON_PASSWORD": mcRconPassword},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	uuid := rec.UUID
	t.Logf("created uuid=%s runtime=%s", uuid, rec.RuntimeID)
	// 清理安全網:移除實例(停容器 + 回收埠/機密/DB)。用 defer(非 t.Cleanup):defer 於函式返回時
	// LIFO 執行,晚註冊者先跑,故此 remove 會早於上方 `defer rt.Shutdown()` 執行——若改用 t.Cleanup
	// 則會在 Shutdown 關閉後端「之後」才跑,導致移除失敗、容器外洩(實測踩過)。冪等,已移除則忽略。
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = rt.RemoveInstance(cctx, uuid, true)
	}()

	rt.Start()

	// Start:啟動並輪詢就緒。注意:狀態機就緒探針是「TCP 撥遊戲埠」,而 Docker 對已發布埠的
	// userland proxy 會在容器程序尚未實際綁定前即接受 TCP 連線 → 就緒可能「假陽性」提早回 Running
	// (實測 mem~18MB、disk~0,伺服器仍在下載 Paper/啟動 JVM)。故 StartInstance 返回後,再以 RCON
	// `list` 真正回應作為「真就緒」閘門(伺服器完全啟動才會回應 RCON),逾時放寬至 6 分鐘。
	startCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()
	if err := rt.StartInstance(startCtx, uuid); err != nil {
		t.Fatalf("StartInstance(等就緒): %v", err)
	}
	listRes := sendUntilOK(t, rt, uuid, protocol.GameCommand{ProtocolID: "mc-rcon", Raw: "list"}, 6*time.Minute)
	if !listRes.Success || listRes.Output == "" {
		t.Fatalf("list 指令回顯失敗: %+v", listRes)
	}
	t.Logf("list 回顯(真就緒閘門): %q", listRes.Output)

	// GetSnapshot:輪詢至收到至少一筆資源取樣,斷言記憶體有值(真就緒後 JVM 記憶體應顯著)。
	snap := waitStats(t, rt, uuid, 40*time.Second)
	if snap.Stats.MemoryBytes == 0 {
		t.Fatalf("快照記憶體應 >0: %+v", snap.Stats)
	}
	t.Logf("snapshot: cpu=%.3f%% mem=%d disk=%v observed=%s hasStats=%v",
		snap.Stats.CPUPercent, snap.Stats.MemoryBytes, derefU64(snap.Stats.DataDiskBytes), snap.ObservedState, snap.HasStats)

	// SubscribeLogs:訂閱後等待任一 log 行(證明串流打通)。以 say 產生 console 輸出(best-effort,
	// 於 goroutine 送;不硬性要求成功——前景 RCON 與背景玩家數輪詢(亦走 RCON `list`)會競用連線,
	// say 可能偶發逾時)。即便 say 失敗,背景輪詢的 RCON 連線本身也會產生 "RCON Client ..." log 行,
	// 故等到任一行即可。
	logCh, logID := rt.SubscribeLogs(uuid)
	defer rt.UnsubscribeLogs(uuid, logID)
	go func() {
		ictx, icancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer icancel()
		_, _ = rt.SendCommand(ictx, uuid, protocol.GameCommand{ProtocolID: "mc-rcon", Raw: "say e2e-log-probe"})
	}()
	select {
	case ln := <-logCh:
		t.Logf("log line: [%s] %s", ln.Stream, ln.Line)
	case <-time.After(90 * time.Second):
		t.Fatal("SubscribeLogs 未在時限內收到任何 log 行")
	}

	// GetCommandCapability=rcon。
	if cp, err := rt.CommandCapability(uuid); err != nil || cp.Kind != "rcon" {
		t.Fatalf("CommandCapability 應為 rcon: cp=%+v err=%v", cp, err)
	}

	// BackupNow(自動停機→備份→拉回);備份後應收斂回 Running。
	meta, err := rt.BackupNow(ctx, uuid)
	if err != nil {
		t.Fatalf("BackupNow: %v", err)
	}
	t.Logf("backup id=%s checksum=%s", meta.BackupID, meta.Checksum)
	if rr, _ := rt.Instance(uuid); rr.ObservedState != protocol.InstanceStateRunning {
		t.Fatalf("備份後應回 Running,得 %s", rr.ObservedState)
	}

	// ListBackups 應有一筆。
	backups, err := rt.ListBackups(ctx, uuid)
	if err != nil || len(backups) < 1 {
		t.Fatalf("ListBackups 應 >=1: err=%v n=%d", err, len(backups))
	}

	// 備份會停機→備份→重啟,伺服器又回早期啟動(假陽性就緒)。還原前先等真就緒:還原的優雅停機
	// 走 hooks.stop→RCON,對啟動中伺服器 RCON 會 hang 至逾時,使代理 /stop 超過 NodeClient 30s HTTP
	// 逾時而失敗(實測踩過)。等 RCON 真回應後再還原,停機才乾淨。
	sendUntilOK(t, rt, uuid, protocol.GameCommand{ProtocolID: "mc-rcon", Raw: "list"}, 6*time.Minute)

	before, _ := rt.Instance(uuid)
	oldRID := before.RuntimeID
	// 取得 oldRID 後**立即**註冊舊容器清理(冪等、容忍不存在),再做還原與斷言:還原成功後舊容器
	// 成停止孤兒(agent Restore 不自動移除,生產由下次啟動對帳 RECONCILE_ORPHAN 清理);而還原本身或
	// 後續斷言若失敗而早退,此 defer 仍清掉舊容器,避免 gsm 殘留。no-purge:不動共享的實例資料(新
	// 容器資料由最終 RemoveInstance(purge) 清理)。註冊於斷言之前,故任一 t.Fatalf 都被涵蓋。
	defer func() {
		octx, ocancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ocancel()
		_ = rt.backend.Remove(octx, oldRID, agent.RemoveOpts{Purge: false})
	}()

	// RestoreBackup:斷言新 runtime ID(≠ 舊)、依原 desired 回 Running。
	if err := rt.RestoreBackup(ctx, uuid, meta.BackupID); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	after, _ := rt.Instance(uuid)
	if after.RuntimeID == oldRID || after.RuntimeID == "" {
		t.Fatalf("還原後 runtime ID 應改變,舊=%s 新=%s", oldRID, after.RuntimeID)
	}
	if after.ObservedState != protocol.InstanceStateRunning {
		t.Fatalf("還原後應回 Running,得 %s", after.ObservedState)
	}
	t.Logf("restore: 新 runtime=%s (舊=%s) observed=%s", after.RuntimeID, oldRID, after.ObservedState)

	// Stop → Remove(purge)。還原後伺服器又重啟(早期啟動);最終 teardown 的優雅停機可能因 RCON
	// 尚未就緒而逾時——優雅停機已於備份/還原的前置停機階段驗證過,故此處容忍停機逾時,交由
	// RemoveInstance(purge)強制清理(其內含 best-effort stop + 強制移除容器)。
	if err := rt.StopInstance(ctx, uuid); err != nil {
		t.Logf("最終 StopInstance 未乾淨返回(容忍;RemoveInstance 會強制清理): %v", err)
	}
	if err := rt.RemoveInstance(ctx, uuid, true); err != nil {
		t.Fatalf("RemoveInstance: %v", err)
	}
	if _, err := rt.Instance(uuid); err == nil {
		t.Fatal("移除後不應還能取得實例記錄")
	}
}

// TestE2E_RestartReconcile 驗證重啟後對帳(R12/R13):Bootstrap A 建立+啟動一個長駐容器後,
// 不 Stop 直接 Shutdown(容器續跑)→ Bootstrap B(同資料根)→ 斷言實例清單重現、observed=Running
// (對帳接管)、且監控串流可重建(收到一筆 stats)。用 nginx 長駐範本使就緒/停啟快速可靠。
func TestE2E_RestartReconcile(t *testing.T) {
	dataRoot := t.TempDir()
	writeUserTemplate(t, dataRoot, "e2e-longrun.toml", e2eFastTemplate)
	builtinEmpty := t.TempDir()

	// ---- Bootstrap A:建立 + 啟動,不停止即 Shutdown ----
	rtA := bootstrapE2E(t, dataRoot, builtinEmpty, 60*time.Second)
	ctx := context.Background()
	recA, err := rtA.Create(ctx, core.CreateOptions{TemplateID: "e2e-longrun"})
	if err != nil {
		rtA.Shutdown()
		t.Fatalf("A Create: %v", err)
	}
	uuid := recA.UUID
	// A 建容器成功後**立即**註冊 fallback 清理:B 尚未接手主清理前,若後續任一 t.Fatalf 早退
	// (StartInstance/斷言失敗),此 net 仍移除已建容器,避免 gsm 殘留。冪等:B 的 RemoveInstance
	// 已移除時,對不存在容器的強制移除為 no-op。經 docker CLI(非 rtA.backend)——rtA.Shutdown 後其
	// 後端已關閉,無法再用;RuntimeID 即容器 ID,docker rm -f 不受應用生命週期影響。
	t.Cleanup(func() { forceRemoveContainer(recA.RuntimeID) })
	if err := rtA.StartInstance(ctx, uuid); err != nil {
		rtA.Shutdown()
		t.Fatalf("A StartInstance: %v", err)
	}
	if r, _ := rtA.Instance(uuid); r.ObservedState != protocol.InstanceStateRunning {
		rtA.Shutdown()
		t.Fatalf("A: 啟動後應 Running,得 %s", r.ObservedState)
	}
	rtA.Shutdown() // 不觸碰容器:容器續跑,交由 B 對帳接管。

	// ---- Bootstrap B(同資料根):對帳接管續跑容器 ----
	rtB := bootstrapE2E(t, dataRoot, builtinEmpty, 60*time.Second)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = rtB.RemoveInstance(cctx, uuid, true)
		rtB.Shutdown()
	})
	rtB.Start() // 啟動對帳於此同步執行:List 找到續跑容器並比對接管。

	insts, err := rtB.Instances()
	if err != nil || !hasInstance(insts, uuid) {
		t.Fatalf("B: 實例清單應重現 %s: err=%v", uuid, err)
	}
	if !waitObserved(t, rtB, uuid, protocol.InstanceStateRunning, 30*time.Second) {
		r, _ := rtB.Instance(uuid)
		t.Fatalf("B: 對帳後 observed 應 Running,得 %s", r.ObservedState)
	}
	t.Logf("B: 對帳接管 uuid=%s observed=Running", uuid)

	// 監控串流可重建:對帳後訂閱 stats 應收到資料(證明 registry dialer 重新撥到續跑容器)。
	ch, id := rtB.SubscribeStats(uuid)
	defer rtB.UnsubscribeStats(uuid, id)
	select {
	case s := <-ch:
		t.Logf("B: 對帳後 stats 訂閱打通 cpu=%.3f%% mem=%d", s.CPUPercent, s.MemoryBytes)
	case <-time.After(20 * time.Second):
		t.Fatal("B: 對帳後 stats 訂閱未收到資料(監控未重建?)")
	}
}

// TestE2E_Palworld 走 Palworld REST 流程(盡力;需先快取約 6-8GB 映像、~8GB RAM)。
// 為避免測試期間下載大檔,映像未快取即跳過(手動 `docker pull` 後可執行)。
func TestE2E_Palworld(t *testing.T) {
	const image = "thijsvanloef/palworld-server-docker:v2.5.1"
	if !dockerImagePresent(image) {
		t.Skipf("Palworld 映像 %s 未快取(約 6-8GB);為避免測試期間大檔下載而跳過。"+
			"手動執行 `docker pull %s` 後可跑 -run TestE2E_Palworld。", image, image)
	}
	dataRoot := t.TempDir()
	rt := bootstrapE2E(t, dataRoot, e2eTemplatesDir, 15*time.Minute)
	defer rt.Shutdown()

	ctx := context.Background()
	rec, err := rt.Create(ctx, core.CreateOptions{
		TemplateID: "palworld",
		Secrets:    map[string]string{"ADMIN_PASSWORD": "e2e-palworld-admin"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	uuid := rec.UUID
	// 移除用 defer(早於上方 defer rt.Shutdown() 執行,理由見 Minecraft 測試註)。
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = rt.RemoveInstance(cctx, uuid, true)
	}()

	rt.Start()

	// Palworld 啟動 3-5 分鐘;就緒探針為 REST(kind=rest),逾時放寬 15 分。
	startCtx, cancel := context.WithTimeout(ctx, 16*time.Minute)
	defer cancel()
	if err := rt.StartInstance(startCtx, uuid); err != nil {
		t.Fatalf("StartInstance(Palworld 就緒): %v", err)
	}

	// GetCommandCapability=rest。
	if cp, err := rt.CommandCapability(uuid); err != nil || cp.Kind != "rest" {
		t.Fatalf("CommandCapability 應為 rest: cp=%+v err=%v", cp, err)
	}

	// SendCommand(action=players)回顯。
	res := sendUntilOK(t, rt, uuid, protocol.GameCommand{ProtocolID: "palworld-rest", ActionID: "players"}, 90*time.Second)
	t.Logf("Palworld players 回顯: %q", res.Output)

	if err := rt.StopInstance(ctx, uuid); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
}

// TestE2E_PerfBaseline 量測工具程序層的效能基準(NFR;數字回填 design.md「效能基準」節):
// (1) 閒置(0 實例)60 秒的程序 CPU% 與工作集;(2) 監控 1 個執行中實例相對閒置的 CPU/工作集增量。
// 以自身程序的 Windows 計數器量測(GetProcessTimes/GetProcessMemoryInfo)。log 高流量基準見
// internal/core 的 TestPerf_LogThroughput(核心 fanout 層)。
func TestE2E_PerfBaseline(t *testing.T) {
	const window = 60 * time.Second
	dataRoot := t.TempDir()
	writeUserTemplate(t, dataRoot, "e2e-longrun.toml", e2eFastTemplate)
	builtinEmpty := t.TempDir()

	rt := bootstrapE2E(t, dataRoot, builtinEmpty, 60*time.Second)
	defer rt.Shutdown()
	rt.Start()
	time.Sleep(3 * time.Second) // 背景迴圈進入穩態(初始對帳/首次事件連線)

	// --- 閒置(0 實例)---
	idleCPU, idleWS := sampleProcess(t, window)
	t.Logf("[基準] 閒置(0 實例) %s: CPU 全核=%.3f%% 單核當量=%.3f%% 工作集=%.1f MB",
		window, idleCPU.allCores, idleCPU.oneCore, float64(idleWS)/1e6)

	// --- 每實例監控開銷:啟動 1 個 nginx 實例並開啟監控與 log 訂閱 ---
	ctx := context.Background()
	rec, err := rt.Create(ctx, core.CreateOptions{TemplateID: "e2e-longrun"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	uuid := rec.UUID
	// 移除用 defer(早於上方 defer rt.Shutdown() 執行,理由見 Minecraft 測試註)。
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = rt.RemoveInstance(cctx, uuid, true)
	}()
	if err := rt.StartInstance(ctx, uuid); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	lch, lid := rt.SubscribeLogs(uuid) // 模擬主控台開啟
	defer rt.UnsubscribeLogs(uuid, lid)
	go func() {
		for range lch { // 持續排空,避免背壓影響量測
		}
	}()
	time.Sleep(3 * time.Second) // 監控串流穩態

	oneCPU, oneWS := sampleProcess(t, window)
	t.Logf("[基準] 監控 1 實例 %s: CPU 全核=%.3f%% 單核當量=%.3f%% 工作集=%.1f MB",
		window, oneCPU.allCores, oneCPU.oneCore, float64(oneWS)/1e6)
	t.Logf("[基準] 每實例增量: ΔCPU 全核=%.3f%% ΔWS=%.1f MB",
		oneCPU.allCores-idleCPU.allCores, (float64(oneWS)-float64(idleWS))/1e6)
}

// ---- 測試輔助 ----

func hasTemplate(tmpls []*protocol.GameTemplate, id string) bool {
	for _, tm := range tmpls {
		if tm.ID == id {
			return true
		}
	}
	return false
}

func hasInstance(insts []core.InstanceRecord, uuid string) bool {
	for _, r := range insts {
		if r.UUID == uuid {
			return true
		}
	}
	return false
}

func derefU64(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

func writeUserTemplate(t *testing.T, dataRoot, name, content string) {
	t.Helper()
	tdir := filepath.Join(dataRoot, "templates")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatalf("建立範本目錄: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tdir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("寫入範本 %s: %v", name, err)
	}
}

// waitStats 輪詢至實例快照收到至少一筆資源取樣(HasStats)。
func waitStats(t *testing.T, rt *Runtime, uuid string, timeout time.Duration) core.MonitorSnapshot {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for {
		if snap, ok := rt.Snapshot(uuid); ok && snap.HasStats {
			return snap
		}
		select {
		case <-deadline:
			snap, _ := rt.Snapshot(uuid)
			t.Fatalf("未在 %s 內取得含 stats 的快照: %+v", timeout, snap)
		case <-tick.C:
		}
	}
}

// waitObserved 輪詢至實例 observed 狀態達 want(容忍對帳/監控收斂的短暫延遲)。
func waitObserved(t *testing.T, rt *Runtime, uuid string, want protocol.InstanceState, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		if r, err := rt.Instance(uuid); err == nil && r.ObservedState == want {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-tick.C:
		}
	}
}

// sendUntilOK 輪詢送出指令直到成功(res.Success 且無錯);逾時 t.Fatalf。用於就緒後 RCON/REST
// 可能稍晚才可用的容錯。
func sendUntilOK(t *testing.T, rt *Runtime, uuid string, cmd protocol.GameCommand, timeout time.Duration) protocol.CommandResult {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var last protocol.CommandResult
	var lastErr error
	for {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, err := rt.SendCommand(cctx, uuid, cmd)
		cancel()
		if err == nil && res.Success {
			return res
		}
		last, lastErr = res, err
		select {
		case <-deadline:
			t.Fatalf("指令 %+v 於 %s 內未成功: res=%+v err=%v", cmd, timeout, last, lastErr)
		case <-tick.C:
		}
	}
}

func dockerImagePresent(ref string) bool {
	return exec.Command("docker", "image", "inspect", ref).Run() == nil
}

// forceRemoveContainer 以 docker CLI 強制移除容器(測試清理安全網;冪等,容器不存在時忽略錯誤)。
// 用於「應用生命週期外」的殘留清理——RuntimeID 即容器 ID,不依賴任何仍存活的 backend/runtime。
func forceRemoveContainer(id protocol.RuntimeID) {
	if id == "" {
		return
	}
	_ = exec.Command("docker", "rm", "-f", string(id)).Run()
}

// ---- 程序層效能量測(Windows 自身程序計數器)----

type cpuPct struct {
	allCores float64 // 佔全機所有核心的百分比(dCPU / (wall * NumCPU))
	oneCore  float64 // 單核當量百分比(dCPU / wall)
}

// sampleProcess 量測 window 期間自身程序的平均 CPU% 與量測結束時的工作集。
func sampleProcess(t *testing.T, window time.Duration) (cpuPct, uint64) {
	t.Helper()
	cpu0 := processCPUTime(t)
	wall0 := time.Now()
	time.Sleep(window)
	cpu1 := processCPUTime(t)
	wall := time.Since(wall0)
	ws := processWorkingSet(t)

	oneCore := float64(cpu1-cpu0) / float64(wall) * 100
	allCores := oneCore / float64(runtime.NumCPU())
	return cpuPct{allCores: allCores, oneCore: oneCore}, ws
}

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	psapi                 = syscall.NewLazyDLL("psapi.dll")
	procGetProcessTimes   = kernel32.NewProc("GetProcessTimes")
	procGetCurrentProcess = kernel32.NewProc("GetCurrentProcess")
	procGetProcessMemInfo = psapi.NewProc("GetProcessMemoryInfo")
)

type filetime struct{ low, high uint32 }

func (f filetime) duration() time.Duration {
	ticks := (uint64(f.high) << 32) | uint64(f.low) // 100ns 單位
	return time.Duration(ticks) * 100               // *100ns
}

func currentProcess() uintptr {
	h, _, _ := procGetCurrentProcess.Call()
	return h
}

// processCPUTime 回傳自身程序累計 CPU 時間(kernel + user)。
func processCPUTime(t *testing.T) time.Duration {
	t.Helper()
	var creation, exit, kernel, user filetime
	r, _, err := procGetProcessTimes.Call(currentProcess(),
		uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		t.Fatalf("GetProcessTimes: %v", err)
	}
	return kernel.duration() + user.duration()
}

// processMemoryCounters 對映 PSAPI PROCESS_MEMORY_COUNTERS。
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// processWorkingSet 回傳自身程序當前工作集(bytes)。
func processWorkingSet(t *testing.T) uint64 {
	t.Helper()
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	r, _, err := procGetProcessMemInfo.Call(currentProcess(),
		uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	if r == 0 {
		t.Fatalf("GetProcessMemoryInfo: %v", err)
	}
	return uint64(pmc.workingSetSize)
}
