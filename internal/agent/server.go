package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"servermonitor/internal/protocol"
)

// Server 把 RuntimeBackend 包成「有版本、有認證」的節點代理 HTTP/WS API(R5)。它是後端的薄層:
// 不擁有實例登錄(那是核心職責),以 gsm.uuid 標籤把路徑 {id}=instance UUID 解析為 RuntimeID;
// 認證用啟動時產生的記憶體 bearer token,寫入端點以記憶體 TTL 快取支援冪等鍵重播,WS 另檢查
// loopback Origin。錯誤以 protocol 統一碼表回結構化 JSON。併發安全。
type Server struct {
	backend       RuntimeBackend
	commands      GameCommandAdapter
	token         string
	statsInterval time.Duration
	idem          *idempotencyCache
	handler       http.Handler

	baseCtx context.Context
	cancel  context.CancelFunc
}

// Config 是 Server 的建構參數。Backend 必填;其餘可留零值採預設。
type Config struct {
	Backend        RuntimeBackend     // 必填:被包裝的執行後端
	Commands       GameCommandAdapter // 遊戲指令轉接器;nil 用 NewCommandDispatcher(依 Kind 選 rcon/rest)
	Token          string             // bearer token;空字串則啟動時隨機產生(以利測試注入)
	StatsInterval  time.Duration      // WS stats 推送週期;<=0 用預設 2s
	IdempotencyTTL time.Duration      // 冪等鍵重播窗口;<=0 用 protocol.IdempotencyKeyTTL(10m)
}

const defaultStatsInterval = 2 * time.Second

// 執行後端層錯誤哨符(與 mock.go 的 ErrNotFound 等並列),供 server 映射到 wire 錯誤碼。
// 埠衝突由後端建立/啟動容器時偵測(如 Docker「port is already allocated」);鎖由上層在
// 序列化衝突時回報。兩者目前的產生端在後續任務接上,此處提供穩定的映射目標。
var (
	// ErrPortConflict 表示宿主埠已被占用(→ ERR_PORT_CONFLICT)。
	ErrPortConflict = errors.New("agent: host port conflict")
	// ErrLocked 表示實例正被其他操作鎖定(→ ERR_LOCKED)。
	ErrLocked = errors.New("agent: instance operation locked")
)

// NewServer 建立一個節點代理 server。回傳後以 Handler()/ServeHTTP 掛到 http.Server,
// 或直接交給 httptest。Token() 取得有效 token。
func NewServer(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("agent: NewServer 需要 Backend")
	}
	token := cfg.Token
	if token == "" {
		t, err := randomToken()
		if err != nil {
			return nil, err
		}
		token = t
	}
	commands := cfg.Commands
	if commands == nil {
		commands = NewCommandDispatcher()
	}
	statsInterval := cfg.StatsInterval
	if statsInterval <= 0 {
		statsInterval = defaultStatsInterval
	}
	ttl := cfg.IdempotencyTTL
	if ttl <= 0 {
		ttl = protocol.IdempotencyKeyTTL
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		backend:       cfg.Backend,
		commands:      commands,
		token:         token,
		statsInterval: statsInterval,
		idem:          newIdempotencyCache(ttl),
		baseCtx:       ctx,
		cancel:        cancel,
	}
	s.handler = s.withAuth(s.routes())
	return s, nil
}

// Token 回傳有效的 bearer token(供 core NodeClient 或測試使用)。
func (s *Server) Token() string { return s.token }

// Handler 回傳掛載了認證的 http.Handler。
func (s *Server) Handler() http.Handler { return s.handler }

// ServeHTTP 讓 Server 本身即為 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Close 收束所有進行中的 WS 串流(經 baseCtx)。冪等。
func (s *Server) Close() error {
	s.cancel()
	return nil
}

// routes 註冊版本化端點({id}=instance UUID)。寫入端點包上冪等中介層;認證由外層 withAuth 統一施加。
// 刻意無 /restart(restart 由核心編排)、無 PUT/PATCH(首版不支援 update)。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	base := "/" + protocol.AgentAPIVersion

	mux.HandleFunc("GET "+base+"/health", s.handleHealth)
	mux.HandleFunc("GET "+base+"/instances", s.handleList)
	mux.Handle("POST "+base+"/instances", s.idempotent(http.HandlerFunc(s.handleCreate)))
	mux.HandleFunc("GET "+base+"/instances/{id}", s.handleGet)
	mux.Handle("DELETE "+base+"/instances/{id}", s.idempotent(http.HandlerFunc(s.handleDelete)))
	mux.Handle("POST "+base+"/instances/{id}/start", s.idempotent(http.HandlerFunc(s.handleStart)))
	mux.Handle("POST "+base+"/instances/{id}/stop", s.idempotent(http.HandlerFunc(s.handleStop)))
	mux.HandleFunc("GET "+base+"/instances/{id}/status", s.handleStatus)
	mux.HandleFunc("GET "+base+"/instances/{id}/stats", s.handleStatsWS)
	mux.HandleFunc("GET "+base+"/instances/{id}/logs", s.handleLogsWS)
	mux.HandleFunc("GET "+base+"/events", s.handleEventsWS)
	mux.Handle("POST "+base+"/instances/{id}/command", s.idempotent(http.HandlerFunc(s.handleCommand)))
	// 上傳具名 mount 檔(R11 手動模組包);上傳採覆寫、天然冪等,故不套冪等中介層。
	mux.HandleFunc("PUT "+base+"/instances/{id}/mounts/{name}", s.handleUploadMount)
	mux.HandleFunc("GET "+base+"/instances/{id}/backups", s.handleListBackups)
	mux.Handle("DELETE "+base+"/instances/{id}/backups/{backupID}", s.idempotent(http.HandlerFunc(s.handleDeleteBackup)))
	mux.Handle("POST "+base+"/instances/{id}/backup", s.idempotent(http.HandlerFunc(s.handleBackup)))
	mux.Handle("POST "+base+"/instances/{id}/restore", s.idempotent(http.HandlerFunc(s.handleRestore)))
	// 階段 4:Docker 資源管理。映像/容器為節點層(無 {id}→實例對映);刪除以精確 id、天然冪等
	// (再刪回 404),故如 mount 上傳不套冪等中介層。磁碟用量以 uuid 定位、不需容器解析。
	mux.HandleFunc("GET "+base+"/images", s.handleListImages)
	mux.HandleFunc("DELETE "+base+"/images/{id}", s.handleRemoveImage)
	mux.HandleFunc("POST "+base+"/images/prune", s.handlePruneImages)
	mux.HandleFunc("GET "+base+"/containers", s.handleListContainers)
	mux.HandleFunc("DELETE "+base+"/containers/{id}", s.handleRemoveContainer)
	mux.HandleFunc("GET "+base+"/instances/{id}/diskusage", s.handleDiskUsage)

	return mux
}

// ---- 認證 ----

// withAuth 對所有端點(含 WS)施加 bearer token 檢查;未授權回 401 ERR_UNAUTHORIZED。
// 成功時以原始 ResponseWriter 續傳,故 WS handler 仍可 hijack 升級。
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, protocol.APIError{
				Code: protocol.ErrUnauthorized, Message: "missing or invalid bearer token",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorized 以常數時間比對 Authorization: Bearer <token>。
func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		tok, ok = strings.CutPrefix(h, "bearer ")
	}
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1
}

// ---- uuid→RuntimeID 解析 ----

// resolve 以 gsm.uuid 標籤把 API 路徑的 instance UUID 解析為後端 RuntimeID。無狀態(不持登錄),
// 每次向後端 List 對帳,對齊「薄層」定位;DockerBackend 一律注入 gsm.uuid 標籤,故必定可解析。
func (s *Server) resolve(ctx context.Context, uuid string) (protocol.RuntimeID, error) {
	if uuid == "" {
		return "", ErrNotFound
	}
	refs, err := s.backend.List(ctx)
	if err != nil {
		return "", err
	}
	for _, ref := range refs {
		if ref.Labels[labelUUID] == uuid {
			return ref.ID, nil
		}
	}
	return "", ErrNotFound
}

// ---- HTTP handlers ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{Status: "ok", Version: protocol.AgentAPIVersion})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	refs, err := s.backend.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]protocol.InstanceSummary, 0, len(refs))
	for _, ref := range refs {
		out = append(out, protocol.InstanceSummary{
			UUID:      ref.Labels[labelUUID],
			RuntimeID: ref.ID,
			State:     ref.State,
		})
	}
	writeJSON(w, http.StatusOK, protocol.ListInstancesResponse{Instances: out})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateInstanceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	rid, err := s.backend.Create(r.Context(), req.Spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, protocol.CreateInstanceResponse{UUID: req.Spec.UUID, RuntimeID: rid})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	rid, err := s.resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	info, err := s.backend.Inspect(r.Context(), rid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// instanceDataPurger 由能「不依賴容器、直接以 uuid 清除實例宿主資料/備份」的後端實作(B7)。
// docker/native 皆以其資料根/備份根實作;dispatchBackend 轉發至各子後端。
type instanceDataPurger interface {
	PurgeInstanceData(ctx context.Context, uuid string) error
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req protocol.RemoveInstanceRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	uuid := r.PathValue("id")
	if err := validateInstanceUUID(uuid); err != nil {
		writeErr(w, err) // 路徑遍歷防禦:purge 以 uuid 直接清宿主資料/備份,先驗 uuid 格式
		return
	}
	rid, err := s.resolve(r.Context(), uuid)
	if err != nil {
		// B7:容器已 out-of-band 移除。purge 時仍以 uuid 直接清宿主資料/備份——否則 resolve 短路使
		// backend.Remove(Purge) 的磁碟清理被跳過,而 core 把 ErrNodeNotFound 視為已移除續刪 DB,造成
		// 資料+備份永久孤兒卻回報成功。清理後仍回原 ErrNotFound(container 確實不存在,core 據此收斂)。
		if errors.Is(err, ErrNotFound) && req.Purge {
			if purger, ok := s.backend.(instanceDataPurger); ok {
				if perr := purger.PurgeInstanceData(r.Context(), uuid); perr != nil {
					writeErr(w, perr)
					return
				}
			}
		}
		writeErr(w, err)
		return
	}
	if err := s.backend.Remove(r.Context(), rid, RemoveOpts{Purge: req.Purge}); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	rid, err := s.resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.backend.Start(r.Context(), rid); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	rid, err := s.resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var req protocol.StopInstanceRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	opts := StopOpts{PlannedStopToken: req.PlannedStopToken}
	if req.GraceSeconds > 0 {
		opts.Grace = time.Duration(req.GraceSeconds) * time.Second
	}
	if err := s.backend.Stop(r.Context(), rid, opts); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	rid, err := s.resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	st, err := s.backend.Status(r.Context(), rid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	// 解析以確認實例存在(未知 UUID → 404);指令實際送達由 GameCommandAdapter 負責。
	if _, err := s.resolve(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	var req protocol.CommandRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	// 埠/機密/協定解析屬核心職責:core 已於 req.Target 帶入 kind/host/port/password/actions,
	// 此處依 Kind 選 adapter 執行(見 commandDispatcher)。
	res, err := s.commands.Send(r.Context(), req.Target, req.Command)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.CommandResponse{Result: res})
}

// handleUploadMount 接收 body=檔案位元組(application/octet-stream),寫入實例的具名 mount
// 宿主目錄下的 <filename>(R11 手動模組包檔)。filename 由 query 帶入並 sanitize(拒路徑分隔/..);
// 實例或 mount 不存在 → 404;成功回 201。上傳採覆寫,天然冪等。
func (s *Server) handleUploadMount(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("id")
	name := r.PathValue("name")
	filename := r.URL.Query().Get("filename")
	if err := validateMountFilename(filename); err != nil {
		writeError(w, http.StatusBadRequest, protocol.APIError{Code: protocol.ErrBadRequest, Message: "invalid filename"})
		return
	}
	// 確認實例存在(未知 UUID → 404);mount 是否宣告由後端依 spec 判定(未宣告 → 404)。
	if _, err := s.resolve(r.Context(), uuid); err != nil {
		writeErr(w, err)
		return
	}
	writer, ok := s.backend.(MountWriter)
	if !ok {
		writeError(w, http.StatusInternalServerError, protocol.APIError{
			Code: protocol.ErrInternal, Message: "backend does not support mount upload",
		})
		return
	}
	defer r.Body.Close()
	if err := writer.WriteMountFile(r.Context(), uuid, name, filename, r.Body); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("id")
	lister, ok := s.backend.(BackupLister)
	if !ok {
		writeError(w, http.StatusInternalServerError, protocol.APIError{
			Code: protocol.ErrInternal, Message: "backend does not support backup listing",
		})
		return
	}
	metas, err := lister.ListBackups(r.Context(), uuid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.ListBackupsResponse{Backups: metas})
}

// handleDeleteBackup 刪除某實例的一份備份(保留策略;R9)。{id}=instance UUID、{backupID}=備份 ID
// 皆為路徑參數;以 UUID 過濾避免跨實例刪除。後端未實作 BackupDeleter → 500;備份不存在 → 404;
// 成功回 204。刪除採冪等中介層(重播回原結果)。
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("id")
	backupID := protocol.BackupID(r.PathValue("backupID"))
	// 路徑遍歷第一層防禦:URL 路徑段(已解碼,如 "..%5C"→"..\")進後端前先驗 uuid 與 backupID 格式,拒絕 → 400。
	if err := validateInstanceUUID(uuid); err != nil {
		writeErr(w, err)
		return
	}
	if err := validateBackupID(backupID); err != nil {
		writeErr(w, err)
		return
	}
	deleter, ok := s.backend.(BackupDeleter)
	if !ok {
		writeError(w, http.StatusInternalServerError, protocol.APIError{
			Code: protocol.ErrInternal, Message: "backend does not support backup deletion",
		})
		return
	}
	if err := deleter.DeleteBackup(r.Context(), uuid, backupID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("id")
	rid, err := s.resolve(r.Context(), uuid)
	if err != nil {
		writeErr(w, err)
		return
	}
	bid, err := s.backend.Archive(r.Context(), rid)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 盡力補齊完整中繼(checksum 等);後端不支援查詢時退回僅帶 ID 的中繼。
	meta := protocol.BackupMeta{BackupID: bid, InstanceUUID: uuid, TsUTC: time.Now().UTC()}
	if lister, ok := s.backend.(BackupLister); ok {
		if metas, lerr := lister.ListBackups(r.Context(), uuid); lerr == nil {
			for _, m := range metas {
				if m.BackupID == bid {
					meta = m
					break
				}
			}
		}
	}
	writeJSON(w, http.StatusCreated, protocol.BackupResponse{Backup: meta})
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	rid, err := s.resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var req protocol.RestoreRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.BackupID == "" {
		writeError(w, http.StatusBadRequest, protocol.APIError{Code: protocol.ErrBadRequest, Message: "backup_id required"})
		return
	}
	newID, err := s.backend.Restore(r.Context(), rid, req.BackupID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.RestoreResponse{RuntimeID: newID})
}

// ---- 階段 4:Docker 資源管理 handlers ----
//
// 後端未實作對應能力(如 native-only 節點無 ImageManager/ContainerManager)→ ErrUnsupported
// (HTTP 501)。生產頂層後端為 dispatchBackend,恆實作這些介面,能力缺失於其內部按有無 docker 子
// 後端回 ErrUnsupported;非 dispatch 後端(測試 MockBackend)則於此型別斷言失敗回同一碼,兩路收斂。

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.backend.(ImageManager)
	if !ok {
		writeErr(w, ErrUnsupported)
		return
	}
	imgs, err := mgr.ListImages(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.ListImagesResponse{Images: imgs})
}

func (s *Server) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.backend.(ImageManager)
	if !ok {
		writeErr(w, ErrUnsupported)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, protocol.APIError{Code: protocol.ErrBadRequest, Message: "image id required"})
		return
	}
	if err := mgr.RemoveImage(r.Context(), id, r.URL.Query().Get("force") == "true"); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePruneImages(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.backend.(ImageManager)
	if !ok {
		writeErr(w, ErrUnsupported)
		return
	}
	res, err := mgr.PruneImages(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.backend.(ContainerManager)
	if !ok {
		writeErr(w, ErrUnsupported)
		return
	}
	cs, err := mgr.ListContainers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.ListContainersResponse{Containers: cs})
}

func (s *Server) handleRemoveContainer(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.backend.(ContainerManager)
	if !ok {
		writeErr(w, ErrUnsupported)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, protocol.APIError{Code: protocol.ErrBadRequest, Message: "container id required"})
		return
	}
	if err := mgr.RemoveContainer(r.Context(), id, r.URL.Query().Get("force") == "true"); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDiskUsage 回報某實例的宿主磁碟用量(資料/備份根)。以 uuid 直接走 diskUsager,不做
// resolve(磁碟用量以 uuid 定位、不需容器存在);後端不支援 → ErrUnsupported。
func (s *Server) handleDiskUsage(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("id")
	if err := validateInstanceUUID(uuid); err != nil {
		writeErr(w, err) // 路徑遍歷防禦:磁碟用量以 uuid 定位宿主目錄,先驗格式
		return
	}
	usager, ok := s.backend.(instanceDiskUsager)
	if !ok {
		writeErr(w, ErrUnsupported)
		return
	}
	du, err := usager.InstanceDiskUsage(r.Context(), uuid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, du)
}

// ---- 錯誤映射 / JSON 輔助 ----

// apiErrorFor 把內部錯誤映射為 HTTP 狀態碼 + 統一 wire 錯誤碼。
func apiErrorFor(err error) (int, protocol.APIError) {
	var apiErr *protocol.APIError
	switch {
	case errors.As(err, &apiErr):
		return statusForCode(apiErr.Code), *apiErr
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, protocol.APIError{Code: protocol.ErrNotFound, Message: err.Error()}
	case errors.Is(err, ErrInvalidFilename):
		return http.StatusBadRequest, protocol.APIError{Code: protocol.ErrBadRequest, Message: err.Error()}
	case errors.Is(err, ErrInvalidBackupID):
		return http.StatusBadRequest, protocol.APIError{Code: protocol.ErrBadRequest, Message: err.Error()}
	case errors.Is(err, ErrInvalidInstanceUUID):
		return http.StatusBadRequest, protocol.APIError{Code: protocol.ErrBadRequest, Message: err.Error()}
	case errors.Is(err, ErrPortConflict):
		return http.StatusConflict, protocol.APIError{Code: protocol.ErrPortConflict, Message: err.Error()}
	case errors.Is(err, ErrLocked):
		return http.StatusLocked, protocol.APIError{Code: protocol.ErrLocked, Message: err.Error()}
	case errors.Is(err, ErrUnsupported):
		return http.StatusNotImplemented, protocol.APIError{Code: protocol.ErrUnsupported, Message: err.Error()}
	case errors.Is(err, ErrBackendClosed):
		return http.StatusServiceUnavailable, protocol.APIError{Code: protocol.ErrInternal, Message: err.Error()}
	default:
		return http.StatusInternalServerError, protocol.APIError{Code: protocol.ErrInternal, Message: err.Error()}
	}
}

// statusForCode 把 wire 錯誤碼映射為 HTTP 狀態碼。
func statusForCode(c protocol.ErrorCode) int {
	switch c {
	case protocol.ErrBadRequest:
		return http.StatusBadRequest
	case protocol.ErrUnauthorized:
		return http.StatusUnauthorized
	case protocol.ErrNotFound:
		return http.StatusNotFound
	case protocol.ErrConflict, protocol.ErrPortConflict:
		return http.StatusConflict
	case protocol.ErrLocked:
		return http.StatusLocked
	case protocol.ErrUnsupported:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

func writeErr(w http.ResponseWriter, err error) {
	status, apiErr := apiErrorFor(err)
	writeError(w, status, apiErr)
}

func writeError(w http.ResponseWriter, status int, apiErr protocol.APIError) {
	writeJSON(w, status, apiErr)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON 解析必需的請求主體;失敗回 ERR_BAD_REQUEST。
func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return &protocol.APIError{Code: protocol.ErrBadRequest, Message: "invalid request body: " + err.Error()}
	}
	return nil
}

// decodeOptionalJSON 解析可省略的請求主體(空主體視為零值,如 stop/delete 未帶 body)。
func decodeOptionalJSON(r *http.Request, v any) error {
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return &protocol.APIError{Code: protocol.ErrBadRequest, Message: "invalid request body: " + err.Error()}
	}
	return nil
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agent: 產生 token 失敗: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
