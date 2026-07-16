package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argAfter 回傳 args 中 flag 的下一個元素(找不到回空字串)。假 exec 用來取得 installer 目的目錄。
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// fakeInstaller 是 ServerInstaller 的測試替身,記錄是否被呼叫並回傳預設結果。
type fakeInstaller struct {
	called bool
	ret    InstalledServer
	err    error
}

func (f *fakeInstaller) Install(_ context.Context, _ InstallRequest, _ ProgressFunc) (InstalledServer, error) {
	f.called = true
	return f.ret, f.err
}

func TestInstallServerByLoader_RoutesToCorrectInstaller(t *testing.T) {
	p := New(t.TempDir())

	vanilla := &fakeInstaller{ret: InstalledServer{ServerJar: "vanilla"}}
	paper := &fakeInstaller{ret: InstalledServer{ServerJar: "paper"}}
	fabric := &fakeInstaller{ret: InstalledServer{ServerJar: "fabric"}}
	forge := &fakeInstaller{ret: InstalledServer{StartScript: "forge"}}
	neoforge := &fakeInstaller{ret: InstalledServer{StartScript: "neoforge"}}
	p.Vanilla, p.Paper, p.Fabric, p.Forge, p.NeoForge = vanilla, paper, fabric, forge, neoforge

	cases := []struct {
		loader string
		want   *fakeInstaller
		mark   string
	}{
		{"vanilla", vanilla, "vanilla"},
		{"Paper", paper, "paper"},      // 大小寫不敏感。
		{" fabric ", fabric, "fabric"}, // 前後空白容忍。
		{"forge", forge, "forge"},
		{"NEOFORGE", neoforge, "neoforge"},
	}
	for _, c := range cases {
		t.Run(c.loader, func(t *testing.T) {
			got, err := p.InstallServerByLoader(context.Background(), c.loader, InstallRequest{}, nil)
			if err != nil {
				t.Fatalf("InstallServerByLoader(%q) 失敗: %v", c.loader, err)
			}
			if !c.want.called {
				t.Fatalf("loader %q 未路由到預期安裝器", c.loader)
			}
			if got.ServerJar != c.mark && got.StartScript != c.mark {
				t.Fatalf("loader %q 回傳標記不符,實得 %+v", c.loader, got)
			}
			c.want.called = false // 重置供後續斷言其他安裝器未被誤呼叫。
		})
	}
}

func TestInstallServerByLoader_UnknownLoaderErrors(t *testing.T) {
	p := New(t.TempDir())
	_, err := p.InstallServerByLoader(context.Background(), "spigot", InstallRequest{}, nil)
	if err == nil {
		t.Fatal("未知 loader 應回錯,實得 nil")
	}
	if got := err.Error(); !strings.Contains(got, "spigot") || !strings.Contains(got, "不支援") {
		t.Fatalf("錯誤訊息應點名未知 loader,實得: %v", err)
	}
}

func TestInstallServerByLoader_PropagatesInstallerError(t *testing.T) {
	p := New(t.TempDir())
	wantErr := errors.New("boom")
	p.Forge = &fakeInstaller{err: wantErr}
	_, err := p.InstallServerByLoader(context.Background(), "forge", InstallRequest{}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("安裝器錯誤應原樣傳回,實得: %v", err)
	}
}

func TestNew_WiresAllFiveInstallers(t *testing.T) {
	p := New(t.TempDir())
	if p.Vanilla == nil || p.Paper == nil || p.Fabric == nil || p.Forge == nil || p.NeoForge == nil {
		t.Fatalf("New 應建構全部五安裝器,實得 Vanilla=%v Paper=%v Fabric=%v Forge=%v NeoForge=%v",
			p.Vanilla != nil, p.Paper != nil, p.Fabric != nil, p.Forge != nil, p.NeoForge != nil)
	}
	if p.SteamCMD == nil {
		t.Fatal("New 應建構 SteamCMD 供應器")
	}
}

// ---- 共用假 installer exec ----

// forgeStyleFakeExec 回傳模擬 Forge/NeoForge installer --installServer 的假 exec:於目的目錄
// (--installServer 之後的參數)寫出現代結構檔案。argsRel 為 libraries 下 win_args.txt 的相對路徑;
// 若 omitWinArgs 為真則故意不寫該檔(模擬不支援的舊版結構/安裝失敗)。
func forgeStyleFakeExec(t *testing.T, argsRel string, omitWinArgs bool) InstallerExecFunc {
	t.Helper()
	return func(_ context.Context, _ string, args []string, onLine func(string)) error {
		dir := argAfter(args, "--installServer")
		if dir == "" {
			t.Fatalf("假 exec 未收到 --installServer 目的目錄,args=%v", args)
		}
		writeFile(t, filepath.Join(dir, "run.bat"), "@echo off\r\njava @user_jvm_args.txt @libraries/args.txt %*\r\n")
		writeFile(t, filepath.Join(dir, "run.sh"), "#!/usr/bin/env sh\njava @user_jvm_args.txt\n")
		writeFile(t, filepath.Join(dir, "user_jvm_args.txt"), "# JVM args\n-Xmx2G\n")
		if !omitWinArgs {
			writeFile(t, filepath.Join(dir, filepath.FromSlash(argsRel)), "--launchTarget forgeserver")
		}
		onLine("The server installed successfully")
		return nil
	}
}

// fabricStyleFakeExec 回傳模擬 Fabric installer server 子命令的假 exec:於 -dir 目的目錄寫出
// fabric-server-launch.jar(內容為 launch)。writeLaunch 為假時不寫(模擬 installer 未產出)。
func fabricStyleFakeExec(t *testing.T, launch string, writeLaunch bool) InstallerExecFunc {
	t.Helper()
	return func(_ context.Context, _ string, args []string, onLine func(string)) error {
		dir := argAfter(args, "-dir")
		if dir == "" {
			t.Fatalf("假 exec 未收到 -dir 目的目錄,args=%v", args)
		}
		if writeLaunch {
			writeFile(t, filepath.Join(dir, fabricServerLaunchJar), launch)
			writeFile(t, filepath.Join(dir, "server.jar"), "vanilla-server") // -downloadMinecraft 產物。
		}
		onLine("Done installing Fabric server")
		return nil
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建立目錄失敗: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("寫入 %q 失敗: %v", path, err)
	}
}
