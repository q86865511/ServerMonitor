package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"servermonitor/internal/protocol"
)

// 節點層錯誤哨符(R5)。NodeClient 把代理 API 的統一錯誤碼映射為這些 Go 錯誤,
// 使上層可用 errors.Is 判別而不必解析字串;ErrNodeUnreachable 專指「連不上/逾時」的
// 傳輸層失敗(有別於代理已回應的 API 錯誤),供 NodeRegistry 據以標記節點離線。
var (
	// ErrNodeUnreachable 表示無法連上代理(連線被拒/逾時/DNS 等傳輸層失敗)。
	ErrNodeUnreachable = errors.New("core: 節點代理無法連線")
	// ErrNodePortConflict 對應 ERR_PORT_CONFLICT。
	ErrNodePortConflict = errors.New("core: 節點回報埠衝突")
	// ErrNodeNotFound 對應 ERR_NOT_FOUND。
	ErrNodeNotFound = errors.New("core: 節點查無此實例")
	// ErrNodeLocked 對應 ERR_LOCKED。
	ErrNodeLocked = errors.New("core: 節點回報實例鎖定")
	// ErrNodeUnauthorized 對應 ERR_UNAUTHORIZED。
	ErrNodeUnauthorized = errors.New("core: 節點拒絕(未授權)")
	// ErrNodeConflict 對應 ERR_CONFLICT。
	ErrNodeConflict = errors.New("core: 節點回報衝突")
)

// NodeClient 是核心對「節點代理 HTTP/WS API」(T5 server)的客戶端(R5)。
// 綁定 base URL 與每次啟動的 bearer token(建構參數注入),包裝所有版本化端點;
// {id} 一律為 instance UUID。回應錯誤依統一碼表映射回節點層哨符(見上)。
// 併發安全(底層 http.Client 與無狀態欄位)。
type NodeClient struct {
	baseURL string // 如 http://127.0.0.1:PORT(不含版本前綴)
	token   string
	hc      *http.Client
}

// 逾時分級:短操作(GET/建立/狀態/指令等)用 30s;停機鏈(hooks.stop 探測 + 寬限期)與備份/
// 還原(大檔封存)可遠超過 30s,故各給更長的預設。逾時改由 do() 逐請求以 ctx deadline 施加
// (而非全域 http.Client.Timeout),使長短操作可分別控制、且呼叫端顯式帶 deadline 時以其為準;
// WS 串流不受此限(另建 Dialer)。
// 為 var(非 const)以利單元測試注入小值驗證分級機制,不必等真實 30s 級逾時;正式路徑不改寫。
var (
	defaultNodeHTTPTimeout = 30 * time.Second // 短操作:單次 HTTP 呼叫預設逾時
	defaultNodeStopTimeout = 90 * time.Second // 停機鏈:hooks.stop RCON 探測可 hang 至逾時 + 寬限期
	defaultNodeLongTimeout = 30 * time.Minute // 備份/還原/上傳:大檔封存或傳輸可長
)

// NewNodeClient 建立 NodeClient。baseURL 為代理根位址(如 http://127.0.0.1:PORT);
// hc 可為 nil,採預設 http.Client(不設全域 Timeout——逾時由 do() 逐請求以 ctx 施加,見上;
// 全域 Timeout 會一併截斷長操作與 WS 升級後的讀取,故不用)。
func NewNodeClient(baseURL, token string, hc *http.Client) *NodeClient {
	if hc == nil {
		hc = &http.Client{}
	}
	return &NodeClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      hc,
	}
}

// BaseURL 回傳代理根位址(供診斷)。
func (c *NodeClient) BaseURL() string { return c.baseURL }

// ---- 端點包裝(HTTP)----

// Health 查詢代理健康(GET /health)。
func (c *NodeClient) Health(ctx context.Context) (protocol.HealthResponse, error) {
	var out protocol.HealthResponse
	err := c.do(ctx, http.MethodGet, "/health", "", nil, &out)
	return out, err
}

// Create 建立一個 runtime 實例(POST /instances)。以 spec.UUID 作冪等鍵,使重試回原結果。
func (c *NodeClient) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.CreateInstanceResponse, error) {
	var out protocol.CreateInstanceResponse
	err := c.do(ctx, http.MethodPost, "/instances", spec.UUID,
		protocol.CreateInstanceRequest{Spec: spec}, &out)
	return out, err
}

// List 列舉實例(GET /instances)。
func (c *NodeClient) List(ctx context.Context) ([]protocol.InstanceSummary, error) {
	var out protocol.ListInstancesResponse
	err := c.do(ctx, http.MethodGet, "/instances", "", nil, &out)
	return out.Instances, err
}

// Inspect 查詢單一實例詳情(GET /instances/{id})。
func (c *NodeClient) Inspect(ctx context.Context, uuid string) (protocol.RuntimeInfo, error) {
	var out protocol.RuntimeInfo
	err := c.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(uuid), "", nil, &out)
	return out, err
}

// Remove 移除實例(DELETE /instances/{id});purge 一併刪除 data/backups。
// 冪等鍵由 do() 對本次呼叫自動產生新值(不重用 uuid,避免與其他寫入端點的重播窗口互相污染)。
func (c *NodeClient) Remove(ctx context.Context, uuid string, purge bool) error {
	return c.do(ctx, http.MethodDelete, "/instances/"+url.PathEscape(uuid), "",
		protocol.RemoveInstanceRequest{Purge: purge}, nil)
}

// Start 啟動實例(POST /instances/{id}/start)。冪等鍵由 do() 逐次呼叫自動產生。
func (c *NodeClient) Start(ctx context.Context, uuid string) error {
	return c.do(ctx, http.MethodPost, "/instances/"+url.PathEscape(uuid)+"/start", "", nil, nil)
}

// Stop 停止實例(POST /instances/{id}/stop)。冪等鍵由 do() 逐次呼叫自動產生。
// 用較長預設逾時:代理端會先跑 hooks.stop(RCON/REST 探測,對啟動中伺服器可能 hang 至逾時)再等
// 優雅停機寬限期,合計可超過短操作的 30s(呼叫端未帶 deadline 時,30s 會在停機完成前錯誤截斷)。
func (c *NodeClient) Stop(ctx context.Context, uuid string, req protocol.StopInstanceRequest) error {
	return c.doWithTimeout(ctx, http.MethodPost, "/instances/"+url.PathEscape(uuid)+"/stop", "", req, nil, defaultNodeStopTimeout)
}

// Status 查詢 runtime 即時狀態(GET /instances/{id}/status)。
func (c *NodeClient) Status(ctx context.Context, uuid string) (protocol.RuntimeStatus, error) {
	var out protocol.RuntimeStatus
	err := c.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(uuid)+"/status", "", nil, &out)
	return out, err
}

// Command 送出遊戲指令(POST /instances/{id}/command)。target 由核心解析範本+埠+機密後帶入,
// 供 agent 依 Kind 選 adapter。冪等鍵由 do() 逐次呼叫自動產生(同一實例的連續兩個不同指令
// 不可共用冪等鍵,否則第二個會被重播為第一個的結果)。
func (c *NodeClient) Command(ctx context.Context, uuid string, target protocol.CommandTarget, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	var out protocol.CommandResponse
	err := c.do(ctx, http.MethodPost, "/instances/"+url.PathEscape(uuid)+"/command", "",
		protocol.CommandRequest{Command: cmd, Target: target}, &out)
	return out.Result, err
}

// UploadMount 上傳一個具名 mount 檔(PUT /instances/{id}/mounts/{name}),body 為檔案位元組
// (R11 手動模組包檔)。上傳採覆寫、天然冪等,故不帶冪等鍵。
func (c *NodeClient) UploadMount(ctx context.Context, uuid, name, filename string, r io.Reader) error {
	path := "/instances/" + url.PathEscape(uuid) + "/mounts/" + url.PathEscape(name) +
		"?filename=" + url.QueryEscape(filename)
	return c.doUpload(ctx, path, r)
}

// ListBackups 列舉某實例備份中繼(GET /instances/{id}/backups)。
func (c *NodeClient) ListBackups(ctx context.Context, uuid string) ([]protocol.BackupMeta, error) {
	var out protocol.ListBackupsResponse
	err := c.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(uuid)+"/backups", "", nil, &out)
	return out.Backups, err
}

// DeleteBackup 刪除某實例的一份備份(DELETE /instances/{id}/backups/{backupID});供保留策略
// 刪最舊(R9)。冪等鍵由 do() 逐次呼叫自動產生;備份不存在回 ErrNodeNotFound。
func (c *NodeClient) DeleteBackup(ctx context.Context, uuid string, backupID protocol.BackupID) error {
	return c.do(ctx, http.MethodDelete,
		"/instances/"+url.PathEscape(uuid)+"/backups/"+url.PathEscape(string(backupID)), "", nil, nil)
}

// Backup 觸發一次備份(POST /instances/{id}/backup)。冪等鍵由 do() 逐次呼叫自動產生
// (否則同一實例的連續兩次備份,第二次會被重播為第一次的結果而未真正執行)。
func (c *NodeClient) Backup(ctx context.Context, uuid string) (protocol.BackupMeta, error) {
	var out protocol.BackupResponse
	// 用較長預設逾時:停機一致快照的 tar+checksum 封存對大型世界可耗數分鐘,遠超短操作 30s。
	err := c.doWithTimeout(ctx, http.MethodPost, "/instances/"+url.PathEscape(uuid)+"/backup", "", nil, &out, defaultNodeLongTimeout)
	return out.Backup, err
}

// Restore 以備份還原(POST /instances/{id}/restore),回傳新 runtime ID(R9)。冪等鍵由 do() 逐次呼叫自動產生。
func (c *NodeClient) Restore(ctx context.Context, uuid string, backupID protocol.BackupID) (protocol.RuntimeID, error) {
	var out protocol.RestoreResponse
	// 用較長預設逾時:還原需解壓封存並重建容器,對大型世界可耗數分鐘(同 Backup)。
	err := c.doWithTimeout(ctx, http.MethodPost, "/instances/"+url.PathEscape(uuid)+"/restore", "",
		protocol.RestoreRequest{BackupID: backupID}, &out, defaultNodeLongTimeout)
	return out.RuntimeID, err
}

// ---- 端點包裝(WebSocket 串流)----
//
// WS 端點回傳已升級的連線,由消費端(T8 事件訂閱、T10 監控聚合)讀取與關閉。
// 帶 Authorization bearer 與 loopback Origin 以通過代理的認證與 Origin 檢查。

// EventsWS 訂閱執行事件串流(WS /events);since 為 reconnect 對帳游標(空=只收新事件)。
func (c *NodeClient) EventsWS(ctx context.Context, since protocol.Cursor) (*websocket.Conn, error) {
	path := "/events"
	if since != "" {
		path += "?since=" + url.QueryEscape(string(since))
	}
	return c.dialWS(ctx, path)
}

// StatsWS 訂閱資源取樣串流(WS /instances/{id}/stats)。
func (c *NodeClient) StatsWS(ctx context.Context, uuid string) (*websocket.Conn, error) {
	return c.dialWS(ctx, "/instances/"+url.PathEscape(uuid)+"/stats")
}

// LogsWS 訂閱即時 log 串流(WS /instances/{id}/logs)。
func (c *NodeClient) LogsWS(ctx context.Context, uuid string) (*websocket.Conn, error) {
	return c.dialWS(ctx, "/instances/"+url.PathEscape(uuid)+"/logs")
}

// ---- 內部:HTTP 與 WS 傳輸 ----

// do 執行一次 HTTP 呼叫:組 URL(base + 版本前綴 + path)、帶 bearer 與可選冪等鍵、
// 送出並依狀態碼映射錯誤。out 非 nil 且回應成功時解碼回應主體。
// 傳輸層失敗(連不上)回 ErrNodeUnreachable 包裝,供 NodeRegistry 標離線。
//
// 冪等鍵語意(對應代理端 idempotent 中介層,見 server_idempotency.go):快取鍵為
// method+path+key,同鍵於 TTL 內重播首次回應而不重跑後端動作。呼叫端未指定 idemKey
// (傳空字串)且為非 GET 寫入呼叫時,此處對「這一次呼叫」自動產生一把新鍵——
// 避免呼叫端誤把可重複使用的識別碼(如實例 UUID)當冪等鍵,導致同一實例的後續不同
// 操作在 TTL 窗口內被錯誤重播。呼叫端若需要「重試回原結果」的語意(如 Create 以
// spec.UUID 為鍵),仍可顯式傳入 idemKey 覆蓋此自動產生行為。
func (c *NodeClient) do(ctx context.Context, method, path, idemKey string, body, out any) error {
	return c.doWithTimeout(ctx, method, path, idemKey, body, out, defaultNodeHTTPTimeout)
}

// doWithTimeout 同 do,但以 def 作為「呼叫端 ctx 未帶 deadline 時」套用的預設逾時。長操作
// (Stop/Backup/Restore)傳入較長的 def;呼叫端已帶 deadline 時以其為準、不覆寫(取較嚴者:
// context.WithTimeout 對已較早到期的父 ctx 不會延後)。cancel 以 defer 於本函式返回時呼叫,
// 請求已於函式內讀盡回應主體,故不洩漏 context。
func (c *NodeClient) doWithTimeout(ctx context.Context, method, path, idemKey string, body, out any, def time.Duration) error {
	if def > 0 {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, def)
			defer cancel()
		}
	}
	if idemKey == "" && method != http.MethodGet {
		idemKey = newUUIDv4()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("序列化請求主體失敗: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), reader)
	if err != nil {
		return fmt.Errorf("建立請求失敗: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set(protocol.IdempotencyKeyHeader, idemKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// 傳輸層失敗(連線被拒/逾時/context 取消)→ 節點不可達。
		return fmt.Errorf("%w: %v", ErrNodeUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return c.mapError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解碼回應失敗: %w", err)
	}
	return nil
}

// doUpload 以 PUT 送出 application/octet-stream 原始位元組(不經 JSON 編碼、不帶冪等鍵)。
// 傳輸層失敗回 ErrNodeUnreachable;代理回 4xx/5xx 依統一碼表映射(見 mapError)。
func (c *NodeClient) doUpload(ctx context.Context, path string, r io.Reader) error {
	// 全域 http.Client.Timeout 已移除(見 NewNodeClient),故此處對「呼叫端未帶 deadline」補一層
	// 較長預設逾時——手動模組包檔可達數百 MB,傳輸不宜以短逾時截斷,但仍需上界避免無限 hang。
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultNodeLongTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.endpoint(path), r)
	if err != nil {
		return fmt.Errorf("建立請求失敗: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNodeUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return c.mapError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// mapError 解析代理的統一錯誤主體並映射為節點層哨符;無法解析時回泛用 APIError。
func (c *NodeClient) mapError(resp *http.Response) error {
	data, _ := io.ReadAll(resp.Body)
	var ae protocol.APIError
	if json.Unmarshal(data, &ae) != nil || ae.Code == "" {
		return &protocol.APIError{
			Code:    protocol.ErrInternal,
			Message: fmt.Sprintf("代理回應 %d: %s", resp.StatusCode, strings.TrimSpace(string(data))),
		}
	}
	switch ae.Code {
	case protocol.ErrPortConflict:
		return fmt.Errorf("%w: %s", ErrNodePortConflict, ae.Message)
	case protocol.ErrNotFound:
		return fmt.Errorf("%w: %s", ErrNodeNotFound, ae.Message)
	case protocol.ErrLocked:
		return fmt.Errorf("%w: %s", ErrNodeLocked, ae.Message)
	case protocol.ErrUnauthorized:
		return fmt.Errorf("%w: %s", ErrNodeUnauthorized, ae.Message)
	case protocol.ErrConflict:
		return fmt.Errorf("%w: %s", ErrNodeConflict, ae.Message)
	default:
		return &ae
	}
}

// dialWS 升級一個 WS 連線;傳輸層失敗回 ErrNodeUnreachable 包裝。
func (c *NodeClient) dialWS(ctx context.Context, path string) (*websocket.Conn, error) {
	endpoint := c.endpoint(path)
	// http→ws / https→wss(TrimRight 已去尾斜線,前綴替換僅動 scheme)。
	wsEndpoint := "ws" + strings.TrimPrefix(endpoint, "http")

	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	h.Set("Origin", "http://127.0.0.1") // 通過代理的 loopback Origin 檢查

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsEndpoint, h)
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return nil, c.mapError(resp)
		}
		return nil, fmt.Errorf("%w: %v", ErrNodeUnreachable, err)
	}
	return conn, nil
}

// endpoint 組出完整 URL:base + "/" + 版本前綴 + path。
func (c *NodeClient) endpoint(path string) string {
	return c.baseURL + "/" + protocol.AgentAPIVersion + path
}
