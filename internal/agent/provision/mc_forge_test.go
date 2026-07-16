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

// fakeForge 模擬 Forge promotions_slim.json 與 maven installer 制品(含 .sha1)。promos 直接以傳入
// map 提供,便於測 recommended/latest 的取捨。
func fakeForge(t *testing.T, promos map[string]string, mcVersion, forgeVer string, installerJar []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	coord := mcVersion + "-" + forgeVer
	jarPath := fmt.Sprintf("/net/minecraftforge/forge/%s/forge-%s-installer.jar", coord, coord)

	mux.HandleFunc("/net/minecraftforge/forge/promotions_slim.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(forgePromotions{Promos: promos})
	})
	mux.HandleFunc(jarPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(installerJar)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(installerJar)
	})
	mux.HandleFunc(jarPath+".sha1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s\n", sha1Hex(installerJar))
	})

	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestForgeInstaller_InstallsProducesRunScript(t *testing.T) {
	installerJar := []byte("fake-forge-installer")
	mc, forgeVer := "1.21.1", "52.1.0"
	promos := map[string]string{mc + "-recommended": forgeVer, mc + "-latest": "52.1.15"}
	srv := fakeForge(t, promos, mc, forgeVer, installerJar)

	argsRel := fmt.Sprintf("libraries/net/minecraftforge/forge/%s-%s/win_args.txt", mc, forgeVer)
	var gotArgs []string
	exec := func(ctx context.Context, java string, args []string, onLine func(string)) error {
		gotArgs = args
		return forgeStyleFakeExec(t, argsRel, false)(ctx, java, args, onLine)
	}
	inst := NewForgeInstaller(srv.Client(), srv.URL, srv.URL, exec)

	targetDir := t.TempDir()
	got, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: mc, TargetDir: targetDir, JavaExe: "java.exe"}, nil)
	if err != nil {
		t.Fatalf("Install 失敗: %v", err)
	}

	// 選 recommended(52.1.0),非 latest。
	if !strings.Contains(strings.Join(gotArgs, " "), "--installServer") {
		t.Fatalf("installer 參數應含 --installServer,實得: %v", gotArgs)
	}
	wantScript := filepath.Join(targetDir, "run.bat")
	if got.StartScript != wantScript {
		t.Fatalf("StartScript = %q,期望 %q", got.StartScript, wantScript)
	}
	if got.ArgsFile != filepath.Join(targetDir, "user_jvm_args.txt") {
		t.Fatalf("ArgsFile = %q", got.ArgsFile)
	}
	// 產物落地檢查:run.bat 與 libraries 下 win_args.txt 都在 TargetDir。
	for _, rel := range []string{"run.bat", "user_jvm_args.txt", argsRel} {
		if _, err := os.Stat(filepath.Join(targetDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("預期產物 %q 不存在: %v", rel, err)
		}
	}
}

func TestForgeInstaller_FallsBackToLatest(t *testing.T) {
	installerJar := []byte("fake-forge-latest")
	mc, forgeVer := "1.21.1", "52.1.15"
	promos := map[string]string{mc + "-latest": forgeVer} // 無 recommended。
	srv := fakeForge(t, promos, mc, forgeVer, installerJar)

	argsRel := fmt.Sprintf("libraries/net/minecraftforge/forge/%s-%s/win_args.txt", mc, forgeVer)
	inst := NewForgeInstaller(srv.Client(), srv.URL, srv.URL, forgeStyleFakeExec(t, argsRel, false))

	if _, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: mc, TargetDir: t.TempDir(), JavaExe: "java.exe"}, nil); err != nil {
		t.Fatalf("無 recommended 時應退回 latest,實得錯誤: %v", err)
	}
}

func TestForgeInstaller_VersionNotFound(t *testing.T) {
	srv := fakeForge(t, map[string]string{"1.20.4-recommended": "49.0.0"}, "1.20.4", "49.0.0", []byte("x"))
	inst := NewForgeInstaller(srv.Client(), srv.URL, srv.URL, forgeStyleFakeExec(t, "libraries/x/win_args.txt", false))

	_, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.99.9", TargetDir: t.TempDir(), JavaExe: "java.exe"}, nil)
	if err == nil || !strings.Contains(err.Error(), "1.99.9") {
		t.Fatalf("未知 MC 版本應回錯並點名版本,實得: %v", err)
	}
}

func TestForgeInstaller_LegacyStructureRejectedNoPartial(t *testing.T) {
	installerJar := []byte("fake-forge-legacy")
	mc, forgeVer := "1.21.1", "52.1.0"
	promos := map[string]string{mc + "-recommended": forgeVer}
	srv := fakeForge(t, promos, mc, forgeVer, installerJar)

	argsRel := fmt.Sprintf("libraries/net/minecraftforge/forge/%s-%s/win_args.txt", mc, forgeVer)
	// omitWinArgs=true 模擬舊版 forge-universal 結構(無 win_args.txt)→ 應被驗證擋下。
	inst := NewForgeInstaller(srv.Client(), srv.URL, srv.URL, forgeStyleFakeExec(t, argsRel, true))

	targetDir := t.TempDir()
	_, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: mc, TargetDir: targetDir, JavaExe: "java.exe"}, nil)
	if err == nil || !strings.Contains(err.Error(), "win_args.txt") {
		t.Fatalf("缺 win_args.txt 應回不支援結構錯誤,實得: %v", err)
	}
	assertTargetClean(t, targetDir)
}

func TestForgeInstaller_ChecksumMismatchNoPartial(t *testing.T) {
	installerJar := []byte("fake-forge-installer")
	mc, forgeVer := "1.21.1", "52.1.0"
	promos := map[string]string{mc + "-recommended": forgeVer}

	// 自建 mux:installer.jar.sha1 回傳錯誤摘要 → download 應以 ErrChecksumMismatch 失敗。
	mux := http.NewServeMux()
	coord := mc + "-" + forgeVer
	jarPath := fmt.Sprintf("/net/minecraftforge/forge/%s/forge-%s-installer.jar", coord, coord)
	mux.HandleFunc("/net/minecraftforge/forge/promotions_slim.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(forgePromotions{Promos: promos})
	})
	mux.HandleFunc(jarPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(installerJar)))
		_, _ = w.Write(installerJar)
	})
	mux.HandleFunc(jarPath+".sha1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, strings.Repeat("0", 40)) // 錯誤 sha1。
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	// exec 不應被呼叫(下載即失敗);若被呼叫則測試 fail。
	exec := func(ctx context.Context, java string, args []string, onLine func(string)) error {
		t.Fatal("校驗失敗後不應執行 installer")
		return nil
	}
	inst := NewForgeInstaller(srv.Client(), srv.URL, srv.URL, exec)

	targetDir := t.TempDir()
	_, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: mc, TargetDir: targetDir, JavaExe: "java.exe"}, nil)
	if err == nil {
		t.Fatal("installer jar 校驗不符應回錯,實得 nil")
	}
	assertTargetClean(t, targetDir)
}
