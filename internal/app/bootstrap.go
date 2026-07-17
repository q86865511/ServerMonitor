// Package app 把 core 與 agent 的所有後端元件組裝成一個可運行的應用實例(Runtime),
// 並提供啟動/優雅關閉的編排。Wails 綁定層(main 套件的 App)是本套件之上的薄轉發殼:
// 綁定方法呼叫 Runtime 持有的服務、事件推送訂閱 Runtime 的 MonitorHub。
//
// 組裝刻意可測試:後端經 BackendFactory 注入,測試以 agent.MockBackend 免 Docker 跑通
// 整條裝配鏈(見 bootstrap_test.go)。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/agent/provision"
	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// BackendFactory 依 agent.BackendOptions 建立節點代理的頂層執行後端(native-backend R2)。
// 生產為 defaultBackendFactory(Windows 回 dispatchBackend 併 native+docker;非 Windows 回
// docker-only);測試注入回傳 agent.MockBackend 的工廠以免 Docker。
type BackendFactory func(agent.BackendOptions) (agent.RuntimeBackend, error)

// DockerFactory 只建立一個 docker 子後端,供 RetryDocker 於 dispatch 頂層熱替換 docker 能力
// (不重建整個 agent);非 dispatch 頂層(docker-only/測試)不用它。
type DockerFactory func(agent.BackendOptions) (agent.RuntimeBackend, error)

// defaultBackendFactory 是生產預設頂層後端工廠(native-backend R2/R13):
//   - Windows:建 NativeBackend(恆成功,免 Docker 亦可運行)+ 盡力建 DockerBackend(失敗則
//     docker=nil,節點不因 Docker down 離線)→ 回 dispatchBackend。
//   - 非 Windows:維持 docker-only 現行為(native 不支援)——docker 建構失敗即回錯,沿用既有
//     離線啟動流程。
func defaultBackendFactory(opts agent.BackendOptions) (agent.RuntimeBackend, error) {
	if runtime.GOOS != "windows" {
		return defaultDockerFactory(opts)
	}
	native, err := agent.NewNativeBackend(agent.NativeOptions{
		DataRoot:   opts.InstancesRoot,
		BackupRoot: opts.BackupRoot,
		CacheRoot:  opts.CacheRoot,
		Node:       opts.Node,
		Prov:       agent.NewProvisionAdapter(opts.CacheRoot, provision.WithCurseForgeAPIKey(opts.CurseForgeAPIKey)),
	})
	if err != nil {
		return nil, fmt.Errorf("建立 native 後端失敗: %w", err)
	}
	var docker agent.RuntimeBackend
	if d, derr := defaultDockerFactory(opts); derr == nil {
		docker = d
	}
	return agent.NewDispatchBackend(native, docker)
}

// defaultDockerFactory 由 BackendOptions 建立官方 Docker SDK 後端(dispatch 子後端與非 Windows
// 頂層共用)。
func defaultDockerFactory(opts agent.BackendOptions) (agent.RuntimeBackend, error) {
	return agent.NewDockerBackend(agent.DockerOptions{
		DataRoot:   opts.InstancesRoot,
		BackupRoot: opts.BackupRoot,
		Node:       opts.Node,
	})
}

// Options 是 Bootstrap 的輸入。零值皆有合理預設。
type Options struct {
	// DataRoot 覆寫資料根目錄;空字串用 config 的 data_root,再空則用 %LOCALAPPDATA%\ServerMonitor。
	DataRoot string
	// BuiltinTemplateDir 是隨執行檔散布的內建範本目錄;空字串用「執行檔目錄/templates」。
	BuiltinTemplateDir string
	// BackendFactory 建立頂層執行後端;nil 用 defaultBackendFactory(Windows dispatch、其餘 docker)。
	BackendFactory BackendFactory
	// DockerFactory 只建 docker 子後端,供 RetryDocker 於 dispatch 頂層熱替換;nil 用 defaultDockerFactory。
	// 主要供測試注入假 docker 後端以驗證熱替換,不必真連 Docker daemon。
	DockerFactory DockerFactory
	// AlertChannel 覆寫告警送出通道(nil 用 Discord webhook 通道)。供測試注入 fake 觀測告警。
	AlertChannel core.AlertChannel
	// Now 供測試注入固定時鐘;nil 用 time.Now。
	Now func() time.Time
	// ReadyTimeout 覆寫生命週期啟動就緒逾時(Orchestrator.awaitReady);<=0 用預設 60s(R3)。
	// 重載遊戲(如 Minecraft 冷啟需下載/世界生成、Palworld 啟動數分鐘)或 E2E 測試可放寬之。
	ReadyTimeout time.Duration
}

// appConfigFileName 是資料根下的一般設定檔名(UTF-8 JSON;毀損時降級,見 core.LoadAppConfig)。
const appConfigFileName = "config.json"

const (
	subDirJournalCreate = "journal/create" // 建立 journal(Reconciler 解讀為未完成建立)
	subDirJournalOps    = "journal/ops"    // 備份/還原 op-journal(必與 create 相異)
	subDirInstances     = "instances"      // agent dataRoot
	subDirBackups       = "backups"        // agent backupRoot
	subDirCache         = "cache"          // native 供應共用快取根(JRE/SteamCMD)
	subDirTemplates     = "templates"      // 使用者自訂範本
	appDBFileName       = "app.db"
	fallbackFileName    = "events-fallback.ndjson"
)

// announceRestartMessage 是排程重啟前的公告文案(範本 hooks.announce 的 {msg})。
const announceRestartMessage = "伺服器即將依排程重新啟動,請留意。"

// Runtime 是組裝完成的應用後端。持有全部服務與背景迴圈的生命週期控制。
// 綁定層經其匯出方法/欄位操作 core;背景迴圈由 Start 啟動、Shutdown 依序收束。
type Runtime struct {
	node     string
	dataRoot string

	// 基礎設施。
	lock    *core.AppLock
	store   *core.Store
	secrets *core.SecretStore
	events  *core.EventLog
	engine  *core.TemplateEngine
	journal *core.Journal
	opJrnl  *core.OpJournal

	// 節點代理(本機 loopback)。
	agentMu   sync.Mutex // 保護 agent 相關欄位的重試替換(RetryDocker)
	backend   agent.RuntimeBackend
	agentSrv  *agent.Server
	agentHTTP *http.Server
	agentLn   net.Listener

	// core 服務。
	registry  *core.NodeRegistry
	client    *core.NodeClient
	instances *core.InstanceService
	prober    *core.HealthProber
	recon     *core.Reconciler
	orch      *core.Orchestrator
	commands  *core.CommandService
	backups   *core.BackupService
	restart   *core.RestartPolicy
	health    *core.HealthMonitor
	sched     *core.Scheduler
	alerts    *core.AlertDispatcher
	discord   *core.DiscordWebhookChannel
	threshold *core.ThresholdMonitor
	monitor   *core.MonitorHub

	backendOpts    agent.BackendOptions // 供 RetryDocker 重建後端用的固定參數
	backendFactory BackendFactory       // 建立頂層後端的工廠(RetryDocker 於非 dispatch 頂層沿用;測試可注入)
	dockerFactory  DockerFactory        // 只建 docker 子後端(RetryDocker 於 dispatch 頂層熱替換用)

	// 背景迴圈的 context(各自獨立以支援 Shutdown 的有序收束)。
	rootCtx      context.Context
	rootCancel   context.CancelFunc
	eventCtx     context.Context
	eventCancel  context.CancelFunc
	schedCtx     context.Context
	schedCancel  context.CancelFunc
	healthCtx    context.Context
	healthCancel context.CancelFunc

	watchedMu sync.Mutex
	watched   map[string]bool // ThresholdMonitor 正在監看的實例(供 Shutdown 逐一 Stop)

	// 關閉收束(T15 雙審 #3)。
	inflight sync.WaitGroup // 綁定層觸發的進行中長操作;Shutdown 於關 Store 前等待其收束
	loopWg   sync.WaitGroup // 事件/排程/存活三個背景 Run 迴圈;Shutdown cancel 後 Wait

	started      bool
	shutdownOnce sync.Once
}

// Node 回傳本機節點識別。
func (r *Runtime) Node() string { return r.node }

// ErrAppLocked 由 Bootstrap 回傳(包裹 core.ErrAppLocked),供綁定層以 dialog 提示後退出(R13)。
var ErrAppLocked = core.ErrAppLocked

// Bootstrap 依組裝順序建立整個後端。任一致命步驟失敗回錯誤(呼叫端不進入 GUI 主迴圈,
// 但 AppLock 衝突除外——它以 ErrAppLocked 回報,由綁定層決定顯示與退出)。
//
// Docker daemon 不可用不是致命錯誤:代理建構失敗時節點標離線、GUI 仍可啟動(R5),
// 稍後可經 RetryDocker 重試。
func Bootstrap(opts Options) (*Runtime, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	factory := opts.BackendFactory
	if factory == nil {
		factory = defaultBackendFactory
	}
	dockerFactory := opts.DockerFactory
	if dockerFactory == nil {
		dockerFactory = defaultDockerFactory
	}

	// 1) 解析資料根:先讀設定(可指定 data_root),再定案有效根。
	baseRoot := opts.DataRoot
	if baseRoot == "" {
		baseRoot = defaultBaseRoot()
	}
	if err := os.MkdirAll(baseRoot, 0o755); err != nil {
		return nil, fmt.Errorf("建立資料根失敗: %w", err)
	}
	cfg, cfgRecovered, err := core.LoadAppConfig(filepath.Join(baseRoot, appConfigFileName))
	if err != nil {
		return nil, fmt.Errorf("讀取設定檔失敗: %w", err)
	}
	dataRoot := baseRoot
	if opts.DataRoot == "" && cfg.DataRoot != "" {
		dataRoot = cfg.DataRoot
	}
	node := cfg.Node
	if node == "" {
		node = "local"
	}

	// 2) 建目錄骨架。
	createDir := filepath.Join(dataRoot, subDirJournalCreate)
	opsDir := filepath.Join(dataRoot, subDirJournalOps)
	instancesDir := filepath.Join(dataRoot, subDirInstances)
	backupsDir := filepath.Join(dataRoot, subDirBackups)
	cacheDir := filepath.Join(dataRoot, subDirCache)
	userTemplatesDir := filepath.Join(dataRoot, subDirTemplates)
	for _, d := range []string{dataRoot, createDir, opsDir, instancesDir, backupsDir, cacheDir, userTemplatesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("建立目錄 %s 失敗: %w", d, err)
		}
	}
	// op-journal 與建立 journal 目錄必須相異(否則對帳會把備份中實例誤判為未完成建立而刪容器)。
	if createDir == opsDir {
		return nil, fmt.Errorf("建立 journal 與 op-journal 目錄不可相同: %s", createDir)
	}

	// 3) AppLock 最先取(在重量級初始化之前)。衝突→回 ErrAppLocked。
	lock, err := core.AcquireAppLock(dataRoot)
	if err != nil {
		if errors.Is(err, core.ErrAppLocked) {
			return nil, err
		}
		return nil, fmt.Errorf("取得單一實例鎖失敗: %w", err)
	}

	// 從此點起若中途失敗,須釋放已取得的資源。
	r := &Runtime{
		node:     node,
		dataRoot: dataRoot,
		lock:     lock,
		watched:  make(map[string]bool),
	}
	fail := func(e error) (*Runtime, error) {
		r.releasePartial()
		return nil, e
	}

	// 4) Store(毀損隔離)+ fallback + EventLog。
	fallback := core.NewFallbackRecorder(filepath.Join(dataRoot, fallbackFileName))
	store, err := core.Open(filepath.Join(dataRoot, appDBFileName), core.Options{Fallback: fallback, Now: now})
	if err != nil {
		return fail(fmt.Errorf("開啟資料庫失敗: %w", err))
	}
	r.store = store
	events := core.NewEventLog(store, fallback, core.EventLogOptions{RetentionMax: cfg.EventRetentionMax, Now: now})
	r.events = events
	if cfgRecovered {
		details, _ := json.Marshal(map[string]string{"path": filepath.Join(baseRoot, appConfigFileName)})
		_ = events.Append(protocol.Event{Code: core.EventConfigRecovered, Severity: protocol.SeverityWarning, DetailsJSON: details})
	}

	// 5) SecretStore、TemplateEngine(內建 + 使用者目錄)。
	r.secrets = core.NewSecretStore(core.DefaultKeyringService)
	r.engine = core.NewTemplateEngine(core.DefaultAdapterRegistry(), events)
	loadTemplates(r.engine, opts.BuiltinTemplateDir, userTemplatesDir)

	// 5b) CurseForge 覆蓋金鑰:實值存 OS 金鑰庫(#11:不明文落 config.json);讀到舊版明文則遷入
	//     金鑰庫、清空 config.json 並記事件。解析失敗為非致命(CF 為選用能力,失敗僅停用)。
	cfgPath := filepath.Join(baseRoot, appConfigFileName)
	cfKey, cfMigrated, cfErr := core.ResolveCurseForgeOverrideKey(r.secrets, cfgPath, &cfg)
	if cfErr != nil {
		cfKey = ""
	} else if cfMigrated {
		details, _ := json.Marshal(map[string]string{"path": cfgPath})
		_ = events.Append(protocol.Event{Code: core.EventConfigCurseForgeKeyMigrated, Severity: protocol.SeverityInfo, DetailsJSON: details})
	}

	// 6) Journal / OpJournal。
	journal, err := core.NewJournal(createDir)
	if err != nil {
		return fail(fmt.Errorf("建立 journal 失敗: %w", err))
	}
	opJrnl, err := core.NewOpJournal(opsDir)
	if err != nil {
		return fail(fmt.Errorf("建立 op-journal 失敗: %w", err))
	}
	r.journal = journal
	r.opJrnl = opJrnl

	// 7) 節點登錄。
	r.registry = core.NewNodeRegistry(events)

	// 8) 節點代理(loopback);Docker 不可用時標離線但不致命(Windows dispatch 頂層恆在線,詳見
	//    startAgentLocked/RetryDocker)。
	r.backendOpts = agent.BackendOptions{InstancesRoot: instancesDir, BackupRoot: backupsDir, CacheRoot: cacheDir, Node: node, CurseForgeAPIKey: cfKey}
	r.backendFactory = factory
	r.dockerFactory = dockerFactory
	if err := r.startAgentLocked(factory); err != nil {
		// R5:daemon 不可用→節點離線、GUI 可啟動;註冊佔位 client 使呼叫自然失敗、可經 RetryDocker 重試。
		placeholder := core.NewNodeClient(offlinePlaceholderURL, "", nil)
		r.registry.Register(node, placeholder)
		r.registry.MarkOffline(node, err)
		// r.client 同步設為佔位:MonitorHub 已改經 registry dialer(不再直讀 r.client),但保持
		// 「r.client 恆非 nil」的不變式,杜絕其他潛在使用點的 nil deref(T15 雙審 #1)。
		r.client = placeholder
	}

	// 9) core 服務接線。
	rootCtx, rootCancel := context.WithCancel(context.Background())
	r.rootCtx, r.rootCancel = rootCtx, rootCancel

	r.instances = core.NewInstanceService(core.InstanceServiceConfig{
		Store: store, Secrets: r.secrets, Events: events, Engine: r.engine,
		Registry: r.registry, Journal: journal, Node: node, Now: now,
	})
	r.commands = core.NewCommandService(store, r.engine, r.secrets, r.registry)
	r.prober = core.NewHealthProber(core.ProberConfig{
		Store: store, Engine: r.engine, Registry: r.registry, Commander: r.commands,
	})
	r.recon = core.NewReconciler(core.ReconcilerConfig{
		Store: store, Events: events, Registry: r.registry, Journal: journal, Now: now,
	})

	// 告警:Discord webhook 通道(URL 未設→Configured()=false→AlertDispatcher no-op)。
	r.discord = core.NewDiscordWebhookChannel(r.secrets, core.DiscordWebhookSecretKey)
	channel := opts.AlertChannel
	if channel == nil {
		channel = r.discord
	}
	r.alerts = core.NewAlertDispatcher(core.AlertDispatcherConfig{Channel: channel, Now: now})

	// 自動重啟策略(建構後以 SetRestart 注入 orch,打破建構循環)。
	r.restart = core.NewRestartPolicy(core.RestartPolicyConfig{
		Store: store, Events: events, Alerts: r.alerts, Now: now, BaseCtx: rootCtx,
	})

	// CrashHook 組合函式(T13 遺留 INSTANCE_CRASHED producer 接線):
	//   1) RestartPolicy.OnCrash —— 自動重啟記帳/退避(於 per-instance lock 內同步呼叫,契約允許)。
	//   2) 經 AlertDispatcher.Alert 派發 INSTANCE_CRASHED 告警 —— best-effort、於 goroutine 內,
	//      不阻塞持鎖路徑(比照 RestartPolicy 經 after 於 lock 外派發的模式)。
	// INSTANCE_CRASHED 事件本身已由 Orchestrator.handleDie/RecoverStuck 寫入 EventLog;此處僅補上
	// 「同一事件也走告警管道」這條先前缺席的接線(AlertPayload 已支援 INSTANCE_CRASHED 類別)。
	crashHook := func(uuid string, exitCode *int) {
		r.restart.OnCrash(uuid, exitCode)
		go r.dispatchCrashAlert(uuid, exitCode)
	}

	r.orch = core.NewOrchestrator(core.OrchestratorConfig{
		Store: store, Events: events, Registry: r.registry, Reconciler: r.recon,
		Prober: r.prober, Now: now, CrashHook: crashHook, StopHook: r.commands.RunStopHook,
		ReadyTimeout: opts.ReadyTimeout, // <=0 → NewOrchestrator 用預設 60s
	})
	r.restart.SetRestart(r.orch.Start, r.orch.MarkGiveup)

	r.backups = core.NewBackupService(core.BackupServiceConfig{
		Store: store, Events: events, Registry: r.registry, Orchestrator: r.orch, OpJournal: opJrnl, Now: now,
	})

	// 排程:restart=orch.Restart(announce 由 scheduler 既有邏輯在重啟前呼叫);
	// announce=CommandService.Announce(包裝為固定文案);backup=BackupService.Backup。
	announce := func(ctx context.Context, uuid string) error {
		_, aerr := r.commands.Announce(ctx, uuid, announceRestartMessage)
		return aerr
	}
	r.sched = core.NewScheduler(core.SchedulerConfig{
		Store: store, Events: events, Alerts: r.alerts, Now: now,
		Restart: r.orch.Restart, Announce: announce, Backup: r.backups.Backup,
	})

	r.health = core.NewHealthMonitor(core.HealthMonitorConfig{
		Store: store, Prober: r.prober, Recoverer: r.orch,
	})

	// 監控聚合:dialer=registry 委派(每次撥號取當前 client,解離線啟動 nil 與 RetryDocker 舊連線
	// 兩問題,T15 #1)、sender=CommandService、prober=HealthProber 適配器(線上維度不再恆「不適用」,
	// T15 #10)、lookup=Store+TemplateEngine。stats 來源供 ThresholdMonitor。
	r.monitor = core.NewMonitorHub(
		core.NewRegistryStreamDialer(r.registry, node),
		r.commands,
		core.NewHealthOnlineProber(r.prober),
		core.NewStoreInstanceLookup(store, r.engine),
		core.MonitorConfig{},
	)
	r.threshold = core.NewThresholdMonitor(core.ThresholdMonitorConfig{
		Store: store, Events: events, Alerts: r.alerts, Stats: r.monitor, Now: now,
	})

	return r, nil
}

// dispatchCrashAlert 組一則 INSTANCE_CRASHED 事件(補 node/template)並經 AlertDispatcher 派發。
// best-effort:去重/未設管道由 AlertDispatcher 處理;此處吞掉錯誤(來源事件已入 EventLog)。
func (r *Runtime) dispatchCrashAlert(uuid string, exitCode *int) {
	ev := protocol.Event{
		Code:         protocol.EventInstanceCrashed,
		Severity:     protocol.SeverityError,
		InstanceUUID: &uuid,
	}
	if rec, err := r.store.GetInstance(uuid); err == nil {
		node, tmpl := rec.Node, rec.TemplateID
		ev.Node, ev.TemplateID = &node, &tmpl
	}
	if exitCode != nil {
		ev.DetailsJSON, _ = json.Marshal(map[string]any{"exit_code": *exitCode})
	}
	_ = r.alerts.Alert(r.rootCtx, ev)
}

// startAgentLocked 建立後端 + 代理 Server,綁 127.0.0.1:0(ephemeral)並在 goroutine 內服務;
// 斷言 listener 為 loopback(R5)。成功後以真實位址註冊 NodeClient 並更新 r.client。
// 呼叫端須確保序列化(Bootstrap 單執行緒;RetryDocker 持 agentMu)。
func (r *Runtime) startAgentLocked(factory BackendFactory) error {
	backend, err := factory(r.backendOpts)
	if err != nil {
		return fmt.Errorf("建立執行後端失敗: %w", err)
	}
	srv, err := agent.NewServer(agent.Config{Backend: backend})
	if err != nil {
		_ = closeBackend(backend)
		return fmt.Errorf("建立節點代理失敗: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = srv.Close()
		_ = closeBackend(backend)
		return fmt.Errorf("代理綁定 loopback 失敗: %w", err)
	}
	// R5 斷言:實際綁定位址必為 loopback。
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		_ = srv.Close()
		_ = closeBackend(backend)
		return fmt.Errorf("代理未綁定於 loopback: %v", ln.Addr())
	}
	httpSrv := &http.Server{Handler: srv}
	go func() { _ = httpSrv.Serve(ln) }()

	baseURL := "http://" + ln.Addr().String()
	client := core.NewNodeClient(baseURL, srv.Token(), nil)

	r.backend = backend
	r.agentSrv = srv
	r.agentHTTP = httpSrv
	r.agentLn = ln
	r.client = client
	r.registry.Register(r.node, client)
	return nil
}

// RetryDocker 重試 Docker 能力(native-backend R5/R13)。兩種頂層後端型態:
//   - dispatch 頂層(Windows 生產):節點已因 native 在線,只需重建 docker 子後端並熱替換
//     (SwapDocker),不重建整個 agent、不動 native 與既有 client/listener。docker 已可用則 no-op。
//   - 非 dispatch 頂層(docker-only/測試):沿用整個 agent 重建的離線恢復路徑(節點在線即 no-op)。
//
// dispatch 熱替換後,MonitorHub 的 registry-dialer 與 client/listener 均不變(agent 未重建),
// docker 事件經 dispatchBackend 的 pump 於替換後自動接上(見 dispatchBackend.SwapDocker)。
func (r *Runtime) RetryDocker() error {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()

	if dc, ok := r.backend.(agent.DockerCapable); ok {
		if dc.DockerAvailable() {
			return nil // docker 子後端已可用
		}
		docker, err := r.dockerFactory(r.backendOpts)
		if err != nil {
			return err // 節點維持在線(native),僅 docker 能力仍缺
		}
		dc.SwapDocker(docker)
		return nil
	}

	// 非 dispatch 頂層:沿用既有整個 agent 重建的離線恢復流程(T15 雙審 #1(b))。
	if st, ok := r.registry.Status(r.node); ok && st.Online {
		return nil // 已線上,無需重試
	}
	if err := r.startAgentLocked(r.backendFactory); err != nil {
		r.registry.MarkOffline(r.node, err)
		return err
	}
	return nil
}

// DockerAvailable 回報本節點 Docker 能力是否就緒(native-backend R12/R13:節點在線與 Docker
// 能力分離)。dispatch 頂層直接查子後端;非 dispatch 頂層(docker-only)則等同節點在線。
// 供上層(Wails 綁定,T12)顯示 docker 選項是否置灰。
func (r *Runtime) DockerAvailable() bool {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	if dc, ok := r.backend.(agent.DockerCapable); ok {
		return dc.DockerAvailable()
	}
	if st, ok := r.registry.Status(r.node); ok {
		return st.Online
	}
	return false
}

// CurseForgeEnabled 回報本節點是否啟用 CurseForge 模組包(native-backend R14:建置內嵌或設定覆蓋
// 了 API key)。dispatch 頂層委派 native 子後端;非 dispatch 頂層(docker-only/測試)一律回 false。
// 供 Wails 綁定(App.CurseForgeEnabled)決定 GUI 是否顯示 native CurseForge 模組包選項。
func (r *Runtime) CurseForgeEnabled() bool {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	if cc, ok := r.backend.(agent.CurseForgeCapable); ok {
		return cc.CurseForgeEnabled()
	}
	return false
}

// closeBackend 關閉實作了 Close 的後端(Mock/Docker 皆有;介面未含 Close,故型別斷言)。
func closeBackend(b agent.RuntimeBackend) error {
	if c, ok := b.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// releasePartial 於 Bootstrap 中途失敗時,反向釋放已取得的資源(store/agent/lock)。
func (r *Runtime) releasePartial() {
	if r.agentHTTP != nil {
		_ = r.agentHTTP.Close()
	}
	if r.agentSrv != nil {
		_ = r.agentSrv.Close()
	}
	if r.backend != nil {
		_ = closeBackend(r.backend)
	}
	if r.store != nil {
		_ = r.store.Close()
	}
	if r.lock != nil {
		_ = r.lock.Release()
	}
}

// defaultBaseRoot 回傳預設資料根 %LOCALAPPDATA%\ServerMonitor(退回 UserConfigDir)。
func defaultBaseRoot() string {
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return filepath.Join(la, "ServerMonitor")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "ServerMonitor")
	}
	return filepath.Join(".", "ServerMonitor-data")
}

// offlinePlaceholderURL 是代理未啟動時註冊的佔位位址(對其呼叫必失敗→節點維持離線)。
const offlinePlaceholderURL = "http://127.0.0.1:9"

// loadTemplates 載入內建範本目錄(空則用執行檔目錄/templates)與使用者範本目錄,best-effort:
// 目錄不存在只略過,不致命(範本可於執行期由使用者補入後重啟載入)。
func loadTemplates(engine *core.TemplateEngine, builtinDir, userDir string) {
	if builtinDir == "" {
		if exe, err := os.Executable(); err == nil {
			builtinDir = filepath.Join(filepath.Dir(exe), subDirTemplates)
		}
	}
	if builtinDir != "" {
		if _, err := os.Stat(builtinDir); err == nil {
			_, _ = engine.LoadDir(builtinDir)
		}
	}
	if userDir != "" {
		if _, err := os.Stat(userDir); err == nil {
			_, _ = engine.LoadDir(userDir)
		}
	}
}
