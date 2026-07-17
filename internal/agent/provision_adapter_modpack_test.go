package agent

// provisionAdapter.InstallModpack 的路由單元測試(native-backend R11/R14):無 key 建置的
// "curseforge" 回明確的「未啟用」錯誤且不誤觸 ModProviders 通用查無訊息;"modrinth" 正確委派並
// 轉譯 Ref/ArchivePath。真實 mrpack/cfzip 解析/下載屬 internal/agent/provision 套件單元測
// (modprovider_test.go/modrinth_test.go/curseforge_test.go)。

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"servermonitor/internal/protocol"
)

func TestProvisionAdapter_InstallModpack_CurseForgeDisabledWithoutKey(t *testing.T) {
	// 無 key 建置(NewProvisionAdapter 不帶 WithCurseForgeAPIKey):curseforge 未註冊 ModProvider,
	// 回明確的「未啟用」錯誤(而非通用「不支援的模組包來源」)。
	adapter := NewProvisionAdapter(t.TempDir())
	err := adapter.InstallModpack(context.Background(), ModpackInstallRequest{
		Type: "curseforge", Ref: "123:456", TargetDir: t.TempDir(),
	}, nil)
	if err == nil {
		t.Fatal("期望無 key 時 CurseForge 回明確錯誤,實際成功")
	}
	const want = "CurseForge 模組包未啟用"
	if got := err.Error(); !strings.Contains(got, want) {
		t.Fatalf("錯誤訊息 = %q,應包含 %q", got, want)
	}
	if adapter.CurseForgeEnabled() {
		t.Error("無 key 時 CurseForgeEnabled 應為 false")
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
