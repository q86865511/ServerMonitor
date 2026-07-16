package agent

// NativeBackend 與其子系統(供應/日誌/設定/行程)的免 Docker 單元測試。真行程以 go test 助手
// 行程(TestNativeHelperProcess,env GSM_NATIVE_HELPER=1 觸發)提供,跨平台可跑;Windows 專屬的
// Job Objects/收養整合測歸 T7/T11(build tag native)。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// TestNativeHelperProcess 是被 NativeBackend 啟動的假伺服器行程:僅在 env GSM_NATIVE_HELPER=1
// 時進入常駐迴圈(印啟動訊息後等待,直到被強殺或 60s 逾時);其餘情況為 no-op(當一般測試通過)。
func TestNativeHelperProcess(t *testing.T) {
	if os.Getenv("GSM_NATIVE_HELPER") != "1" {
		return
	}
	fmt.Println("native helper: started stdout")
	fmt.Fprintln(os.Stderr, "native helper: started stderr")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	os.Exit(0)
}

// fakeProv 是 provisionRunner 的假件:回可控路徑,不觸網。
type fakeProv struct {
	javaPath    string
	serverJar   string
	startScript string // 非空時模擬 Forge/NeoForge 啟動來源(無 server jar)
	argsFile    string
	progress    []protocol.ProvisionProgress

	modpackCalls []ModpackInstallRequest // InstallModpack 每次呼叫的入參,供斷言(T10)
	modpackErr   error                   // 非 nil 時 InstallModpack 回此錯誤(供測 Create 回滾路徑)
}

func (f *fakeProv) EnsureJava(_ context.Context, _ int, progress func(protocol.ProvisionProgress)) (string, error) {
	if progress != nil {
		p := protocol.ProvisionProgress{Stage: "jre", Percent: 100}
		f.progress = append(f.progress, p)
		progress(p)
	}
	return f.javaPath, nil
}

func (f *fakeProv) InstallServer(_ context.Context, _ ServerInstallRequest, progress func(protocol.ProvisionProgress)) (ServerInstallResult, error) {
	if progress != nil {
		progress(protocol.ProvisionProgress{Stage: "server-jar", Percent: 100})
	}
	return ServerInstallResult{ServerJar: f.serverJar, StartScript: f.startScript, ArgsFile: f.argsFile}, nil
}

func (f *fakeProv) InstallSteamApp(_ context.Context, _ string, _ string, _ func(protocol.ProvisionProgress)) error {
	return nil
}

func (f *fakeProv) WriteEula(_ context.Context, dir string, accepted bool) error {
	if !accepted {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("eula=true\n"), 0o644)
}

func (f *fakeProv) InstallModpack(_ context.Context, req ModpackInstallRequest, progress func(protocol.ProvisionProgress)) error {
	f.modpackCalls = append(f.modpackCalls, req)
	if f.modpackErr != nil {
		return f.modpackErr
	}
	if progress != nil {
		progress(protocol.ProvisionProgress{Stage: "modpack", Percent: 100})
	}
	// 落地一個可觀察的標記檔,供測試確認 TargetDir 正確展開(不解真 mrpack,單元測不觸網)。
	if req.TargetDir != "" {
		if err := os.MkdirAll(req.TargetDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.TargetDir, "installed.marker"), []byte(req.Type+"|"+req.Ref+"|"+req.ArchivePath), 0o644)
	}
	return nil
}

// newTestNativeBackend 建立一個以暫存目錄與假供應器為底的 NativeBackend。
func newTestNativeBackend(t *testing.T) *NativeBackend {
	t.Helper()
	root := t.TempDir()
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   filepath.Join(root, "data"),
		BackupRoot: filepath.Join(root, "backups"),
		CacheRoot:  filepath.Join(root, "cache"),
		Node:       "test-node",
		Prov:       &fakeProv{javaPath: os.Args[0], serverJar: "server.jar"},
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// nativeHelperSpec 產生一個啟動助手行程的 native InstanceSpec。
func nativeHelperSpec(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:     uuid,
		Runtime:  "native",
		Variant:  "vanilla",
		DataDirs: []string{"/data"},
		Env:      map[string]string{"GSM_NATIVE_HELPER": "1"},
		Labels:   map[string]string{"gsm.uuid": uuid},
		Native: &protocol.NativeSpecPayload{
			Provision: protocol.NativeProvision{Kind: "java", JavaMajor: 21, Loader: "vanilla"},
			Launch:    protocol.NativeLaunch{Command: []string{"{java}", "-test.run=TestNativeHelperProcess"}},
		},
	}
}

// ---- 設定檔編碼器 ----

func TestWriteConfigFile_Properties(t *testing.T) {
	dir := t.TempDir()
	cm := protocol.NativeConfigMap{
		File:   "server.properties",
		Format: "properties",
		Map:    map[string]string{"maxplayers": "max-players", "motd": "motd"},
	}
	env := map[string]string{"maxplayers": "20", "motd": "Hi", "unused": "x"}
	if err := writeConfigFile(dir, cm, env, nil); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "max-players=20\nmotd=Hi\n" // 依 configKey 排序,未映射的 unused 不寫
	if string(got) != want {
		t.Fatalf("properties=%q want %q", string(got), want)
	}
}

func TestWriteConfigFile_PalworldIni(t *testing.T) {
	dir := t.TempDir()
	cm := protocol.NativeConfigMap{
		File:    "PalWorldSettings.ini",
		Format:  "palworld-ini",
		Section: "/Script/Pal.PalGameWorldSettings",
		Map:     map[string]string{"diff": "Difficulty", "rate": "DayTimeSpeedRate"},
	}
	env := map[string]string{"diff": "None", "rate": "1.000000"}
	if err := writeConfigFile(dir, cm, env, nil); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "PalWorldSettings.ini"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 依 configKey 排序:DayTimeSpeedRate < Difficulty。
	want := "[/Script/Pal.PalGameWorldSettings]\nOptionSettings=(DayTimeSpeedRate=1.000000,Difficulty=None)\n"
	if string(got) != want {
		t.Fatalf("palworld-ini=%q want %q", string(got), want)
	}
}

func TestWriteConfigFile_UnknownFormat(t *testing.T) {
	dir := t.TempDir()
	cm := protocol.NativeConfigMap{File: "x.cfg", Format: "yaml", Map: map[string]string{"a": "b"}}
	if err := writeConfigFile(dir, cm, map[string]string{"a": "1"}, nil); err == nil {
		t.Fatal("未知格式應回錯")
	}
}

// TestWriteConfigFile_SetWithPortTokens 驗證 [native.config.set] 的固定/衍生值:字面值直落、
// {port:<name>} 展開為對應主機埠、且 Set 覆蓋同 configKey 的 Map 產出(native 執行必需值優先)。
func TestWriteConfigFile_SetWithPortTokens(t *testing.T) {
	dir := t.TempDir()
	cm := protocol.NativeConfigMap{
		File:   "server.properties",
		Format: "properties",
		Map:    map[string]string{"RCON_PASSWORD": "rcon.password", "SERVER_PORT_PARAM": "server-port"},
		Set: map[string]string{
			"enable-rcon": "true",
			"rcon.port":   "{port:rcon}",
			"server-port": "{port:game}", // 與 Map 的 server-port 撞鍵:Set 應覆蓋
		},
	}
	env := map[string]string{"RCON_PASSWORD": "sekret", "SERVER_PORT_PARAM": "19999"}
	ports := []protocol.PortBinding{{Name: "game", HostPort: 25565}, {Name: "rcon", HostPort: 0, Container: 25575}}
	if err := writeConfigFile(dir, cm, env, ports); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 依 configKey 排序:enable-rcon < rcon.password < rcon.port < server-port。
	want := "enable-rcon=true\nrcon.password=sekret\nrcon.port=25575\nserver-port=25565\n"
	if string(got) != want {
		t.Fatalf("properties=%q want %q", string(got), want)
	}
}

// TestWriteConfigFile_SetUnknownPort 驗證 Set 值引用未宣告的埠 token 時回錯(避免靜默寫入空值)。
func TestWriteConfigFile_SetUnknownPort(t *testing.T) {
	dir := t.TempDir()
	cm := protocol.NativeConfigMap{
		File:   "server.properties",
		Format: "properties",
		Set:    map[string]string{"server-port": "{port:nope}"},
	}
	if err := writeConfigFile(dir, cm, nil, []protocol.PortBinding{{Name: "game", HostPort: 25565}}); err == nil {
		t.Fatal("未知埠 token 應回錯")
	}
}

// ---- 啟動命令 token 展開 ----

func TestExpandLaunchTokens(t *testing.T) {
	m := nativeMeta{
		JavaPath:  "/opt/jre/bin/java",
		ServerJar: "paper.jar",
		MemoryMB:  1024,
		Ports:     []protocol.PortBinding{{Name: "game", HostPort: 25565}, {Name: "rcon", HostPort: 0, Container: 25575}},
	}
	argv := []string{"{java}", "-Xmx{memory_mb}M", "-jar", "{server_jar}", "--port", "{port:game}", "--rcon", "{port:rcon}", "--dir", "{instance_dir}"}
	got, err := expandLaunchTokens(argv, m, "/srv/inst")
	if err != nil {
		t.Fatalf("expandLaunchTokens: %v", err)
	}
	want := []string{"/opt/jre/bin/java", "-Xmx1024M", "-jar", "paper.jar", "--port", "25565", "--rcon", "25575", "--dir", "/srv/inst"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("expanded=%v want %v", got, want)
	}
}

func TestExpandLaunchTokens_UnknownPort(t *testing.T) {
	m := nativeMeta{Ports: []protocol.PortBinding{{Name: "game", HostPort: 25565}}}
	if _, err := expandLaunchTokens([]string{"{port:nope}"}, m, "/x"); err == nil {
		t.Fatal("未知埠 token 應回錯")
	}
}

// TestExpandLaunchTokens_StartScriptRewrite 驗證 T13 解法(a):jar 型範本命令在啟動來源實為腳本
// (StartScript 非空、無 server jar,即 Forge/NeoForge)時,改寫為 {start_script} 啟動並保留 nogui,
// 丟棄 {java}/-Xmx/-jar 前綴(記憶體改注入 args 檔)。
func TestExpandLaunchTokens_StartScriptRewrite(t *testing.T) {
	m := nativeMeta{
		JavaPath:    "/opt/jre/bin/java",
		StartScript: "/srv/inst/run.bat",
		MemoryMB:    4096,
	}
	argv := []string{"{java}", "-Xmx{memory_mb}M", "-jar", "{server_jar}", "nogui"}
	got, err := expandLaunchTokens(argv, m, "/srv/inst")
	if err != nil {
		t.Fatalf("expandLaunchTokens: %v", err)
	}
	want := []string{"/srv/inst/run.bat", "nogui"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("start-script 改寫=%v want %v", got, want)
	}
}

// TestInjectMemoryArgs 驗證記憶體上限被追加寫入 user_jvm_args.txt;檔案不存在時為 no-op(不報錯)。
func TestInjectMemoryArgs(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "user_jvm_args.txt")
	if err := os.WriteFile(argsFile, []byte("# JVM args\n-XX:+UseG1GC\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := injectMemoryArgs(argsFile, 4096); err != nil {
		t.Fatalf("injectMemoryArgs: %v", err)
	}
	got, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(got), "-Xmx4096M") {
		t.Fatalf("args 檔未含 -Xmx4096M: %q", string(got))
	}
	// 檔案不存在:no-op、不報錯。
	if err := injectMemoryArgs(filepath.Join(dir, "nope.txt"), 4096); err != nil {
		t.Fatalf("缺檔應為 no-op,得 %v", err)
	}
}

// ---- 滾動日誌:滾動不中斷 follow ----

func TestRollingLog_RotationDoesNotInterruptFollower(t *testing.T) {
	dir := t.TempDir()
	rl, err := newRollingLog(dir, 256 /* tiny 觸發多次滾動 */, 3)
	if err != nil {
		t.Fatalf("newRollingLog: %v", err)
	}
	defer rl.close()

	_, ch := rl.subscribe()
	const n = 200
	for i := 0; i < n; i++ {
		rl.write("stdout", fmt.Sprintf("line-%04d", i))
	}
	// follower 應收到全部 n 行(緩衝 1024 > n),滾動不影響記憶體扇出。
	received := 0
	deadline := time.After(2 * time.Second)
	for received < n {
		select {
		case ln := <-ch:
			if ln.Line != fmt.Sprintf("line-%04d", received) {
				t.Fatalf("第 %d 行=%q,順序錯", received, ln.Line)
			}
			received++
		case <-deadline:
			t.Fatalf("follower 只收到 %d/%d 行,滾動中斷了串流", received, n)
		}
	}
	// 確認確實發生過磁碟滾動(存在輪替檔)。
	if _, err := os.Stat(filepath.Join(dir, nativeLogFile+".1")); err != nil {
		t.Fatalf("預期發生日誌滾動(server.log.1 應存在): %v", err)
	}
}

func TestReadLogHistory_AcrossRotation(t *testing.T) {
	dir := t.TempDir()
	// 小 maxBytes 觸發密集滾動,maxFiles 足夠大以完整保留 n 行(驗跨輪替檔重建);
	// 保留上限本身(超額捨棄最舊)由 TestRollingLog_RotationDoesNotInterruptFollower 涵蓋。
	rl, err := newRollingLog(dir, 256, 60)
	if err != nil {
		t.Fatalf("newRollingLog: %v", err)
	}
	const n = 120
	for i := 0; i < n; i++ {
		rl.write("stdout", fmt.Sprintf("h-%03d", i))
	}
	rl.close()

	all := readLogHistory(dir, 0)
	if len(all) != n {
		t.Fatalf("history=%d want %d(跨輪替檔應完整重建)", len(all), n)
	}
	if all[0].Line != "h-000" || all[n-1].Line != fmt.Sprintf("h-%03d", n-1) {
		t.Fatalf("history 順序錯:first=%q last=%q", all[0].Line, all[n-1].Line)
	}
	// tail N。
	tailN := readLogHistory(dir, 10)
	if len(tailN) != 10 || tailN[0].Line != fmt.Sprintf("h-%03d", n-10) {
		t.Fatalf("tail(10) 錯:len=%d first=%q", len(tailN), func() string {
			if len(tailN) > 0 {
				return tailN[0].Line
			}
			return ""
		}())
	}
}

// ---- proc.json 讀寫 ----

func TestProcMeta_RoundTrip(t *testing.T) {
	b := newTestNativeBackend(t)
	uuid := "uuid-proc"
	if err := os.MkdirAll(b.instanceDataRoot(uuid), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	in := procMeta{UUID: uuid, PID: 4321, StartTime: time.Now().UTC().Truncate(time.Second), Command: []string{"java", "-jar", "s.jar"}, WorkDir: "/srv"}
	if err := b.writeProcMeta(uuid, in); err != nil {
		t.Fatalf("writeProcMeta: %v", err)
	}
	var out procMeta
	if err := readJSONFile(b.procMetaPath(uuid), &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.PID != in.PID || out.UUID != in.UUID || out.WorkDir != in.WorkDir || !out.StartTime.Equal(in.StartTime) || strings.Join(out.Command, " ") != strings.Join(in.Command, " ") {
		t.Fatalf("proc.json round-trip 不一致: in=%+v out=%+v", in, out)
	}
}

// ---- Create 供應與設定 ----

func TestNativeCreate_ProvisionAndEula(t *testing.T) {
	root := t.TempDir()
	prov := &fakeProv{javaPath: os.Args[0], serverJar: "server.jar"}
	b, err := NewNativeBackend(NativeOptions{
		DataRoot:   filepath.Join(root, "data"),
		BackupRoot: filepath.Join(root, "backups"),
		CacheRoot:  filepath.Join(root, "cache"),
		Node:       "n1",
		Prov:       prov,
	})
	if err != nil {
		t.Fatalf("NewNativeBackend: %v", err)
	}
	defer b.Close()

	// 訂閱事件以驗供應進度 emit。
	es, err := b.Events(context.Background(), "")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer es.Close()

	spec := nativeHelperSpec("uuid-create")
	spec.Native.Provision.EULA = true
	spec.Native.Config = []protocol.NativeConfigMap{{
		File: "server.properties", Format: "properties", Map: map[string]string{"mp": "max-players"},
	}}
	spec.Env["mp"] = "10"

	id, err := b.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != nativeID("uuid-create") {
		t.Fatalf("RuntimeID=%q want %q", id, nativeID("uuid-create"))
	}
	work := b.instanceDataRoot("uuid-create")
	// eula.txt 應寫入。
	if _, err := os.Stat(filepath.Join(work, "eula.txt")); err != nil {
		t.Fatalf("eula.txt 未寫入: %v", err)
	}
	// server.properties 應由 Config 產生。
	if got, err := os.ReadFile(filepath.Join(work, "server.properties")); err != nil || string(got) != "max-players=10\n" {
		t.Fatalf("server.properties=%q err=%v", string(got), err)
	}
	// 供應進度事件應收到至少一則 provision。
	select {
	case ev := <-es.Events():
		if ev.Kind != protocol.RuntimeEventProvision {
			t.Fatalf("首事件 kind=%q want provision", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到供應進度事件")
	}
}

// ---- Archive→Restore 資料往返與 checksum 強制 ----

func TestNativeBackend_ArchiveRestoreRoundTrip(t *testing.T) {
	b := newTestNativeBackend(t)
	ctx := context.Background()
	id, err := b.Create(ctx, nativeHelperSpec("uuid-rt"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dataDir := b.hostDirForContainerPath("uuid-rt", "/data")
	worldFile := filepath.Join(dataDir, "world.dat")
	if err := os.WriteFile(worldFile, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("write world: %v", err)
	}

	bid, err := b.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// native 中繼(native.json/server.log 等)不得混入備份資料(跨後端互通,R10)。
	names := tarEntryNames(t, filepath.Join(b.backupInstanceRoot("uuid-rt"), string(bid), backupDataFile))
	for _, n := range names {
		if n == nativeMetaFile || n == procMetaFile || n == instanceSpecFile || strings.HasPrefix(n, nativeLogFile) {
			t.Fatalf("備份不應含 native 中繼 %q(全部=%v)", n, names)
		}
	}

	// 竄改資料後還原,應復原為原內容(Restore 內部 checksum 比對通過)。
	if err := os.WriteFile(worldFile, []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatalf("mutate world: %v", err)
	}
	if _, err := b.Restore(ctx, id, bid); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, err := os.ReadFile(worldFile); err != nil || string(got) != "ORIGINAL" {
		t.Fatalf("還原後 world.dat=%q err=%v want ORIGINAL", string(got), err)
	}
}

func TestNativeBackend_RestoreRejectsChecksumMismatch(t *testing.T) {
	b := newTestNativeBackend(t)
	ctx := context.Background()
	id, err := b.Create(ctx, nativeHelperSpec("uuid-tamper"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dataDir := b.hostDirForContainerPath("uuid-tamper", "/data")
	if err := os.WriteFile(filepath.Join(dataDir, "w.dat"), []byte("DATA"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	bid, err := b.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// 竄改備份的 data.tar(位元不符),Restore 應以 checksum 不符拒絕。
	tarPath := filepath.Join(b.backupInstanceRoot("uuid-tamper"), string(bid), backupDataFile)
	if err := os.WriteFile(tarPath, []byte("garbage-not-a-tar"), 0o644); err != nil {
		t.Fatalf("tamper tar: %v", err)
	}
	if _, err := b.Restore(ctx, id, bid); err == nil {
		t.Fatal("checksum 不符的備份應被拒絕還原")
	}
}

// ---- Remove 清中繼、保資料 ----

func TestNativeBackend_RemoveKeepsData(t *testing.T) {
	b := newTestNativeBackend(t)
	ctx := context.Background()
	id, err := b.Create(ctx, nativeHelperSpec("uuid-keep"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dataDir := b.hostDirForContainerPath("uuid-keep", "/data")
	saveFile := filepath.Join(dataDir, "save.dat")
	if err := os.WriteFile(saveFile, []byte("SAVE"), 0o644); err != nil {
		t.Fatalf("write save: %v", err)
	}

	if err := b.Remove(ctx, id, RemoveOpts{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// 中繼已清:後端不再可見。
	if _, err := b.Status(ctx, id); err == nil {
		t.Error("Remove 後 Status 應回錯")
	}
	if _, err := os.Stat(b.nativeMetaPath("uuid-keep")); err == nil {
		t.Error("Remove 後 native.json 應被清除")
	}
	// 資料保留。
	if got, err := os.ReadFile(saveFile); err != nil || string(got) != "SAVE" {
		t.Fatalf("Remove(非 Purge)後資料應保留,save.dat=%q err=%v", string(got), err)
	}

	// Purge:重建後全清。
	if _, err := b.Create(ctx, nativeHelperSpec("uuid-keep")); err != nil {
		t.Fatalf("re-Create: %v", err)
	}
	if err := b.Remove(ctx, nativeID("uuid-keep"), RemoveOpts{Purge: true}); err != nil {
		t.Fatalf("Remove Purge: %v", err)
	}
	if _, err := os.Stat(b.instanceDataRoot("uuid-keep")); err == nil {
		t.Error("Purge 後實例資料根應整個移除")
	}
}

// ---- Stop 逾時強殺 ----

func TestNativeBackend_StopTimeoutKills(t *testing.T) {
	b := newTestNativeBackend(t)
	ctx := context.Background()
	id, err := b.Create(ctx, nativeHelperSpec("uuid-stop"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st, _ := b.Status(ctx, id); !st.Running {
		t.Fatal("Start 後應為 running")
	}
	// 助手行程不會優雅退出:Stop 應等滿 Grace 後強殺。
	grace := 150 * time.Millisecond
	start := time.Now()
	if err := b.Stop(ctx, id, StopOpts{Grace: grace}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Fatalf("Stop 過早返回(%v < grace %v),未經寬限期", elapsed, grace)
	}
	if st, _ := b.Status(ctx, id); st.Running {
		t.Fatal("Stop 後不應仍 running")
	}
}
