//go:build docker

// R11 真 Docker 整合測試(需 daemon;以 -tags docker 執行):itzg + Modrinth 模組包。
// 驗證 core 端 buildSpec 產出的 spec(TYPE=MODRINTH + MODRINTH_MODPACK 透傳)確實驅動 itzg
// 進入 Modrinth 模組包安裝路徑、並使模組落入 /data/mods(模組生效)。預設(無 tag)建置不含本檔。
//
// 注意:下載量大(itzg 映像 ~600MB + Fabric loader + 模組包 + JVM 啟動),故 timeout 放寬。
// 手動模組包(mrpack/curseforge-zip)的真 Docker 匯入未涵蓋於此——手動檔位元組送達 agent 宿主
// 掛載目錄需 T9 邊界能力(見 modpack.go 頭註),非本任務可端到端驗證。
package core

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// modrinthTestSlug 是用於測試的 Modrinth 模組包 slug;可經環境變數覆寫以換小型包。
func modrinthTestSlug() string {
	if s := os.Getenv("GSM_TEST_MODRINTH_SLUG"); s != "" {
		return s
	}
	return "adrenaline" // 輕量 fabric 伺服器相容模組包(可被 GSM_TEST_MODRINTH_SLUG 覆寫)
}

// TestModpackDocker_ModrinthTakesEffect 端到端:以真 DockerBackend 建立並啟動一個帶 Modrinth
// 模組包的 Minecraft 容器,驗證 buildSpec 的 env 透傳使 itzg 安裝模組(/data/mods 出現 .jar)。
func TestModpackDocker_ModrinthTakesEffect(t *testing.T) {
	backend, err := agent.NewDockerBackend(agent.DockerOptions{
		DataRoot:   t.TempDir(),
		BackupRoot: t.TempDir(),
		Node:       "itest-mod",
		Schema:     "1",
	})
	if err != nil {
		t.Fatalf("NewDockerBackend: %v", err)
	}
	defer backend.Close()

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer probeCancel()
	if _, lerr := backend.List(probeCtx); lerr != nil {
		t.Skipf("Docker daemon 不可用,跳過: %v", lerr)
	}

	// 載入真 minecraft 範本(含 [mods] 與 fabric 變體),經 core buildSpec 產出 spec。
	events, _ := newTestEventLog(t)
	eng := NewTemplateEngine(nil, events)
	if _, lerr := eng.LoadDir(builtinTemplatesDir); lerr != nil {
		t.Fatalf("LoadDir(%s): %v", builtinTemplatesDir, lerr)
	}
	tmpl, ok := eng.Get("minecraft")
	if !ok {
		t.Fatal("內建 minecraft 範本未載入")
	}

	opts := CreateOptions{
		TemplateID: "minecraft",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackModrinth, Ref: modrinthTestSlug()},
	}
	variantEnv, verr := resolveVariant(tmpl, opts.Variant)
	if verr != nil {
		t.Fatalf("resolveVariant: %v", verr)
	}
	// buildSpec 不觸碰 InstanceService 欄位,可於零值上呼叫以取得純 spec。
	spec := (&InstanceService{}).buildSpec("mod-itest", "itest-mod", tmpl, opts, variantEnv, runtimeDocker, nil)

	// 確認 core 端 env 透傳正確(前置健全檢查)。
	if spec.Env["TYPE"] != "MODRINTH" || spec.Env["MODRINTH_MODPACK"] != modrinthTestSlug() {
		t.Fatalf("spec env 未正確透傳 Modrinth: %+v", spec.Env)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	rid, err := backend.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create(真 Docker): %v", err)
	}
	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_ = backend.Stop(rmCtx, rid, agent.StopOpts{Grace: 10 * time.Second})
		_ = backend.Remove(rmCtx, rid, agent.RemoveOpts{Purge: true})
	}()

	if err := backend.Start(ctx, rid); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 追蹤 log,確認 itzg 進入 Modrinth 模組包安裝路徑,並輪詢 /data/mods 直到出現 .jar(模組生效)。
	sawModrinth := followUntilModrinth(ctx, t, backend, rid)
	if !sawModrinth {
		t.Fatal("log 未見 itzg 進入 Modrinth 模組包安裝路徑(env 透傳可能未生效)")
	}

	if !pollModsInstalled(ctx, t, backend, rid) {
		t.Fatal("/data/mods 未出現 .jar,模組未生效")
	}
}

// followUntilModrinth 追蹤容器 log,直到出現 Modrinth 模組包處理跡象或 ctx 逾時。
func followUntilModrinth(ctx context.Context, t *testing.T, backend *agent.DockerBackend, rid protocol.RuntimeID) bool {
	t.Helper()
	stream, err := backend.Logs(ctx, rid, agent.LogOpts{Follow: true, Tail: 0})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer stream.Close()
	for {
		select {
		case <-ctx.Done():
			return false
		case ln, ok := <-stream.Lines():
			if !ok {
				return false
			}
			low := strings.ToLower(ln.Line)
			if strings.Contains(low, "modrinth") || strings.Contains(low, "modpack") {
				return true
			}
		}
	}
}

// pollModsInstalled 每 10s 於容器內 `ls /data/mods`,直到出現 .jar 或 ctx 逾時。
func pollModsInstalled(ctx context.Context, t *testing.T, backend *agent.DockerBackend, rid protocol.RuntimeID) bool {
	t.Helper()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		res, err := backend.ExecProcess(ctx, rid, agent.ExecCmd{Cmd: []string{"sh", "-c", "ls /data/mods 2>/dev/null"}})
		if err == nil && res.ExitCode == 0 && strings.Contains(res.Stdout, ".jar") {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
