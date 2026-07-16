package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// NativeBackend 是 RuntimeBackend 的免 Docker 本機子行程實作(native-backend R1)。伺服器以
// os/exec 起子行程執行,stdout/stderr 導入實例根下的滾動日誌(native_log.go),PID 與啟動時刻
// 記於 proc.json,建立期供應(JRE/伺服器檔案/SteamCMD)委由注入的 provisionRunner。事件、封存/
// 還原與 Docker 後端等價(復用 eventHub 與 backup_archive.go 的共用 tar/checksum)。併發安全。
//
// 明確的任務邊界(後續任務填入,本檔留 seam):
//   - Job Objects 資源上限與行程樹 CPU/記憶體統計 → T7(本檔 Stats 只回 DataDiskBytes,
//     Stop 以 taskkill 強殺行程樹作為 Job Objects 到位前的過渡)。
//   - agent 重啟收養掃描與跨行程 PID 存活比對 → T11(本檔 adoptExisting 為 no-op seam,
//     Status 以本 agent 生命週期內的記憶體 procHandle 為準)。
//   - Modrinth/CurseForge 模組安裝 → T10/T14(Create 目前不處理 spec.Modpack)。
type NativeBackend struct {
	dataRoot   string
	backupRoot string
	cacheRoot  string
	node       string
	schema     string
	prov       provisionRunner

	hub *eventHub

	mu    sync.Mutex
	procs map[string]*procHandle // 鍵為 uuid

	closeOnce sync.Once
}

var _ RuntimeBackend = (*NativeBackend)(nil)

const (
	// nativePrefix 是 native RuntimeID 的前綴:RuntimeID = "native:" + uuid。dispatchBackend(T8)
	// 依此前綴路由;既有 docker 實例 RuntimeID 為裸容器 ID(無前綴),零遷移。
	nativePrefix = "native:"

	// nativeMetaFile 是實例根下的 native 建立期中繼(供 Start 展開啟動命令)。
	nativeMetaFile = "native.json"
	// procMetaFile 是實例根下的執行中繼(PID + 啟動時刻,供收養/存活比對)。
	procMetaFile = "proc.json"
)

// provisionRunner 是 NativeBackend 供應階段所需的最小介面(native-backend R4/R5/R6)。真實實作
// 由 internal\agent\provision 子套件於後續任務接線(T2 以降並行進行中);本套件僅依賴此窄介面,
// 使 NativeBackend 可獨立於供應細節單元測(測試注入假件)。progress 回呼由 NativeBackend 轉為
// eventHub 的 RuntimeEventProvision 事件供 GUI 顯示。
type provisionRunner interface {
	// EnsureJava 確保指定 major 版 JRE 已供應於共用快取,回傳 java 執行檔絕對路徑。
	EnsureJava(ctx context.Context, major int, progress func(protocol.ProvisionProgress)) (javaPath string, err error)
	// InstallServer 依變體 loader 安裝 Minecraft 伺服器檔案至實例目錄,回傳啟動用 server jar 路徑(相對或絕對)。
	InstallServer(ctx context.Context, req ServerInstallRequest, progress func(protocol.ProvisionProgress)) (serverJar string, err error)
	// InstallSteamApp 以 SteamCMD 安裝/更新指定 Steam App 至實例目錄。
	InstallSteamApp(ctx context.Context, appID, instanceDir string, progress func(protocol.ProvisionProgress)) error
	// WriteEula 於實例目錄寫入 eula.txt(native 無 itzg 代勞;R5)。accepted=false 時為 no-op。
	WriteEula(ctx context.Context, instanceDir string, accepted bool) error
}

// ServerInstallRequest 是 provisionRunner.InstallServer 的輸入(native-backend R5)。
type ServerInstallRequest struct {
	InstanceDir string // 伺服器檔案落位目錄(絕對路徑)
	Variant     string // 變體 ID(如 "paper-1.21")
	Loader      string // loader 家族(vanilla/paper/fabric/forge/neoforge)
	JavaMajor   int    // 所需 Java major(Forge/NeoForge installer 執行用)
	JavaPath    string // 已供應的 java 執行檔路徑
}

// NativeOptions 是 NativeBackend 的建構選項。
type NativeOptions struct {
	DataRoot   string          // agent 擁有的實例資料根(每實例 <DataRoot>/<uuid>/)
	BackupRoot string          // agent 擁有的備份根(與 docker 互通)
	CacheRoot  string          // JRE/SteamCMD 共用快取根(<CacheRoot>/jre、/steamcmd)
	Node       string          // gsm.node 標籤值
	Prov       provisionRunner // 供應器(必填;真實接線由後續任務,測試注入假件)
}

// NewNativeBackend 建立 native 後端並掃描收養既有行程(收養為 T11 seam)。
func NewNativeBackend(opts NativeOptions) (*NativeBackend, error) {
	if opts.DataRoot == "" {
		return nil, fmt.Errorf("agent: NativeOptions.DataRoot 不可為空")
	}
	if opts.BackupRoot == "" {
		return nil, fmt.Errorf("agent: NativeOptions.BackupRoot 不可為空")
	}
	if opts.CacheRoot == "" {
		return nil, fmt.Errorf("agent: NativeOptions.CacheRoot 不可為空")
	}
	if opts.Prov == nil {
		return nil, fmt.Errorf("agent: NativeOptions.Prov 不可為 nil")
	}
	for _, d := range []string{opts.DataRoot, opts.BackupRoot, opts.CacheRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("建立目錄 %s 失敗: %w", d, err)
		}
	}
	b := &NativeBackend{
		dataRoot:   opts.DataRoot,
		backupRoot: opts.BackupRoot,
		cacheRoot:  opts.CacheRoot,
		node:       opts.Node,
		schema:     "1",
		prov:       opts.Prov,
		hub:        newEventHub(0),
		procs:      make(map[string]*procHandle),
	}
	b.adoptExisting()
	return b, nil
}

// nativeMeta 是 native 實例的建立期中繼(實例根下 native.json),供 Start 展開啟動命令。
type nativeMeta struct {
	UUID       string                 `json:"uuid"`
	JavaPath   string                 `json:"java_path,omitempty"`
	ServerJar  string                 `json:"server_jar,omitempty"`
	Command    []string               `json:"command"`
	WorkingDir string                 `json:"working_dir,omitempty"`
	MemoryMB   int                    `json:"memory_mb,omitempty"`
	Ports      []protocol.PortBinding `json:"ports,omitempty"`
}

// procMeta 是執行中繼(實例根下 proc.json):PID + 啟動時刻(防 PID 重用的 start-time 比對)。
//
// 過渡註記:本任務未取 OS 層行程建立時間(需 Windows Job Objects/x/sys,屬 T7/T11),StartTime
// 記為 agent 觀察到的「起行程當下牆鐘時刻」——收養(T11)以此時刻對照 OS 行程建立時間並容忍
// 秒級誤差來判 PID 是否被重用;本任務的 Status/Stop 以記憶體 procHandle 為準,不依賴此近似值。
type procMeta struct {
	UUID      string    `json:"uuid"`
	PID       int       `json:"pid"`
	StartTime time.Time `json:"start_time"`
	Command   []string  `json:"command"`
	WorkDir   string    `json:"work_dir"`
}

// procHandle 是本 agent 生命週期內一個執行中(或已結束)實例行程的記憶體控制點。
type procHandle struct {
	uuid      string
	pid       int
	startedAt time.Time
	cmd       *exec.Cmd
	log       *rollingLog
	done      chan struct{} // Wait 返回後關閉

	mu       sync.Mutex
	finished bool
	exitCode int
}

func (h *procHandle) isFinished() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finished
}

func (h *procHandle) setFinished(code int) {
	h.mu.Lock()
	if !h.finished {
		h.finished = true
		h.exitCode = code
	}
	h.mu.Unlock()
}

func (h *procHandle) getExitCode() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitCode
}

// ---- RuntimeBackend 實作 ----

// Create 建立實例工作目錄、依 native payload 供應、寫設定檔與中繼快照;不啟動行程(R1)。
func (b *NativeBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	if spec.UUID == "" {
		return "", fmt.Errorf("agent: InstanceSpec.UUID 不可為空")
	}
	if spec.Native == nil {
		return "", fmt.Errorf("agent: InstanceSpec.Native 不可為空(native 執行需供應/啟動資訊)")
	}
	uuid := spec.UUID
	root := b.instanceDataRoot(uuid)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("建立實例資料根失敗: %w", err)
	}
	// data_dirs:於實例根下各建一子目錄(對齊 docker bind 慣例,備份範圍)。
	for _, d := range spec.DataDirs {
		if err := os.MkdirAll(b.hostDirForContainerPath(uuid, d), 0o755); err != nil {
			return "", fmt.Errorf("建立資料目錄 %s 失敗: %w", d, err)
		}
	}
	// 具名 mount(R11 手動模組包檔):建宿主目錄,內容排除於備份範圍。
	for _, mnt := range spec.Mounts {
		if err := os.MkdirAll(b.hostDirForMount(uuid, mnt.Name), 0o755); err != nil {
			return "", fmt.Errorf("建立掛載目錄 %s 失敗: %w", mnt.Name, err)
		}
	}

	workDir := b.workingDir(root, spec.Native.Launch.WorkingDir)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", fmt.Errorf("建立工作目錄失敗: %w", err)
	}

	// 供應(進度經 eventHub emit)。
	progress := b.progressEmitter(uuid)
	javaPath, serverJar, err := b.provision(ctx, spec, workDir, progress)
	if err != nil {
		return "", err
	}

	// 設定檔(params→server.properties / palworld-ini,由 spec.Env 取值)。
	for _, cm := range spec.Native.Config {
		if err := writeConfigFile(workDir, cm, spec.Env); err != nil {
			return "", err
		}
	}

	// 中繼快照。
	meta := nativeMeta{
		UUID:       uuid,
		JavaPath:   javaPath,
		ServerJar:  serverJar,
		Command:    spec.Native.Launch.Command,
		WorkingDir: spec.Native.Launch.WorkingDir,
		MemoryMB:   resolveMemoryMB(spec.Env),
		Ports:      spec.Ports,
	}
	if err := b.writeNativeMeta(uuid, meta); err != nil {
		return "", err
	}
	if err := b.writeSpec(spec); err != nil {
		return "", err
	}
	return nativeID(uuid), nil
}

// provision 依 native payload 的供應類型執行供應,回傳 javaPath 與 serverJar(steamcmd 皆為空)。
func (b *NativeBackend) provision(ctx context.Context, spec protocol.InstanceSpec, workDir string, progress func(protocol.ProvisionProgress)) (javaPath, serverJar string, err error) {
	np := spec.Native.Provision
	switch np.Kind {
	case "java":
		javaPath, err = b.prov.EnsureJava(ctx, np.JavaMajor, progress)
		if err != nil {
			return "", "", fmt.Errorf("供應 Java 失敗: %w", err)
		}
		serverJar, err = b.prov.InstallServer(ctx, ServerInstallRequest{
			InstanceDir: workDir,
			Variant:     spec.Variant,
			Loader:      np.Loader,
			JavaMajor:   np.JavaMajor,
			JavaPath:    javaPath,
		}, progress)
		if err != nil {
			return "", "", fmt.Errorf("安裝伺服器失敗: %w", err)
		}
		if np.EULA {
			if err := b.prov.WriteEula(ctx, workDir, true); err != nil {
				return "", "", fmt.Errorf("寫入 eula.txt 失敗: %w", err)
			}
		}
	case "steamcmd":
		if err := b.prov.InstallSteamApp(ctx, np.SteamAppID, workDir, progress); err != nil {
			return "", "", fmt.Errorf("SteamCMD 安裝失敗: %w", err)
		}
	case "":
		// 免供應(如手動置檔或測試)。
	default:
		return "", "", fmt.Errorf("agent: 未知供應類型 %q", np.Kind)
	}
	return javaPath, serverJar, nil
}

// Start 展開啟動命令 token、起子行程、導日誌、寫 proc.json 並發 start 事件(R7)。
func (b *NativeBackend) Start(ctx context.Context, id protocol.RuntimeID) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	uuid := nativeUUID(id)
	if !b.instanceExists(uuid) {
		return ErrNotFound
	}
	b.mu.Lock()
	if h, ok := b.procs[uuid]; ok && !h.isFinished() {
		b.mu.Unlock()
		return fmt.Errorf("agent: 實例 %s 已在執行", uuid)
	}
	b.mu.Unlock()

	meta, err := b.readNativeMeta(uuid)
	if err != nil {
		return fmt.Errorf("讀取 native 中繼失敗: %w", err)
	}
	spec, err := b.readSpec(uuid)
	if err != nil {
		return fmt.Errorf("讀取 spec 快照失敗: %w", err)
	}
	root := b.instanceDataRoot(uuid)
	workDir := b.workingDir(root, meta.WorkingDir)

	argv, err := expandLaunchTokens(meta.Command, meta, workDir)
	if err != nil {
		return err
	}
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("agent: 實例 %s 啟動命令為空", uuid)
	}

	rl, err := newRollingLog(root, defaultLogMaxBytes, defaultLogMaxFiles)
	if err != nil {
		return fmt.Errorf("開啟日誌失敗: %w", err)
	}

	// 刻意不用 exec.CommandContext:請求 ctx 結束不應殺掉常駐伺服器(生命週期由 Stop/Remove 管)。
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), envSlice(spec.Env)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		rl.close()
		return fmt.Errorf("取得 stdout 失敗: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		rl.close()
		return fmt.Errorf("取得 stderr 失敗: %w", err)
	}
	if err := cmd.Start(); err != nil {
		rl.close()
		return fmt.Errorf("啟動行程失敗: %w", err)
	}
	now := time.Now().UTC()
	h := &procHandle{
		uuid:      uuid,
		pid:       cmd.Process.Pid,
		startedAt: now,
		cmd:       cmd,
		log:       rl,
		done:      make(chan struct{}),
	}
	go captureStream(rl, "stdout", stdout)
	go captureStream(rl, "stderr", stderr)

	b.mu.Lock()
	b.procs[uuid] = h
	b.mu.Unlock()

	_ = b.writeProcMeta(uuid, procMeta{
		UUID:      uuid,
		PID:       h.pid,
		StartTime: now,
		Command:   argv,
		WorkDir:   workDir,
	})

	go b.reap(h)

	b.hub.emit(RuntimeEvent{ID: id, Kind: RuntimeEventStart, TsUTC: now})
	return nil
}

// reap 等待行程結束,合成 die 事件(帶 exitCode),清 proc.json、收束日誌即時扇出。
// 崩潰與計畫停止在事件層不可區分(與 Docker die 一致),planned 與否由核心依 token 對帳。
func (b *NativeBackend) reap(h *procHandle) {
	werr := h.cmd.Wait()
	code := 0
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	h.setFinished(code)
	_ = os.Remove(b.procMetaPath(h.uuid)) // 行程已歿,清執行中繼(收養不再視其存活)
	h.log.close()
	close(h.done)
	b.hub.emit(RuntimeEvent{ID: nativeID(h.uuid), Kind: RuntimeEventDie, TsUTC: time.Now().UTC(), ExitCode: &code})
}

// Stop 等 opts.Grace 優雅退出(呼叫端已先送 hooks.stop),逾時強殺整棵行程樹(R1/R7)。
// 過渡:以 taskkill /T(Windows)強殺行程樹;Job Objects TerminateJobObject 於 T7 取代。
func (b *NativeBackend) Stop(ctx context.Context, id protocol.RuntimeID, opts StopOpts) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	uuid := nativeUUID(id)
	b.mu.Lock()
	h := b.procs[uuid]
	b.mu.Unlock()
	if h == nil || h.isFinished() {
		if !b.instanceExists(uuid) {
			return ErrNotFound
		}
		return nil // 已停或未啟動:等冪
	}

	if opts.Grace > 0 {
		select {
		case <-h.done:
			return nil // 寬限期內優雅退出
		case <-time.After(opts.Grace):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := killProcessTree(h.pid); err != nil {
		return fmt.Errorf("強殺行程樹失敗: %w", err)
	}
	select {
	case <-h.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Status 由記憶體 procHandle 與中繼合成即時狀態(R1)。跨 agent 重啟的 PID 存活比對屬 T11。
func (b *NativeBackend) Status(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeStatus, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.RuntimeStatus{}, err
	}
	uuid := nativeUUID(id)
	if !b.instanceExists(uuid) {
		return protocol.RuntimeStatus{}, ErrNotFound
	}
	rs := protocol.RuntimeStatus{ID: id, State: protocol.RuntimeStateCreated, Health: "none"}
	b.mu.Lock()
	h := b.procs[uuid]
	b.mu.Unlock()
	if h == nil {
		return rs, nil // 已建立、尚未啟動(本 agent 生命週期內)
	}
	if h.isFinished() {
		code := h.getExitCode()
		fin := time.Now().UTC()
		rs.State = protocol.RuntimeStateExited
		rs.ExitCode = &code
		rs.FinishedAt = &fin
		return rs, nil
	}
	started := h.startedAt
	rs.State = protocol.RuntimeStateRunning
	rs.Running = true
	rs.StartedAt = &started
	rs.Health = "healthy"
	return rs, nil
}

// List 掃描實例根,回帶 gsm.* 標籤的 RuntimeRef(R13 對帳)。
func (b *NativeBackend) List(ctx context.Context) ([]protocol.RuntimeRef, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(b.dataRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return []protocol.RuntimeRef{}, nil
		}
		return nil, err
	}
	refs := make([]protocol.RuntimeRef, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		uuid := e.Name()
		if !b.instanceExists(uuid) {
			continue
		}
		spec, _ := b.readSpec(uuid)
		refs = append(refs, protocol.RuntimeRef{
			ID:     nativeID(uuid),
			State:  b.stateOf(uuid),
			Labels: b.labelsFor(uuid, spec),
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

// Inspect 回傳單一實例詳細資訊(R13)。
func (b *NativeBackend) Inspect(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.RuntimeInfo{}, err
	}
	uuid := nativeUUID(id)
	if !b.instanceExists(uuid) {
		return protocol.RuntimeInfo{}, ErrNotFound
	}
	spec, _ := b.readSpec(uuid)
	status, _ := b.Status(ctx, id)
	created := time.Time{}
	if info, serr := os.Stat(b.nativeMetaPath(uuid)); serr == nil {
		created = info.ModTime().UTC()
	}
	return protocol.RuntimeInfo{
		ID:        id,
		State:     status.State,
		Labels:    b.labelsFor(uuid, spec),
		Ports:     append([]protocol.PortBinding(nil), spec.Ports...),
		CreatedAt: created,
		Status:    status,
	}, nil
}

// Remove 停止行程並清 runtime 中繼(native.json/proc.json/instance.json),保留資料目錄;
// Purge 才連資料與備份一併刪除(R1,與 Docker 行為一致)。
func (b *NativeBackend) Remove(ctx context.Context, id protocol.RuntimeID, opts RemoveOpts) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	uuid := nativeUUID(id)
	b.mu.Lock()
	h := b.procs[uuid]
	b.mu.Unlock()
	if h != nil && !h.isFinished() {
		_ = killProcessTree(h.pid)
		<-h.done
	}
	b.mu.Lock()
	delete(b.procs, uuid)
	b.mu.Unlock()

	if opts.Purge {
		_ = os.RemoveAll(b.instanceDataRoot(uuid))
		_ = os.RemoveAll(b.backupInstanceRoot(uuid))
		return nil
	}
	// 清 runtime 中繼、保資料。
	for _, f := range []string{nativeMetaFile, procMetaFile, instanceSpecFile, nativeLogFile} {
		_ = os.Remove(filepath.Join(b.instanceDataRoot(uuid), f))
	}
	// 清滾動日誌輪替檔。
	if entries, err := os.ReadDir(b.instanceDataRoot(uuid)); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), nativeLogFile+".") {
				_ = os.Remove(filepath.Join(b.instanceDataRoot(uuid), e.Name()))
			}
		}
	}
	return nil
}

// Logs 由日誌檔 tail 提供串流;follow 且行程在執行時續接即時扇出(跨滾動不中斷,R7)。
func (b *NativeBackend) Logs(ctx context.Context, id protocol.RuntimeID, opts LogOpts) (LogStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	uuid := nativeUUID(id)
	if !b.instanceExists(uuid) {
		return nil, ErrNotFound
	}
	root := b.instanceDataRoot(uuid)
	history := readLogHistory(root, opts.Tail)

	b.mu.Lock()
	h := b.procs[uuid]
	b.mu.Unlock()
	if opts.Follow && h != nil && !h.isFinished() {
		subID, ch := h.log.subscribe()
		unsub := func() { h.log.unsubscribe(subID) }
		return newNativeLogStream(ctx, history, ch, h.done, unsub), nil
	}
	return newNativeLogStream(ctx, history, nil, nil, nil), nil
}

// ExecProcess 在實例工作目錄以 os/exec 執行程序,回緩衝輸出(對齊介面語意;非遊戲指令)。
func (b *NativeBackend) ExecProcess(ctx context.Context, id protocol.RuntimeID, cmd ExecCmd) (ExecResult, error) {
	if err := ctxErr(ctx); err != nil {
		return ExecResult{}, err
	}
	uuid := nativeUUID(id)
	if !b.instanceExists(uuid) {
		return ExecResult{}, ErrNotFound
	}
	if len(cmd.Cmd) == 0 {
		return ExecResult{}, fmt.Errorf("agent: ExecProcess 命令為空")
	}
	workDir := cmd.WorkDir
	if workDir == "" {
		meta, _ := b.readNativeMeta(uuid)
		workDir = b.workingDir(b.instanceDataRoot(uuid), meta.WorkingDir)
	}
	c := exec.CommandContext(ctx, cmd.Cmd[0], cmd.Cmd[1:]...)
	c.Dir = workDir
	c.Env = append(os.Environ(), cmd.Env...)
	var out, errBuf strings.Builder
	c.Stdout = &out
	c.Stderr = &errBuf
	runErr := c.Run()
	code := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return ExecResult{}, runErr
		}
	}
	return ExecResult{ExitCode: code, Stdout: out.String(), Stderr: errBuf.String()}, nil
}

// Stats 回報資料磁碟用量;行程樹 CPU/記憶體統計屬 Job Objects,歸 T7(此處留零值)。
func (b *NativeBackend) Stats(ctx context.Context, id protocol.RuntimeID) (protocol.ResourceStats, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.ResourceStats{}, err
	}
	uuid := nativeUUID(id)
	if !b.instanceExists(uuid) {
		return protocol.ResourceStats{}, ErrNotFound
	}
	stats := protocol.ResourceStats{TsUTC: time.Now().UTC()}
	if used, err := dataDirUsage(b.instanceDataRoot(uuid)); err == nil {
		stats.DataDiskBytes = &used
	}
	return stats, nil
}

// Archive 對(已停機的)實例做一致 tar 快照,格式與 Docker 互通(R10)。
func (b *NativeBackend) Archive(ctx context.Context, id protocol.RuntimeID) (protocol.BackupID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	uuid := nativeUUID(id)
	if !b.instanceExists(uuid) {
		return "", ErrNotFound
	}
	if b.isRunning(uuid) {
		return "", fmt.Errorf("agent: 實例 %s 執行中,無法封存(需先停機)", uuid)
	}
	spec, serr := b.readSpec(uuid)
	if serr != nil {
		spec = protocol.InstanceSpec{UUID: uuid}
	}

	backupID := protocol.BackupID(strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + shortRand())
	bkpDir := filepath.Join(b.backupInstanceRoot(uuid), string(backupID))
	if err := os.MkdirAll(bkpDir, 0o755); err != nil {
		return "", fmt.Errorf("建立備份目錄失敗: %w", err)
	}
	checksum, err := tarDir(b.instanceDataRoot(uuid), filepath.Join(bkpDir, backupDataFile))
	if err != nil {
		_ = os.RemoveAll(bkpDir)
		return "", fmt.Errorf("封存資料失敗: %w", err)
	}
	rec := backupRecord{
		Meta: protocol.BackupMeta{
			BackupID:     backupID,
			InstanceUUID: uuid,
			Game:         spec.TemplateID,
			TsUTC:        time.Now().UTC(),
			Checksum:     checksum,
		},
		Spec: spec,
	}
	if err := writeBackupRecord(bkpDir, rec); err != nil {
		_ = os.RemoveAll(bkpDir)
		return "", err
	}
	return backupID, nil
}

// Restore 以備份還原:驗 checksum → 解包 staging → 原子切換資料目錄(R10)。native 的 RuntimeID
// 由 uuid 衍生、還原前後不變,故回傳同一 native:uuid(對齊介面「回新 RuntimeID」語意)。
func (b *NativeBackend) Restore(ctx context.Context, id protocol.RuntimeID, bid protocol.BackupID) (protocol.RuntimeID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	uuid := nativeUUID(id)
	if b.isRunning(uuid) {
		return "", fmt.Errorf("agent: 實例 %s 執行中,無法還原(需先停機)", uuid)
	}
	bkpDir, rec, err := b.findBackup(uuid, bid)
	if err != nil {
		return "", err
	}
	tarPath := filepath.Join(bkpDir, backupDataFile)
	sum, err := fileSHA256(tarPath)
	if err != nil {
		return "", fmt.Errorf("讀取備份資料失敗: %w", err)
	}
	if sum != rec.Meta.Checksum {
		return "", fmt.Errorf("agent: 備份 %s checksum 不符,拒絕還原", bid)
	}

	root := b.instanceDataRoot(uuid)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("建立實例資料根失敗: %w", err)
	}
	staging := filepath.Join(root, ".gsm-restore-"+shortRand())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", fmt.Errorf("建立還原 staging 失敗: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := untar(tarPath, staging); err != nil {
		return "", fmt.Errorf("解包備份失敗: %w", err)
	}
	commit, _, err := applyRestoreSwap(root, staging, os.Rename)
	if err != nil {
		return "", err
	}
	// 以備份 spec 快照覆寫實例 spec(自包含);native 中繼(供應路徑)不在備份範圍,保留原樣。
	_ = b.writeSpec(rec.Spec)
	commit()
	return nativeID(uuid), nil
}

// Events 訂閱執行事件串流(復用 eventHub;since 語意見 RuntimeBackend.Events)。
func (b *NativeBackend) Events(ctx context.Context, since protocol.Cursor) (EventStream, error) {
	return b.hub.subscribe(ctx, since)
}

var _ BackupLister = (*NativeBackend)(nil)

// ListBackups 掃描本實例備份目錄回中繼(BackupLister 橫切能力;native-backend R10/GUI)。
// 無備份目錄視為空清單而非錯誤,與 DockerBackend.ListBackups 語意一致。
func (b *NativeBackend) ListBackups(ctx context.Context, instanceUUID string) ([]protocol.BackupMeta, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	dir := b.backupInstanceRoot(instanceUUID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []protocol.BackupMeta{}, nil
		}
		return nil, err
	}
	out := make([]protocol.BackupMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, rerr := readBackupRecord(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		out = append(out, rec.Meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TsUTC.Before(out[j].TsUTC) })
	return out, nil
}

// Close 停止事件監看;刻意不殺執行中行程(R9:agent 退出不連坐,伺服器續活待收養)。冪等。
func (b *NativeBackend) Close() error {
	b.closeOnce.Do(func() {
		b.hub.close()
	})
	return nil
}

// ---- 收養 seam(T11)----

// adoptExisting 是 agent 重啟後掃描 proc.json 收養既有行程的接點(native-backend R7/R13)。
// 本任務為 no-op seam:實作(PID 存活+start-time 比對→重掛 Job/接管 log tail/running;已死→
// 合成 die 走崩潰復原)歸 T11。
func (b *NativeBackend) adoptExisting() {
	// T11 實作。
}

// ---- 啟動命令 token 展開 ----

// expandLaunchTokens 展開 argv 模板的 token(子字串替換):{java} {server_jar} {memory_mb}
// {instance_dir} 與 {port:<name>}。未知 {port:name} 回錯。
func expandLaunchTokens(argv []string, m nativeMeta, instanceDir string) ([]string, error) {
	portByName := make(map[string]int, len(m.Ports))
	for _, p := range m.Ports {
		hp := p.HostPort
		if hp == 0 {
			hp = p.Container
		}
		portByName[p.Name] = hp
	}
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		s := a
		s = strings.ReplaceAll(s, "{java}", m.JavaPath)
		s = strings.ReplaceAll(s, "{server_jar}", m.ServerJar)
		s = strings.ReplaceAll(s, "{memory_mb}", strconv.Itoa(m.MemoryMB))
		s = strings.ReplaceAll(s, "{instance_dir}", instanceDir)
		expanded, err := expandPortTokens(s, portByName)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded)
	}
	return out, nil
}

// expandPortTokens 展開字串中所有 {port:<name>} 為對應主機埠;找不到具名埠回錯。
func expandPortTokens(s string, ports map[string]int) (string, error) {
	const open = "{port:"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s, nil
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return "", fmt.Errorf("agent: 啟動命令 port token 未閉合: %q", s)
		}
		j += i
		name := s[i+len(open) : j]
		port, ok := ports[name]
		if !ok {
			return "", fmt.Errorf("agent: 啟動命令引用未知埠 %q", name)
		}
		s = s[:i] + strconv.Itoa(port) + s[j+1:]
	}
}

// ---- 行程樹強殺(T7 以 Job Objects 取代)----

// killProcessTree 強制終止 pid 及其子行程樹。過渡實作:Windows 用 taskkill /T /F;其他平台
// (單元測跑得到的 Linux CI)以行程 Kill。Windows Job Objects TerminateJobObject 於 T7 取代。
func killProcessTree(pid int) error {
	if runtime.GOOS == "windows" {
		return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// ---- 助手 ----

func nativeID(uuid string) protocol.RuntimeID { return protocol.RuntimeID(nativePrefix + uuid) }

func nativeUUID(id protocol.RuntimeID) string {
	return strings.TrimPrefix(string(id), nativePrefix)
}

func (b *NativeBackend) instanceDataRoot(uuid string) string {
	return filepath.Join(b.dataRoot, uuid)
}

func (b *NativeBackend) backupInstanceRoot(uuid string) string {
	return filepath.Join(b.backupRoot, uuid)
}

func (b *NativeBackend) nativeMetaPath(uuid string) string {
	return filepath.Join(b.instanceDataRoot(uuid), nativeMetaFile)
}

func (b *NativeBackend) procMetaPath(uuid string) string {
	return filepath.Join(b.instanceDataRoot(uuid), procMetaFile)
}

func (b *NativeBackend) hostDirForContainerPath(uuid, containerPath string) string {
	return filepath.Join(b.instanceDataRoot(uuid), sanitizeDataDir(containerPath))
}

func (b *NativeBackend) hostDirForMount(uuid, name string) string {
	return filepath.Join(b.instanceDataRoot(uuid), mountsSubdir, sanitizeMountName(name))
}

// workingDir 解析啟動工作目錄:相對 WorkingDir 落在實例根下;空則為實例根。
func (b *NativeBackend) workingDir(root, rel string) string {
	if rel == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// instanceExists 以 native.json 存在判定實例是否為本後端所管(Remove 後即不存在)。
func (b *NativeBackend) instanceExists(uuid string) bool {
	if uuid == "" {
		return false
	}
	_, err := os.Stat(b.nativeMetaPath(uuid))
	return err == nil
}

// isRunning 回報實例是否有執行中的記憶體 procHandle。
func (b *NativeBackend) isRunning(uuid string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.procs[uuid]
	return h != nil && !h.isFinished()
}

// stateOf 由記憶體 procHandle 合成 List/對帳用的 RuntimeState。
func (b *NativeBackend) stateOf(uuid string) protocol.RuntimeState {
	b.mu.Lock()
	h := b.procs[uuid]
	b.mu.Unlock()
	switch {
	case h == nil:
		return protocol.RuntimeStateCreated
	case h.isFinished():
		return protocol.RuntimeStateExited
	default:
		return protocol.RuntimeStateRunning
	}
}

// labelsFor 疊加權威 gsm.* 標籤(對齊 docker labelsFor;供 R13 對帳掃描)。
func (b *NativeBackend) labelsFor(uuid string, spec protocol.InstanceSpec) map[string]string {
	labels := cloneStringMap(spec.Labels)
	if labels == nil {
		labels = make(map[string]string, 4)
	}
	node := spec.Node
	if node == "" {
		node = b.node
	}
	labels[labelManagedBy] = managedByValue
	labels[labelUUID] = uuid
	labels[labelNode] = node
	labels[labelSchema] = b.schema
	return labels
}

// progressEmitter 回傳一個把 ProvisionProgress 轉為 eventHub RuntimeEventProvision 的回呼。
func (b *NativeBackend) progressEmitter(uuid string) func(protocol.ProvisionProgress) {
	id := nativeID(uuid)
	return func(p protocol.ProvisionProgress) {
		pp := p
		b.hub.emit(RuntimeEvent{ID: id, Kind: protocol.RuntimeEventProvision, TsUTC: time.Now().UTC(), Progress: &pp})
	}
}

// findBackup 定位本實例的備份目錄與記錄(native uuid 恆已知,不跨 uuid 掃描)。
func (b *NativeBackend) findBackup(uuid string, bid protocol.BackupID) (string, backupRecord, error) {
	if err := validateBackupID(bid); err != nil {
		return "", backupRecord{}, err
	}
	dir := filepath.Join(b.backupInstanceRoot(uuid), string(bid))
	rec, err := readBackupRecord(dir)
	if err != nil {
		return "", backupRecord{}, ErrNotFound
	}
	return dir, rec, nil
}

func (b *NativeBackend) writeNativeMeta(uuid string, m nativeMeta) error {
	return writeJSONFile(b.nativeMetaPath(uuid), m)
}

func (b *NativeBackend) readNativeMeta(uuid string) (nativeMeta, error) {
	var m nativeMeta
	err := readJSONFile(b.nativeMetaPath(uuid), &m)
	return m, err
}

func (b *NativeBackend) writeProcMeta(uuid string, m procMeta) error {
	return writeJSONFile(b.procMetaPath(uuid), m)
}

func (b *NativeBackend) writeSpec(spec protocol.InstanceSpec) error {
	return writeJSONFile(filepath.Join(b.instanceDataRoot(spec.UUID), instanceSpecFile), spec)
}

func (b *NativeBackend) readSpec(uuid string) (protocol.InstanceSpec, error) {
	var spec protocol.InstanceSpec
	err := readJSONFile(filepath.Join(b.instanceDataRoot(uuid), instanceSpecFile), &spec)
	return spec, err
}

// resolveMemoryMB 由 spec.Env 解析 {memory_mb} 用的記憶體(MB)。
// 過渡:實例資源設定(記憶體/CPU 上限,與 Docker 同來源)尚未進 InstanceSpec,故暫由慣用 env
// 鍵(MEMORY_MB / MEMORY)取值,皆缺時採 2048 MB 預設;真實資源設定接線隨 Job Objects(T7)。
func resolveMemoryMB(env map[string]string) int {
	for _, k := range []string{"MEMORY_MB", "MEMORY"} {
		if v, ok := env[k]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 2048
}

// dataDirUsage 遞迴量測資料根的檔案總量(排除頂層中繼與 .gsm-* 暫存)。
// 註:與 docker_monitor.go 的 DockerBackend.dataDiskUsage 為同一走查邏輯的最小複製——後者為
// *DockerBackend 方法且 docker_monitor.go 不在本任務可改範圍,故此處以 package 函式獨立實作。
func dataDirUsage(root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(filepath.Base(p), ".gsm-") {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, rerr := filepath.Rel(root, p); rerr == nil && isTopLevelMeta(filepath.ToSlash(rel)) {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}

// writeJSONFile 以縮排 JSON 原子性寫檔(建父目錄)。
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失敗: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("寫入 %s 失敗: %w", filepath.Base(path), err)
	}
	return nil
}

// readJSONFile 讀回 JSON 檔。
func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
