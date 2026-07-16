package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"servermonitor/internal/protocol"
)

// dispatchBackend 是把 native 與 docker 兩個 RuntimeBackend 併入單一節點代理後端的組合實作
// (native-backend R2/R13)。Create 依 InstanceSpec.Runtime 路由;其餘生命週期方法依 RuntimeID
// 前綴路由(native RuntimeID 帶 "native:" 前綴、docker 為裸容器 ID)。List/Events 聚合兩子後端,
// 且可個別軟降級——docker daemon 不可用時 native 對帳與事件不受影響,節點不因 Docker down 離線。
//
// 子後端所有權邊界:
//   - native 於 Windows 恆存在、生命週期與 dispatchBackend 相同(不熱替換)。
//   - docker 可能為 nil(daemon 建構失敗),且可經 SwapDocker 於 RetryDocker 熱替換;dockerMu 保護。
//
// 事件聚合:dispatchBackend 自持 eventHub,對每個子後端各起一條 pump goroutine 訂閱其 Events 並
// 轉 emit 至本 hub。子後端已各自對其 RuntimeID 加妥前綴(native)或裸穿(docker),故 pump 不改寫
// ID——核心遂見單一 cursor 空間、reconnect/resync 語意不變。SwapDocker 取消舊 docker pump、對新
// docker 起新 pump,使 RetryDocker 重建的 docker 後端事件能接上。併發安全。
type dispatchBackend struct {
	native RuntimeBackend // Windows 恆有;Linux 為 nil
	hub    *eventHub      // 聚合事件(復用 event_hub.go)

	rootCtx    context.Context    // 所有 pump 的父 context(Close 時 cancel)
	rootCancel context.CancelFunc // 取消全部 pump
	wg         sync.WaitGroup     // 等待 pump goroutine 收束

	dockerMu     sync.RWMutex       // 保護 docker 與 dockerCancel 的替換
	docker       RuntimeBackend     // 可能 nil(daemon 不可用)
	dockerCancel context.CancelFunc // 取消目前 docker pump(SwapDocker/Close 用)

	closeOnce sync.Once
	closed    bool // 於 dockerMu 下讀寫:closed 後 SwapDocker 不再起新 pump
}

var _ RuntimeBackend = (*dispatchBackend)(nil)

// BackendOptions 是頂層後端工廠(app.BackendFactory)的泛化注入參數(native-backend R2)。取代
// 舊有只夠 docker 用的 DockerOptions:InstancesRoot/BackupRoot 由 docker 與 native 共用(備份格式
// 互通,R10),CacheRoot 為 native 供應器的 JRE/SteamCMD 共用快取根(docker 不需)。
type BackendOptions struct {
	InstancesRoot string // 每實例資料根(<InstancesRoot>/<uuid>/);docker/native 共用
	BackupRoot    string // 備份根(docker/native 互通)
	CacheRoot     string // native 供應共用快取根(JRE/SteamCMD);docker 忽略
	Node          string // gsm.node 標籤值
}

// DockerCapable 由 dispatchBackend 實作,暴露 docker 子後端的能力查詢與熱替換(供 app 層
// RetryDocker 與 R12 的 GUI DockerAvailable 綁定)。以介面表達使 app 層無需相依 dispatchBackend
// 具體型別即可操作,並讓非 dispatch 的頂層後端(如測試的 MockBackend)自然不符合、走既有路徑。
type DockerCapable interface {
	// DockerAvailable 回報 docker 子後端目前是否可用(非 nil)。
	DockerAvailable() bool
	// SwapDocker 熱替換 docker 子後端(nil 表示標記為不可用);替換前的舊 docker 後端會被關閉。
	SwapDocker(newDocker RuntimeBackend)
}

var _ DockerCapable = (*dispatchBackend)(nil)

// NewDispatchBackend 建立組合後端。native 與 docker 皆可為 nil,但至少一個非 nil。建構後立即為
// 各非 nil 子後端起事件 pump。
func NewDispatchBackend(native, docker RuntimeBackend) (*dispatchBackend, error) {
	if native == nil && docker == nil {
		return nil, errors.New("agent: NewDispatchBackend 需要至少一個非 nil 子後端")
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &dispatchBackend{
		native:     native,
		hub:        newEventHub(0),
		rootCtx:    ctx,
		rootCancel: cancel,
		docker:     docker,
	}
	if native != nil {
		d.wg.Add(1)
		go d.pump(ctx, native)
	}
	if docker != nil {
		d.startDockerPumpLocked(docker)
	}
	return d, nil
}

// ---- 路由輔助 ----

// isNativeID 判定一個 RuntimeID 是否路由到 native 子後端(帶 "native:" 前綴)。
func isNativeID(id protocol.RuntimeID) bool {
	return strings.HasPrefix(string(id), nativePrefix)
}

// dockerBackend 以讀鎖取當前 docker 子後端(可能為 nil)。
func (d *dispatchBackend) dockerBackend() RuntimeBackend {
	d.dockerMu.RLock()
	defer d.dockerMu.RUnlock()
	return d.docker
}

// backendFor 依 RuntimeID 前綴選出目標子後端;目標為 nil(該 runtime 不可用)回明確錯誤。
func (d *dispatchBackend) backendFor(id protocol.RuntimeID) (RuntimeBackend, error) {
	if isNativeID(id) {
		if d.native == nil {
			return nil, unavailableRuntime("native")
		}
		return d.native, nil
	}
	docker := d.dockerBackend()
	if docker == nil {
		return nil, unavailableRuntime("docker")
	}
	return docker, nil
}

// unavailableRuntime 回傳「該 runtime 於本節點不可用」錯誤(包裝 ErrRuntimeUnavailable 供 errors.Is)。
func unavailableRuntime(runtime string) error {
	return &runtimeUnavailableError{runtime: runtime}
}

// ErrRuntimeUnavailable 是「請求的 runtime 子後端於本節點不可用」的哨符(Docker daemon 未就緒、
// 或非 Windows 平台請求 native)。經 errors.Is 比對。
var ErrRuntimeUnavailable = errors.New("agent: 請求的 runtime 於本節點不可用")

type runtimeUnavailableError struct{ runtime string }

func (e *runtimeUnavailableError) Error() string {
	return "agent: runtime " + e.runtime + " 於本節點不可用"
}
func (e *runtimeUnavailableError) Is(target error) bool { return target == ErrRuntimeUnavailable }

// ---- RuntimeBackend 實作 ----

// Create 依 spec.Runtime 路由:空/"docker"→docker、"native"→native、其餘→ERR_BAD_REQUEST。
func (d *dispatchBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	switch spec.Runtime {
	case "", "docker":
		docker := d.dockerBackend()
		if docker == nil {
			return "", unavailableRuntime("docker")
		}
		return docker.Create(ctx, spec)
	case "native":
		if d.native == nil {
			return "", unavailableRuntime("native")
		}
		return d.native.Create(ctx, spec)
	default:
		// 未知 runtime 為呼叫端錯誤:以 APIError 直接攜帶 wire 錯誤碼(對齊 server.go 的 bad-request 慣例)。
		return "", &protocol.APIError{Code: protocol.ErrBadRequest, Message: "未知 runtime: " + spec.Runtime}
	}
}

func (d *dispatchBackend) Start(ctx context.Context, id protocol.RuntimeID) error {
	b, err := d.backendFor(id)
	if err != nil {
		return err
	}
	return b.Start(ctx, id)
}

func (d *dispatchBackend) Stop(ctx context.Context, id protocol.RuntimeID, opts StopOpts) error {
	b, err := d.backendFor(id)
	if err != nil {
		return err
	}
	return b.Stop(ctx, id, opts)
}

func (d *dispatchBackend) Status(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeStatus, error) {
	b, err := d.backendFor(id)
	if err != nil {
		return protocol.RuntimeStatus{}, err
	}
	return b.Status(ctx, id)
}

func (d *dispatchBackend) Inspect(ctx context.Context, id protocol.RuntimeID) (protocol.RuntimeInfo, error) {
	b, err := d.backendFor(id)
	if err != nil {
		return protocol.RuntimeInfo{}, err
	}
	return b.Inspect(ctx, id)
}

func (d *dispatchBackend) Remove(ctx context.Context, id protocol.RuntimeID, opts RemoveOpts) error {
	b, err := d.backendFor(id)
	if err != nil {
		return err
	}
	return b.Remove(ctx, id, opts)
}

func (d *dispatchBackend) Logs(ctx context.Context, id protocol.RuntimeID, opts LogOpts) (LogStream, error) {
	b, err := d.backendFor(id)
	if err != nil {
		return nil, err
	}
	return b.Logs(ctx, id, opts)
}

func (d *dispatchBackend) ExecProcess(ctx context.Context, id protocol.RuntimeID, cmd ExecCmd) (ExecResult, error) {
	b, err := d.backendFor(id)
	if err != nil {
		return ExecResult{}, err
	}
	return b.ExecProcess(ctx, id, cmd)
}

func (d *dispatchBackend) Stats(ctx context.Context, id protocol.RuntimeID) (protocol.ResourceStats, error) {
	b, err := d.backendFor(id)
	if err != nil {
		return protocol.ResourceStats{}, err
	}
	return b.Stats(ctx, id)
}

func (d *dispatchBackend) Archive(ctx context.Context, id protocol.RuntimeID) (protocol.BackupID, error) {
	b, err := d.backendFor(id)
	if err != nil {
		return "", err
	}
	return b.Archive(ctx, id)
}

func (d *dispatchBackend) Restore(ctx context.Context, id protocol.RuntimeID, bid protocol.BackupID) (protocol.RuntimeID, error) {
	b, err := d.backendFor(id)
	if err != nil {
		return "", err
	}
	return b.Restore(ctx, id, bid)
}

// List 聚合兩子後端的 refs(native 已帶前綴、docker 裸穿,天然不重疊)。單一子後端錯誤時軟降級
// (略過其結果,不整體失敗),使 docker daemon 不可用不影響 native 對帳(R13)。惟當所有存在的
// 子後端皆錯誤時回傳錯誤——避免回空清單被 Reconciler 誤判為「全部實例已消失」而做破壞性對帳。
func (d *dispatchBackend) List(ctx context.Context) ([]protocol.RuntimeRef, error) {
	var refs []protocol.RuntimeRef
	present := 0  // 存在的子後端數
	failed := 0   // 錯誤的子後端數
	var lastErr error

	if d.native != nil {
		present++
		if nr, err := d.native.List(ctx); err != nil {
			failed++
			lastErr = err
		} else {
			refs = append(refs, nr...)
		}
	}
	if docker := d.dockerBackend(); docker != nil {
		present++
		if dr, err := docker.List(ctx); err != nil {
			failed++
			lastErr = err
		} else {
			refs = append(refs, dr...)
		}
	}
	if present > 0 && failed == present {
		return nil, lastErr // 全數失敗:回錯讓上層(Reconciler)略過本輪,不誤刪
	}
	return refs, nil
}

// Events 由 dispatchBackend 自持的聚合 hub 供應串流(pump 持續把子後端事件轉 emit 進來)。
func (d *dispatchBackend) Events(ctx context.Context, since protocol.Cursor) (EventStream, error) {
	return d.hub.subscribe(ctx, since)
}

// ---- DockerCapable 實作 ----

// DockerAvailable 回報 docker 子後端目前是否可用。
func (d *dispatchBackend) DockerAvailable() bool {
	return d.dockerBackend() != nil
}

// SwapDocker 熱替換 docker 子後端:取消舊 docker pump、替換引用、對新 docker 起新 pump,並關閉
// 被替換掉的舊 docker 後端。native 恆不受影響。RetryDocker 於節點 docker 能力恢復時呼叫。
func (d *dispatchBackend) SwapDocker(newDocker RuntimeBackend) {
	d.dockerMu.Lock()
	old := d.docker
	if d.dockerCancel != nil {
		d.dockerCancel()
		d.dockerCancel = nil
	}
	d.docker = newDocker
	if newDocker != nil && !d.closed {
		d.startDockerPumpLocked(newDocker)
	}
	d.dockerMu.Unlock()

	if old != nil {
		_ = closeSubBackend(old)
	}
}

// ---- 事件 pump ----

// startDockerPumpLocked 為 docker 子後端起一條可獨立取消的 pump(呼叫端須持 dockerMu 寫鎖,
// 或在建構時尚未發佈 d 之前呼叫)。
func (d *dispatchBackend) startDockerPumpLocked(docker RuntimeBackend) {
	ctx, cancel := context.WithCancel(d.rootCtx)
	d.dockerCancel = cancel
	d.wg.Add(1)
	go d.pump(ctx, docker)
}

// pump 訂閱一個子後端的事件並轉 emit 至聚合 hub,直到 ctx 取消或子後端串流結束。以空 cursor 訂閱
// (只收訂閱後的新事件);聚合 hub 自身的緩衝/cursor 提供核心 reconnect 對帳所需的歷史。
func (d *dispatchBackend) pump(ctx context.Context, sub RuntimeBackend) {
	defer d.wg.Done()
	stream, err := sub.Events(ctx, "")
	if err != nil {
		return
	}
	defer stream.Close()
	ch := stream.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			ev.Cursor = "" // 由聚合 hub 重新指派 cursor(單一空間)
			d.hub.emit(ev)
		}
	}
}

// Close 停止所有 pump、關閉聚合 hub,並關閉兩子後端(agent 退出時的收束)。冪等。
func (d *dispatchBackend) Close() error {
	d.closeOnce.Do(func() {
		d.dockerMu.Lock()
		d.closed = true
		d.dockerMu.Unlock()

		d.rootCancel() // 取消全部 pump(含 docker pump)
		d.wg.Wait()
		d.hub.close()

		if d.native != nil {
			_ = closeSubBackend(d.native)
		}
		d.dockerMu.Lock()
		docker := d.docker
		d.dockerMu.Unlock()
		if docker != nil {
			_ = closeSubBackend(docker)
		}
	})
	return nil
}

// closeSubBackend 關閉實作了 Close 的子後端(Docker/Native 皆有;RuntimeBackend 介面未含 Close)。
func closeSubBackend(b RuntimeBackend) error {
	if c, ok := b.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ---- 橫切能力介面轉發(BackupLister / BackupDeleter / MountWriter)----
//
// 這些是 RuntimeBackend 之外的可選介面,節點代理 Server 以型別斷言啟用對應端點。dispatchBackend
// 作為頂層後端須轉發之,否則備份列舉/刪除、mount 上傳等既有端點於生產(dispatch 為頂層)會失效。
// 皆以 bare instanceUUID 定址(無前綴),dispatchBackend 無法由 uuid 反推所有權,故轉發給所有實作
// 該介面的子後端並合併/取首個處理結果——與子後端是否共用檔案系統根無關,皆正確。

var (
	_ BackupLister  = (*dispatchBackend)(nil)
	_ BackupDeleter = (*dispatchBackend)(nil)
	_ MountWriter   = (*dispatchBackend)(nil)
)

// subBackends 回傳目前存在的子後端(native 在前、docker 在後),供橫切能力轉發走訪。
func (d *dispatchBackend) subBackends() []RuntimeBackend {
	subs := make([]RuntimeBackend, 0, 2)
	if d.native != nil {
		subs = append(subs, d.native)
	}
	if docker := d.dockerBackend(); docker != nil {
		subs = append(subs, docker)
	}
	return subs
}

// ListBackups 聚合各子後端對該實例回報的備份中繼,並依 BackupID 去重(子後端若共用備份根會回報
// 同一批目錄,去重後不重複)。任一子後端錯誤即回錯(備份列舉非對帳熱路徑,寧錯不吞)。
func (d *dispatchBackend) ListBackups(ctx context.Context, instanceUUID string) ([]protocol.BackupMeta, error) {
	seen := make(map[protocol.BackupID]struct{})
	out := make([]protocol.BackupMeta, 0, 4)
	for _, sub := range d.subBackends() {
		lister, ok := sub.(BackupLister)
		if !ok {
			continue
		}
		metas, err := lister.ListBackups(ctx, instanceUUID)
		if err != nil {
			return nil, err
		}
		for _, m := range metas {
			if _, dup := seen[m.BackupID]; dup {
				continue
			}
			seen[m.BackupID] = struct{}{}
			out = append(out, m)
		}
	}
	return out, nil
}

// DeleteBackup 逐一嘗試各實作 BackupDeleter 的子後端:回首個非 ErrNotFound 結果(成功或真實錯誤);
// 全數 ErrNotFound 則回 ErrNotFound。子後端共用備份根時,首個成功者即已移除實體目錄。
func (d *dispatchBackend) DeleteBackup(ctx context.Context, instanceUUID string, backupID protocol.BackupID) error {
	sawDeleter := false
	for _, sub := range d.subBackends() {
		deleter, ok := sub.(BackupDeleter)
		if !ok {
			continue
		}
		sawDeleter = true
		err := deleter.DeleteBackup(ctx, instanceUUID, backupID)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if !sawDeleter {
		return ErrNotFound // 無任何子後端支援刪除:視為找不到(對齊端點語意)
	}
	return ErrNotFound
}

// WriteMountFile 逐一嘗試各實作 MountWriter 的子後端:回首個非 ErrNotFound 結果;全數 ErrNotFound
// (該 mount 未於任一子後端的實例 spec 宣告)則回 ErrNotFound。
func (d *dispatchBackend) WriteMountFile(ctx context.Context, instanceUUID, mountName, filename string, r io.Reader) error {
	sawWriter := false
	for _, sub := range d.subBackends() {
		writer, ok := sub.(MountWriter)
		if !ok {
			continue
		}
		sawWriter = true
		err := writer.WriteMountFile(ctx, instanceUUID, mountName, filename, r)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if !sawWriter {
		return ErrNotFound
	}
	return ErrNotFound
}
