package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFabricMeta 模擬 FabricMC meta API(loader + installer 端點)與 installer jar 載點(含 .sha1
// 旁檔)。loaderVer/installerVer 標為 stable。serveSHA1 為假時不提供旁檔(模擬 404,測跳過校驗)。
func fakeFabricMeta(t *testing.T, mcVersion, loaderVer, installerVer string, installerJar []byte, serveSHA1 bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/v2/versions/loader/"+mcVersion, func(w http.ResponseWriter, r *http.Request) {
		var e fabricLoaderEntry
		e.Loader.Version = loaderVer
		e.Loader.Stable = true
		// 首元素給不穩定版,確認選取邏輯會略過取 stable。
		var older fabricLoaderEntry
		older.Loader.Version = loaderVer + "-unstable"
		older.Loader.Stable = false
		_ = json.NewEncoder(w).Encode([]fabricLoaderEntry{older, e})
	})
	mux.HandleFunc("/v2/versions/installer", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]fabricInstallerEntry{
			{URL: srv.URL + "/installer.jar", Version: installerVer, Stable: true},
		})
	})
	mux.HandleFunc("/installer.jar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(installerJar)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(installerJar)
	})
	mux.HandleFunc("/installer.jar.sha1", func(w http.ResponseWriter, r *http.Request) {
		if !serveSHA1 {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "%s  fabric-installer.jar\n", sha1Hex(installerJar))
	})

	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFabricInstaller_InstallsAndReturnsLaunchJar(t *testing.T) {
	installerJar := []byte("fake-fabric-installer")
	srv := fakeFabricMeta(t, "1.21.1", "0.16.5", "1.0.1", installerJar, true)

	var gotArgs []string
	exec := func(ctx context.Context, java string, args []string, onLine func(string)) error {
		gotArgs = args
		return fabricStyleFakeExec(t, "launch-jar-bytes", true)(ctx, java, args, onLine)
	}
	inst := NewFabricInstaller(srv.Client(), srv.URL, exec)

	targetDir := t.TempDir()
	got, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.21.1", TargetDir: targetDir, JavaExe: "java.exe"}, nil)
	if err != nil {
		t.Fatalf("Install 失敗: %v", err)
	}

	wantJar := filepath.Join(targetDir, fabricServerLaunchJar)
	if got.ServerJar != wantJar {
		t.Fatalf("ServerJar = %q,期望 %q", got.ServerJar, wantJar)
	}
	data, err := os.ReadFile(wantJar)
	if err != nil {
		t.Fatalf("讀取啟動 jar 失敗: %v", err)
	}
	if string(data) != "launch-jar-bytes" {
		t.Fatalf("啟動 jar 內容不符: %q", data)
	}

	// 驗證 installer 參數:server 子命令 + 選定的 stable loader + downloadMinecraft。
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"server", "-mcversion 1.21.1", "-loader 0.16.5", "-downloadMinecraft"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("installer 參數缺 %q,實得: %v", want, gotArgs)
		}
	}
}

func TestFabricInstaller_MissingSidecarHardFails(t *testing.T) {
	// 雙審 #12:官方 maven 對每個制品必附 .sha1 旁檔;旁檔缺失(404)不再靜默跳過校驗,改硬失敗。
	installerJar := []byte("fake-fabric-installer-nosha")
	srv := fakeFabricMeta(t, "1.21.1", "0.16.5", "1.0.1", installerJar, false) // 無 .sha1 旁檔。

	inst := NewFabricInstaller(srv.Client(), srv.URL, fabricStyleFakeExec(t, "x", true))
	targetDir := t.TempDir()
	_, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.21.1", TargetDir: targetDir, JavaExe: "java.exe"}, nil)
	if err == nil {
		t.Fatal("無 .sha1 旁檔應硬失敗,實得 nil")
	}
	if !strings.Contains(err.Error(), ".sha1") {
		t.Fatalf("錯誤訊息應點名缺 .sha1 旁檔,實得: %v", err)
	}
	// 校驗失敗即不應留下半成品。
	assertTargetClean(t, targetDir)
}

func TestFabricInstaller_MissingJavaExeErrors(t *testing.T) {
	installerJar := []byte("fake")
	srv := fakeFabricMeta(t, "1.21.1", "0.16.5", "1.0.1", installerJar, true)
	inst := NewFabricInstaller(srv.Client(), srv.URL, fabricStyleFakeExec(t, "x", true))

	_, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.21.1", TargetDir: t.TempDir()}, nil) // JavaExe 空。
	if err == nil || !strings.Contains(err.Error(), "JavaExe") {
		t.Fatalf("缺 JavaExe 應回明確錯誤,實得: %v", err)
	}
}

func TestFabricInstaller_InstallerFailureNoPartial(t *testing.T) {
	installerJar := []byte("fake")
	srv := fakeFabricMeta(t, "1.21.1", "0.16.5", "1.0.1", installerJar, true)

	failExec := func(ctx context.Context, java string, args []string, onLine func(string)) error {
		onLine("Exception in installer")
		return fmt.Errorf("exit status 1")
	}
	inst := NewFabricInstaller(srv.Client(), srv.URL, failExec)

	targetDir := t.TempDir()
	_, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.21.1", TargetDir: targetDir, JavaExe: "java.exe"}, nil)
	if err == nil {
		t.Fatal("installer 失敗應回錯,實得 nil")
	}
	assertTargetClean(t, targetDir)
}

func TestFabricInstaller_MissingOutputNoPartial(t *testing.T) {
	installerJar := []byte("fake")
	srv := fakeFabricMeta(t, "1.21.1", "0.16.5", "1.0.1", installerJar, true)

	// exec 成功回 0 但未產出 fabric-server-launch.jar → 應被輸出驗證擋下且不留半成品。
	inst := NewFabricInstaller(srv.Client(), srv.URL, fabricStyleFakeExec(t, "", false))

	targetDir := t.TempDir()
	_, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.21.1", TargetDir: targetDir, JavaExe: "java.exe"}, nil)
	if err == nil || !strings.Contains(err.Error(), fabricServerLaunchJar) {
		t.Fatalf("缺啟動 jar 應回輸出驗證錯誤,實得: %v", err)
	}
	assertTargetClean(t, targetDir)
}

// assertTargetClean 斷言 dir 下無任何檔案(installer 失敗後不留半成品),且無 .mc-loader 工作目錄殘留。
func assertTargetClean(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("讀取目錄失敗: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("失敗後不應留下任何內容,實見: %q", e.Name())
	}
	// installer 工作目錄建在父層(.mc-loader-*),失敗後應由 defer 清除。
	parent, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		return
	}
	for _, e := range parent {
		if strings.HasPrefix(e.Name(), ".mc-loader") {
			t.Fatalf("殘留 installer 工作目錄: %q", e.Name())
		}
	}
}
