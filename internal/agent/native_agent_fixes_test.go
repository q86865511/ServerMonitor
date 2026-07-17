package agent

// native-backend 雙審裁決修正的跨平台單元/整合測(#2 實裝路徑、#3 ImportDir 接線、#6 建立中中繼
// 與供應失敗清理、#10 Remove 不無限阻塞、#17 並發 Start 互斥)。Windows 專屬修正(#4/#5/#7)的真機
// 測歸 native_job_windows_test.go / native_adopt_windows_test.go。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/agent/provision"
	"servermonitor/internal/protocol"
)

// ---- #2:真 provider + 迷你 mrpack 走 native 手動上傳,斷言實裝落點(非雙層巢狀)----

// realModrinthProv 是把 NativeBackend 的 InstallModpack 轉呼真 provision.ModrinthProvider 的測試
// provisionRunner(其餘方法沿用 fakeProv 假件);用於驗證 native 傳入的 TargetDir 經真 provider 落地後
// jar 落 <root>/mods/、overrides 落 <root>/<override 相對路徑>,而非雙層 <root>/mods/mods/。
type realModrinthProv struct {
	fakeProv
	provider *provision.ModrinthProvider
}

func (p *realModrinthProv) InstallModpack(ctx context.Context, req ModpackInstallRequest, _ func(protocol.ProvisionProgress)) error {
	var ref *provision.ModpackRef
	if req.ArchivePath == "" {
		ref = &provision.ModpackRef{Type: req.Type, Ref: req.Ref}
	}
	return p.provider.InstallModpack(ctx, provision.ModpackInstallRequest{
		Ref:         ref,
		ArchivePath: req.ArchivePath,
		TargetDir:   req.TargetDir,
		MCVersion:   req.MCVersion,
		Loader:      req.Loader,
	}, nil)
}

// buildMiniMrpack 建一個最小 mrpack:一個模組檔(path "mods/test.jar",自 downloadURL 下載)＋一個
// overrides 檔(overrides/config/foo.toml,相對實例根落地)。
func buildMiniMrpack(t *testing.T, downloadURL, jarSHA512 string, jarSize int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	index := fmt.Sprintf(`{"formatVersion":1,"game":"minecraft","versionId":"v1","name":"mini",`+
		`"dependencies":{"minecraft":"1.20.1","fabric-loader":"0.15.0"},`+
		`"files":[{"path":"mods/test.jar","hashes":{"sha512":%q},"downloads":[%q],"fileSize":%d}]}`,
		jarSHA512, downloadURL, jarSize)
	mustWriteEntry(t, zw, "modrinth.index.json", []byte(index))
	mustWriteEntry(t, zw, "overrides/config/foo.toml", []byte("hello=1\n"))
	if err := zw.Close(); err != nil {
		t.Fatalf("關閉 mrpack zip: %v", err)
	}
	return buf.Bytes()
}

func mustWriteEntry(t *testing.T, zw *zip.Writer, name string, content []byte) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("建立 zip 條目 %s: %v", name, err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("寫入 zip 條目 %s: %v", name, err)
	}
}

func TestNativeBackend_WriteMountFile_RealProvider_LandsUnnested(t *testing.T) {
	jar := []byte("PK-not-really-a-jar-but-bytes")
	sum := sha512.Sum512(jar)
	jarSHA512 := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jar)
	}))
	defer srv.Close()

	prov := &realModrinthProv{
		fakeProv: fakeProv{javaPath: os.Args[0]},
		provider: provision.NewModrinthProvider(srv.Client(), srv.URL),
	}
	b, uuid := createNativeInstanceWithModpackMount(t, prov)

	mrpack := buildMiniMrpack(t, srv.URL+"/test.jar", jarSHA512, int64(len(jar)))
	if err := b.WriteMountFile(context.Background(), uuid, "modpack", "mini.mrpack", bytes.NewReader(mrpack)); err != nil {
		t.Fatalf("WriteMountFile: %v", err)
	}

	root := b.instanceDataRoot(uuid)
	// #2×#7(複審 A):落位跟隨 workDir(=<root>/data,見 helper 設 WorkingDir="data")——
	// jar 落 <root>/data/mods/test.jar(單層),override 落 <root>/data/config/foo.toml;
	// 不得落在實例根層(<root>/mods)或雙層巢狀(mods/mods)。
	if _, err := os.Stat(filepath.Join(root, "data", "mods", "test.jar")); err != nil {
		t.Errorf("模組 jar 未落於 <root>/data/mods/test.jar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "mods", "test.jar")); err == nil {
		t.Error("模組 jar 錯落於實例根層 <root>/mods(未跟隨 workDir)")
	}
	if _, err := os.Stat(filepath.Join(root, "data", "mods", "mods", "test.jar")); err == nil {
		t.Error("偵測到雙層巢狀 mods/mods(TargetDir 語意退化)")
	}
	if _, err := os.Stat(filepath.Join(root, "data", "config", "foo.toml")); err != nil {
		t.Errorf("override 未落於 <root>/data/config/foo.toml: %v", err)
	}
}

// ---- #3:CurseForge ImportDir 接線(<cacheRoot>/cf-imports 被填入且目錄自動建立)----

type capturingModProvider struct {
	req provision.ModpackInstallRequest
}

func (c *capturingModProvider) InstallModpack(_ context.Context, req provision.ModpackInstallRequest, _ provision.ProgressFunc) error {
	c.req = req
	return nil
}

func TestProvisionAdapter_InstallModpack_FillsCurseForgeImportDir(t *testing.T) {
	cacheRoot := t.TempDir()
	adapter := NewProvisionAdapter(cacheRoot)
	capCF := &capturingModProvider{}
	adapter.p.ModProviders["curseforge"] = capCF // 注入假 provider(繞過無 key 時 curseforge 未註冊)

	if err := adapter.InstallModpack(context.Background(), ModpackInstallRequest{
		Type: "curseforge", Ref: "1:2", TargetDir: t.TempDir(),
	}, nil); err != nil {
		t.Fatalf("InstallModpack: %v", err)
	}
	wantImport := filepath.Join(cacheRoot, cfImportSubdir)
	if capCF.req.ImportDir != wantImport {
		t.Errorf("ImportDir = %q,期望 %q", capCF.req.ImportDir, wantImport)
	}
	if info, err := os.Stat(wantImport); err != nil || !info.IsDir() {
		t.Errorf("cf-imports 匯入目錄未自動建立: err=%v", err)
	}

	// modrinth 路徑不應帶 ImportDir(僅 CurseForge 降級路徑使用)。
	capMR := &capturingModProvider{}
	adapter.p.ModProviders["modrinth"] = capMR
	if err := adapter.InstallModpack(context.Background(), ModpackInstallRequest{
		Type: "modrinth", Ref: "x", TargetDir: t.TempDir(),
	}, nil); err != nil {
		t.Fatalf("InstallModpack(modrinth): %v", err)
	}
	if capMR.req.ImportDir != "" {
		t.Errorf("modrinth 路徑不應帶 ImportDir,實得 %q", capMR.req.ImportDir)
	}
}

// ---- #6:建立中最小中繼 + 供應失敗後 List 可見、Remove 可清理 ----

func nativeProvisionSpec(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:     uuid,
		Runtime:  "native",
		Variant:  "vanilla",
		DataDirs: []string{"/data"},
		Labels:   map[string]string{"gsm.uuid": uuid},
		Native: &protocol.NativeSpecPayload{
			Provision: protocol.NativeProvision{Kind: "java", JavaMajor: 21, Loader: "vanilla"},
			Launch:    protocol.NativeLaunch{Command: []string{"noop"}},
		},
	}
}

func TestNativeBackend_Create_ProvisionFailure_VisibleAndRemovable(t *testing.T) {
	prov := &fakeProv{javaPath: os.Args[0], serverErr: fmt.Errorf("模擬供應中途失敗")}
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

	uuid := "uuid-provfail"
	if _, err := b.Create(context.Background(), nativeProvisionSpec(uuid)); err == nil {
		t.Fatal("期望 Create 因供應失敗而回錯,實際成功")
	}

	// #6:供應中途失敗後,最小中繼已寫入 → 實例對本後端可見(instanceExists / List),核心回滾可認領。
	if !b.instanceExists(uuid) {
		t.Fatal("供應失敗後最小中繼缺失,實例不可見(核心回滾 Remove 將拿 404、殘留目錄)")
	}
	refs, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, ref := range refs {
		if ref.Labels[labelUUID] == uuid {
			found = true
		}
	}
	if !found {
		t.Fatal("List 未列出供應失敗的實例(resolve 將無法解析)")
	}

	// Remove(Purge)照樣清理殘留目錄。
	if err := b.Remove(context.Background(), nativeID(uuid), RemoveOpts{Purge: true}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if b.instanceExists(uuid) {
		t.Fatal("Remove 後實例仍存在")
	}
	if _, err := os.Stat(b.instanceDataRoot(uuid)); !os.IsNotExist(err) {
		t.Fatalf("Remove(Purge)後資料目錄應已刪除,stat err=%v", err)
	}
}

// TestNativeBackend_Start_RejectsProvisioningInstance:僅有建立中最小中繼(Provisioning=true)的實例
// 不可啟動(#6 防禦:供應途中崩潰的殘留)。
func TestNativeBackend_Start_RejectsProvisioningInstance(t *testing.T) {
	b := newTestNativeBackend(t)
	uuid := "uuid-provisioning"
	if err := b.writeNativeMeta(uuid, nativeMeta{UUID: uuid, Provisioning: true}); err != nil {
		t.Fatalf("writeNativeMeta: %v", err)
	}
	err := b.Start(context.Background(), nativeID(uuid))
	if err == nil || !strings.Contains(err.Error(), "供應未完成") {
		t.Fatalf("期望啟動供應中實例被拒,實得: %v", err)
	}
}

// ---- #10:Remove 不因 forceKill 失敗而無限阻塞(ctx 取消為逃生閥)----

func TestNativeBackend_Remove_ContextCancelUnblocks(t *testing.T) {
	b := newTestNativeBackend(t)
	uuid := "uuid-removeblock"
	if err := b.writeNativeMeta(uuid, nativeMeta{UUID: uuid}); err != nil {
		t.Fatalf("writeNativeMeta: %v", err)
	}
	// 植入一個「永不結束」的 procHandle:done 永不 close、job=nil(forceKill 走 killProcessTree,對不存在
	// 的 pid 回錯),模擬 forceKill 失敗且行程不死 → 舊碼會在 <-h.done 無限阻塞。
	h := &procHandle{uuid: uuid, pid: 1 << 30, done: make(chan struct{})}
	b.mu.Lock()
	b.procs[uuid] = h
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消:Remove 應迅速回 ctx.Err() 而非卡死

	done := make(chan error, 1)
	go func() { done <- b.Remove(ctx, nativeID(uuid), RemoveOpts{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("期望 Remove 回 ctx 取消錯誤,實得 nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Remove 在 forceKill 失敗且行程不死時無限阻塞(#10 未修正)")
	}
}

// ---- #17:並發雙 Start 只有其一成功起行程 ----

func TestNativeBackend_Start_ConcurrentSingleWinner(t *testing.T) {
	b := newTestNativeBackend(t)
	ctx := context.Background()
	id, err := b.Create(ctx, nativeHelperSpec("uuid-concstart"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop(ctx, id, StopOpts{}) })

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = b.Start(ctx, id)
		}(i)
	}
	close(start)
	wg.Wait()

	success := 0
	for _, e := range errs {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("並發 %d 個 Start 應恰一成功(其餘回已在執行/啟動中),實得 %d 成功", n, success)
	}
	if st, _ := b.Status(ctx, id); !st.Running {
		t.Fatal("成功 Start 後實例應 running")
	}
}
