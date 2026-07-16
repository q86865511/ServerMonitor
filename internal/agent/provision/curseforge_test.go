package provision

// CurseForgeProvider 單元測試(native-backend R14):cfzip manifest 解析 + 逐檔 API 取連結 + 下載
// 落位 + overrides 解壓;downloadUrl=null 走 BlockedMods 路徑(回 BlockedModsError + 發 blocked-mods
// 事件);匯入資料夾自動補齊(降級方案的重試路徑);x-api-key header 帶入;key 缺/覆蓋語意。
// 所有 key 皆為假字串(絕不使用真實金鑰)。API 形狀查證見 curseforge.go 頂註(docs.curseforge.com)。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeCFKey = "fake-test-key-not-real" // 測試用假金鑰(絕非真實 key)

// cfTestMod 描述一個測試模組:API 端點回應與(未被擋時)下載內容由此推導。
type cfTestMod struct {
	projectID  int
	fileID     int
	fileName   string
	content    []byte
	blocked    bool   // downloadUrl=null(作者停用第三方散布)
	websiteURL string // GET /v1/mods/{id} 回報的 links.websiteUrl
}

// cfServer 起單一 httptest server,扮演 CurseForge API(files/mod 端點)與檔案下載端點。
// wantKey 非空時,files 端點強制校驗 x-api-key(缺/不符回 401),供驗證金鑰確有帶入。
func cfServer(t *testing.T, mods []cfTestMod, wantKey string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	for _, m := range mods {
		m := m
		mux.HandleFunc(fmt.Sprintf("/v1/mods/%d/files/%d", m.projectID, m.fileID), func(w http.ResponseWriter, r *http.Request) {
			if wantKey != "" && r.Header.Get("x-api-key") != wantKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f := cfFile{ID: m.fileID, ModID: m.projectID, FileName: m.fileName, FileLength: int64(len(m.content))}
			if !m.blocked {
				url := srv.URL + "/dl/" + m.fileName
				f.DownloadURL = &url
				sum := sha1.Sum(m.content)
				f.Hashes = []cfHash{{Value: hex.EncodeToString(sum[:]), Algo: cfHashAlgoSha1}}
			}
			_ = json.NewEncoder(w).Encode(cfFileResponse{Data: f})
		})
		mux.HandleFunc(fmt.Sprintf("/v1/mods/%d", m.projectID), func(w http.ResponseWriter, r *http.Request) {
			var resp cfModResponse
			resp.Data.Links.WebsiteURL = m.websiteURL
			_ = json.NewEncoder(w).Encode(resp)
		})
		if !m.blocked {
			content := m.content
			mux.HandleFunc("/dl/"+m.fileName, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(content)
			})
		}
	}
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// buildCurseForgeZip 建立測試 cfzip:manifest.json(files[] 由 mods 的 project/file id 導出)+
// overridesDir 下的 override 檔。
func buildCurseForgeZip(t *testing.T, mods []cfTestMod, overridesDir string, overrides map[string]string) []byte {
	t.Helper()
	m := cfManifest{Name: "Test Pack", Version: "1.0", Overrides: overridesDir}
	m.Minecraft.Version = "1.20.1"
	for _, mod := range mods {
		m.Files = append(m.Files, cfManifestFile{ProjectID: mod.projectID, FileID: mod.fileID, Required: true})
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	mustWriteZipEntry(t, zw, "manifest.json", b)
	for p, content := range overrides {
		mustWriteZipEntry(t, zw, strings.TrimSuffix(overridesDir, "/")+"/"+p, []byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("關閉 cfzip 失敗: %v", err)
	}
	return buf.Bytes()
}

func writeTempCfzip(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pack.zip")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("寫入暫存 cfzip 失敗: %v", err)
	}
	return p
}

func TestCurseForgeProvider_InstallModpack_HappyPath(t *testing.T) {
	mods := []cfTestMod{
		{projectID: 100, fileID: 1, fileName: "A.jar", content: []byte("AAA")},
		{projectID: 200, fileID: 2, fileName: "B.jar", content: []byte("BBBB")},
	}
	srv := cfServer(t, mods, fakeCFKey) // wantKey 強制:安裝成功即證明 x-api-key 有帶入
	archive := writeTempCfzip(t, buildCurseForgeZip(t, mods, "overrides", map[string]string{"config/base.txt": "base"}))

	p := NewCurseForgeProvider(srv.Client(), srv.URL, fakeCFKey)
	targetDir := t.TempDir()
	lastStage := ""
	lastPct := -99
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		ArchivePath: archive,
		TargetDir:   targetDir,
	}, func(pp ProvisionProgress) {
		lastStage = pp.Stage
		lastPct = pp.Percent
	})
	if err != nil {
		t.Fatalf("InstallModpack: %v", err)
	}
	if lastStage != "modpack" || lastPct != 100 {
		t.Errorf("最終進度 stage=%q pct=%d,期望 modpack/100", lastStage, lastPct)
	}
	assertFileContent(t, filepath.Join(targetDir, "mods", "A.jar"), "AAA")
	assertFileContent(t, filepath.Join(targetDir, "mods", "B.jar"), "BBBB")
	assertFileContent(t, filepath.Join(targetDir, "config", "base.txt"), "base")
}

func TestCurseForgeProvider_InstallModpack_MissingKey_401(t *testing.T) {
	// 供應器 apiKey 為空,但 server 要求 key:fetchFile 應因 401 失敗(模擬設定/內嵌皆缺 key 的誤用)。
	mods := []cfTestMod{{projectID: 100, fileID: 1, fileName: "A.jar", content: []byte("AAA")}}
	srv := cfServer(t, mods, fakeCFKey)
	archive := writeTempCfzip(t, buildCurseForgeZip(t, mods, "overrides", nil))

	p := NewCurseForgeProvider(srv.Client(), srv.URL, "") // 無 key
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{ArchivePath: archive, TargetDir: t.TempDir()}, nil)
	if err == nil {
		t.Fatal("缺 key 時應因 API 401 失敗,實際成功")
	}
}

func TestCurseForgeProvider_InstallModpack_BlockedMods(t *testing.T) {
	mods := []cfTestMod{
		{projectID: 100, fileID: 1, fileName: "A.jar", content: []byte("AAA")},
		{projectID: 300, fileID: 3, fileName: "Blocked.jar", blocked: true, websiteURL: "https://www.curseforge.com/minecraft/mc-mods/blocked"},
	}
	srv := cfServer(t, mods, "")
	archive := writeTempCfzip(t, buildCurseForgeZip(t, mods, "overrides", nil))

	p := NewCurseForgeProvider(srv.Client(), srv.URL, fakeCFKey)
	targetDir := t.TempDir()
	var blockedDetail string
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		ArchivePath: archive,
		TargetDir:   targetDir,
	}, func(pp ProvisionProgress) {
		if pp.Stage == "blocked-mods" {
			blockedDetail = pp.Detail
		}
	})

	var bmErr *BlockedModsError
	if !errors.As(err, &bmErr) {
		t.Fatalf("期望 BlockedModsError,實得: %v", err)
	}
	if len(bmErr.Mods) != 1 || bmErr.Mods[0].FileName != "Blocked.jar" {
		t.Fatalf("被擋清單 = %+v,期望 1 個 Blocked.jar", bmErr.Mods)
	}
	if want := "https://www.curseforge.com/minecraft/mc-mods/blocked/files/3"; bmErr.Mods[0].URL != want {
		t.Errorf("被擋模組 URL = %q,期望 %q", bmErr.Mods[0].URL, want)
	}
	// 可下載的部分仍已安裝。
	assertFileContent(t, filepath.Join(targetDir, "mods", "A.jar"), "AAA")
	// blocked-mods 事件已發出且 detail 為含該模組的 JSON 清單。
	if !strings.Contains(blockedDetail, "Blocked.jar") {
		t.Errorf("blocked-mods 事件 detail = %q,應含 Blocked.jar", blockedDetail)
	}
	var parsed []BlockedMod
	if err := json.Unmarshal([]byte(blockedDetail), &parsed); err != nil || len(parsed) != 1 {
		t.Errorf("blocked-mods detail 應為 JSON 清單,解析結果 err=%v len=%d", err, len(parsed))
	}
}

func TestCurseForgeProvider_ImportBlockedMods_RetrySucceeds(t *testing.T) {
	blockedContent := []byte("MANUALLY-DOWNLOADED")
	mods := []cfTestMod{
		{projectID: 300, fileID: 3, fileName: "Blocked.jar", blocked: true, websiteURL: "https://x/blocked"},
	}
	srv := cfServer(t, mods, "")
	archive := writeTempCfzip(t, buildCurseForgeZip(t, mods, "overrides", nil))

	// 使用者已手動下載被擋模組至匯入資料夾(長度須與 API 回報的 fileLength 相符)。
	importDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(importDir, "Blocked.jar"), blockedContent, 0o644); err != nil {
		t.Fatalf("寫入匯入檔失敗: %v", err)
	}
	// 讓 server 回報的 fileLength 與匯入內容一致(cfServer 由 content 導出;此模組 blocked 無 content,
	// fileLength=0,importBlockedMods 於 0 時略過長度檢查——涵蓋 API 未提供長度的情況)。

	p := NewCurseForgeProvider(srv.Client(), srv.URL, fakeCFKey)
	targetDir := t.TempDir()
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		ArchivePath: archive,
		TargetDir:   targetDir,
		ImportDir:   importDir,
	}, nil)
	if err != nil {
		t.Fatalf("有匯入檔時應安裝成功,實得: %v", err)
	}
	assertFileContent(t, filepath.Join(targetDir, "mods", "Blocked.jar"), string(blockedContent))
}

func TestCurseForgeProvider_ImportBlockedMods_LengthMismatchStaysBlocked(t *testing.T) {
	// 匯入檔長度與 API 回報 fileLength 不符時視為錯檔/不完整,不匯入,仍回 BlockedModsError。
	blocked := []BlockedMod{{ProjectID: 1, FileID: 2, FileName: "X.jar", FileLength: 100}}
	importDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(importDir, "X.jar"), []byte("too-short"), 0o644); err != nil {
		t.Fatalf("寫入匯入檔失敗: %v", err)
	}
	remaining, err := importBlockedMods(importDir, t.TempDir(), blocked, nil)
	if err != nil {
		t.Fatalf("importBlockedMods: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("長度不符應維持被擋,remaining=%d 期望 1", len(remaining))
	}
}

func TestCurseForgeProvider_Ref_ProjectFileID(t *testing.T) {
	// Ref="<projectID>:<fileID>" 解析:先查該 modpack 檔取 downloadUrl 下載 cfzip,再安裝其內模組。
	inner := []cfTestMod{{projectID: 100, fileID: 1, fileName: "A.jar", content: []byte("AAA")}}
	cfzip := buildCurseForgeZip(t, inner, "overrides", nil)

	mux := http.NewServeMux()
	var srv *httptest.Server
	// modpack 本身作為一個 CF 檔(project 500 file 9),downloadUrl 指向 cfzip。
	mux.HandleFunc("/v1/mods/500/files/9", func(w http.ResponseWriter, r *http.Request) {
		url := srv.URL + "/dl/pack.zip"
		_ = json.NewEncoder(w).Encode(cfFileResponse{Data: cfFile{ID: 9, ModID: 500, FileName: "pack.zip", DownloadURL: &url}})
	})
	mux.HandleFunc("/dl/pack.zip", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(cfzip) })
	// 內部模組端點。
	mux.HandleFunc("/v1/mods/100/files/1", func(w http.ResponseWriter, r *http.Request) {
		url := srv.URL + "/dl/A.jar"
		sum := sha1.Sum(inner[0].content)
		_ = json.NewEncoder(w).Encode(cfFileResponse{Data: cfFile{ID: 1, ModID: 100, FileName: "A.jar", FileLength: 3, DownloadURL: &url, Hashes: []cfHash{{Value: hex.EncodeToString(sum[:]), Algo: 1}}}})
	})
	mux.HandleFunc("/dl/A.jar", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(inner[0].content) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := NewCurseForgeProvider(srv.Client(), srv.URL, fakeCFKey)
	targetDir := t.TempDir()
	if err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		Ref:       &ModpackRef{Type: "curseforge", Ref: "500:9"},
		TargetDir: targetDir,
	}, nil); err != nil {
		t.Fatalf("InstallModpack(Ref): %v", err)
	}
	assertFileContent(t, filepath.Join(targetDir, "mods", "A.jar"), "AAA")
}

func TestParseCurseForgeRef(t *testing.T) {
	cases := []struct {
		ref        string
		proj, file int
		wantErr    bool
	}{
		{"500:9", 500, 9, false},
		{"500/9", 500, 9, false},
		{"abc:9", 0, 0, true},
		{"500", 0, 0, true},
	}
	for _, c := range cases {
		proj, file, err := parseCurseForgeRef(c.ref)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseCurseForgeRef(%q) 應回錯", c.ref)
			}
			continue
		}
		if err != nil || proj != c.proj || file != c.file {
			t.Errorf("parseCurseForgeRef(%q) = (%d,%d,%v),期望 (%d,%d,nil)", c.ref, proj, file, err, c.proj, c.file)
		}
	}
}

// ---- Provisioner 層:key 啟用/覆蓋語意(以假 key 字串,不觸網)----

func TestProvisioner_CurseForgeDisabledWhenNoKey(t *testing.T) {
	orig := curseforgeAPIKey
	t.Cleanup(func() { curseforgeAPIKey = orig })
	curseforgeAPIKey = "" // 無內嵌 key

	p := New(t.TempDir())
	if p.CurseForgeEnabled() {
		t.Error("無 key 時 CurseForgeEnabled 應為 false")
	}
	if _, ok := p.ModProviders["curseforge"]; ok {
		t.Error("無 key 時不應註冊 curseforge ModProvider")
	}
}

func TestProvisioner_CurseForgeConfigOverridesEmbedded(t *testing.T) {
	orig := curseforgeAPIKey
	t.Cleanup(func() { curseforgeAPIKey = orig })
	curseforgeAPIKey = "embedded-fake-key" // 模擬 ldflags 內嵌值(假字串)

	// 無覆蓋:採內嵌值,啟用並註冊。
	p := New(t.TempDir())
	if !p.CurseForgeEnabled() || p.curseforgeKey != "embedded-fake-key" {
		t.Fatalf("無覆蓋應採內嵌 key,實得 enabled=%v key=%q", p.CurseForgeEnabled(), p.curseforgeKey)
	}
	if _, ok := p.ModProviders["curseforge"]; !ok {
		t.Error("有內嵌 key 時應註冊 curseforge ModProvider")
	}

	// 設定覆蓋:使用者 key 勝出。
	p2 := New(t.TempDir(), WithCurseForgeAPIKey("user-fake-key"))
	if p2.curseforgeKey != "user-fake-key" {
		t.Errorf("設定覆蓋應勝出,實得 key=%q", p2.curseforgeKey)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀取 %s 失敗: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s 內容 = %q,期望 %q", path, got, want)
	}
}
