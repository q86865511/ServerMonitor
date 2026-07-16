package provision

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNeoForge 模擬 NeoForged maven-metadata.xml 與 installer 制品(含 .sha1)。versions 為
// maven-metadata 列出的全部版本;installer 端點只對 wantVer 供檔(其餘版本無檔,確認選版正確)。
func fakeNeoForge(t *testing.T, versions []string, wantVer string, installerJar []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/releases/net/neoforged/neoforge/maven-metadata.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		var b strings.Builder
		b.WriteString("<metadata><versioning><versions>")
		for _, v := range versions {
			b.WriteString("<version>" + v + "</version>")
		}
		b.WriteString("</versions></versioning></metadata>")
		_, _ = w.Write([]byte(b.String()))
	})
	jarPath := fmt.Sprintf("/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar", wantVer, wantVer)
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

func TestNeoForgeInstaller_SelectsLatestStableForMCVersion(t *testing.T) {
	installerJar := []byte("fake-neoforge-installer")
	// MC 1.21.1 → 前綴 21.1.;應選最高 build 的穩定版 21.1.100(略過 beta 與其他 MC 前綴)。
	versions := []string{
		"20.4.237",      // 別的 MC,略過。
		"21.1.50",       // 21.1. 前綴,較低。
		"21.1.100",      // 21.1. 前綴,最高穩定 → 應選中。
		"21.1.120-beta", // beta,略過(有穩定版時)。
		"21.0.10",       // MC 1.21.0 前綴,略過。
	}
	wantVer := "21.1.100"
	srv := fakeNeoForge(t, versions, wantVer, installerJar)

	argsRel := fmt.Sprintf("libraries/net/neoforged/neoforge/%s/win_args.txt", wantVer)
	inst := NewNeoForgeInstaller(srv.Client(), srv.URL, forgeStyleFakeExec(t, argsRel, false))

	targetDir := t.TempDir()
	got, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.21.1", TargetDir: targetDir, JavaExe: "java.exe"}, nil)
	if err != nil {
		t.Fatalf("Install 失敗(應選中 %s): %v", wantVer, err)
	}
	if got.StartScript != filepath.Join(targetDir, "run.bat") {
		t.Fatalf("StartScript = %q", got.StartScript)
	}
}

func TestNeoForgeInstaller_FallsBackToBetaWhenNoStable(t *testing.T) {
	installerJar := []byte("fake-neoforge-beta")
	versions := []string{"21.2.0-beta", "21.2.5-beta"} // 前綴 21.2. 全為 beta。
	wantVer := "21.2.5-beta"
	srv := fakeNeoForge(t, versions, wantVer, installerJar)

	argsRel := fmt.Sprintf("libraries/net/neoforged/neoforge/%s/win_args.txt", wantVer)
	inst := NewNeoForgeInstaller(srv.Client(), srv.URL, forgeStyleFakeExec(t, argsRel, false))

	if _, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.21.2", TargetDir: t.TempDir(), JavaExe: "java.exe"}, nil); err != nil {
		t.Fatalf("同前綴全 beta 時應退回最高 beta,實得錯誤: %v", err)
	}
}

func TestNeoForgeInstaller_NoMatchingVersion(t *testing.T) {
	srv := fakeNeoForge(t, []string{"20.4.100"}, "20.4.100", []byte("x"))
	inst := NewNeoForgeInstaller(srv.Client(), srv.URL, forgeStyleFakeExec(t, "libraries/x/win_args.txt", false))

	_, err := inst.Install(context.Background(),
		InstallRequest{MCVersion: "1.21.1", TargetDir: t.TempDir(), JavaExe: "java.exe"}, nil)
	if err == nil || !strings.Contains(err.Error(), "21.1.") {
		t.Fatalf("無匹配版本應回錯並點名前綴,實得: %v", err)
	}
}

func TestNeoForgePrefix(t *testing.T) {
	cases := map[string]string{
		"1.21.1": "21.1.",
		"1.21":   "21.0.",
		"1.20.4": "20.4.",
	}
	for mc, want := range cases {
		got, err := neoForgePrefix(mc)
		if err != nil {
			t.Fatalf("neoForgePrefix(%q) 錯誤: %v", mc, err)
		}
		if got != want {
			t.Fatalf("neoForgePrefix(%q) = %q,期望 %q", mc, got, want)
		}
	}
	if _, err := neoForgePrefix("2.0.0"); err == nil {
		t.Fatal("非 1.x 版本應回錯")
	}
}

func TestNeoForgeBuild(t *testing.T) {
	cases := []struct {
		v, prefix string
		want      int
		ok        bool
	}{
		{"21.1.238", "21.1.", 238, true},
		{"21.1.240-beta", "21.1.", 240, true},
		{"21.1.x", "21.1.", 0, false},
	}
	for _, c := range cases {
		got, ok := neoForgeBuild(c.v, c.prefix)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("neoForgeBuild(%q,%q) = (%d,%v),期望 (%d,%v)", c.v, c.prefix, got, ok, c.want, c.ok)
		}
	}
}
