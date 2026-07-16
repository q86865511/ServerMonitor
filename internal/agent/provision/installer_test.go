package provision

import (
	"context"
	"crypto/sha1"
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
	"testing"
)

// ---- Vanilla ----

// fakeMojang 是模擬 Mojang version manifest + 版本 JSON + 下載載點的 httptest server。
func fakeMojang(t *testing.T, versionID string, jarBytes []byte, sha1sum string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/mc/game/version_manifest_v2.json", func(w http.ResponseWriter, r *http.Request) {
		manifest := mojangVersionManifest{}
		manifest.Versions = []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}{
			{ID: versionID, URL: srv.URL + "/version/" + versionID + ".json"},
		}
		_ = json.NewEncoder(w).Encode(manifest)
	})
	mux.HandleFunc("/version/"+versionID+".json", func(w http.ResponseWriter, r *http.Request) {
		detail := mojangVersionDetail{}
		detail.Downloads.Server.SHA1 = sha1sum
		detail.Downloads.Server.URL = srv.URL + "/download/server.jar"
		_ = json.NewEncoder(w).Encode(detail)
	})
	mux.HandleFunc("/download/server.jar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jarBytes)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jarBytes)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestVanillaInstaller_InstallsAndVerifiesChecksum(t *testing.T) {
	jarBytes := []byte("fake-vanilla-server-jar")
	sum := sha1Hex(jarBytes)
	srv := fakeMojang(t, "1.20.4", jarBytes, sum)

	inst := NewVanillaInstaller(srv.Client(), srv.URL+"/mc/game/version_manifest_v2.json")
	targetDir := t.TempDir()

	var stages []string
	progress := func(pp ProvisionProgress) { stages = append(stages, pp.Stage) }

	got, err := inst.Install(context.Background(), InstallRequest{MCVersion: "1.20.4", TargetDir: targetDir}, progress)
	if err != nil {
		t.Fatalf("Install 失敗: %v", err)
	}
	if got.ServerJar != filepath.Join(targetDir, "server.jar") {
		t.Fatalf("ServerJar 路徑不符,實得 %q", got.ServerJar)
	}
	data, err := os.ReadFile(got.ServerJar)
	if err != nil {
		t.Fatalf("讀取 server.jar 失敗: %v", err)
	}
	if string(data) != string(jarBytes) {
		t.Fatalf("server.jar 內容不符")
	}
	if len(stages) == 0 {
		t.Fatal("進度回呼從未被呼叫")
	}
}

func TestVanillaInstaller_ChecksumMismatchNoPartial(t *testing.T) {
	jarBytes := []byte("fake-vanilla-server-jar")
	srv := fakeMojang(t, "1.20.4", jarBytes, strings.Repeat("0", 40)) // 錯誤 sha1

	inst := NewVanillaInstaller(srv.Client(), srv.URL+"/mc/game/version_manifest_v2.json")
	targetDir := t.TempDir()

	_, err := inst.Install(context.Background(), InstallRequest{MCVersion: "1.20.4", TargetDir: targetDir}, nil)
	if err == nil {
		t.Fatal("校驗不符應回錯,實得 nil")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("錯誤應可 errors.Is ErrChecksumMismatch,實得: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(targetDir, "server.jar")); !os.IsNotExist(statErr) {
		t.Fatalf("校驗失敗後 server.jar 不應存在: %v", statErr)
	}
	assertNoLeftover(t, targetDir)
}

func TestVanillaInstaller_VersionNotFound(t *testing.T) {
	jarBytes := []byte("fake-vanilla-server-jar")
	srv := fakeMojang(t, "1.20.4", jarBytes, sha1Hex(jarBytes))

	inst := NewVanillaInstaller(srv.Client(), srv.URL+"/mc/game/version_manifest_v2.json")
	targetDir := t.TempDir()

	_, err := inst.Install(context.Background(), InstallRequest{MCVersion: "1.99.9", TargetDir: targetDir}, nil)
	if err == nil {
		t.Fatal("不存在的版本應回錯,實得 nil")
	}
	if !strings.Contains(err.Error(), "1.99.9") {
		t.Fatalf("錯誤訊息應提及版本號,實得: %v", err)
	}
}

// ---- Paper ----

// fakePaper 是模擬 PaperMC v3 build 清單 + 下載載點的 httptest server。builds 依 channel
// 查詢參數過濾,不篩選時回傳全部(對照實測的 fill.papermc.io 行為)。builds 內下載項的 URL
// 欄位若留空,回應時自動填入本 server 的 /download/server.jar(啟動前無法預知 srv.URL,故
// 採延後填入,免去呼叫端手動回填的麻煩)。
func fakePaper(t *testing.T, mcVersion string, builds []paperBuild, jarBytes []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc(fmt.Sprintf("/v3/projects/paper/versions/%s/builds", mcVersion), func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")
		filtered := make([]paperBuild, 0, len(builds))
		for _, b := range builds {
			if channel != "" && b.Channel != channel {
				continue
			}
			nb := b
			nb.Downloads = make(map[string]paperDownload, len(b.Downloads))
			for k, dl := range b.Downloads {
				if dl.URL == "" {
					dl.URL = srv.URL + "/download/server.jar"
				}
				nb.Downloads[k] = dl
			}
			filtered = append(filtered, nb)
		}
		_ = json.NewEncoder(w).Encode(filtered)
	})
	mux.HandleFunc("/download/server.jar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jarBytes)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jarBytes)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPaperInstaller_InstallsLatestStableBuild(t *testing.T) {
	jarBytes := []byte("fake-paper-server-jar")
	sum := sha256Hex(jarBytes)
	builds := []paperBuild{
		{ID: 10, Channel: "STABLE", Downloads: map[string]paperDownload{
			paperDownloadKey: {Name: "paper-10.jar", Checksums: paperChecksums{SHA256: sum}},
		}},
		{ID: 9, Channel: "STABLE", Downloads: map[string]paperDownload{
			paperDownloadKey: {Name: "paper-9.jar", Checksums: paperChecksums{SHA256: sum}},
		}},
	}
	srv := fakePaper(t, "1.20.4", builds, jarBytes)

	inst := NewPaperInstaller(srv.Client(), srv.URL)
	targetDir := t.TempDir()

	got, err := inst.Install(context.Background(), InstallRequest{MCVersion: "1.20.4", TargetDir: targetDir}, nil)
	if err != nil {
		t.Fatalf("Install 失敗: %v", err)
	}
	data, err := os.ReadFile(got.ServerJar)
	if err != nil {
		t.Fatalf("讀取 server.jar 失敗: %v", err)
	}
	if string(data) != string(jarBytes) {
		t.Fatalf("server.jar 內容不符")
	}
}

func TestPaperInstaller_ChecksumMismatchNoPartial(t *testing.T) {
	jarBytes := []byte("fake-paper-server-jar")
	builds := []paperBuild{
		{ID: 10, Channel: "STABLE", Downloads: map[string]paperDownload{
			paperDownloadKey: {Name: "paper-10.jar", Checksums: paperChecksums{SHA256: strings.Repeat("0", 64)}},
		}},
	}
	srv := fakePaper(t, "1.20.4", builds, jarBytes)

	inst := NewPaperInstaller(srv.Client(), srv.URL)
	targetDir := t.TempDir()

	_, err := inst.Install(context.Background(), InstallRequest{MCVersion: "1.20.4", TargetDir: targetDir}, nil)
	if err == nil {
		t.Fatal("校驗不符應回錯,實得 nil")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("錯誤應可 errors.Is ErrChecksumMismatch,實得: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(targetDir, "server.jar")); !os.IsNotExist(statErr) {
		t.Fatalf("校驗失敗後 server.jar 不應存在: %v", statErr)
	}
	assertNoLeftover(t, targetDir)
}

func TestPaperInstaller_NoBuildError(t *testing.T) {
	srv := fakePaper(t, "1.20.4", nil, nil) // 空清單,模擬該版本無任何 build

	inst := NewPaperInstaller(srv.Client(), srv.URL)
	targetDir := t.TempDir()

	_, err := inst.Install(context.Background(), InstallRequest{MCVersion: "1.20.4", TargetDir: targetDir}, nil)
	if err == nil {
		t.Fatal("無 build 應回錯,實得 nil")
	}
	if !strings.Contains(err.Error(), "1.20.4") {
		t.Fatalf("錯誤訊息應提及版本號,實得: %v", err)
	}
}

func TestPaperInstaller_VersionNotFound(t *testing.T) {
	srv := fakePaper(t, "1.20.4", nil, nil)

	inst := NewPaperInstaller(srv.Client(), srv.URL)
	targetDir := t.TempDir()

	// 請求一個 mux 未註冊路由的版本,模擬真實 API 對不存在版本回 404。
	_, err := inst.Install(context.Background(), InstallRequest{MCVersion: "99.99.99", TargetDir: targetDir}, nil)
	if err == nil {
		t.Fatal("不存在的版本應回錯,實得 nil")
	}
	if !strings.Contains(err.Error(), "99.99.99") {
		t.Fatalf("錯誤訊息應提及版本號,實得: %v", err)
	}
}

func TestPaperInstaller_FallsBackWhenNoStableBuild(t *testing.T) {
	jarBytes := []byte("fake-paper-alpha-jar")
	sum := sha256Hex(jarBytes)
	builds := []paperBuild{
		{ID: 5, Channel: "ALPHA", Downloads: map[string]paperDownload{
			paperDownloadKey: {Name: "paper-5.jar", Checksums: paperChecksums{SHA256: sum}},
		}},
	}
	srv := fakePaper(t, "1.21.0", builds, jarBytes)

	inst := NewPaperInstaller(srv.Client(), srv.URL)
	targetDir := t.TempDir()

	got, err := inst.Install(context.Background(), InstallRequest{MCVersion: "1.21.0", TargetDir: targetDir}, nil)
	if err != nil {
		t.Fatalf("無 STABLE build 時應退回其他 channel,實得錯誤: %v", err)
	}
	if _, statErr := os.Stat(got.ServerJar); statErr != nil {
		t.Fatalf("server.jar 應存在: %v", statErr)
	}
}

// ---- eula.txt ----

func TestWriteEula_WritesTrueSetting(t *testing.T) {
	dir := t.TempDir()
	if err := WriteEula(dir); err != nil {
		t.Fatalf("WriteEula 失敗: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "eula.txt"))
	if err != nil {
		t.Fatalf("讀取 eula.txt 失敗: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "eula=true") {
		t.Fatalf("eula.txt 應含 eula=true,實得: %q", content)
	}
	if !strings.Contains(content, "MinecraftEULA") {
		t.Fatalf("eula.txt 應含官方 EULA 說明,實得: %q", content)
	}
}

func TestWriteEula_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "instance")
	if err := WriteEula(dir); err != nil {
		t.Fatalf("WriteEula 應自動建立不存在的目錄,實得錯誤: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "eula.txt")); err != nil {
		t.Fatalf("eula.txt 應存在: %v", err)
	}
}
