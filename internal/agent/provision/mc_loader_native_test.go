//go:build native

// Package provision 的 loader 安裝器整合測試:實際執行官方 Fabric/Forge/NeoForge installer
// (需真 JRE 與網路),斷言產出可啟動配置的檔案結構。以 build tag `native` 隔離,一般 CI/開發
// 的 `go test ./...` 不會觸發;需真 JRE 的環境以 `go test -tags native ./internal/agent/provision/`
// 執行。找不到 java 時整測自動 skip(不誤判為失敗)。
//
// 這批測試涵蓋單元測(假 exec)無法覆蓋的部分:installer 真實輸出結構與檔名,是 spike 中「查不到
// 的實測結構以執行 installer 後檔案存在性斷言涵蓋」的落地(見 requirements Q2 / design 風險節)。
package provision

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// resolveJavaExe 找一個可用的 java 執行檔:優先 PATH 上的 java,退回 JAVA_HOME/bin/java(.exe)。
// 找不到回空字串(呼叫端 skip)。
func resolveJavaExe(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("java"); err == nil {
		return p
	}
	if home := os.Getenv("JAVA_HOME"); home != "" {
		for _, name := range []string{"java.exe", "java"} {
			cand := filepath.Join(home, "bin", name)
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
	}
	return ""
}

func TestNative_FabricInstallerRealOutput(t *testing.T) {
	java := resolveJavaExe(t)
	if java == "" {
		t.Skip("找不到 java(未安裝 JRE 或不在 PATH/JAVA_HOME),跳過真 installer 整合測")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	inst := NewFabricInstaller(nil, "", nil) // 真 meta API + 真下載 + 真 exec。
	targetDir := t.TempDir()
	got, err := inst.Install(ctx,
		InstallRequest{MCVersion: "1.21.1", TargetDir: targetDir, JavaExe: java}, nil)
	if err != nil {
		t.Fatalf("Fabric 真 installer 失敗: %v", err)
	}
	if _, err := os.Stat(got.ServerJar); err != nil {
		t.Fatalf("Fabric 啟動 jar 應存在: %v", err)
	}
}

func TestNative_ForgeInstallerRealOutput(t *testing.T) {
	java := resolveJavaExe(t)
	if java == "" {
		t.Skip("找不到 java,跳過真 installer 整合測")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	inst := NewForgeInstaller(nil, "", "", nil)
	targetDir := t.TempDir()
	got, err := inst.Install(ctx,
		InstallRequest{MCVersion: "1.21.1", TargetDir: targetDir, JavaExe: java}, nil)
	if err != nil {
		t.Fatalf("Forge 真 installer 失敗: %v", err)
	}
	// 現代結構:run.bat + user_jvm_args.txt 必須存在。
	for _, f := range []string{got.StartScript, got.ArgsFile} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("Forge 產物 %q 應存在: %v", f, err)
		}
	}
}

func TestNative_NeoForgeInstallerRealOutput(t *testing.T) {
	java := resolveJavaExe(t)
	if java == "" {
		t.Skip("找不到 java,跳過真 installer 整合測")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	inst := NewNeoForgeInstaller(nil, "", nil)
	targetDir := t.TempDir()
	got, err := inst.Install(ctx,
		InstallRequest{MCVersion: "1.21.1", TargetDir: targetDir, JavaExe: java}, nil)
	if err != nil {
		t.Fatalf("NeoForge 真 installer 失敗: %v", err)
	}
	for _, f := range []string{got.StartScript, got.ArgsFile} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("NeoForge 產物 %q 應存在: %v", f, err)
		}
	}
}
