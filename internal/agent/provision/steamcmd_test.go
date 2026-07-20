package provision

import (
	"archive/zip"
	"bytes"
	"context"
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

// makeSteamCMDZip 產生一個最小的假 SteamCMD zip(steamcmd.exe 位於頂層,模擬 Valve 官方佈局)。
func makeSteamCMDZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("steamcmd.exe")
	if err != nil {
		t.Fatalf("建立 zip 條目失敗: %v", err)
	}
	if _, err := w.Write([]byte("fake-steamcmd-binary")); err != nil {
		t.Fatalf("寫入 zip 條目失敗: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("關閉 zip 失敗: %v", err)
	}
	return buf.Bytes()
}

// fakeSteamCMDDownload 是模擬 steamcmd.zip 載點的 httptest server。hits 累計下載次數。
func fakeSteamCMDDownload(t *testing.T, zipBytes []byte, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(zipBytes)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSteamCMDProvisioner_EnsureDownloadsAndCaches(t *testing.T) {
	zipBytes := makeSteamCMDZip(t)
	var hits int32
	srv := fakeSteamCMDDownload(t, zipBytes, &hits)

	cacheRoot := t.TempDir()
	s := NewSteamCMDProvisioner(cacheRoot, WithSteamCMDHTTPClient(srv.Client()), WithSteamCMDBaseURL(srv.URL))

	exe1, err := s.Ensure(context.Background(), nil)
	if err != nil {
		t.Fatalf("第一次 Ensure 失敗: %v", err)
	}
	if _, err := os.Stat(exe1); err != nil {
		t.Fatalf("steamcmd.exe 不存在: %v", err)
	}
	wantExe := filepath.Join(cacheRoot, "steamcmd", "steamcmd.exe")
	if exe1 != wantExe {
		t.Fatalf("回傳路徑 = %q,期望 %q", exe1, wantExe)
	}

	exe2, err := s.Ensure(context.Background(), nil)
	if err != nil {
		t.Fatalf("第二次 Ensure 失敗: %v", err)
	}
	if exe1 != exe2 {
		t.Fatalf("兩次回傳路徑不一致: %q vs %q", exe1, exe2)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("快取命中應不重下,實際下載次數 = %d", got)
	}
}

func TestSteamCMDProvisioner_EnsureCacheHitSkipsDownload(t *testing.T) {
	cacheRoot := t.TempDir()
	dir := filepath.Join(cacheRoot, "steamcmd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("預先建立快取目錄失敗: %v", err)
	}
	exePath := filepath.Join(dir, "steamcmd.exe")
	if err := os.WriteFile(exePath, []byte("preexisting"), 0o755); err != nil {
		t.Fatalf("預先寫入 steamcmd.exe 失敗: %v", err)
	}

	var hits int32
	// server 存在但不應被打到:baseURL 指向會累計 hits 的 server,若被呼叫測試會抓到。
	srv := fakeSteamCMDDownload(t, makeSteamCMDZip(t), &hits)
	s := NewSteamCMDProvisioner(cacheRoot, WithSteamCMDHTTPClient(srv.Client()), WithSteamCMDBaseURL(srv.URL))

	exe, err := s.Ensure(context.Background(), nil)
	if err != nil {
		t.Fatalf("Ensure 失敗: %v", err)
	}
	if exe != exePath {
		t.Fatalf("回傳路徑 = %q,期望 %q", exe, exePath)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("快取命中不應下載,實際下載次數 = %d", got)
	}
}

func TestSteamCMDProvisioner_EnsureProgressCallbackInvoked(t *testing.T) {
	zipBytes := makeSteamCMDZip(t)
	var hits int32
	srv := fakeSteamCMDDownload(t, zipBytes, &hits)

	cacheRoot := t.TempDir()
	s := NewSteamCMDProvisioner(cacheRoot, WithSteamCMDHTTPClient(srv.Client()), WithSteamCMDBaseURL(srv.URL))

	var mu sync.Mutex
	var stages []string
	progress := func(pp ProvisionProgress) {
		mu.Lock()
		stages = append(stages, pp.Stage)
		mu.Unlock()
	}

	if _, err := s.Ensure(context.Background(), progress); err != nil {
		t.Fatalf("Ensure 失敗: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	sawDownload := false
	for _, st := range stages {
		if strings.Contains(st, "下載 SteamCMD") {
			sawDownload = true
		}
	}
	if !sawDownload {
		t.Fatalf("進度回呼未涵蓋下載階段,實得: %v", stages)
	}
}

// preSeedCache 在 cacheRoot 下預先放入一個假 steamcmd.exe,讓 InstallApp 測試不需真的下載。
func preSeedCache(t *testing.T, cacheRoot string) string {
	t.Helper()
	dir := filepath.Join(cacheRoot, "steamcmd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("預先建立快取目錄失敗: %v", err)
	}
	exePath := filepath.Join(dir, "steamcmd.exe")
	if err := os.WriteFile(exePath, []byte("preexisting"), 0o755); err != nil {
		t.Fatalf("預先寫入 steamcmd.exe 失敗: %v", err)
	}
	return exePath
}

func TestSteamCMDProvisioner_InstallApp_SuccessReportsProgress(t *testing.T) {
	cacheRoot := t.TempDir()
	wantExe := preSeedCache(t, cacheRoot)

	var gotExe string
	var gotArgs []string
	fakeExec := func(ctx context.Context, exe string, args []string, onLine func(string)) error {
		gotExe = exe
		gotArgs = args
		onLine("Redirecting stderr to 'C:\\steamcmd\\logs\\stderr.txt'")
		onLine("Update state (0x61) downloading, progress: 45.50 (450 / 1000)")
		onLine("Update state (0x81) verifying update, progress: 100.00 (1000 / 1000)")
		onLine("Success! App '2394010' fully installed.")
		return nil
	}

	s := NewSteamCMDProvisioner(cacheRoot, WithSteamCMDExec(fakeExec))

	var mu sync.Mutex
	var percents []int
	progress := func(pp ProvisionProgress) {
		mu.Lock()
		percents = append(percents, pp.Percent)
		mu.Unlock()
	}

	installDir := filepath.Join(t.TempDir(), "palworld")
	if err := s.InstallApp(context.Background(), "2394010", installDir, progress); err != nil {
		t.Fatalf("InstallApp 失敗: %v", err)
	}

	if gotExe != wantExe {
		t.Fatalf("傳入 exec 的 exe = %q,期望 %q", gotExe, wantExe)
	}

	wantArgs := []string{
		"+force_install_dir", installDir,
		"+login", "anonymous",
		"+app_update", "2394010", "validate",
		"+quit",
	}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args 長度 = %d,期望 %d;實得 %v", len(gotArgs), len(wantArgs), gotArgs)
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Fatalf("args[%d] = %q,期望 %q;完整 args = %v", i, gotArgs[i], wantArgs[i], gotArgs)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	sawPct := false
	for _, p := range percents {
		if p == 45 || p == 100 {
			sawPct = true
		}
	}
	if !sawPct {
		t.Fatalf("進度回呼應涵蓋解析出的百分比,實得: %v", percents)
	}
}

func TestSteamCMDProvisioner_InstallApp_ExitError(t *testing.T) {
	cacheRoot := t.TempDir()
	preSeedCache(t, cacheRoot)

	wantErr := errors.New("exit status 7")
	fakeExec := func(ctx context.Context, exe string, args []string, onLine func(string)) error {
		onLine("Update state (0x61) downloading, progress: 10.00 (100 / 1000)")
		return wantErr
	}
	s := NewSteamCMDProvisioner(cacheRoot, WithSteamCMDExec(fakeExec))

	err := s.InstallApp(context.Background(), "2394010", filepath.Join(t.TempDir(), "palworld"), nil)
	if err == nil {
		t.Fatal("exit error 應回錯,實得 nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("錯誤應可 errors.Is 包裝的 exit error,實得: %v", err)
	}
}

func TestSteamCMDProvisioner_InstallApp_ErrorLineInOutput(t *testing.T) {
	cacheRoot := t.TempDir()
	preSeedCache(t, cacheRoot)

	fakeExec := func(ctx context.Context, exe string, args []string, onLine func(string)) error {
		onLine("Update state (0x61) downloading, progress: 10.00 (100 / 1000)")
		onLine("ERROR! Failed to install app '2394010' (No subscription).")
		return nil // exit code 0,但輸出含錯誤標記。
	}
	s := NewSteamCMDProvisioner(cacheRoot, WithSteamCMDExec(fakeExec))

	err := s.InstallApp(context.Background(), "2394010", filepath.Join(t.TempDir(), "palworld"), nil)
	if err == nil {
		t.Fatal("輸出含 Error! 應回錯,實得 nil")
	}
	if !strings.Contains(err.Error(), "No subscription") {
		t.Fatalf("錯誤訊息應包含 SteamCMD 錯誤行內容,實得: %v", err)
	}
}

// TestSteamCMDProvisioner_InstallApp_RetriesFirstRunMissingConfig 驗證 B13:全新 SteamCMD 首跑
// app_update 以 "Missing configuration"(exit 7)失敗時,重試一次即成功(SteamCMD 於首次失敗後自我
// 更新,重試即就緒)。實機已重現此序列。
func TestSteamCMDProvisioner_InstallApp_RetriesFirstRunMissingConfig(t *testing.T) {
	cacheRoot := t.TempDir()
	preSeedCache(t, cacheRoot)

	var calls int32
	fakeExec := func(ctx context.Context, exe string, args []string, onLine func(string)) error {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// 首次:尚未自我更新→app_update 失敗,隨後(同次行程)才自我更新。
			onLine("Connecting anonymously to Steam Public...OK")
			onLine("ERROR! Failed to install app '2394010' (Missing configuration)")
			return errors.New("exit status 7")
		}
		// 重試:SteamCMD 已更新,順利安裝。
		onLine("Update state (0x61) downloading, progress: 100.00 (1000 / 1000)")
		onLine("Success! App '2394010' fully installed.")
		return nil
	}
	s := NewSteamCMDProvisioner(cacheRoot, WithSteamCMDExec(fakeExec))

	if err := s.InstallApp(context.Background(), "2394010", filepath.Join(t.TempDir(), "palworld"), nil); err != nil {
		t.Fatalf("首次 Missing configuration 後重試應成功,實得: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("應嘗試 2 次(首失敗+重試成功),實際 exec 次數 = %d", got)
	}
}

// TestSteamCMDProvisioner_InstallApp_NoRetryOnRealError 驗證 B13 的重試限定:真實永久錯誤
// (No subscription)不重試,第 1 次即返回錯誤。
func TestSteamCMDProvisioner_InstallApp_NoRetryOnRealError(t *testing.T) {
	cacheRoot := t.TempDir()
	preSeedCache(t, cacheRoot)

	var calls int32
	fakeExec := func(ctx context.Context, exe string, args []string, onLine func(string)) error {
		atomic.AddInt32(&calls, 1)
		onLine("ERROR! Failed to install app '2394010' (No subscription).")
		return errors.New("exit status 8")
	}
	s := NewSteamCMDProvisioner(cacheRoot, WithSteamCMDExec(fakeExec))

	err := s.InstallApp(context.Background(), "2394010", filepath.Join(t.TempDir(), "palworld"), nil)
	if err == nil {
		t.Fatal("No subscription 應回錯,實得 nil")
	}
	if !strings.Contains(err.Error(), "No subscription") {
		t.Fatalf("錯誤應含原始 SteamCMD 錯誤行,實得: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("真實錯誤不應重試,實際 exec 次數 = %d", got)
	}
}

func TestSteamCMDProvisioner_ParseSteamCMDLine(t *testing.T) {
	pct, detail := parseSteamCMDLine("Update state (0x61) downloading, progress: 32.15 (1234 / 5678)")
	if pct != 32 {
		t.Fatalf("percent = %d,期望 32", pct)
	}
	if detail == "" {
		t.Fatal("detail 不應為空")
	}

	pct2, _ := parseSteamCMDLine("Connecting anonymously to Steam Public...")
	if pct2 != -1 {
		t.Fatalf("解析不到進度時 percent 應為 -1,實得 %d", pct2)
	}
}
