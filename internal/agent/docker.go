package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"servermonitor/internal/protocol"
)

// gsm.* 標籤(R2:對帳與擁有權標記)。managed-by 固定值標記本工具擁有的容器。
const (
	labelManagedBy = "gsm.managed-by"
	labelUUID      = "gsm.uuid"
	labelNode      = "gsm.node"
	labelSchema    = "gsm.schema"

	managedByValue = "servermonitor"

	// instanceSpecFile 是實例資料根下的 spec 快照檔(在 bind mount 之外,不入容器/備份)。
	instanceSpecFile = "instance.json"

	// mountsSubdir 是實例資料根下承載具名 mount(R11 手動模組包檔)的保留子目錄:
	// 每個 spec.Mounts 條目對映 <dataRoot>/<uuid>/mounts/<name>/。此 namespace 排除於備份
	// 打包/還原範圍外(見 tarInstanceData);data_dir 不得使用容器路徑 "/mounts"(保留名)。
	mountsSubdir = "mounts"
)

// dockerAPI 是本後端用到的 Docker client 方法子集。以介面持有(而非具體 *client.Client)
// 便於隔離與測試;*client.Client 滿足之。
type dockerAPI interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerStats(ctx context.Context, containerID string, stream bool) (container.StatsResponseReader, error)
	ContainerExecCreate(ctx context.Context, containerID string, options container.ExecOptions) (types.IDResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)
	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	Ping(ctx context.Context) (types.Ping, error)
	Close() error
}

// DockerOptions 是 DockerBackend 的建構選項。
//
// DataRoot 為 agent 擁有的實例資料根;每實例一子目錄 <DataRoot>/<uuid>/,其下依範本
// data_dirs 各一 bind mount 子目錄(見備份 spike)。BackupRoot 為 agent 擁有的備份根
// (R9,opaque BackupID);Node/Schema 為 gsm.node/gsm.schema 標籤值(Schema 空則 "1")。
type DockerOptions struct {
	DataRoot   string
	BackupRoot string
	Node       string
	Schema     string
}

// DockerBackend 是 RuntimeBackend 的官方 Docker SDK 實作(R4)。容器生命週期、stats/logs、
// 執行事件、封存/還原皆經此;core 不直碰 Docker SDK。事件經內部 eventHub 轉為與 MockBackend
// 等價的可對帳串流(cursor/reconnect/resync)。併發安全。
type DockerBackend struct {
	cli        dockerAPI
	dataRoot   string
	backupRoot string
	node       string
	schema     string

	hub    *eventHub
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

var _ RuntimeBackend = (*DockerBackend)(nil)

// NewDockerBackend 以環境(DOCKER_HOST 等)建立連線,即時 ping daemon 確認可連線後才啟動事件監看。
// client.NewClientWithOpts 本身是 lazy 的(僅解析設定,不接觸 daemon),若省略 ping,daemon 未啟動
// 時仍會建構成功,導致 DockerAvailable() 誤判為可用,直到實際操作(如拉映像)才爆冗長底層錯誤。
func NewDockerBackend(opts DockerOptions) (*DockerBackend, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("建立 docker client 失敗: %w", err)
	}
	if err := pingDaemon(cli); err != nil {
		_ = cli.Close()
		return nil, err
	}
	return newDockerBackendWithClient(cli, opts)
}

// pingDaemon 以 3 秒逾時 ping daemon 確認可連線,失敗時回傳友善錯誤訊息。獨立成函式(接受
// dockerAPI 而非具體 *client.Client)供單元測試以假 client 注入,免真連 daemon。
func pingDaemon(cli dockerAPI) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		return fmt.Errorf("無法連線 Docker daemon(Docker Desktop 是否未啟動?): %w", err)
	}
	return nil
}

// newDockerBackendWithClient 以既有 client 建立後端(供整合測試注入)。
func newDockerBackendWithClient(cli dockerAPI, opts DockerOptions) (*DockerBackend, error) {
	if opts.DataRoot == "" {
		return nil, fmt.Errorf("agent: DockerOptions.DataRoot 不可為空")
	}
	if opts.BackupRoot == "" {
		return nil, fmt.Errorf("agent: DockerOptions.BackupRoot 不可為空")
	}
	if err := os.MkdirAll(opts.DataRoot, 0o755); err != nil {
		return nil, fmt.Errorf("建立資料根失敗: %w", err)
	}
	if err := os.MkdirAll(opts.BackupRoot, 0o755); err != nil {
		return nil, fmt.Errorf("建立備份根失敗: %w", err)
	}
	schema := opts.Schema
	if schema == "" {
		schema = "1"
	}
	b := &DockerBackend{
		cli:        cli,
		dataRoot:   opts.DataRoot,
		backupRoot: opts.BackupRoot,
		node:       opts.Node,
		schema:     schema,
		hub:        newEventHub(0),
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.wg.Add(1)
	go b.eventPump()
	return b, nil
}

// Create 依規格建容器:掛 host bind mount、打 gsm.* 標籤、綁埠;不自動啟動。
func (b *DockerBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	if spec.UUID == "" {
		return "", fmt.Errorf("agent: InstanceSpec.UUID 不可為空")
	}
	if spec.Image == "" {
		return "", fmt.Errorf("agent: InstanceSpec.Image 不可為空")
	}

	root := b.instanceDataRoot(spec.UUID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("建立實例資料根失敗: %w", err)
	}

	mounts := make([]mount.Mount, 0, len(spec.DataDirs)+len(spec.Mounts))
	for _, d := range spec.DataDirs {
		host := b.hostDirForContainerPath(spec.UUID, d)
		if err := os.MkdirAll(host, 0o755); err != nil {
			return "", fmt.Errorf("建立資料目錄 %s 失敗: %w", d, err)
		}
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: host, Target: d})
	}
	// 具名 mount(R11 手動模組包檔):於 mounts/<name>/ 建宿主目錄並 bind mount;內容排除於備份範圍。
	// MkdirAll 冪等,故 Restore 重建同 spec 時不會清掉先前上傳的檔案。
	for _, mnt := range spec.Mounts {
		host := b.hostDirForMount(spec.UUID, mnt.Name)
		if err := os.MkdirAll(host, 0o755); err != nil {
			return "", fmt.Errorf("建立掛載目錄 %s 失敗: %w", mnt.Name, err)
		}
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: host, Target: mnt.ContainerPath})
	}

	exposed, portMap := buildPorts(spec.Ports)

	cfg := &container.Config{
		Image:        spec.Image,
		Env:          envSlice(spec.Env),
		Labels:       b.labelsFor(spec),
		ExposedPorts: exposed,
	}
	hostCfg := &container.HostConfig{
		Mounts:        mounts,
		PortBindings:  portMap,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled}, // restart 由 core 編排
	}

	if err := b.ensureImage(ctx, spec.Image); err != nil {
		return "", err
	}
	// 持久化 spec 快照(在 bind mount 之外),供 Archive/Restore 自包含重建。
	if err := b.writeInstanceSpec(spec); err != nil {
		return "", err
	}

	name := "gsm-" + spec.UUID + "-" + shortRand()
	resp, err := b.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return "", fmt.Errorf("建立容器失敗: %w", err)
	}
	return protocol.RuntimeID(resp.ID), nil
}

// Start 啟動已建立的容器。
func (b *DockerBackend) Start(ctx context.Context, id protocol.RuntimeID) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return mapDockerErr(b.cli.ContainerStart(ctx, string(id), container.StartOptions{}))
}

// Stop 優雅停機:opts.Grace 對映 Docker StopOptions.Timeout(SIGTERM 後寬限,逾時 SIGKILL)。
// 呼叫端已先送範本 hooks.stop;此處為外層安全網。
func (b *DockerBackend) Stop(ctx context.Context, id protocol.RuntimeID, opts StopOpts) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	sopts := container.StopOptions{}
	if opts.Grace > 0 {
		secs := int(opts.Grace.Round(time.Second).Seconds())
		if secs < 1 {
			secs = 1
		}
		sopts.Timeout = &secs
	}
	return mapDockerErr(b.cli.ContainerStop(ctx, string(id), sopts))
}

// Status 回傳容器層即時狀態。
func (b *DockerBackend) Status(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeStatus, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.RuntimeStatus{}, err
	}
	j, err := b.cli.ContainerInspect(ctx, string(id))
	if err != nil {
		return protocol.RuntimeStatus{}, mapDockerErr(err)
	}
	return statusFromInspect(j), nil
}

// List 列舉本後端所有 runtime(依 gsm.managed-by 標籤過濾;R13 對帳)。
func (b *DockerBackend) List(ctx context.Context) ([]protocol.RuntimeRef, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	f := filters.NewArgs(filters.Arg("label", labelManagedBy+"="+managedByValue))
	cs, err := b.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, mapDockerErr(err)
	}
	refs := make([]protocol.RuntimeRef, 0, len(cs))
	for _, c := range cs {
		refs = append(refs, protocol.RuntimeRef{
			ID:     protocol.RuntimeID(c.ID),
			State:  mapState(c.State),
			Labels: cloneStringMap(c.Labels),
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

// Inspect 回傳單一 runtime 詳細資訊(R13)。
func (b *DockerBackend) Inspect(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.RuntimeInfo{}, err
	}
	j, err := b.cli.ContainerInspect(ctx, string(id))
	if err != nil {
		return protocol.RuntimeInfo{}, mapDockerErr(err)
	}
	info := protocol.RuntimeInfo{
		ID:        protocol.RuntimeID(j.ID),
		Status:    statusFromInspect(j),
		CreatedAt: parseDockerTimeVal(j.Created),
		Ports:     portsFromInspect(j),
	}
	if j.State != nil {
		info.State = mapState(j.State.Status)
	}
	if j.Config != nil {
		info.Image = j.Config.Image
		info.Labels = cloneStringMap(j.Config.Labels)
	}
	return info, nil
}

// Remove 移除容器;預設保留 data/backups,Purge 才刪 host 資料與備份。
func (b *DockerBackend) Remove(ctx context.Context, id protocol.RuntimeID, opts RemoveOpts) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	var uuid string
	if opts.Purge {
		if j, err := b.cli.ContainerInspect(ctx, string(id)); err == nil && j.Config != nil {
			uuid = j.Config.Labels[labelUUID]
		}
	}
	if err := b.cli.ContainerRemove(ctx, string(id), container.RemoveOptions{Force: true}); err != nil {
		return mapDockerErr(err)
	}
	if opts.Purge && uuid != "" {
		_ = os.RemoveAll(b.instanceDataRoot(uuid))
		_ = os.RemoveAll(b.backupInstanceRoot(uuid))
	}
	return nil
}

// Close 停止事件監看並關閉 client。冪等。
func (b *DockerBackend) Close() error {
	b.closeOnce.Do(func() {
		b.cancel()
		b.hub.close()
		b.wg.Wait()
		b.closeErr = b.cli.Close()
	})
	return b.closeErr
}

// ---- 助手 ----

func (b *DockerBackend) instanceDataRoot(uuid string) string {
	return filepath.Join(b.dataRoot, uuid)
}

func (b *DockerBackend) backupInstanceRoot(uuid string) string {
	return filepath.Join(b.backupRoot, uuid)
}

// hostDirForContainerPath 將容器內 data_dir 路徑對映到實例資料根下的 host 子目錄。
func (b *DockerBackend) hostDirForContainerPath(uuid, containerPath string) string {
	return filepath.Join(b.instanceDataRoot(uuid), sanitizeDataDir(containerPath))
}

// hostDirForMount 將具名 mount 對映到實例資料根下的 mounts/<name>/ host 子目錄(備份範圍外)。
func (b *DockerBackend) hostDirForMount(uuid, name string) string {
	return filepath.Join(b.instanceDataRoot(uuid), mountsSubdir, sanitizeMountName(name))
}

// WriteMountFile 把上傳的檔案位元組寫入某實例的具名 mount 宿主目錄(R11 手動模組包檔傳輸)。
// 以實例 spec 快照確認 mount 已宣告(否則 ErrNotFound);filename 防禦性再驗(ErrInvalidFilename);
// 寫入採覆寫(O_TRUNC),故重試天然冪等。
func (b *DockerBackend) WriteMountFile(ctx context.Context, instanceUUID, mountName, filename string, r io.Reader) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := validateMountFilename(filename); err != nil {
		return err
	}
	spec, err := b.readInstanceSpec(instanceUUID)
	if err != nil || !specHasMount(spec, mountName) {
		return ErrNotFound
	}
	dir := b.hostDirForMount(instanceUUID, mountName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("建立掛載目錄失敗: %w", err)
	}
	dst := filepath.Join(dir, filename)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("開啟掛載檔失敗: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("寫入掛載檔失敗: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("關閉掛載檔失敗: %w", err)
	}
	return nil
}

// labelsFor 疊加權威 gsm.* 標籤到 spec.Labels 之上。
func (b *DockerBackend) labelsFor(spec protocol.InstanceSpec) map[string]string {
	labels := cloneStringMap(spec.Labels)
	if labels == nil {
		labels = make(map[string]string, 4)
	}
	node := spec.Node
	if node == "" {
		node = b.node
	}
	labels[labelManagedBy] = managedByValue
	labels[labelUUID] = spec.UUID
	labels[labelNode] = node
	labels[labelSchema] = b.schema
	return labels
}

// writeInstanceSpec 把 spec 快照寫到實例資料根(bind mount 之外)。
func (b *DockerBackend) writeInstanceSpec(spec protocol.InstanceSpec) error {
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 spec 失敗: %w", err)
	}
	p := filepath.Join(b.instanceDataRoot(spec.UUID), instanceSpecFile)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return fmt.Errorf("寫入 spec 快照失敗: %w", err)
	}
	return nil
}

// readInstanceSpec 讀回實例資料根的 spec 快照。
func (b *DockerBackend) readInstanceSpec(uuid string) (protocol.InstanceSpec, error) {
	var spec protocol.InstanceSpec
	p := filepath.Join(b.instanceDataRoot(uuid), instanceSpecFile)
	data, err := os.ReadFile(p)
	if err != nil {
		return spec, fmt.Errorf("讀取 spec 快照失敗: %w", err)
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("解析 spec 快照失敗: %w", err)
	}
	return spec, nil
}

// ensureImage 若本機無此映像則拉取。
func (b *DockerBackend) ensureImage(ctx context.Context, ref string) error {
	if _, _, err := b.cli.ImageInspectWithRaw(ctx, ref); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return friendlyDockerErr(err, fmt.Sprintf("檢查映像 %s 失敗", ref))
	}
	rc, err := b.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return friendlyDockerErr(err, fmt.Sprintf("拉取映像 %s 失敗", ref))
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil { // 須排空至結束才算拉完
		return friendlyDockerErr(err, fmt.Sprintf("拉取映像 %s 串流失敗", ref))
	}
	return nil
}

// friendlyDockerErr 判定 err 是否為連線類錯誤(daemon 未啟動/斷線),是則轉為使用者友善訊息;
// 否則以 fallback 包裝原始錯誤(維持既有訊息格式)。
func friendlyDockerErr(err error, fallback string) error {
	if client.IsErrConnectionFailed(err) {
		return fmt.Errorf("Docker 未啟動或連線中斷,請啟動 Docker Desktop 後重試: %w", err)
	}
	return fmt.Errorf("%s: %w", fallback, err)
}

// sanitizeDataDir 把容器路徑轉為安全的單層 host 目錄名。
func sanitizeDataDir(p string) string {
	s := strings.Trim(p, "/\\")
	s = strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(s)
	if s == "" {
		return "root"
	}
	return s
}

// buildPorts 由 protocol.PortBinding 組出 Docker ExposedPorts 與 PortBindings。
// HostPort==0 表示動態分配(綁定不帶 host port,由 Docker 指派)。
func buildPorts(ports []protocol.PortBinding) (nat.PortSet, nat.PortMap) {
	exposed := nat.PortSet{}
	portMap := nat.PortMap{}
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		np, err := nat.NewPort(proto, strconv.Itoa(p.Container))
		if err != nil {
			continue
		}
		exposed[np] = struct{}{}
		binding := nat.PortBinding{HostIP: p.BindIP}
		if p.HostPort > 0 {
			binding.HostPort = strconv.Itoa(p.HostPort)
		}
		portMap[np] = append(portMap[np], binding)
	}
	return exposed, portMap
}

// envSlice 把 env map 轉為排序後的 "K=V" 切片(輸出穩定)。
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// mapState 將 Docker 容器狀態字串映射為 protocol.RuntimeState。
func mapState(s string) protocol.RuntimeState {
	switch s {
	case "created":
		return protocol.RuntimeStateCreated
	case "running", "restarting":
		return protocol.RuntimeStateRunning
	case "paused":
		return protocol.RuntimeStatePaused
	case "exited", "removing":
		return protocol.RuntimeStateExited
	case "dead":
		return protocol.RuntimeStateDead
	default:
		return protocol.RuntimeStateUnknown
	}
}

// statusFromInspect 由 inspect 結果組 RuntimeStatus。
func statusFromInspect(j types.ContainerJSON) protocol.RuntimeStatus {
	rs := protocol.RuntimeStatus{ID: protocol.RuntimeID(j.ID), Health: "none"}
	if j.State == nil {
		return rs
	}
	st := j.State
	rs.State = mapState(st.Status)
	rs.Running = st.Running
	if st.Status == "exited" || st.Status == "dead" {
		code := st.ExitCode
		rs.ExitCode = &code
	}
	if st.Health != nil && st.Health.Status != "" {
		rs.Health = st.Health.Status
	}
	rs.StartedAt = parseDockerTime(st.StartedAt)
	rs.FinishedAt = parseDockerTime(st.FinishedAt)
	return rs
}

// portsFromInspect 由 inspect 的實際埠綁定重建 PortBinding(名稱不可還原,留空)。
func portsFromInspect(j types.ContainerJSON) []protocol.PortBinding {
	if j.NetworkSettings == nil {
		return nil
	}
	var out []protocol.PortBinding
	for np, binds := range j.NetworkSettings.Ports {
		container := np.Int()
		proto := np.Proto()
		for _, bnd := range binds {
			hp, _ := strconv.Atoi(bnd.HostPort)
			out = append(out, protocol.PortBinding{
				Container: container,
				HostPort:  hp,
				BindIP:    bnd.HostIP,
				Protocol:  proto,
			})
		}
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Container != out[k].Container {
			return out[i].Container < out[k].Container
		}
		return out[i].HostPort < out[k].HostPort
	})
	return out
}

// parseDockerTime 解析 Docker RFC3339Nano 時間;零值時間回 nil。
func parseDockerTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.IsZero() || t.Year() <= 1 {
		return nil
	}
	u := t.UTC()
	return &u
}

// parseDockerTimeVal 同上但回值(零值時間回 time 零值)。
func parseDockerTimeVal(s string) time.Time {
	if t := parseDockerTime(s); t != nil {
		return *t
	}
	return time.Time{}
}

// mapDockerErr 將 Docker not-found 映射為 agent.ErrNotFound,其餘原樣回傳。
func mapDockerErr(err error) error {
	if err == nil {
		return nil
	}
	if errdefs.IsNotFound(err) {
		return ErrNotFound
	}
	// daemon 中途斷線(啟動後關閉 Docker Desktop)時,所有操作路徑統一回友善訊息,
	// 不讓 named-pipe 原始錯誤直達 GUI。
	if client.IsErrConnectionFailed(err) {
		return fmt.Errorf("Docker 未啟動或連線中斷,請啟動 Docker Desktop 後重試: %w", err)
	}
	return err
}

// shortRand 回傳 8 位十六進位隨機字串(容器命名去衝突用)。
func shortRand() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}
