//go:build native

// native 後端端到端整合測試(native-backend R5/R7/R8/R10/R12;需真 Windows + 網路,以 -tags native
// 執行)。走最接近「乾淨 Windows 無 Docker」GUI 的程式化路徑:Bootstrap(Windows dispatch,native 為
// 預設 runtime)→ 建立(真下載 Adoptium JRE + Mojang vanilla server.jar)→ 啟動(RCON 就緒探針)→
// 監控/日誌/指令 → 備份/還原 → 停止/移除。全程無任何外部安裝需求(R12)。
//
// 執行:
//
//	go test -tags native -run TestE2E_Native ./internal/app/                 # 全部 native E2E
//	go test -tags native -run TestE2E_NativeMinecraftFullLifecycle ./internal/app/
//	GSM_NATIVE_PALWORLD_E2E=1 go test -tags native -run TestE2E_NativePalworld ./internal/app/  # 6-8GB,opt-in
//
// heavy(真下載/啟動)測試掛 testing.Short() 保護:`-short` 時跳過。native 僅 Windows,非 Windows Skip。
// 下載量約 100-200MB(JRE + vanilla server.jar)屬預期。清理:所有實例於結束(含失敗路徑)purge 移除。
package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// nvTemplatesDir 是 repo 根 templates/ 相對本套件的路徑(內建 minecraft/palworld)。
const nvTemplatesDir = "../../templates"

// nvDataRoot 回傳 E2E 用資料根:刻意放在 repo 所在磁碟(../../ 之下)而非 t.TempDir()。
// 原因(T13 E2E 實測 2026-07-17):native 遊戲伺服器(JVM/netty)以 AF_UNIX self-pipe 初始化
// selector,而 socket 建於行程 TEMP(預設 = 資料根下 .gsm-tmp,見 NativeBackend.Start)。實測某些
// 環境的 C:\Users\<user>\AppData\Local\ 子樹(t.TempDir() 即落於此下的 ...\Temp)會使 AF_UNIX
// connect 失敗、伺服器啟動即崩潰;改用 repo 磁碟上的一般目錄可避開。結束時清理。
func nvDataRoot(t *testing.T) string {
	t.Helper()
	base, err := filepath.Abs(filepath.Join("..", "..", ".e2e-native-data")) // 絕對路徑:provision 內部 filepath.Rel 不接受相對根
	if err != nil {
		t.Fatalf("解析 E2E 資料根基底絕對路徑: %v", err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("建立 E2E 資料根基底: %v", err)
	}
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		t.Fatalf("建立 E2E 資料根: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// nvRuntime 建一個以 Windows 生產預設後端(dispatch:native + 盡力 docker)為底的 Runtime;
// 非 Windows 則跳過。native 子後端恆在線,故此處也順帶驗「即使 Docker 缺席亦可運行」(R12)。
func nvRuntime(t *testing.T, dataRoot string, ready time.Duration) *Runtime {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("native 後端僅 Windows 支援,跳過")
	}
	rt, err := Bootstrap(Options{DataRoot: dataRoot, BuiltinTemplateDir: nvTemplatesDir, ReadyTimeout: ready})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if st, ok := rt.registry.Status(rt.node); !ok || !st.Online {
		rt.Shutdown()
		t.Fatalf("native 節點應恆在線(免 Docker 亦可): %+v", st)
	}
	return rt
}

// TestE2E_NativeMinecraftFullLifecycle 走 native Minecraft(vanilla)全流程:真下載 JRE + server.jar
// → 建立(runtime=native)→ 啟動等 RCON 就緒 → Stats 有值 → log 串流打通 → 指令回顯 → 備份 → 還原
// → 停止 → 移除。
func TestE2E_NativeMinecraftFullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy(真下載+啟動 MC),-short 跳過")
	}
	dataRoot := nvDataRoot(t)
	rt := nvRuntime(t, dataRoot, 10*time.Minute)
	defer rt.Shutdown()

	if !nvHasTemplate(rt.Templates(), "minecraft") {
		t.Fatal("minecraft 範本未載入")
	}

	// Create(runtime 未指定 → Windows 預設 native)。此步同步下載 JRE + vanilla server.jar,故給長逾時。
	createCtx, createCancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer createCancel()
	rec, err := rt.Create(createCtx, core.CreateOptions{
		TemplateID: "minecraft",
		Variant:    "vanilla",
		Params:     map[string]string{"EULA": "true"}, // VERSION 沿用範本 default(1.21.1,Java 21)
		Secrets:    map[string]string{"RCON_PASSWORD": "e2e-native-rcon-pw"},
	})
	if err != nil {
		t.Fatalf("Create(native): %v", err)
	}
	uuid := rec.UUID
	t.Logf("created uuid=%s runtime=%s", uuid, rec.RuntimeID)
	if !strings.HasPrefix(string(rec.RuntimeID), "native:") {
		t.Fatalf("runtime ID 應為 native: 前綴,得 %s", rec.RuntimeID)
	}
	// 清理安全網(defer 早於 Shutdown 執行,理由同 docker E2E:t.Cleanup 會在後端關閉後才跑)。
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = rt.RemoveInstance(cctx, uuid, true)
	}()

	rt.Start()

	// 啟動並等就緒(RCON 探針:server.properties 已由範本 [native.config.set] 啟用 RCON、綁 rcon 埠)。
	// vanilla 首啟需生成世界,逾時放寬。
	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	startedAt := time.Now()
	if err := rt.StartInstance(startCtx, uuid); err != nil {
		t.Fatalf("StartInstance(等就緒): %v", err)
	}
	t.Logf("就緒耗時: %s", time.Since(startedAt).Round(time.Second))

	// 指令回顯(RCON list):真就緒閘門。
	listRes := nvSendUntilOK(t, rt, uuid, protocol.GameCommand{ProtocolID: "mc-rcon", Raw: "list"}, 3*time.Minute)
	if !listRes.Success {
		t.Fatalf("list 指令回顯失敗: %+v", listRes)
	}
	t.Logf("list 回顯: %q", listRes.Output)

	// Stats:輪詢至有取樣;native Job Object accounting 應回記憶體(JVM 起來後顯著)。
	snap := nvWaitStats(t, rt, uuid, 40*time.Second)
	t.Logf("snapshot: cpu=%.3f%% mem=%d observed=%s hasStats=%v",
		snap.Stats.CPUPercent, snap.Stats.MemoryBytes, snap.ObservedState, snap.HasStats)

	// Logs:訂閱後等任一行(證明日誌檔 tail 串流打通)。
	logCh, logID := rt.SubscribeLogs(uuid)
	defer rt.UnsubscribeLogs(uuid, logID)
	go func() {
		ictx, icancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer icancel()
		_, _ = rt.SendCommand(ictx, uuid, protocol.GameCommand{ProtocolID: "mc-rcon", Raw: "say e2e-native-probe"})
	}()
	select {
	case ln := <-logCh:
		t.Logf("log line: [%s] %s", ln.Stream, ln.Line)
	case <-time.After(60 * time.Second):
		t.Fatal("SubscribeLogs 未在時限內收到任何 log 行")
	}

	// 備份(自動停機→備份→拉回)。
	ctx := context.Background()
	meta, err := rt.BackupNow(ctx, uuid)
	if err != nil {
		t.Fatalf("BackupNow: %v", err)
	}
	t.Logf("backup id=%s checksum=%s", meta.BackupID, meta.Checksum)

	backups, err := rt.ListBackups(ctx, uuid)
	if err != nil || len(backups) < 1 {
		t.Fatalf("ListBackups 應 >=1: err=%v n=%d", err, len(backups))
	}

	// 備份後伺服器回早期啟動;還原前等真就緒(優雅停機走 RCON,對啟動中伺服器會 hang)。
	nvSendUntilOK(t, rt, uuid, protocol.GameCommand{ProtocolID: "mc-rcon", Raw: "list"}, 4*time.Minute)

	before, _ := rt.Instance(uuid)
	oldRID := before.RuntimeID
	if err := rt.RestoreBackup(ctx, uuid, meta.BackupID); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	after, _ := rt.Instance(uuid)
	// native RuntimeID 由 uuid 衍生、還原前後不變(對齊 NativeBackend.Restore 語意,有別於 docker 換新容器)。
	if after.ObservedState != protocol.InstanceStateRunning {
		t.Fatalf("還原後應回 Running,得 %s", after.ObservedState)
	}
	t.Logf("restore: runtime=%s (還原前=%s) observed=%s", after.RuntimeID, oldRID, after.ObservedState)

	// Stop → Remove(purge)。還原後伺服器又早期啟動,最終停機可能因 RCON 未就緒逾時,容忍(交 Remove 強清)。
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

// TestE2E_NativePalworldSteamCMD 走 native Palworld 全流程(SteamCMD app_update 2394010;約 6-8GB)。
// 預設跳過(避免 CI/一般跑測拉大檔);設 env GSM_NATIVE_PALWORLD_E2E=1 顯式啟用。
func TestE2E_NativePalworldSteamCMD(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy,-short 跳過")
	}
	if v := envOptIn("GSM_NATIVE_PALWORLD_E2E"); !v {
		t.Skip("Palworld native E2E 需 SteamCMD 下載約 6-8GB;設 GSM_NATIVE_PALWORLD_E2E=1 顯式啟用")
	}
	dataRoot := nvDataRoot(t)
	rt := nvRuntime(t, dataRoot, 15*time.Minute)
	defer rt.Shutdown()

	createCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	rec, err := rt.Create(createCtx, core.CreateOptions{
		TemplateID: "palworld",
		Secrets:    map[string]string{"ADMIN_PASSWORD": "e2e-native-pal-admin"},
	})
	if err != nil {
		t.Fatalf("Create(native palworld): %v", err)
	}
	uuid := rec.UUID
	defer func() {
		cctx, c := context.WithTimeout(context.Background(), 3*time.Minute)
		defer c()
		_ = rt.RemoveInstance(cctx, uuid, true)
	}()
	if !strings.HasPrefix(string(rec.RuntimeID), "native:") {
		t.Fatalf("runtime ID 應為 native: 前綴,得 %s", rec.RuntimeID)
	}

	rt.Start()
	startCtx, sc := context.WithTimeout(context.Background(), 16*time.Minute)
	defer sc()
	if err := rt.StartInstance(startCtx, uuid); err != nil {
		t.Fatalf("StartInstance(Palworld native 就緒): %v", err)
	}
	res := nvSendUntilOK(t, rt, uuid, protocol.GameCommand{ProtocolID: "palworld-rest", ActionID: "players"}, 90*time.Second)
	t.Logf("Palworld players 回顯: %q", res.Output)
	if err := rt.StopInstance(context.Background(), uuid); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
}

// ---- 測試輔助(nv 前綴避免與 e2e_docker_test.go 同名助手在 -tags "docker native" 下衝突)----

func nvHasTemplate(tmpls []*protocol.GameTemplate, id string) bool {
	for _, tm := range tmpls {
		if tm.ID == id {
			return true
		}
	}
	return false
}

func nvWaitStats(t *testing.T, rt *Runtime, uuid string, timeout time.Duration) core.MonitorSnapshot {
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

func nvSendUntilOK(t *testing.T, rt *Runtime, uuid string, cmd protocol.GameCommand, timeout time.Duration) protocol.CommandResult {
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

func envOptIn(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true")
}
