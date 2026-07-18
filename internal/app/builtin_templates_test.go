package app

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"servermonitor/internal/core"
)

// mapFSTemplate 是 fstest.MapFS 版本的最小合法範本(與 minimalTemplate 同構,id 相異以便區分)。
func mapFSTemplate(id string) string {
	return `schema_version = 1
id = "` + id + `"
name = "Test Game"
runtime = "docker"
data_dirs = ["/data"]

[docker]
image = "busybox:latest"
`
}

// TestExtractBuiltinTemplates_LoadedByEngine 驗證(a):BuiltinFS 抽出後,engine.LoadDir 該目錄
// 可列出範本(含 icons/ 子目錄一併抽出,不影響範本載入)。
func TestExtractBuiltinTemplates_LoadedByEngine(t *testing.T) {
	mfs := fstest.MapFS{
		"templates/testgame.toml":  {Data: []byte(mapFSTemplate("testgame"))},
		"templates/icons/icon.png": {Data: []byte("\x89PNG\r\n\x1a\n")},
	}
	destDir := filepath.Join(t.TempDir(), subDirBuiltinTemplates)

	if err := extractBuiltinTemplates(mfs, destDir); err != nil {
		t.Fatalf("extractBuiltinTemplates: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "testgame.toml")); err != nil {
		t.Fatalf("抽出的範本檔不存在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "icons", "icon.png")); err != nil {
		t.Fatalf("抽出的 icon 檔不存在: %v", err)
	}

	engine := core.NewTemplateEngine(nil, nil)
	n, err := engine.LoadDir(destDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if n != 1 {
		t.Fatalf("載入份數 = %d, 期望 1", n)
	}
	if _, ok := engine.Get("testgame"); !ok {
		t.Fatal("engine 應列得出 testgame 範本")
	}
	if path, ok := engine.IconPath("testgame"); ok {
		t.Logf("icon 鏈路額外驗證:IconPath 解析到 %s(本範本未宣告 icon 屬性,理論上應 false)", path)
	}
}

// TestExtractBuiltinTemplates_OverwritesStale 驗證(b):二次啟動時,若內建範本已更新(舊範本
// 被移除、新範本加入),抽出目錄應完整覆寫——舊檔不殘留(R14 icon 路徑安全判定以磁碟實體檔案
// 為準,殘留舊檔會讓已刪範本的圖示仍可被服務)。
func TestExtractBuiltinTemplates_OverwritesStale(t *testing.T) {
	destDir := filepath.Join(t.TempDir(), subDirBuiltinTemplates)

	first := fstest.MapFS{
		"templates/old.toml": {Data: []byte(mapFSTemplate("old"))},
	}
	if err := extractBuiltinTemplates(first, destDir); err != nil {
		t.Fatalf("首次抽出: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "old.toml")); err != nil {
		t.Fatalf("首次抽出的 old.toml 應存在: %v", err)
	}

	// 模擬範本更新:第二版移除 old.toml、新增 new.toml。
	second := fstest.MapFS{
		"templates/new.toml": {Data: []byte(mapFSTemplate("new"))},
	}
	if err := extractBuiltinTemplates(second, destDir); err != nil {
		t.Fatalf("二次抽出: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "old.toml")); !os.IsNotExist(err) {
		t.Fatalf("舊檔 old.toml 應被覆寫移除,實際 err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "new.toml")); err != nil {
		t.Fatalf("新檔 new.toml 應存在: %v", err)
	}

	engine := core.NewTemplateEngine(nil, nil)
	if _, err := engine.LoadDir(destDir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if _, ok := engine.Get("old"); ok {
		t.Fatal("engine 不應再列出已被覆寫移除的 old 範本")
	}
	if _, ok := engine.Get("new"); !ok {
		t.Fatal("engine 應列出覆寫後的 new 範本")
	}
}

// TestExtractBuiltinTemplates_FailurePreservesPrevious 驗證:抽出中途失敗(此處以缺 templates/
// 根目錄的 FS 觸發走訪錯誤)時,前一份可用副本不被摧毀,呼叫端可沿用舊目錄載入。
func TestExtractBuiltinTemplates_FailurePreservesPrevious(t *testing.T) {
	destDir := filepath.Join(t.TempDir(), subDirBuiltinTemplates)

	good := fstest.MapFS{
		"templates/old.toml": {Data: []byte(mapFSTemplate("old"))},
	}
	if err := extractBuiltinTemplates(good, destDir); err != nil {
		t.Fatalf("首次抽出: %v", err)
	}

	bad := fstest.MapFS{} // 無 templates/ 根目錄 → WalkDir 必錯
	if err := extractBuiltinTemplates(bad, destDir); err == nil {
		t.Fatal("預期抽出失敗回錯,實際成功")
	}
	if _, err := os.Stat(filepath.Join(destDir, "old.toml")); err != nil {
		t.Fatalf("抽出失敗不應摧毀前一份副本,old.toml 應仍存在: %v", err)
	}
	if _, err := os.Stat(destDir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("失敗後暫存目錄應被清除,實際 err=%v", err)
	}
}

// TestLoadTemplates_ExtractFailureLogged 驗證(c):抽出目的地無法建立(父路徑被一般檔案佔用)
// 時,loadTemplates 不 panic、且把錯誤記入 log(不再靜默吞掉),範本清單維持空。
func TestLoadTemplates_ExtractFailureLogged(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("建立 blocker 檔: %v", err)
	}
	// dataRoot 之下的 subDirBuiltinTemplates 目的地,其父目錄 "blocker" 實為一般檔案,
	// MkdirAll 必失敗。
	dataRoot := blocker

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	mfs := fstest.MapFS{
		"templates/testgame.toml": {Data: []byte(mapFSTemplate("testgame"))},
	}
	engine := core.NewTemplateEngine(nil, nil)

	// 不應 panic。
	loadTemplates(engine, "", mfs, dataRoot, "")

	if len(engine.List()) != 0 {
		t.Fatalf("抽出失敗時不應有任何範本被載入,實際 = %d", len(engine.List()))
	}
	if !strings.Contains(buf.String(), "templates: 抽出內建範本失敗") {
		t.Fatalf("抽出失敗應記入 log,實際 log 內容: %q", buf.String())
	}
}

// TestLoadTemplates_BuiltinTemplateDirTakesPriority 驗證優先序(1):BuiltinTemplateDir 非空時
// 直接使用該目錄,略過 BuiltinFS 抽出(既有測試依賴此行為固定範本目錄)。
func TestLoadTemplates_BuiltinTemplateDirTakesPriority(t *testing.T) {
	fixedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixedDir, "fixed.toml"), []byte(mapFSTemplate("fixed")), 0o644); err != nil {
		t.Fatalf("寫入固定範本: %v", err)
	}
	mfs := fstest.MapFS{
		"templates/shouldnotload.toml": {Data: []byte(mapFSTemplate("shouldnotload"))},
	}
	engine := core.NewTemplateEngine(nil, nil)
	loadTemplates(engine, fixedDir, mfs, t.TempDir(), "")

	if _, ok := engine.Get("fixed"); !ok {
		t.Fatal("engine 應列出 BuiltinTemplateDir 指定目錄下的範本")
	}
	if _, ok := engine.Get("shouldnotload"); ok {
		t.Fatal("BuiltinTemplateDir 非空時不應改用 BuiltinFS 抽出的範本")
	}
}
