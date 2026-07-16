package provision

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// makeJREZip 產生一個最小的假 JRE zip(含 jdk-<major>-jre/bin/java.exe),回傳位元組與其 sha256(hex)。
func makeJREZip(t *testing.T, major int) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// 頂層目錄含版本號,模擬 Adoptium 佈局。
	entries := map[string]string{
		fmt.Sprintf("jdk-%d-jre/bin/java.exe", major): "fake-java-binary",
		fmt.Sprintf("jdk-%d-jre/release", major):      "IMPLEMENTOR=Eclipse",
	}
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("建立 zip 條目失敗: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("寫入 zip 條目失敗: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("關閉 zip 失敗: %v", err)
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

// fakeAdoptium 是模擬 Adoptium API + 下載載點的 httptest server。apiHits 累計 assets 查詢次數。
func fakeAdoptium(t *testing.T, major int, zipBytes []byte, checksum string, apiHits *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc(fmt.Sprintf("/v3/assets/latest/%d/hotspot", major), func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(apiHits, 1)
		asset := adoptiumAsset{}
		asset.Binary.OS = "windows"
		asset.Binary.Architecture = "x64"
		asset.Binary.ImageType = "jre"
		asset.Binary.Package.Name = fmt.Sprintf("jdk-%d-jre.zip", major)
		asset.Binary.Package.Link = srv.URL + "/download/jre.zip"
		asset.Binary.Package.Checksum = checksum
		asset.Version.Semver = fmt.Sprintf("%d.0.0", major)
		_ = json.NewEncoder(w).Encode([]adoptiumAsset{asset})
	})
	mux.HandleFunc("/download/jre.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(zipBytes)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestJavaProvisioner_CacheHitNoRedownload(t *testing.T) {
	zipBytes, sum := makeJREZip(t, 21)
	var apiHits int32
	srv := fakeAdoptium(t, 21, zipBytes, sum, &apiHits)

	cacheRoot := t.TempDir()
	p := New(cacheRoot, WithHTTPClient(srv.Client()), WithJavaAPIBase(srv.URL))

	exe1, err := p.Java.Ensure(context.Background(), 21, nil)
	if err != nil {
		t.Fatalf("第一次 Ensure 失敗: %v", err)
	}
	if _, err := os.Stat(exe1); err != nil {
		t.Fatalf("java.exe 不存在: %v", err)
	}

	exe2, err := p.Java.Ensure(context.Background(), 21, nil)
	if err != nil {
		t.Fatalf("第二次 Ensure 失敗: %v", err)
	}
	if exe1 != exe2 {
		t.Fatalf("兩次回傳路徑不一致: %q vs %q", exe1, exe2)
	}
	if got := atomic.LoadInt32(&apiHits); got != 1 {
		t.Fatalf("快取命中應不重打 API,實際查詢次數 = %d", got)
	}
}

func TestJavaProvisioner_ChecksumMismatchNoPartial(t *testing.T) {
	zipBytes, _ := makeJREZip(t, 17)
	var apiHits int32
	// 提供錯誤 checksum。
	srv := fakeAdoptium(t, 17, zipBytes, strings.Repeat("0", 64), &apiHits)

	cacheRoot := t.TempDir()
	p := New(cacheRoot, WithHTTPClient(srv.Client()), WithJavaAPIBase(srv.URL))

	_, err := p.Java.Ensure(context.Background(), 17, nil)
	if err == nil {
		t.Fatal("校驗不符應回錯,實得 nil")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("錯誤應可 errors.Is ErrChecksumMismatch,實得: %v", err)
	}

	// 目的目錄不得存在(不留半成品)。
	jreDir := filepath.Join(cacheRoot, "jre", "17")
	if _, statErr := os.Stat(jreDir); !os.IsNotExist(statErr) {
		t.Fatalf("校驗失敗後 JRE 快取目錄不應存在: %v", statErr)
	}
	// 暫存/staging 亦不得殘留。
	assertNoLeftover(t, filepath.Join(cacheRoot, "jre"))
}

func TestJavaProvisioner_OfflineError(t *testing.T) {
	zipBytes, sum := makeJREZip(t, 21)
	var apiHits int32
	srv := fakeAdoptium(t, 21, zipBytes, sum, &apiHits)
	client := srv.Client()
	base := srv.URL
	srv.Close() // 關閉 server 模擬離線。

	cacheRoot := t.TempDir()
	p := New(cacheRoot, WithHTTPClient(client), WithJavaAPIBase(base))

	_, err := p.Java.Ensure(context.Background(), 21, nil)
	if err == nil {
		t.Fatal("離線應回錯,實得 nil")
	}
	if !strings.Contains(err.Error(), "需要網路下載 Java 21") {
		t.Fatalf("錯誤訊息應含「需要網路下載 Java 21」,實得: %v", err)
	}
}

func TestJavaProvisioner_ProgressCallbackInvoked(t *testing.T) {
	zipBytes, sum := makeJREZip(t, 21)
	var apiHits int32
	srv := fakeAdoptium(t, 21, zipBytes, sum, &apiHits)

	cacheRoot := t.TempDir()
	p := New(cacheRoot, WithHTTPClient(srv.Client()), WithJavaAPIBase(srv.URL))

	var mu sync.Mutex
	var stages []string
	progress := func(pp ProvisionProgress) {
		mu.Lock()
		stages = append(stages, pp.Stage)
		mu.Unlock()
	}

	if _, err := p.Java.Ensure(context.Background(), 21, progress); err != nil {
		t.Fatalf("Ensure 失敗: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stages) == 0 {
		t.Fatal("進度回呼從未被呼叫")
	}
	// 應至少涵蓋下載階段。
	sawDownload := false
	for _, s := range stages {
		if strings.Contains(s, "下載 Java") {
			sawDownload = true
		}
	}
	if !sawDownload {
		t.Fatalf("進度回呼未涵蓋下載階段,實得: %v", stages)
	}
}

func TestExtractZip_RejectsZipSlip(t *testing.T) {
	cases := map[string]string{
		"路徑穿越": "../evil.txt",
		"絕對路徑": "/etc/passwd",
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			w, err := zw.Create(entry)
			if err != nil {
				t.Fatalf("建立惡意 zip 條目失敗: %v", err)
			}
			_, _ = w.Write([]byte("pwned"))
			if err := zw.Close(); err != nil {
				t.Fatalf("關閉 zip 失敗: %v", err)
			}

			dir := t.TempDir()
			zipPath := filepath.Join(dir, "malicious.zip")
			if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
				t.Fatalf("寫入 zip 失敗: %v", err)
			}

			err = extractZip(zipPath, filepath.Join(dir, "out"), "test", nil)
			if err == nil {
				t.Fatal("zip-slip 應被拒絕,實得 nil")
			}
			if !errors.Is(err, ErrUnsafeZipEntry) {
				t.Fatalf("錯誤應可 errors.Is ErrUnsafeZipEntry,實得: %v", err)
			}
		})
	}
}

func TestExtractZip_ExtractsSafeEntries(t *testing.T) {
	zipBytes, _ := makeJREZip(t, 21)
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "jre.zip")
	if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
		t.Fatalf("寫入 zip 失敗: %v", err)
	}
	out := filepath.Join(dir, "out")
	if err := extractZip(zipPath, out, "test", nil); err != nil {
		t.Fatalf("解壓合法 zip 失敗: %v", err)
	}
	exe, ok := findJavaExe(out)
	if !ok {
		t.Fatal("解壓後應找到 java.exe")
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("java.exe 應存在: %v", err)
	}
}

// assertNoLeftover 檢查 dir 下沒有暫存/staging 殘留(.part / .partial 後綴)。
func assertNoLeftover(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("讀取目錄失敗: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, ".part") || strings.Contains(name, ".partial") {
			t.Fatalf("殘留半成品: %q", name)
		}
	}
}
