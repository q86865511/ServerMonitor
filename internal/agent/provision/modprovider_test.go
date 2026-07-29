package provision

// mrpack 安裝核心邏輯(modprovider.go)的單元測試:index 解析、逐檔下載+校驗、env=unsupported
// 跳過、overrides/server-overrides 分層套用、版本/loader 不相容錯誤、zip-slip 防禦。
// mrpack 格式查證來源見 modprovider.go 頂註(support.modrinth.com)。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// testAllowedHosts 依 rawURL(通常是 httptest server 的 srv.URL)推導單一 host 的允許清單,
// 供直接呼叫 installMrpackArchive 的測試(略過 ModrinthProvider)注入下載白名單(見
// validateModpackDownloadURL);production 路徑改由 ModrinthProvider.allowedDownloadHosts 提供。
func testAllowedHosts(t *testing.T, rawURL string) map[string]struct{} {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析測試伺服器 URL %q 失敗: %v", rawURL, err)
	}
	return map[string]struct{}{u.Hostname(): {}}
}

// mrpackFileSpec 是測試建構 mrpack 用的單一檔案規格;內容位元組經雜湊自動填入 index.json。
type mrpackFileSpec struct {
	path    string
	content []byte
	env     map[string]string
}

// buildMrpackZip 建立一個測試用 mrpack:modrinth.index.json + overrides/、server-overrides/。
// files[] 的下載 URL 指向 downloadBase + "/dl/<path>",呼叫端需另起 httptest server 服務這些路徑
// (見 fakeModDownloadServer)。corruptFile 非空時,對應檔案的下載內容會被竄改(用於測 checksum
// 不符路徑),index.json 內的 hash 仍以「原內容」計算,模擬「來源被竄改但索引未變」的情境。
func buildMrpackZip(t *testing.T, deps map[string]string, files []mrpackFileSpec, overrides, serverOverrides map[string]string, downloadBase string) []byte {
	t.Helper()
	idx := mrpackIndex{
		FormatVersion: 1,
		Game:          "minecraft",
		VersionID:     "test-version",
		Name:          "Test Pack",
		Dependencies:  deps,
	}
	for _, f := range files {
		sha1sum := sha1.Sum(f.content)
		sha512sum := sha512.Sum512(f.content)
		idx.Files = append(idx.Files, mrpackFile{
			Path: f.path,
			Hashes: map[string]string{
				"sha1":   hex.EncodeToString(sha1sum[:]),
				"sha512": hex.EncodeToString(sha512sum[:]),
			},
			Env:       f.env,
			Downloads: []string{downloadBase + "/dl/" + f.path},
			FileSize:  int64(len(f.content)),
		})
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	idxJSON, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	mustWriteZipEntry(t, zw, "modrinth.index.json", idxJSON)
	for p, content := range overrides {
		mustWriteZipEntry(t, zw, "overrides/"+p, []byte(content))
	}
	for p, content := range serverOverrides {
		mustWriteZipEntry(t, zw, "server-overrides/"+p, []byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("關閉 mrpack zip 失敗: %v", err)
	}
	return buf.Bytes()
}

func mustWriteZipEntry(t *testing.T, zw *zip.Writer, name string, content []byte) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("建立 zip 條目 %q 失敗: %v", name, err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("寫入 zip 條目 %q 失敗: %v", name, err)
	}
}

// fakeModDownloadServer 起一個 httptest server,依 files 的 path 提供 /dl/<path> 下載端點。
// serveOverride 非空時,對應 path 改回應 serveOverride 的內容(模擬下載內容被竄改,校驗應失敗)。
func fakeModDownloadServer(t *testing.T, files []mrpackFileSpec, serveOverride map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, f := range files {
		f := f
		content := f.content
		if ov, ok := serveOverride[f.path]; ok {
			content = ov
		}
		mux.HandleFunc("/dl/"+f.path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeTempMrpack(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pack.mrpack")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("寫入暫存 mrpack 失敗: %v", err)
	}
	return p
}

func TestInstallMrpackArchive_HappyPath(t *testing.T) {
	files := []mrpackFileSpec{
		{path: "mods/A.jar", content: []byte("AAA")},
		{path: "mods/ClientOnly.jar", content: []byte("CLIENT"), env: map[string]string{"client": "required", "server": "unsupported"}},
	}
	srv := fakeModDownloadServer(t, files, nil)
	deps := map[string]string{"minecraft": "1.20.1", "fabric-loader": "0.15.0"}
	overrides := map[string]string{"config/base.txt": "base"}
	serverOverrides := map[string]string{"config/base.txt": "server-override"}
	zipBytes := buildMrpackZip(t, deps, files, overrides, serverOverrides, srv.URL)
	archivePath := writeTempMrpack(t, zipBytes)

	targetDir := t.TempDir()
	var lastPct int = -99
	err := installMrpackArchive(context.Background(), srv.Client(), archivePath, targetDir, "1.20.1", "fabric", func(p ProvisionProgress) {
		lastPct = p.Percent
	}, testAllowedHosts(t, srv.URL), true)
	if err != nil {
		t.Fatalf("installMrpackArchive: %v", err)
	}
	if lastPct != 100 {
		t.Errorf("最終進度 Percent=%d,期望 100", lastPct)
	}

	got, err := os.ReadFile(filepath.Join(targetDir, "mods", "A.jar"))
	if err != nil || string(got) != "AAA" {
		t.Fatalf("mods/A.jar 內容=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "mods", "ClientOnly.jar")); !os.IsNotExist(err) {
		t.Errorf("env.server=unsupported 的檔案應被跳過,實際存在(err=%v)", err)
	}
	gotOverride, err := os.ReadFile(filepath.Join(targetDir, "config", "base.txt"))
	if err != nil || string(gotOverride) != "server-override" {
		t.Fatalf("config/base.txt = %q err=%v,期望 server-overrides 覆寫 overrides 為 server-override", gotOverride, err)
	}
}

func TestInstallMrpackArchive_IncompatibleMCVersion(t *testing.T) {
	deps := map[string]string{"minecraft": "1.20.1"}
	zipBytes := buildMrpackZip(t, deps, nil, nil, nil, "http://unused.invalid")
	archivePath := writeTempMrpack(t, zipBytes)

	err := installMrpackArchive(context.Background(), http.DefaultClient, archivePath, t.TempDir(), "1.21.0", "", nil, nil, false)
	if !errors.Is(err, ErrModpackIncompatible) {
		t.Fatalf("期望 ErrModpackIncompatible,實得: %v", err)
	}
}

func TestInstallMrpackArchive_IncompatibleLoader(t *testing.T) {
	deps := map[string]string{"minecraft": "1.20.1", "forge": "47.0.0"}
	zipBytes := buildMrpackZip(t, deps, nil, nil, nil, "http://unused.invalid")
	archivePath := writeTempMrpack(t, zipBytes)

	err := installMrpackArchive(context.Background(), http.DefaultClient, archivePath, t.TempDir(), "1.20.1", "fabric", nil, nil, false)
	if !errors.Is(err, ErrModpackIncompatible) {
		t.Fatalf("期望 ErrModpackIncompatible(缺 fabric-loader 依賴),實得: %v", err)
	}
}

func TestInstallMrpackArchive_CompatibleWhenVersionUnspecified(t *testing.T) {
	// mcVersion/loader 為空時不擋(呼叫端未提供,不誤擋;見 checkMrpackCompatibility 註解)。
	deps := map[string]string{"minecraft": "1.20.1", "forge": "47.0.0"}
	zipBytes := buildMrpackZip(t, deps, nil, nil, nil, "http://unused.invalid")
	archivePath := writeTempMrpack(t, zipBytes)

	if err := installMrpackArchive(context.Background(), http.DefaultClient, archivePath, t.TempDir(), "", "", nil, nil, false); err != nil {
		t.Fatalf("mcVersion/loader 皆空時不應報相容性錯誤: %v", err)
	}
}

func TestInstallMrpackArchive_ChecksumMismatch(t *testing.T) {
	files := []mrpackFileSpec{{path: "mods/A.jar", content: []byte("AAA")}}
	srv := fakeModDownloadServer(t, files, map[string][]byte{"mods/A.jar": []byte("TAMPERED")})
	zipBytes := buildMrpackZip(t, nil, files, nil, nil, srv.URL)
	archivePath := writeTempMrpack(t, zipBytes)

	err := installMrpackArchive(context.Background(), srv.Client(), archivePath, t.TempDir(), "", "", nil, testAllowedHosts(t, srv.URL), true)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("期望 ErrChecksumMismatch,實得: %v", err)
	}
}

func TestInstallMrpackArchive_MissingIndex(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mustWriteZipEntry(t, zw, "readme.txt", []byte("not a modpack"))
	if err := zw.Close(); err != nil {
		t.Fatalf("關閉 zip 失敗: %v", err)
	}
	archivePath := writeTempMrpack(t, buf.Bytes())

	if err := installMrpackArchive(context.Background(), http.DefaultClient, archivePath, t.TempDir(), "", "", nil, nil, false); err == nil {
		t.Fatal("缺 modrinth.index.json 應報錯,實際成功")
	}
}

func TestSafeModpackFilePath_RejectsTraversalAndAbsolute(t *testing.T) {
	targetDir := t.TempDir()
	// 同 TestExtractZip_RejectsZipSlip(provision_test.go)驗證的兩類穿越模式。
	cases := []string{"../evil.jar", "mods/../../evil.jar", "/etc/passwd"}
	for _, rel := range cases {
		if _, err := safeModpackFilePath(targetDir, rel); !errors.Is(err, ErrUnsafeZipEntry) {
			t.Errorf("safeModpackFilePath(%q) 應拒絕(ErrUnsafeZipEntry),實得: %v", rel, err)
		}
	}
}

func TestSafeModpackFilePath_AllowsNestedRelative(t *testing.T) {
	targetDir := t.TempDir()
	got, err := safeModpackFilePath(targetDir, "mods/sub/A.jar")
	if err != nil {
		t.Fatalf("safeModpackFilePath: %v", err)
	}
	want := filepath.Join(targetDir, "mods", "sub", "A.jar")
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

// validateModpackDownloadURL 拒絕白名單外 host 與非 https scheme(SSRF 防禦):mrpack index 由
// 第三方模組包提供,downloads 陣列若不受限,可誘使節點對任意內部/外部位址發起請求。
func TestValidateModpackDownloadURL_RejectsDisallowedHostOrScheme(t *testing.T) {
	allowed := map[string]struct{}{"cdn.modrinth.com": {}}
	cases := []string{
		"http://cdn.modrinth.com/mods/A.jar",     // 白名單 host 但非 https
		"https://evil.example.com/mods/A.jar",    // https 但 host 不在白名單
		"https://169.254.169.254/latest/meta",    // 雲端 metadata 端點,經典 SSRF 目標
		"http://127.0.0.1:8080/internal",         // 內網位址
		"file:///etc/passwd",                     // 非 http(s) scheme
	}
	for _, u := range cases {
		if err := validateModpackDownloadURL(u, allowed, false); err == nil {
			t.Errorf("validateModpackDownloadURL(%q) 應拒絕,實際通過", u)
		}
	}
}

func TestValidateModpackDownloadURL_AllowsWhitelistedHTTPS(t *testing.T) {
	allowed := map[string]struct{}{"cdn.modrinth.com": {}}
	if err := validateModpackDownloadURL("https://cdn.modrinth.com/data/mods/A.jar", allowed, false); err != nil {
		t.Errorf("白名單 host + https 應通過,實得: %v", err)
	}
}
