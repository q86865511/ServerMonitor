package agent

// NativeBackend 的模組包安裝(native-backend R11/T10)單元測試:遠端 Modpack 於 Create 期間安裝、
// 手動上傳(WriteMountFile)於檔案抵達後安裝、範本未宣告 mods_dir 時的明確錯誤。真實 mrpack 解析
// /下載邏輯屬 internal/agent/provision 套件單元測(modprovider_test.go/modrinth_test.go),本檔只驗
// NativeBackend 與 provisionRunner 之間的接線。

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"servermonitor/internal/protocol"
)

// nativeModpackSpec 產生一個宣告 native mods 落位目錄、免啟動供應(kind="")的 InstanceSpec,
// 供只驗證 Create 供應/模組包階段的測試使用(不需真的起行程)。
func nativeModpackSpec(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:     uuid,
		Runtime:  "native",
		Variant:  "fabric",
		DataDirs: []string{"/data"},
		Labels:   map[string]string{"gsm.uuid": uuid},
		Native: &protocol.NativeSpecPayload{
			Provision: protocol.NativeProvision{Kind: "", MCVersion: "1.20.1", Loader: "fabric"},
			Launch:    protocol.NativeLaunch{Command: []string{"noop"}},
			ModsDir:   "mods",
		},
	}
}

func TestNativeBackend_Create_InstallsRemoteModpack(t *testing.T) {
	prov := &fakeProv{javaPath: os.Args[0]}
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   filepath.Join(t.TempDir(), "data"),
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		CacheRoot:  filepath.Join(t.TempDir(), "cache"),
		Node:       "n1",
		Prov:       prov,
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	defer b.Close()

	spec := nativeModpackSpec("uuid-modpack")
	// 比照 minecraft 範本 working_dir="data":TargetDir 必須跟隨 workDir(複審 A 的迴歸鎖)。
	spec.Native.Launch.WorkingDir = "data"
	spec.Modpack = &protocol.ModpackRef{Type: "modrinth", Ref: "cobblemon-fabric"}

	if _, err := b.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(prov.modpackCalls) != 1 {
		t.Fatalf("InstallModpack 呼叫次數=%d,期望 1", len(prov.modpackCalls))
	}
	got := prov.modpackCalls[0]
	// #2×#7(複審 A):TargetDir 為伺服器工作目錄(<root>/data,provider 的 files[].path 自帶
	// mods/ 前綴),非實例根也非 <root>/mods。
	wantDir := filepath.Join(b.instanceDataRoot("uuid-modpack"), "data")
	if got.Type != "modrinth" || got.Ref != "cobblemon-fabric" {
		t.Errorf("Type/Ref = %q/%q,期望 modrinth/cobblemon-fabric", got.Type, got.Ref)
	}
	if got.TargetDir != wantDir {
		t.Errorf("TargetDir = %q,期望實例根 %q", got.TargetDir, wantDir)
	}
	if got.MCVersion != "1.20.1" || got.Loader != "fabric" {
		t.Errorf("MCVersion/Loader = %q/%q,期望 1.20.1/fabric", got.MCVersion, got.Loader)
	}
	if got.ArchivePath != "" {
		t.Errorf("遠端來源不應帶 ArchivePath: %q", got.ArchivePath)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "installed.marker")); err != nil {
		t.Errorf("模組包安裝標記檔缺失: %v", err)
	}
}

// TestNativeBackend_Create_NoModpack_SkipsInstall:spec.Modpack 為 nil 時不呼叫 InstallModpack
// (既有 Create 流程不受影響,回歸確認)。
func TestNativeBackend_Create_NoModpack_SkipsInstall(t *testing.T) {
	prov := &fakeProv{javaPath: os.Args[0]}
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   filepath.Join(t.TempDir(), "data"),
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		CacheRoot:  filepath.Join(t.TempDir(), "cache"),
		Node:       "n1",
		Prov:       prov,
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	defer b.Close()

	if _, err := b.Create(context.Background(), nativeModpackSpec("uuid-nomodpack")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(prov.modpackCalls) != 0 {
		t.Fatalf("未指定 Modpack 卻呼叫了 InstallModpack: %+v", prov.modpackCalls)
	}
}

// TestNativeBackend_Create_ModpackWithoutModsDir_FailsExplicit:範本未宣告 native mods_dir 時,
// 不臆測落位路徑,回明確錯誤且不呼叫 InstallModpack。
func TestNativeBackend_Create_ModpackWithoutModsDir_FailsExplicit(t *testing.T) {
	prov := &fakeProv{javaPath: os.Args[0]}
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   filepath.Join(t.TempDir(), "data"),
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		CacheRoot:  filepath.Join(t.TempDir(), "cache"),
		Node:       "n1",
		Prov:       prov,
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	defer b.Close()

	spec := nativeModpackSpec("uuid-nomodsdir")
	spec.Native.ModsDir = ""
	spec.Modpack = &protocol.ModpackRef{Type: "modrinth", Ref: "cobblemon-fabric"}

	_, err = b.Create(context.Background(), spec)
	if err == nil {
		t.Fatal("期望 Create 失敗(未宣告 mods_dir),實際成功")
	}
	if len(prov.modpackCalls) != 0 {
		t.Errorf("mods_dir 缺失時不應呼叫 InstallModpack: %+v", prov.modpackCalls)
	}
}

// TestNativeBackend_Create_ModpackInstallError_PropagatesFailure:安裝失敗時 Create 整體失敗
// (供核心既有 journal 回滾機制接手,native.go 本身不做特殊處理)。
func TestNativeBackend_Create_ModpackInstallError_PropagatesFailure(t *testing.T) {
	wantErr := errors.New("boom")
	prov := &fakeProv{javaPath: os.Args[0], modpackErr: wantErr}
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   filepath.Join(t.TempDir(), "data"),
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		CacheRoot:  filepath.Join(t.TempDir(), "cache"),
		Node:       "n1",
		Prov:       prov,
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	defer b.Close()

	spec := nativeModpackSpec("uuid-modpackfail")
	spec.Modpack = &protocol.ModpackRef{Type: "modrinth", Ref: "cobblemon-fabric"}

	_, err = b.Create(context.Background(), spec)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Create 應包裝並回傳安裝錯誤,實得: %v", err)
	}
}

// ---- 手動上傳路徑(WriteMountFile,見 mount.go)----

// zipWithEntry 建立一個只含單一具名條目(內容任意)的最小 zip 位元組,供
// detectModpackArchiveType 的辨識測試使用(不需是合法的 mrpack/cfzip 內容)。
func zipWithEntry(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("建立 zip 條目失敗: %v", err)
	}
	if _, err := w.Write([]byte(`{}`)); err != nil {
		t.Fatalf("寫入 zip 條目失敗: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("關閉 zip 失敗: %v", err)
	}
	return buf.Bytes()
}

// createNativeInstanceWithModpackMount 建立一個宣告 modpack 具名掛載的 native 實例,供
// WriteMountFile 測試使用。回傳 backend 與 uuid。
func createNativeInstanceWithModpackMount(t *testing.T, prov provisionRunner) (*NativeBackend, string) {
	t.Helper()
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   filepath.Join(t.TempDir(), "data"),
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		CacheRoot:  filepath.Join(t.TempDir(), "cache"),
		Node:       "n1",
		Prov:       prov,
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	uuid := "uuid-mount"
	spec := nativeModpackSpec(uuid)
	// 比照 minecraft 範本 working_dir="data":模組包落位必須跟隨 workDir(複審 A 的迴歸鎖)。
	spec.Native.Launch.WorkingDir = "data"
	spec.Mounts = []protocol.MountSpec{{Name: "modpack", ContainerPath: "/modpacks"}}
	if _, err := b.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return b, uuid
}

// TestNativeBackend_WriteMountFile_InstallsManualMrpack:手動上傳的 .mrpack(以
// modrinth.index.json 索引檔辨識)於上傳完成後立即解進 mods(R11 手動上傳路徑,Create 早於上傳
// 完成,見 native.go Create 與 mount.go 的說明)。
func TestNativeBackend_WriteMountFile_InstallsManualMrpack(t *testing.T) {
	prov := &fakeProv{javaPath: os.Args[0]}
	b, uuid := createNativeInstanceWithModpackMount(t, prov)

	data := zipWithEntry(t, "modrinth.index.json")
	if err := b.WriteMountFile(context.Background(), uuid, "modpack", "world.mrpack", bytes.NewReader(data)); err != nil {
		t.Fatalf("WriteMountFile: %v", err)
	}

	if len(prov.modpackCalls) != 1 {
		t.Fatalf("InstallModpack 呼叫次數=%d,期望 1", len(prov.modpackCalls))
	}
	got := prov.modpackCalls[0]
	if got.Type != "modrinth" {
		t.Errorf("Type = %q,期望 modrinth(依 modrinth.index.json 辨識)", got.Type)
	}
	wantArchive := filepath.Join(b.instanceDataRoot(uuid), mountsSubdir, "modpack", "world.mrpack")
	if got.ArchivePath != wantArchive {
		t.Errorf("ArchivePath = %q,期望 %q", got.ArchivePath, wantArchive)
	}
	if got.Ref != "" {
		t.Errorf("手動上傳不應帶 Ref: %q", got.Ref)
	}
	// #2×#7(複審 A):TargetDir 為伺服器工作目錄(<root>/data),非實例根也非 <root>/mods。
	wantDir := filepath.Join(b.instanceDataRoot(uuid), "data")
	if got.TargetDir != wantDir {
		t.Errorf("TargetDir = %q,期望工作目錄 %q", got.TargetDir, wantDir)
	}
}

// TestNativeBackend_WriteMountFile_ManualCfzip_RoutedAsCurseForge:手動上傳的 CurseForge zip
// (以 manifest.json 索引檔辨識)被辨識為 "curseforge" 型別並照樣轉呼 provisionRunner——native
// 是否支援由 provisionAdapter 端明確拒絕(見 provision_adapter.go InstallModpack),本層只負責
// 正確辨識與轉發;此處以 fakeProv 模擬 adapter 的拒絕行為,驗證錯誤會透傳出 WriteMountFile。
func TestNativeBackend_WriteMountFile_ManualCfzip_RoutedAsCurseForge(t *testing.T) {
	wantErr := errors.New("agent: CurseForge 模組包於 native 模式尚未支援(R14)")
	prov := &fakeProv{javaPath: os.Args[0], modpackErr: wantErr}
	b, uuid := createNativeInstanceWithModpackMount(t, prov)

	data := zipWithEntry(t, "manifest.json")
	err := b.WriteMountFile(context.Background(), uuid, "modpack", "pack.zip", bytes.NewReader(data))
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("期望透傳 CurseForge 尚未支援錯誤,實得: %v", err)
	}
	if len(prov.modpackCalls) != 1 || prov.modpackCalls[0].Type != "curseforge" {
		t.Fatalf("應以 Type=curseforge 呼叫一次 InstallModpack,實得: %+v", prov.modpackCalls)
	}
}

// TestNativeBackend_WriteMountFile_NonModpackFile_NoInstall:一般掛載檔案(非模組包封存檔,
// 如不含索引檔的任意檔案)寫入後不觸發模組包安裝(mounts 具名掛載本非模組包專用機制)。
func TestNativeBackend_WriteMountFile_NonModpackFile_NoInstall(t *testing.T) {
	prov := &fakeProv{javaPath: os.Args[0]}
	b, uuid := createNativeInstanceWithModpackMount(t, prov)

	if err := b.WriteMountFile(context.Background(), uuid, "modpack", "readme.txt", bytes.NewReader([]byte("hello"))); err != nil {
		t.Fatalf("WriteMountFile: %v", err)
	}
	if len(prov.modpackCalls) != 0 {
		t.Fatalf("非模組包檔案不應觸發 InstallModpack: %+v", prov.modpackCalls)
	}
}
