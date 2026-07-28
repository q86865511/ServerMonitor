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

	mu       sync.Mutex
	procs    map[string]*procHandle // 鍵為 uuid
	starting map[string]struct{}    // 正在 Start 中(尚未登記 procHandle)的 uuid 占位;防並發雙 Start 各自起行程(#17)

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
	// InstallServer 依變體 loader 安裝 Minecraft 伺服器檔案至實例目錄,回傳啟動來源產物路徑
	// (ServerJar 或 Forge/NeoForge 的 StartScript/ArgsFile,依 loader 擇一有值)。
	InstallServer(ctx context.Context, req ServerInstallRequest, progress func(protocol.ProvisionProgress)) (ServerInstallResult, error)
	// InstallSteamApp 以 SteamCMD 安裝/更新指定 Steam App 至實例目錄。
	InstallSteamApp(ctx context.Context, appID, instanceDir string, progress func(protocol.ProvisionProgress)) error
	// WriteEula 於實例目錄寫入 eula.txt(native 無 itzg 代勞;R5)。accepted=false 時為 no-op。
	WriteEula(ctx context.Context, instanceDir string, accepted bool) error
	// InstallModpack 安裝模組包(遠端來源或本機既有封存檔擇一,見 ModpackInstallRequest)至
	// req.TargetDir(native-backend R11)。遠端來源依 req.Type 分派 provision.ModProvider;
	// "curseforge" 尚未支援(R14)一律回明確錯誤。
	InstallModpack(ctx context.Context, req ModpackInstallRequest, progress func(protocol.ProvisionProgress)) error
}

// curseForgeCapable 由供應器實作以回報 CurseForge 模組包能力是否啟用(有內嵌或設定覆蓋的 API key)。
// 以可選介面表達,使 provisionRunner 的測試假件無須實作;NativeBackend.CurseForgeEnabled 於未實作時
// 回 false(native-backend R14)。
type curseForgeCapable interface {
	CurseForgeEnabled() bool
}

// curseForgeConfigurable 由供應器實作以支援 GUI 於執行期熱設定 CurseForge 覆蓋金鑰(native-backend
// R14)。以可選介面表達,使測試假件無須實作;NativeBackend.SetCurseForgeKey 於未實作時為 no-op。
type curseForgeConfigurable interface {
	SetCurseForgeKey(key string)
}

// CurseForgeEnabled 回報本後端的供應器是否啟用 CurseForge 模組包(native-backend R14)。供上層
// (dispatchBackend→Runtime→Wails 綁定)決定 GUI 是否顯示 native CurseForge 模組包選項。
func (b *NativeBackend) CurseForgeEnabled() bool {
	if cc, ok := b.prov.(curseForgeCapable); ok {
		return cc.CurseForgeEnabled()
	}
	return false
}

// SetCurseForgeKey 熱替換供應器的 CurseForge 覆蓋金鑰(GUI 設定;native-backend R14)。空字串=清除
// 覆蓋、回退內嵌金鑰。供應器未實作 curseForgeConfigurable 時為 no-op。
func (b *NativeBackend) SetCurseForgeKey(key string) {
	if cc, ok := b.prov.(curseForgeConfigurable); ok {
		cc.SetCurseForgeKey(key)
	}
}

// ServerInstallRequest 是 provisionRunner.InstallServer 的輸入(native-backend R5)。
type ServerInstallRequest struct {
	InstanceDir string // 伺服器檔案落位目錄(絕對路徑)
	Variant     string // 變體 ID(如 "paper-1.21")
	Loader      string // loader 家族(vanilla/paper/fabric/forge/neoforge)
	MCVersion   string // 目標 Minecraft 版本(T9 buildSpec 填入;adapter 優先取此值,缺值才由 Variant 慣例導出)
	JavaMajor   int    // 所需 Java major(Forge/NeoForge installer 執行用)
	JavaPath    string // 已供應的 java 執行檔路徑
}

// ServerInstallResult 是 provisionRunner.InstallServer 的產物(啟動來源;native-backend R5/R7)。
// ServerJar 為可 `java -jar` 直接啟動的 jar(vanilla/paper/fabric);StartScript/ArgsFile 為
// Forge/NeoForge 以官方 installer --installServer 產出的啟動腳本(run.bat)與 JVM args 檔
// (現代結構無單一 loader server jar,見 provision.InstalledServer)。三者依 loader 擇一有值。
type ServerInstallResult struct {
	ServerJar   string
	StartScript string
	ArgsFile    string
}

// ModpackInstallRequest 是 provisionRunner.InstallModpack 的輸入(native-backend R11)。Type/Ref
// 描述遠端來源(對映 protocol.ModpackRef);ArchivePath 非空時改為安裝本機既有封存檔(手動上傳
// 路徑,見 WriteMountFile),此時 Ref 忽略。Type 恆需有值(供 provisionAdapter 選 ModProvider,
// 手動上傳路徑由 detectModpackArchiveType 依封存檔內容判定)。
type ModpackInstallRequest struct {
	Type        string // "modrinth" | "curseforge"(R14)
	Ref         string // 遠端來源 ref(slug/版本 id);ArchivePath 非空時不用
	ArchivePath string // 本機既有封存檔絕對路徑;非空時優先於 Ref
	TargetDir   string // 實例根絕對路徑(#2):provider 的 files[].path 自帶 mods/ 前綴、overrides 相對此根落地,勿先展開到 <root>/mods
	MCVersion   string
	Loader      string
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
		starting:   make(map[string]struct{}),
	}
	b.adoptExisting()
	return b, nil
}

// nativeMeta 是 native 實例的建立期中繼(實例根下 native.json),供 Start 展開啟動命令。
//
// Provisioning 為建立中標記:Create 一開始(建目錄後、供應前)即寫入僅含 UUID＋此旗標的最小中繼,
// 使實例自建立起即被本後端視為所管(instanceExists→List→resolve 皆可見),供應完成後才覆寫為完整
// (Provisioning=false)中繼。動機:供應中途失敗時,若無此最小中繼,核心回滾 Remove 會因 resolve
// 找不到而拿 404、殘留半下載目錄;有了它,失敗實例可被 List 看見並由 Remove 認領清理(native-backend #6)。
type nativeMeta struct {
	UUID         string                 `json:"uuid"`
	Provisioning bool                   `json:"provisioning,omitempty"` // true=供應未完成的最小中繼(見型別註)
	JavaPath     string                 `json:"java_path,omitempty"`
	ServerJar    string                 `json:"server_jar,omitempty"`
	StartScript  string                 `json:"start_script,omitempty"` // Forge/NeoForge 啟動腳本(run.bat);{start_script} token 展開來源
	ArgsFile     string                 `json:"args_file,omitempty"`    // Forge/NeoForge user_jvm_args.txt(供 supervisor 注入 JVM 參數;T13 範本校正落點)
	Command      []string               `json:"command"`
	WorkingDir   string                 `json:"working_dir,omitempty"`
	MemoryMB     int                    `json:"memory_mb,omitempty"`
	Ports        []protocol.PortBinding `json:"ports,omitempty"`
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

	// job 是強制資源上限並提供行程樹統計的 Windows Job Object(T7;非 Windows 為 no-op 存根)。
	// 恆建立(即使不限額,僅為取 accounting);建立/掛入失敗時降級為 nil(僅監督不強制,見 Start)。
	job    *jobObject
	limits jobLimits // 本實例套用的資源上限(供 Stats 回報 MemoryLimit)

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
	// #6:先寫最小中繼(僅 UUID＋建立中標記),使實例自建立起即為本後端所管——供應中途失敗時
	// List/resolve 仍可見、核心回滾 Remove 可認領清理(否則殘留孤兒目錄)。供應完成後於本函式末覆寫完整版。
	if err := b.writeNativeMeta(uuid, nativeMeta{UUID: uuid, Provisioning: true}); err != nil {
		return "", fmt.Errorf("寫入建立中中繼失敗: %w", err)
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
	javaPath, installed, err := b.provision(ctx, spec, workDir, progress)
	if err != nil {
		return "", err
	}

	// 設定檔(params→server.properties / palworld-ini,由 spec.Env 取值;Set 的固定/衍生值
	// 以 spec.Ports 展開埠 token——native 無 docker 埠映射,server-port/rcon.port 須綁與探針一致)。
	for _, cm := range spec.Native.Config {
		if err := writeConfigFile(workDir, cm, spec.Env, spec.Ports); err != nil {
			return "", err
		}
	}

	// Forge/NeoForge:啟動經 installer 產出的 run.bat 讀 user_jvm_args.txt(無單一 server jar,故
	// 範本的 -Xmx{memory_mb}M -jar {server_jar} 形式不適用);把記憶體上限注入該 args 檔(R7/R9 的
	// supervisor 注入落點,見 nativeMeta.ArgsFile 註)。ServerJar 有值(vanilla/paper/fabric)時不注入。
	if installed.StartScript != "" && installed.ArgsFile != "" && installed.ServerJar == "" {
		if err := injectMemoryArgs(installed.ArgsFile, resolveMemoryMB(spec)); err != nil {
			return "", err
		}
	}

	// 遠端模組包安裝(native-backend R11):spec.Modpack 非 nil 時依 Type 分派安裝至 ModsDir。
	// 手動上傳的模組包檔不在此處處理——Create 早於核心的 UploadMount 呼叫完成(見
	// instance_service.go:295 上傳時序,建容器後才上傳),故手動路徑改由 WriteMountFile 於檔案
	// 抵達時觸發安裝(見該方法)。
	if spec.Modpack != nil {
		// #2×#7(複審 A):TargetDir 傳 workDir(伺服器工作目錄=遊戲根),而非實例根。provider
		// (modprovider.go/curseforge.go)語意為「TargetDir=遊戲根,mrpack files[].path 自帶 mods/ 前綴、
		// overrides 相對遊戲根」;伺服器以 cwd=workDir 讀 mods/ 與設定,故落位必須跟隨 workDir
		// (minecraft 範本 working_dir="data" → 模組落 <root>/data/mods,與 docker itzg 佈局一致)。
		// modsInstallDir 於此僅作「範本有宣告 mods_dir」的前置檢查(未宣告即拒安裝,不臆測落位)。
		if _, derr := b.modsInstallDir(root, spec); derr != nil {
			return "", derr
		}
		if err := b.prov.InstallModpack(ctx, ModpackInstallRequest{
			Type:      spec.Modpack.Type,
			Ref:       spec.Modpack.Ref,
			TargetDir: workDir,
			MCVersion: spec.Native.Provision.MCVersion,
			Loader:    spec.Native.Provision.Loader,
		}, progress); err != nil {
			return "", fmt.Errorf("安裝模組包失敗: %w", err)
		}
	}

	// 中繼快照(含啟動來源:server jar 或 Forge/NeoForge 啟動腳本/args 檔)。
	meta := nativeMeta{
		UUID:        uuid,
		JavaPath:    javaPath,
		ServerJar:   installed.ServerJar,
		StartScript: installed.StartScript,
		ArgsFile:    installed.ArgsFile,
		Command:     spec.Native.Launch.Command,
		WorkingDir:  spec.Native.Launch.WorkingDir,
		MemoryMB:    resolveMemoryMB(spec),
		Ports:       spec.Ports,
	}
	if err := b.writeNativeMeta(uuid, meta); err != nil {
		return "", err
	}
	if err := b.writeSpec(spec); err != nil {
		return "", err
	}
	return nativeID(uuid), nil
}

// provision 依 native payload 的供應類型執行供應,回傳 javaPath 與啟動來源產物(steamcmd 皆為空值)。
func (b *NativeBackend) provision(ctx context.Context, spec protocol.InstanceSpec, workDir string, progress func(protocol.ProvisionProgress)) (javaPath string, installed ServerInstallResult, err error) {
	np := spec.Native.Provision
	switch np.Kind {
	case "java":
		javaPath, err = b.prov.EnsureJava(ctx, np.JavaMajor, progress)
		if err != nil {
			return "", ServerInstallResult{}, fmt.Errorf("供應 Java 失敗: %w", err)
		}
		installed, err = b.prov.InstallServer(ctx, ServerInstallRequest{
			InstanceDir: workDir,
			Variant:     spec.Variant,
			Loader:      np.Loader,
			MCVersion:   np.MCVersion,
			JavaMajor:   np.JavaMajor,
			JavaPath:    javaPath,
		}, progress)
		if err != nil {
			return "", ServerInstallResult{}, fmt.Errorf("安裝伺服器失敗: %w", err)
		}
		if np.EULA {
			if err := b.prov.WriteEula(ctx, workDir, true); err != nil {
				return "", ServerInstallResult{}, fmt.Errorf("寫入 eula.txt 失敗: %w", err)
			}
		}
	case "steamcmd":
		if err := b.prov.InstallSteamApp(ctx, np.SteamAppID, workDir, progress); err != nil {
			return "", ServerInstallResult{}, fmt.Errorf("SteamCMD 安裝失敗: %w", err)
		}
	case "":
		// 免供應(如手動置檔或測試)。
	default:
		return "", ServerInstallResult{}, fmt.Errorf("agent: 未知供應類型 %q", np.Kind)
	}
	return javaPath, installed, nil
}

// maybeUpdateOnStart 於 Provision.UpdateOnStart 且 kind=steamcmd 時,在起行程前重跑 SteamCMD
// app_update(R6:「啟動前檢查更新」)。進度經 eventHub emit provision 事件;失敗回明確錯誤使
// Start 失敗(不以過時檔案啟動)。非 steamcmd 或未開啟時為 no-op。
func (b *NativeBackend) maybeUpdateOnStart(ctx context.Context, uuid string, spec protocol.InstanceSpec, workDir string) error {
	if spec.Native == nil {
		return nil
	}
	np := spec.Native.Provision
	if !np.UpdateOnStart || np.Kind != "steamcmd" {
		return nil
	}
	if err := b.prov.InstallSteamApp(ctx, np.SteamAppID, workDir, b.progressEmitter(uuid)); err != nil {
		return fmt.Errorf("啟動前更新失敗(SteamCMD app_update %s): %w", np.SteamAppID, err)
	}
	return nil
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
	// #17:檢查「未在執行」與登記占位須原子完成,否則並發雙 Start 會各自通過檢查、各起一個行程,
	// 後登記者覆寫 procHandle 致前者洩漏。以 b.mu 下的 starting 占位預留 slot:其一先占,另一即回錯。
	b.mu.Lock()
	if h, ok := b.procs[uuid]; ok && !h.isFinished() {
		b.mu.Unlock()
		return fmt.Errorf("agent: 實例 %s 已在執行", uuid)
	}
	if _, ok := b.starting[uuid]; ok {
		b.mu.Unlock()
		return fmt.Errorf("agent: 實例 %s 正在啟動中", uuid)
	}
	b.starting[uuid] = struct{}{}
	b.mu.Unlock()
	// 占位於本次 Start 返回前釋放:成功時 procHandle 已先登記進 b.procs(後續 Start 見其 running),
	// 失敗時清占位使可重試。
	defer func() {
		b.mu.Lock()
		delete(b.starting, uuid)
		b.mu.Unlock()
	}()

	meta, err := b.readNativeMeta(uuid)
	if err != nil {
		return fmt.Errorf("讀取 native 中繼失敗: %w", err)
	}
	// #6:供應未完成的最小中繼不可啟動(建立中或供應途中崩潰的殘留);待 Create 覆寫完整中繼後才可。
	if meta.Provisioning {
		return fmt.Errorf("agent: 實例 %s 供應未完成,尚不可啟動", uuid)
	}
	spec, err := b.readSpec(uuid)
	if err != nil {
		return fmt.Errorf("讀取 spec 快照失敗: %w", err)
	}
	root := b.instanceDataRoot(uuid)
	workDir := b.workingDir(root, meta.WorkingDir)

	// R6:啟動前更新(steamcmd app_update)。失敗即啟動失敗(帶明確錯誤),不起行程。
	if err := b.maybeUpdateOnStart(ctx, uuid, spec, workDir); err != nil {
		return err
	}

	argv, err := expandLaunchTokens(meta.Command, meta, workDir)
	if err != nil {
		return err
	}
	argv = wrapBatchLaunch(argv)
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
	// 供應的 JRE bin 前置於 PATH:Forge/NeoForge 的 run.bat 以裸 `java` 呼叫,須解析到本後端供應的
	// JRE(而非依賴系統安裝的 java,維持「零外裝」賣點)。JavaPath 為空(steamcmd 供應)時不變動。
	cmd.Env = append(os.Environ(), envSlice(spec.Env)...)
	if meta.JavaPath != "" {
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(meta.JavaPath)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	// 專屬暫存目錄:給每個 native 實例自己的 TEMP/TMP(實例根下 .gsm-tmp),而非共用使用者 %TEMP%。
	// 動機有二:(1) 隔離——伺服器暫存檔不與其他程序在全域 %TEMP% 相互汙染/碰撞,隨實例 purge 清除
	// (mirror 容器 /tmp 隔離);(2) 規避——JDK 於 Windows 以 AF_UNIX self-pipe 初始化 netty selector,
	// 某些使用者 %TEMP% 位置(如受安全軟體攔截或為 reparse point 的 AppData\Local 子樹)會使 AF_UNIX
	// connect 失敗("Invalid argument"),導致 Minecraft 等 netty 伺服器一啟動即崩潰(T13 E2E 實測
	// 2026-07-17)。改指向資料磁碟上的一般 NTFS 目錄即避開。.gsm-tmp 前綴使其自動排除於備份/用量統計。
	if tmp, terr := b.ensureInstanceTemp(root); terr == nil {
		cmd.Env = append(cmd.Env, "TEMP="+tmp, "TMP="+tmp, "TMPDIR="+tmp)
	}
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
	limits := resolveJobLimits(spec)
	h := &procHandle{
		uuid:      uuid,
		pid:       cmd.Process.Pid,
		startedAt: now,
		cmd:       cmd,
		log:       rl,
		done:      make(chan struct{}),
		limits:    limits,
	}
	// Job Object:恆建立以取得行程樹 accounting(R8),有上限時一併強制記憶體/CPU(R9)。
	// 建立或掛入失敗即降級為「僅監督不強制」——記可辨識事件、不阻斷啟動(design 風險節:收養/納管
	// 失敗降級)。Job 刻意不設 KILL_ON_JOB_CLOSE,故 agent 退出不連坐殺伺服器(R9/R7 共存)。
	//
	// #5 競態窗:cmd.Start() 與 AssignProcessToJobObject 之間存在極短空窗,期間頂層行程(如 cmd.exe
	// wrapper)若已派生子行程,該子行程不會被納入 Job(統計/上限漏計)。本任務採小刀方案:不做
	// CREATE_SUSPENDED(列後續);逃逸子行程仍是行程樹後裔,forceKill 的 taskkill /T /F 兜底仍會終止之
	// (僅資源計量受影響,非行程遺留),下方 detectJobEscape 於掛入後查一次以記觀測性警示。
	h.job = b.attachJob(h, limits)
	b.detectJobEscape(h)
	go captureStream(rl, "stdout", stdout)
	go captureStream(rl, "stderr", stderr)

	b.mu.Lock()
	b.procs[uuid] = h
	b.mu.Unlock()

	// #16:proc.json 寫失敗過去被吞——起行程已成功不回滾,但須可觀測(否則收養/存活比對將無中繼可用)。
	if err := b.writeProcMeta(uuid, procMeta{
		UUID:      uuid,
		PID:       h.pid,
		StartTime: now,
		Command:   argv,
		WorkDir:   workDir,
	}); err != nil {
		rl.write("gsm", "寫入 proc.json 失敗(收養/存活比對將缺中繼,行程仍在執行): "+err.Error())
	}

	go b.reap(h)

	b.hub.emit(RuntimeEvent{ID: id, Kind: RuntimeEventStart, TsUTC: now})
	return nil
}

// reap 等待行程結束,合成 die 事件(帶 exitCode),清 proc.json、收束日誌即時扇出。
// 崩潰與計畫停止在事件層不可區分(與 Docker die 一致),planned 與否由核心依 token 對帳。
//
// OOM(R9):Job 記憶體上限被觸發時,先合成 oom 事件(供上層告警標「超出記憶體上限」、區別一般
// crash),再合成 die 事件(仍驅動既有自動重啟狀態機 lifecycle.go:404,語意對齊 Docker 先 oom 後
// die)。oom 訊號由 Job completion port 於配額命中時設旗標(jobObject.memoryLimitHit)。
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
	b.finishReap(h, code)
}

// finishReap 是行程結束後的共用收束:設 finished、清 proc.json、收束日誌、解除 Stop 等待,再依 Job
// 記憶體配額判定合成 oom(如有)後合成 die。由 reap(本 agent 起的行程,cmd.Wait 取碼)與
// reapAdopted(收養行程,OS 等待取碼,T11)共用,語意一致。
func (b *NativeBackend) finishReap(h *procHandle, code int) {
	h.setFinished(code)
	_ = os.Remove(b.procMetaPath(h.uuid)) // 行程已歿,清執行中繼(收養不再視其存活)
	if h.log != nil {
		h.log.close()
	}
	close(h.done) // 先解除 Stop 等待(die/oom 事件於其後 emit,不阻擋 Stop 返回)

	// OOM 判定:記憶體配額命中訊號經 completion port 非同步投遞。乾淨退出(code==0)不可能是 OOM,
	// 直接讀旗標;異常退出則給一小段 settle 窗等待可能仍在途的通知(fast-path:已命中即刻返回)。
	oom := false
	if h.job != nil {
		if code == 0 {
			oom = h.job.memoryLimitHit()
		} else {
			oom = h.job.awaitMemoryLimit(nativeOOMSettleWindow)
		}
	}
	now := time.Now().UTC()
	if oom {
		b.hub.emit(RuntimeEvent{ID: nativeID(h.uuid), Kind: RuntimeEventOOM, TsUTC: now})
	}
	b.hub.emit(RuntimeEvent{ID: nativeID(h.uuid), Kind: RuntimeEventDie, TsUTC: now, ExitCode: &code})
	if h.job != nil {
		_ = h.job.close() // 釋放 Job/completion port 控制代碼(未設 KILL_ON_JOB_CLOSE,不影響已歿行程)
	}
}

// reapAdopted 監看一個收養行程的結束(T11)。收養無 exec.Cmd,以 OS 層等待行程結束並取退出碼
// (waitForAdoptedExit,平台專屬),再走與 reap 相同的收束路徑(合成 die/oom、驅動崩潰復原)。
func (b *NativeBackend) reapAdopted(h *procHandle) {
	code := waitForAdoptedExit(h.pid)
	b.finishReap(h, code)
}

// Stop 等 opts.Grace 優雅退出(呼叫端已先送 hooks.stop),逾時強殺整棵行程樹(R1/R7)。
// 強殺以 TerminateJobObject 終止整個 Job(行程樹一次收束,T7);無 Job(降級或非 Windows 存根)
// 時 fallback 至 taskkill /T(Windows)或行程 Kill。
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
	if err := b.forceKill(h); err != nil {
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
		// #10:forceKill 失敗仍無條件 <-h.done 會無限阻塞(行程未死→reap 永不 close done)。改為:
		// forceKill 錯誤記可觀測日誌,等待改 select ctx.Done() 逃生閥,ctx 取消即回錯,不永久卡住 Remove。
		if err := b.forceKill(h); err != nil && h.log != nil {
			h.log.write("gsm", "Remove 強殺行程樹失敗(將等待行程自然結束或 ctx 取消): "+err.Error())
		}
		select {
		case <-h.done:
		case <-ctx.Done():
			return ctx.Err()
		}
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

// PurgeInstanceData 以 uuid 直接清除實例宿主資料與備份(不依賴行程;B7:行程/容器已消失時的 purge
// 路徑,resolve 短路使 Remove(Purge) 的磁碟清理被跳過)。等同 Remove(Purge) 的清磁碟部分。
func (b *NativeBackend) PurgeInstanceData(ctx context.Context, uuid string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(b.instanceDataRoot(uuid)); err != nil {
		return fmt.Errorf("清除實例資料失敗: %w", err)
	}
	if err := os.RemoveAll(b.backupInstanceRoot(uuid)); err != nil {
		return fmt.Errorf("清除實例備份失敗: %w", err)
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

// Stats 回報行程樹 CPU/記憶體(經 Job Object accounting)與資料磁碟用量(R6/R8)。CPU 正規化語意
// 與 Docker 一致(占所有核心的百分比,docker_monitor.go:117):取兩次行程樹累計 CPU 時間,除以
// 取樣牆鐘間隔 × NumCPU。記憶體=列舉 Job 內所有行程 WorkingSetSize 加總(涵蓋 java/steamcmd 子
// 行程)。行程未執行或 Job 降級(無 h.job)時,CPU/記憶體留零值,由 GUI 呈現「不適用」。
func (b *NativeBackend) Stats(ctx context.Context, id protocol.RuntimeID) (protocol.ResourceStats, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.ResourceStats{}, err
	}
	uuid := nativeUUID(id)
	if !b.instanceExists(uuid) {
		return protocol.ResourceStats{}, ErrNotFound
	}
	stats := protocol.ResourceStats{TsUTC: time.Now().UTC()}

	b.mu.Lock()
	h := b.procs[uuid]
	b.mu.Unlock()
	if h != nil && !h.isFinished() && h.job != nil {
		if s1, err := h.job.stats(); err == nil {
			t0 := time.Now()
			time.Sleep(nativeStatSampleWindow)
			if s2, err := h.job.stats(); err == nil {
				stats.CPUPercent = calcNativeCPUPercent(s2.CPUTime-s1.CPUTime, time.Since(t0), runtime.NumCPU())
				stats.MemoryBytes = s2.WorkingSetSum
			}
		}
		if h.limits.MemoryBytes > 0 {
			stats.MemoryLimit = h.limits.MemoryBytes
		}
	}

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

// adoptExisting 於 agent 重啟(NativeBackend 建構)時掃 <DataRoot>/*/proc.json,收養仍在執行的
// 行程、恢復管理(native-backend R7/R13)。對每個帶 proc.json 的實例:
//   - PID 存活且 OS 行程建立時間與記錄的起行程時刻相符(容忍時鐘誤差,防 PID 重用)→ 收養:
//     OpenProcess/重掛 Job(新 Job＋AssignProcessToJobObject,含 Resources 上限重建;行程已在他 Job
//     內致 Assign 失敗 → 沿用 T7 慣例降級為僅監督)、接管日誌 tail(續開 rollingLog)、恢復 reap
//     監看(OS 等待→die/oom),Status 回 running。
//   - PID 已死或起行程時刻不符 → 清 proc.json、合成 die 走既有崩潰復原路徑。
//
// 事件兜底:die/adopt 於建構期 emit,此時 eventHub 尚無即時訂閱者(核心於 agent 重啟後才重訂閱),
// 事件僅留存於 hub 有界歷史;故收養結果對消費端的權威來源是 List/Status(收養活→running、死→exited),
// 由核心 Reconciler 經聚合 List 對帳兜底(R13)。resubscribe 帶游標者仍會於 resync 補得該 die。
func (b *NativeBackend) adoptExisting() {
	entries, err := os.ReadDir(b.dataRoot)
	if err != nil {
		return // 無資料根(全新 agent):無可收養。
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		uuid := e.Name()
		if !b.instanceExists(uuid) {
			continue // 非本後端所管(無 native.json):略過。
		}
		pm, perr := b.readProcMeta(uuid)
		if perr != nil {
			continue // 無 proc.json:實例未在執行(或已乾淨停機),無需收養。
		}
		b.adoptOne(uuid, pm)
	}
}

// adoptOne 依 proc.json 的 PID 存活與起行程時刻比對,決定收養(存活)或合成 die(已死/PID 重用)。
func (b *NativeBackend) adoptOne(uuid string, pm procMeta) {
	alive, createdAt := adoptedProcessStatus(pm.PID)
	if !alive || !startTimeMatches(pm.StartTime, createdAt) {
		b.adoptDead(uuid)
		return
	}
	b.adoptAlive(uuid, pm, createdAt)
}

// adoptDead 處理「proc.json 存在但行程已歿(或 PID 已被重用)」:清 proc.json、登記一個已結束的
// procHandle 使 Status/List 回 exited(供 Reconciler 修正 DB running→stopped,R13),並合成 die 走
// 既有崩潰復原路徑(退出碼未知,記 -1)。
func (b *NativeBackend) adoptDead(uuid string) {
	_ = os.Remove(b.procMetaPath(uuid))
	code := -1
	now := time.Now().UTC()
	h := &procHandle{uuid: uuid, done: make(chan struct{})}
	h.setFinished(code)
	close(h.done)
	b.mu.Lock()
	b.procs[uuid] = h
	b.mu.Unlock()
	b.hub.emit(RuntimeEvent{ID: nativeID(uuid), Kind: RuntimeEventDie, TsUTC: now, ExitCode: &code})
}

// adoptAlive 收養一個仍在執行的行程:重掛 Job(含 Resources 上限;失敗降級為僅監督)、續開滾動
// 日誌(供 Logs follow tail——注意收養已失去原 stdout/stderr 管線,無法再擷取新輸出,follow 僅
// 續 tail 既有內容,屬收養固有限制)、登記 running 的 procHandle 並起 OS 等待的 reap 監看。
func (b *NativeBackend) adoptAlive(uuid string, pm procMeta, createdAt time.Time) {
	spec, _ := b.readSpec(uuid)
	root := b.instanceDataRoot(uuid)
	rl, lerr := newRollingLog(root, defaultLogMaxBytes, defaultLogMaxFiles)
	if lerr != nil {
		rl = nil // 無法接管日誌不阻斷收養(history 仍可經 readLogHistory 提供)。
	}
	startedAt := createdAt
	if startedAt.IsZero() {
		startedAt = pm.StartTime // 取不到 OS 建立時間:退回記錄的起行程時刻。
	}
	h := &procHandle{
		uuid:      uuid,
		pid:       pm.PID,
		startedAt: startedAt,
		cmd:       nil, // 收養無 exec.Cmd(原起行程的 agent 已退出)。
		log:       rl,
		done:      make(chan struct{}),
		limits:    resolveJobLimits(spec),
	}
	// 重掛 Job(R9:收養後重新納入 Job 管理)。失敗(如行程已在他人 Job 內)降級為僅監督不強制,
	// 沿用 T7 attachJob 的降級慣例(記可辨識日誌、不阻斷收養)。
	h.job = b.attachJob(h, h.limits)
	b.mu.Lock()
	b.procs[uuid] = h
	b.mu.Unlock()
	go b.reapAdopted(h)
}

// nativeAdoptStartSkew 是收養時「proc.json 記錄的起行程牆鐘時刻」與「OS 行程建立時間」的容忍誤差。
// proc.json 的 StartTime 記為 agent 呼叫 cmd.Start() 當下的牆鐘,與 OS 實際建立時間僅相差毫秒級;
// 若 PID 已被別的行程重用,其建立時間會與記錄時刻明顯不同(遠超此容忍),據以判 PID 重用→不收養。
const nativeAdoptStartSkew = 30 * time.Second

// startTimeMatches 比對記錄的起行程時刻與 OS 行程建立時間是否在容忍誤差內(防 PID 重用)。取不到
// OS 建立時間(osCreated 為零值)時寬鬆採信「存活」即可(不因缺建立時間而拒收養活行程)。
func startTimeMatches(recorded, osCreated time.Time) bool {
	if osCreated.IsZero() {
		return true
	}
	d := recorded.Sub(osCreated)
	if d < 0 {
		d = -d
	}
	return d <= nativeAdoptStartSkew
}

// ---- 啟動命令 token 展開 ----

// expandLaunchTokens 展開 argv 模板的 token(子字串替換):{java} {server_jar} {start_script}
// {memory_mb} {instance_dir} 與 {port:<name>}。未知 {port:name} 回錯。
//
// 啟動來源語意(R7):Forge/NeoForge 以官方 installer 產出啟動腳本(StartScript,如 run.bat)而非
// 單一 server jar。LaunchSpec 是範本級(非變體級,見 protocol.LaunchSpec),故 forge/neoforge 變體
// 與 vanilla/paper/fabric 共用同一條 jar 型 command。當實例的啟動來源實為腳本(StartScript 非空、
// 無 server jar)時,rewriteForStartScript 就地把 jar 型命令改寫為等價的腳本啟動(丟棄 {java}
// -Xmx -jar {server_jar},保留 {server_jar} 之後的伺服器參數如 nogui,前置 {start_script});記憶體
// 上限改由 injectMemoryArgs 注入 user_jvm_args.txt(見 Create)。此為 T13 選定的小刀解法(a):不擴充
// LaunchSpec 至變體級,改由 backend 依實際供應產物在啟動期橋接。
func expandLaunchTokens(argv []string, m nativeMeta, instanceDir string) ([]string, error) {
	argv = rewriteForStartScript(argv, m)
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
		s = strings.ReplaceAll(s, "{start_script}", m.StartScript)
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

// rewriteForStartScript 在「啟動來源實為腳本(StartScript 非空且無 server jar)」時,把 jar 型
// 啟動命令改寫為等價的腳本啟動:丟棄 {server_jar} token 及其之前的所有元素(即 {java} / -Xmx /
// -jar 等 jar 專屬前綴),以 {start_script} 取代,並保留 {server_jar} 之後的伺服器參數(如 nogui)。
// 記憶體上限不再經 -Xmx 傳遞(改注入 user_jvm_args.txt,見 Create.injectMemoryArgs)。
//
// 不符改寫條件(有 server jar,或命令本就以 {start_script} 表達)時原樣返回。命令未引用 {server_jar}
// 的腳本型範本(直接寫 {start_script})不受影響。
func rewriteForStartScript(argv []string, m nativeMeta) []string {
	if m.StartScript == "" || m.ServerJar != "" {
		return argv
	}
	jarIdx := -1
	for i, a := range argv {
		if strings.Contains(a, "{server_jar}") {
			jarIdx = i
			break
		}
	}
	if jarIdx < 0 {
		return argv // 已是 {start_script} 型(或無 jar token):不改寫。
	}
	out := make([]string, 0, len(argv)-jarIdx+1)
	out = append(out, "{start_script}")
	out = append(out, argv[jarIdx+1:]...) // 保留 server jar 之後的參數(nogui 等)
	return out
}

// wrapBatchLaunch 於 argv[0] 為 .bat/.cmd(Forge/NeoForge 的 run.bat)時前置 cmd /c 以透過命令
// 直譯器執行(Windows;os/exec 不直接執行批次檔)。其他情況(jar 型 java 命令、非 Windows)原樣返回。
func wrapBatchLaunch(argv []string) []string {
	if len(argv) == 0 || runtime.GOOS != "windows" {
		return argv
	}
	switch strings.ToLower(filepath.Ext(argv[0])) {
	case ".bat", ".cmd":
		return append([]string{"cmd", "/c"}, argv...)
	}
	return argv
}

// injectMemoryArgs 把記憶體上限以 `-Xmx<mem>M` 追加寫入 Forge/NeoForge 的 user_jvm_args.txt
// (installer 產出的 JVM 參數檔;run.bat 以 @user_jvm_args.txt 讀入)。argsFile 不存在時視為無此
// loader 結構、跳過(不視為錯誤)。memMB<=0 時 no-op。冪等性非必要:每實例僅 Create 一次呼叫。
func injectMemoryArgs(argsFile string, memMB int) error {
	if memMB <= 0 {
		return nil
	}
	if _, err := os.Stat(argsFile); err != nil {
		return nil // 無 args 檔(非現代 forge 結構或測試假件未落檔):跳過。
	}
	f, err := os.OpenFile(argsFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("開啟 %s 注入記憶體參數失敗: %w", filepath.Base(argsFile), err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\n-Xmx%dM\n", memMB); err != nil {
		return fmt.Errorf("寫入記憶體參數至 %s 失敗: %w", filepath.Base(argsFile), err)
	}
	return nil
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

// ---- Job Objects:資源上限、行程樹統計與強殺(T7)----

// nativeStatSampleWindow 是 Stats 取兩次 Job accounting 之間的取樣窗;CPU% 為此窗內的行程樹
// CPU 時間對牆鐘 × NumCPU 的比值(語意同 Docker 單次 stats 的 pre/cur 差分)。
const nativeStatSampleWindow = 200 * time.Millisecond

// nativeOOMSettleWindow 是行程異常退出後,reap 等待 Job completion port 投遞記憶體配額通知的上限。
// 命中時 fast-path 立即返回;僅在異常退出且尚未命中時才走滿此窗(不影響 Stop 返回,見 reap)。
const nativeOOMSettleWindow = 250 * time.Millisecond

// newJob 是 newJobObject 的間接層,供測試注入建立失敗以驗降級路徑(平時即 newJobObject 本身)。
var newJob = newJobObject

// jobLimits 是建立 Job Object 時的資源上限。零值欄位=該維度不限額(Job 仍建立,僅為 accounting)。
type jobLimits struct {
	MemoryBytes uint64 // 記憶體上限(bytes);0=不限
	CPUPercent  int    // CPU 上限:占所有核心的百分比(1-100);0=不限
}

// jobStats 是一次 Job Object 行程樹統計快照。
type jobStats struct {
	CPUTime       time.Duration // 行程樹累計 CPU 時間(TotalUser+TotalKernel)
	WorkingSetSum uint64        // Job 內所有行程 WorkingSetSize 加總(bytes)
}

// resolveJobLimits 由實例資源設定(Resources,與 Docker 同來源)導出 Job Object 上限。無設定=全零
// (不限額)。
func resolveJobLimits(spec protocol.InstanceSpec) jobLimits {
	var lim jobLimits
	if r := spec.Resources; r != nil {
		if r.MemoryMB > 0 {
			lim.MemoryBytes = uint64(r.MemoryMB) * 1024 * 1024
		}
		if r.CPUPercent > 0 {
			lim.CPUPercent = r.CPUPercent
		}
	}
	return lim
}

// calcNativeCPUPercent 由兩次行程樹 CPU 累計時間差(cpuDelta)、取樣牆鐘間隔(wallDelta)與核心數
// 算正規化 CPU%——占所有核心的百分比,語意同 docker_monitor.go calcCPUPercent(100=用滿全部核心):
// 全機容量 = wallDelta × NumCPU(CPU·秒),cpuDelta 為實耗 CPU·秒,兩者比值 ×100 即得。
func calcNativeCPUPercent(cpuDelta, wallDelta time.Duration, numCPU int) float64 {
	if wallDelta <= 0 || numCPU <= 0 || cpuDelta < 0 {
		return 0
	}
	pct := float64(cpuDelta) / (float64(wallDelta) * float64(numCPU)) * 100.0
	if pct < 0 {
		return 0
	}
	return pct
}

// attachJob 建立 Job Object 並掛入行程 pid;成功回 *jobObject,失敗降級回 nil 並向實例日誌記一則
// 可辨識訊息(design 風險節:Job 納管失敗降級為僅監督不強制上限,不阻斷啟動)。恆嘗試建立——即使
// 不限額,亦用於取行程樹 accounting(R8)。收養重掛(T11)共用 attachJob。
func (b *NativeBackend) attachJob(h *procHandle, limits jobLimits) *jobObject {
	job, err := newJob(limits)
	if err != nil {
		h.log.write("gsm", "資源上限套用失敗,降級為僅監督(不強制上限、不計行程樹統計): "+err.Error())
		return nil
	}
	if job == nil {
		return nil // 平台不支援 Job(非 Windows 存根 newJobObject 回 nil):走無 Job 路徑
	}
	if err := job.assign(h.pid); err != nil {
		_ = job.close()
		h.log.write("gsm", "行程掛入 Job 失敗(可能已在其他 Job 內),降級為僅監督: "+err.Error())
		return nil
	}
	return job
}

// detectJobEscape 於 Job 掛入後查一次:頂層行程在掛入前的競態窗內若已派生子行程,該子行程未被納入
// Job(見 Start 的 #5 競態窗註)。偵測到即向實例日誌記可辨識警示(觀測性,不阻斷——逃逸子行程仍受
// forceKill 的 taskkill /T 兜底終止,僅資源計量漏計)。h.job 為 nil(降級/非 Windows)或查詢失敗時 no-op。
func (b *NativeBackend) detectJobEscape(h *procHandle) {
	if h.job == nil {
		return
	}
	escaped, err := h.job.escapedChildren(h.pid)
	if err != nil || len(escaped) == 0 {
		return
	}
	if h.log != nil {
		h.log.write("gsm", fmt.Sprintf("偵測到 %d 個子行程於 Job 掛入前已派生、未納入資源管理(統計/上限可能漏計此空窗期行程): %v", len(escaped), escaped))
	}
}

// forceKill 強殺實例行程樹:有 Job 時以 TerminateJobObject 一次收束整個 Job;否則 fallback 至
// taskkill /T(Windows)或行程 Kill。
func (b *NativeBackend) forceKill(h *procHandle) error {
	if h.job != nil {
		if err := h.job.terminate(); err == nil {
			return nil
		}
		// Terminate 失敗(罕見):退回 PID 層強殺,避免行程遺留。
	}
	return killProcessTree(h.pid)
}

// killProcessTree 強制終止 pid 及其子行程樹(Job 不可用時的 fallback)。Windows 用 taskkill /T /F;
// 其他平台以行程 Kill。
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

// nativeTempSubdir 是 native 實例的專屬暫存子目錄名(實例根下)。.gsm- 前綴使 tarDir 與
// dataDirUsage 自動整棵排除(不入備份、不計資料用量)。
const nativeTempSubdir = ".gsm-tmp"

// ensureInstanceTemp 於實例根下建專屬暫存目錄並回其絕對路徑(供 Start 設子行程 TEMP/TMP)。
func (b *NativeBackend) ensureInstanceTemp(root string) (string, error) {
	dir := filepath.Join(root, nativeTempSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// workingDir 解析啟動工作目錄:相對 WorkingDir 落在實例根下;空則為實例根。
func (b *NativeBackend) workingDir(root, rel string) string {
	if rel == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// modsInstallDir 解析範本宣告的 native 模組落位目錄(NativeSpecPayload.ModsDir,相對實例根)
// 為絕對路徑;未宣告時回明確錯誤,不臆測預設值(R11——範本須以 [native.mods] mods_dir 宣告
// 才能安裝模組包)。
func (b *NativeBackend) modsInstallDir(root string, spec protocol.InstanceSpec) (string, error) {
	if spec.Native == nil || spec.Native.ModsDir == "" {
		return "", fmt.Errorf("agent: 範本未宣告 native mods 落位目錄(mods_dir),無法安裝模組包")
	}
	return filepath.Join(root, filepath.FromSlash(spec.Native.ModsDir)), nil
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
		pp.InstanceUUID = uuid // 供 GUI 以 provision:<uuid> 分派至該伺服器主控台
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

func (b *NativeBackend) readProcMeta(uuid string) (procMeta, error) {
	var m procMeta
	err := readJSONFile(b.procMetaPath(uuid), &m)
	return m, err
}

func (b *NativeBackend) writeSpec(spec protocol.InstanceSpec) error {
	p := filepath.Join(b.instanceDataRoot(spec.UUID), instanceSpecFile)
	if err := writeJSONFile(p, spec); err != nil {
		return err
	}
	// 與 DockerBackend.writeInstanceSpec 同理:spec 的 Env 含明文機密,權限收斂為 0600
	// (writeJSONFile 為各中繼檔共用的 0644;Windows 上 mode 語意有限,失敗不視為錯誤)。
	_ = os.Chmod(p, 0o600)
	return nil
}

func (b *NativeBackend) readSpec(uuid string) (protocol.InstanceSpec, error) {
	var spec protocol.InstanceSpec
	err := readJSONFile(filepath.Join(b.instanceDataRoot(uuid), instanceSpecFile), &spec)
	return spec, err
}

// resolveMemoryMB 解析 {memory_mb} token 用的記憶體(MB):優先取實例資源上限
// Resources.MemoryMB(與 Docker 同來源,T9/T12 填入);無值時退回慣用 env 鍵(MEMORY_MB /
// MEMORY);皆缺時採 2048 MB 預設。
func resolveMemoryMB(spec protocol.InstanceSpec) int {
	if spec.Resources != nil && spec.Resources.MemoryMB > 0 {
		return spec.Resources.MemoryMB
	}
	for _, k := range []string{"MEMORY_MB", "MEMORY"} {
		if v, ok := spec.Env[k]; ok {
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
