package provision

// ModrinthProvider 單元測試(native-backend R11):ArchivePath 手動路徑略過解析;Ref 解析先試
// 直接版本 ID(GET /version/{id}),404 時退回 slug/專案 ID 查相容版本清單(GET
// /project/{ref}/version);清單為空時回明確錯誤。API 端點形狀查證見 modrinth.go 頂註。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModrinthProvider_InstallModpack_ArchivePath_SkipsAPI(t *testing.T) {
	apiHit := false
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiHit = true
		w.WriteHeader(http.StatusNotFound)
	})
	apiSrv := httptest.NewServer(apiMux)
	defer apiSrv.Close()

	files := []mrpackFileSpec{{path: "mods/A.jar", content: []byte("AAA")}}
	dlSrv := fakeModDownloadServer(t, files, nil)
	zipBytes := buildMrpackZip(t, map[string]string{"minecraft": "1.20.1"}, files, nil, nil, dlSrv.URL)
	archivePath := writeTempMrpack(t, zipBytes)

	p := NewModrinthProvider(dlSrv.Client(), apiSrv.URL)
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		ArchivePath: archivePath,
		TargetDir:   t.TempDir(),
		MCVersion:   "1.20.1",
	}, nil)
	if err != nil {
		t.Fatalf("InstallModpack: %v", err)
	}
	if apiHit {
		t.Error("ArchivePath 非空時不應查詢 Modrinth API")
	}
}

func TestModrinthProvider_InstallModpack_MissingSource(t *testing.T) {
	p := NewModrinthProvider(nil, "")
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{TargetDir: t.TempDir()}, nil)
	if err == nil {
		t.Fatal("Ref 與 ArchivePath 皆空應回錯,實際成功")
	}
}

// modrinthAPIAndDownloadServer 起單一 httptest server,同時扮演 Modrinth API 與檔案下載端點
// (mrpack 官方下載連結亦落在同網域下,合併省一個 server)。
func modrinthAPIAndDownloadServer(t *testing.T, register func(mux *http.ServeMux, srv **httptest.Server)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	register(mux, &srv)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestModrinthProvider_Resolve_DirectVersionID(t *testing.T) {
	files := []mrpackFileSpec{{path: "mods/A.jar", content: []byte("AAA")}}
	var mrpackBytes []byte // 延後填,resolve 後才需序列化下載端點內容
	var projectVersionHit bool

	srv := modrinthAPIAndDownloadServer(t, func(mux *http.ServeMux, srvRef **httptest.Server) {
		mux.HandleFunc("/version/ver123", func(w http.ResponseWriter, r *http.Request) {
			ver := modrinthVersion{
				ID: "ver123",
				Files: []modrinthVersionFile{
					{Filename: "pack.mrpack", Primary: true, URL: (*srvRef).URL + "/dl/pack.mrpack"},
				},
			}
			_ = json.NewEncoder(w).Encode(ver)
		})
		mux.HandleFunc("/project/", func(w http.ResponseWriter, r *http.Request) {
			projectVersionHit = true
			w.WriteHeader(http.StatusNotFound)
		})
		mux.HandleFunc("/dl/pack.mrpack", func(w http.ResponseWriter, r *http.Request) {
			w.Write(mrpackBytes)
		})
		mux.HandleFunc("/dl/mods/A.jar", func(w http.ResponseWriter, r *http.Request) {
			w.Write(files[0].content)
		})
	})

	mrpackBytes = buildMrpackZip(t, map[string]string{"minecraft": "1.20.1"}, files, nil, nil, srv.URL)

	p := NewModrinthProvider(srv.Client(), srv.URL)
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		Ref:       &ModpackRef{Type: "modrinth", Ref: "ver123"},
		TargetDir: t.TempDir(),
		MCVersion: "1.20.1",
	}, nil)
	if err != nil {
		t.Fatalf("InstallModpack: %v", err)
	}
	if projectVersionHit {
		t.Error("直接版本 ID 命中時不應退回 /project/{ref}/version 查詢")
	}
}

func TestModrinthProvider_Resolve_SlugFallsBackToProjectVersion(t *testing.T) {
	files := []mrpackFileSpec{{path: "mods/A.jar", content: []byte("AAA")}}
	var mrpackBytes []byte
	var gotQuery string

	srv := modrinthAPIAndDownloadServer(t, func(mux *http.ServeMux, srvRef **httptest.Server) {
		mux.HandleFunc("/version/cobblemon-fabric", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound) // 非版本 ID,退回 slug 查詢。
		})
		mux.HandleFunc("/project/cobblemon-fabric/version", func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			versions := []modrinthVersion{{
				ID: "verABC",
				Files: []modrinthVersionFile{
					{Filename: "cobblemon.mrpack", Primary: true, URL: (*srvRef).URL + "/dl/pack.mrpack"},
				},
			}}
			_ = json.NewEncoder(w).Encode(versions)
		})
		mux.HandleFunc("/dl/pack.mrpack", func(w http.ResponseWriter, r *http.Request) {
			w.Write(mrpackBytes)
		})
		mux.HandleFunc("/dl/mods/A.jar", func(w http.ResponseWriter, r *http.Request) {
			w.Write(files[0].content)
		})
	})

	mrpackBytes = buildMrpackZip(t, map[string]string{"minecraft": "1.20.1", "fabric-loader": "0.15.0"}, files, nil, nil, srv.URL)

	p := NewModrinthProvider(srv.Client(), srv.URL)
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		Ref:       &ModpackRef{Type: "modrinth", Ref: "cobblemon-fabric"},
		TargetDir: t.TempDir(),
		MCVersion: "1.20.1",
		Loader:    "fabric",
	}, nil)
	if err != nil {
		t.Fatalf("InstallModpack: %v", err)
	}
	if !strings.Contains(gotQuery, "game_versions") || !strings.Contains(gotQuery, "loaders") {
		t.Errorf("查詢參數應含 game_versions 與 loaders,實得 %q", gotQuery)
	}
}

func TestModrinthProvider_Resolve_NoCompatibleVersion(t *testing.T) {
	srv := modrinthAPIAndDownloadServer(t, func(mux *http.ServeMux, srvRef **httptest.Server) {
		mux.HandleFunc("/version/some-modpack", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		mux.HandleFunc("/project/some-modpack/version", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]modrinthVersion{})
		})
	})

	p := NewModrinthProvider(srv.Client(), srv.URL)
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		Ref:       &ModpackRef{Type: "modrinth", Ref: "some-modpack"},
		TargetDir: t.TempDir(),
		MCVersion: "9.9.9",
	}, nil)
	if err == nil {
		t.Fatal("無相容版本應回錯,實際成功")
	}
}

func TestModrinthProvider_Resolve_VersionWithoutMrpackFile(t *testing.T) {
	srv := modrinthAPIAndDownloadServer(t, func(mux *http.ServeMux, srvRef **httptest.Server) {
		mux.HandleFunc("/version/ver-nofile", func(w http.ResponseWriter, r *http.Request) {
			ver := modrinthVersion{ID: "ver-nofile", Files: []modrinthVersionFile{{Filename: "readme.txt", Primary: true, URL: "http://x/readme.txt"}}}
			_ = json.NewEncoder(w).Encode(ver)
		})
	})

	p := NewModrinthProvider(srv.Client(), srv.URL)
	err := p.InstallModpack(context.Background(), ModpackInstallRequest{
		Ref:       &ModpackRef{Type: "modrinth", Ref: "ver-nofile"},
		TargetDir: t.TempDir(),
	}, nil)
	if err == nil {
		t.Fatal("版本無 .mrpack 檔應回錯,實際成功")
	}
}
