package agent

// 收養(T11)與啟動來源接線的跨平台單元測。真收養活行程需真 Windows(見 native_adopt_windows_test.go);
// 本檔涵蓋:死行程收養→合成 die(非 Windows 恆走死路徑,Windows 以已死 PID 觸發)、{start_script}
// token 與 server_jar 校驗、MCVersion 優先序、steamcmd update_on_start。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// newNativeBackendAt 以指定根目錄與供應器建 NativeBackend(供收養需跨實例共用同一資料根的測試)。
func newNativeBackendAt(t *testing.T, root string, prov provisionRunner) *NativeBackend {
	t.Helper()
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   filepath.Join(root, "data"),
		BackupRoot: filepath.Join(root, "backups"),
		CacheRoot:  filepath.Join(root, "cache"),
		Node:       "test-node",
		Prov:       prov,
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	return b
}

// spawnDeadPID 起一個立即結束的行程並回收,回傳其(已死)PID。
func spawnDeadPID(t *testing.T) int {
	t.Helper()
	var name string
	var args []string
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/c", "exit"}
	} else {
		name, args = "true", nil
	}
	c := exec.Command(name, args...)
	if err := c.Start(); err != nil {
		t.Fatalf("啟動短命行程: %v", err)
	}
	pid := c.Process.Pid
	_ = c.Wait()
	return pid
}

// TestNativeAdopt_DeadProcessSynthesizesDie:proc.json 指向已死 PID(且起行程時刻遠在過去,防 PID
// 重用誤判)→ 新 backend 建構收養時清 proc.json、Status 回 exited(供 Reconciler 修正 running→
// stopped)、並合成 die 事件(留存於 hub 歷史,以游標重播可得)。跨平台:非 Windows 恆走死路徑。
func TestNativeAdopt_DeadProcessSynthesizesDie(t *testing.T) {
	root := t.TempDir()
	prov := &fakeProv{javaPath: os.Args[0], serverJar: "server.jar"}
	ctx := context.Background()

	b1 := newNativeBackendAt(t, root, prov)
	id, err := b1.Create(ctx, nativeHelperSpec("uuid-dead"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	deadPID := spawnDeadPID(t)
	if err := b1.writeProcMeta("uuid-dead", procMeta{
		UUID: "uuid-dead", PID: deadPID,
		StartTime: time.Unix(1_000_000_000, 0).UTC(), // 2001 年:即便 PID 被重用,start-time 亦不符
		Command:   []string{"x"}, WorkDir: root,
	}); err != nil {
		t.Fatalf("writeProcMeta: %v", err)
	}
	_ = b1.Close()

	// 新 agent:建構即收養掃描。
	b2 := newNativeBackendAt(t, root, prov)
	t.Cleanup(func() { _ = b2.Close() })

	// proc.json 應被清除。
	if _, serr := os.Stat(b2.procMetaPath("uuid-dead")); !os.IsNotExist(serr) {
		t.Fatalf("死行程收養應清 proc.json, stat err=%v", serr)
	}
	// Status 應回 exited(Reconciler 據以修正 DB running→stopped)。
	st, err := b2.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != protocol.RuntimeStateExited {
		t.Fatalf("死行程收養後 State=%s want exited", st.State)
	}
	// List 亦應回 exited。
	refs, _ := b2.List(ctx)
	found := false
	for _, r := range refs {
		if r.ID == id {
			found = true
			if r.State != protocol.RuntimeStateExited {
				t.Fatalf("List State=%s want exited", r.State)
			}
		}
	}
	if !found {
		t.Fatal("List 未含收養實例")
	}
	// 合成 die 應留於 hub 歷史(以 since=0 重播)。
	es, err := b2.Events(ctx, "0")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer es.Close()
	select {
	case ev := <-es.Events():
		if ev.ID != id || ev.Kind != RuntimeEventDie {
			t.Fatalf("重播首事件=%+v, 期望 die/%s", ev, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未於歷史重播得合成 die")
	}
}

// TestExpandLaunchTokens_StartScript:{start_script} 展開為腳本路徑;StartScript 供應(無 jar)卻
// 引用 {server_jar} 時,T13 解法(a)就地改寫為腳本啟動(丟棄 jar 前綴、保留尾隨參數),不回錯。
func TestExpandLaunchTokens_StartScript(t *testing.T) {
	m := nativeMeta{JavaPath: "java", StartScript: "/srv/run.bat", MemoryMB: 1024}
	got, err := expandLaunchTokens([]string{"{start_script}", "--dir", "{instance_dir}"}, m, "/srv")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got[0] != "/srv/run.bat" || got[2] != "/srv" {
		t.Fatalf("expanded=%v", got)
	}
	// StartScript 供應、範本仍為 jar 型命令(引用 {server_jar}):改寫為 [start_script, <尾隨參數>]。
	got2, err := expandLaunchTokens([]string{"{java}", "-jar", "{server_jar}", "nogui"}, m, "/srv")
	if err != nil {
		t.Fatalf("改寫路徑不應回錯: %v", err)
	}
	if len(got2) != 2 || got2[0] != "/srv/run.bat" || got2[1] != "nogui" {
		t.Fatalf("start-script 改寫=%v,期望 [/srv/run.bat nogui]", got2)
	}
}

// TestMCVersionOrDerive:payload MCVersion 優先;缺值退回 Variant 慣例導出。
func TestMCVersionOrDerive(t *testing.T) {
	if v := mcVersionOrDerive(ServerInstallRequest{MCVersion: "1.21.1", Variant: "paper-1.20", Loader: "paper"}); v != "1.21.1" {
		t.Fatalf("payload 應優先, 得 %q", v)
	}
	if v := mcVersionOrDerive(ServerInstallRequest{Variant: "paper-1.20", Loader: "paper"}); v != "1.20" {
		t.Fatalf("缺 payload 應由 Variant 導出 1.20, 得 %q", v)
	}
}

// countingSteamProv 記錄 InstallSteamApp 呼叫次數,可注入失敗(驗 update_on_start)。
type countingSteamProv struct {
	fakeProv
	mu     sync.Mutex
	calls  int
	failAt bool
}

func (p *countingSteamProv) InstallSteamApp(_ context.Context, _ string, _ string, _ func(protocol.ProvisionProgress)) error {
	p.mu.Lock()
	p.calls++
	fail := p.failAt
	p.mu.Unlock()
	if fail {
		return errors.New("模擬 SteamCMD 更新失敗")
	}
	return nil
}

func (p *countingSteamProv) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *countingSteamProv) setFail(v bool) {
	p.mu.Lock()
	p.failAt = v
	p.mu.Unlock()
}

// steamHelperSpec 產生一個 steamcmd 供應、UpdateOnStart 開啟、以助手行程為啟動命令的 native spec。
func steamHelperSpec(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:     uuid,
		Runtime:  "native",
		DataDirs: []string{"/data"},
		Env:      map[string]string{"GSM_NATIVE_HELPER": "1"},
		Labels:   map[string]string{"gsm.uuid": uuid},
		Native: &protocol.NativeSpecPayload{
			Provision: protocol.NativeProvision{Kind: "steamcmd", SteamAppID: "2394010", UpdateOnStart: true},
			Launch:    protocol.NativeLaunch{Command: []string{os.Args[0], "-test.run=TestNativeHelperProcess"}},
		},
	}
}

// TestNativeStart_UpdateOnStart:kind=steamcmd 且 UpdateOnStart 時,Start 前重跑 InstallSteamApp;
// 更新失敗使啟動失敗(不起行程)。
func TestNativeStart_UpdateOnStart(t *testing.T) {
	root := t.TempDir()
	prov := &countingSteamProv{fakeProv: fakeProv{javaPath: os.Args[0]}}
	b := newNativeBackendAt(t, root, prov)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()

	id, err := b.Create(ctx, steamHelperSpec("uuid-upd"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if prov.callCount() != 1 {
		t.Fatalf("Create 應供應一次 SteamCMD, 得 %d", prov.callCount())
	}

	// 成功路徑:Start 前重跑 app_update(呼叫數 +1),行程起動。
	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if prov.callCount() != 2 {
		t.Fatalf("啟動前應重跑 app_update(呼叫應為 2), 得 %d", prov.callCount())
	}
	if st, _ := b.Status(ctx, id); !st.Running {
		t.Fatal("Start 後應 running")
	}
	if err := b.Stop(ctx, id, StopOpts{Grace: 100 * time.Millisecond}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// 失敗路徑:更新失敗 → Start 回錯、不起行程。
	prov.setFail(true)
	if err := b.Start(ctx, id); err == nil {
		t.Fatal("update_on_start 失敗應使 Start 失敗")
	}
	if st, _ := b.Status(ctx, id); st.Running {
		t.Fatal("更新失敗後不應有執行中行程")
	}
}
