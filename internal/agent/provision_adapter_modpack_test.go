package agent

// provisionAdapter.InstallModpack 的路由單元測試(native-backend R11/T10):"curseforge" 尚未
// 支援時回明確錯誤且不誤觸 ModProviders 查找;"modrinth" 正確委派並轉譯 Ref/ArchivePath。
// 真實 mrpack 解析/下載屬 internal/agent/provision 套件單元測(modprovider_test.go/modrinth_test.go)。

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"servermonitor/internal/protocol"
)

func TestProvisionAdapter_InstallModpack_CurseForgeNotSupported(t *testing.T) {
	adapter := NewProvisionAdapter(t.TempDir())
	err := adapter.InstallModpack(context.Background(), ModpackInstallRequest{
		Type: "curseforge", Ref: "all-the-mods-8", TargetDir: t.TempDir(),
	}, nil)
	if err == nil {
		t.Fatal("期望 CurseForge 回明確錯誤,實際成功")
	}
	const want = "CurseForge 模組包於 native 模式尚未支援"
	if got := err.Error(); !strings.Contains(got, want) {
		t.Fatalf("錯誤訊息 = %q,應包含 %q", got, want)
	}
}

func TestProvisionAdapter_InstallModpack_UnknownType(t *testing.T) {
	adapter := NewProvisionAdapter(t.TempDir())
	err := adapter.InstallModpack(context.Background(), ModpackInstallRequest{
		Type: "nexusmods", Ref: "x", TargetDir: t.TempDir(),
	}, nil)
	if err == nil {
		t.Fatal("期望未知來源型別回錯,實際成功")
	}
}

// TestProvisionAdapter_InstallModpack_ManualArchive_SkipsResolve:ArchivePath 非空時(手動上傳)
// 直接安裝該檔,不經 Ref 解析(以一個只含 modrinth.index.json、無 files 的最小 mrpack 驗證能
// 解析與落地完成,不觸網)。
func TestProvisionAdapter_InstallModpack_ManualArchive_SkipsResolve(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "manual.mrpack")
	indexJSON := `{"formatVersion":1,"game":"minecraft","versionId":"v1","name":"manual","files":[],"dependencies":{"minecraft":"1.20.1"}}`
	if err := writeMinimalMrpack(archive, indexJSON); err != nil {
		t.Fatalf("寫入測試 mrpack 失敗: %v", err)
	}

	adapter := NewProvisionAdapter(t.TempDir())
	targetDir := t.TempDir()
	if err := adapter.InstallModpack(context.Background(), ModpackInstallRequest{
		Type:        "modrinth",
		ArchivePath: archive,
		TargetDir:   targetDir,
		MCVersion:   "1.20.1",
	}, func(protocol.ProvisionProgress) {}); err != nil {
		t.Fatalf("InstallModpack: %v", err)
	}
}

// writeMinimalMrpack 寫出一個只含 modrinth.index.json 的最小 zip,供 adapter 路由測試使用
// (內容是否為完整 mrpack 由 provision 套件自身單元測把關,此處只需能通過解析)。
func writeMinimalMrpack(path, indexJSON string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, err := zw.Create("modrinth.index.json")
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(indexJSON)); err != nil {
		return err
	}
	return zw.Close()
}
