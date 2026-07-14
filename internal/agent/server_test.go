package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"servermonitor/internal/protocol"
)

const testToken = "test-token-abc123"

var apiBase = "/" + protocol.AgentAPIVersion

// ---- 測試輔助 ----

func newTestServer(t *testing.T, backend RuntimeBackend) *httptest.Server {
	t.Helper()
	s, err := NewServer(Config{
		Backend:       backend,
		Token:         testToken,
		StatsInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		hs.Close()
		_ = s.Close()
	})
	return hs
}

// request 發一個 HTTP 請求;token 空字串則不帶 Authorization。回傳狀態碼與主體。
func request(t *testing.T, hs *httptest.Server, method, path, token, idemKey string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, hs.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set(protocol.IdempotencyKeyHeader, idemKey)
	}
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func mustAPIError(t *testing.T, data []byte) protocol.APIError {
	t.Helper()
	var ae protocol.APIError
	if err := json.Unmarshal(data, &ae); err != nil {
		t.Fatalf("unmarshal APIError from %q: %v", data, err)
	}
	return ae
}

// specWithUUID 造一個帶 gsm.uuid 標籤的 spec,使 server 的 resolve(以標籤解析)在 MockBackend 上可用。
func specWithUUID(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:   uuid,
		Image:  "example/image:latest",
		Labels: map[string]string{labelUUID: uuid},
	}
}

func wsURL(hs *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(hs.URL, "http") + path
}

func dialWS(t *testing.T, hs *httptest.Server, path, token, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return websocket.DefaultDialer.Dial(wsURL(hs, path), h)
}

// wrapBackend 包住 MockBackend:計數 Create/Start 呼叫、可注入錯誤,供冪等與錯誤碼測試。
type wrapBackend struct {
	*MockBackend
	mu          sync.Mutex
	createCalls int
	startCalls  int
	createErr   error
	startErr    error
}

func newWrapBackend() *wrapBackend { return &wrapBackend{MockBackend: NewMockBackend()} }

func (b *wrapBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	b.mu.Lock()
	b.createCalls++
	b.mu.Unlock()
	if b.createErr != nil {
		return "", b.createErr
	}
	return b.MockBackend.Create(ctx, spec)
}

func (b *wrapBackend) Start(ctx context.Context, id protocol.RuntimeID) error {
	b.mu.Lock()
	b.startCalls++
	b.mu.Unlock()
	if b.startErr != nil {
		return b.startErr
	}
	return b.MockBackend.Start(ctx, id)
}

func (b *wrapBackend) counts() (create, start int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.createCalls, b.startCalls
}

// ---- 測試 ----

func TestServer_Unauthorized(t *testing.T) {
	hs := newTestServer(t, NewMockBackend())

	// 無 token。
	status, data := request(t, hs, http.MethodGet, apiBase+"/health", "", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("no token: status=%d, body=%s", status, data)
	}
	if ae := mustAPIError(t, data); ae.Code != protocol.ErrUnauthorized {
		t.Fatalf("no token: code=%s", ae.Code)
	}

	// 錯誤 token。
	status, data = request(t, hs, http.MethodGet, apiBase+"/health", "wrong-token", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong token: status=%d", status)
	}
	if ae := mustAPIError(t, data); ae.Code != protocol.ErrUnauthorized {
		t.Fatalf("wrong token: code=%s", ae.Code)
	}
}

func TestServer_HealthAuthorized(t *testing.T) {
	hs := newTestServer(t, NewMockBackend())
	status, data := request(t, hs, http.MethodGet, apiBase+"/health", testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var hr protocol.HealthResponse
	if err := json.Unmarshal(data, &hr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if hr.Status != "ok" || hr.Version != protocol.AgentAPIVersion {
		t.Fatalf("unexpected health: %+v", hr)
	}
}

func TestServer_CreateListStatus_UUIDRouting(t *testing.T) {
	hs := newTestServer(t, NewMockBackend())
	const uuid = "11111111-2222-3333-4444-555555555555"

	// POST /instances(帶正確 token → 成功)。
	status, data := request(t, hs, http.MethodPost, apiBase+"/instances", testToken, "", protocol.CreateInstanceRequest{Spec: specWithUUID(uuid)})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", status, data)
	}
	var cr protocol.CreateInstanceResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	if cr.UUID != uuid || cr.RuntimeID == "" {
		t.Fatalf("unexpected create response: %+v", cr)
	}

	// GET /instances 列出。
	status, data = request(t, hs, http.MethodGet, apiBase+"/instances", testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	var lr protocol.ListInstancesResponse
	if err := json.Unmarshal(data, &lr); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(lr.Instances) != 1 || lr.Instances[0].UUID != uuid {
		t.Fatalf("unexpected list: %+v", lr.Instances)
	}

	// GET /instances/{uuid}/status —— 以 UUID 路由解析為 runtime。
	status, data = request(t, hs, http.MethodGet, apiBase+"/instances/"+uuid+"/status", testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status status=%d body=%s", status, data)
	}
	var rs protocol.RuntimeStatus
	if err := json.Unmarshal(data, &rs); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if rs.ID != cr.RuntimeID {
		t.Fatalf("status runtime id=%s want %s", rs.ID, cr.RuntimeID)
	}
}

func TestServer_NotFound(t *testing.T) {
	hs := newTestServer(t, NewMockBackend())
	status, data := request(t, hs, http.MethodGet, apiBase+"/instances/does-not-exist/status", testToken, "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ae := mustAPIError(t, data); ae.Code != protocol.ErrNotFound {
		t.Fatalf("code=%s", ae.Code)
	}
}

func TestServer_IdempotencyReplay(t *testing.T) {
	backend := newWrapBackend()
	hs := newTestServer(t, backend)
	const uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// 先建立實例(不帶冪等鍵)。
	if status, data := request(t, hs, http.MethodPost, apiBase+"/instances", testToken, "", protocol.CreateInstanceRequest{Spec: specWithUUID(uuid)}); status != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", status, data)
	}

	path := apiBase + "/instances/" + uuid + "/start"

	// 首次 start 帶 Idempotency-Key。
	status1, _ := request(t, hs, http.MethodPost, path, testToken, "key-1", nil)
	if status1 != http.StatusNoContent {
		t.Fatalf("first start status=%d", status1)
	}
	// 同鍵重播 → 回原結果、不重複執行。
	status2, _ := request(t, hs, http.MethodPost, path, testToken, "key-1", nil)
	if status2 != http.StatusNoContent {
		t.Fatalf("replay start status=%d", status2)
	}
	if _, startCalls := backend.counts(); startCalls != 1 {
		t.Fatalf("same idempotency key executed backend %d times, want 1", startCalls)
	}

	// 不同鍵 → 重新執行。
	status3, _ := request(t, hs, http.MethodPost, path, testToken, "key-2", nil)
	if status3 != http.StatusNoContent {
		t.Fatalf("new key start status=%d", status3)
	}
	if _, startCalls := backend.counts(); startCalls != 2 {
		t.Fatalf("distinct idempotency key: startCalls=%d, want 2", startCalls)
	}
}

func TestServer_IdempotencyReplaysBody(t *testing.T) {
	backend := newWrapBackend()
	hs := newTestServer(t, backend)

	body := protocol.CreateInstanceRequest{Spec: specWithUUID("dup-uuid")}
	s1, d1 := request(t, hs, http.MethodPost, apiBase+"/instances", testToken, "ck", body)
	s2, d2 := request(t, hs, http.MethodPost, apiBase+"/instances", testToken, "ck", body)
	if s1 != http.StatusCreated || s2 != http.StatusCreated {
		t.Fatalf("status1=%d status2=%d", s1, s2)
	}
	if !bytes.Equal(d1, d2) {
		t.Fatalf("replay body mismatch:\n first=%s\n replay=%s", d1, d2)
	}
	if createCalls, _ := backend.counts(); createCalls != 1 {
		t.Fatalf("createCalls=%d, want 1", createCalls)
	}
}

func TestServer_ErrorCodeMapping(t *testing.T) {
	t.Run("port_conflict", func(t *testing.T) {
		backend := newWrapBackend()
		backend.createErr = ErrPortConflict
		hs := newTestServer(t, backend)
		status, data := request(t, hs, http.MethodPost, apiBase+"/instances", testToken, "", protocol.CreateInstanceRequest{Spec: specWithUUID("x")})
		if status != http.StatusConflict {
			t.Fatalf("status=%d body=%s", status, data)
		}
		if ae := mustAPIError(t, data); ae.Code != protocol.ErrPortConflict {
			t.Fatalf("code=%s", ae.Code)
		}
	})

	t.Run("locked", func(t *testing.T) {
		backend := newWrapBackend()
		backend.startErr = ErrLocked
		hs := newTestServer(t, backend)
		const uuid = "locked-uuid"
		if _, err := backend.MockBackend.Create(context.Background(), specWithUUID(uuid)); err != nil {
			t.Fatalf("seed create: %v", err)
		}
		status, data := request(t, hs, http.MethodPost, apiBase+"/instances/"+uuid+"/start", testToken, "", nil)
		if status != http.StatusLocked {
			t.Fatalf("status=%d body=%s", status, data)
		}
		if ae := mustAPIError(t, data); ae.Code != protocol.ErrLocked {
			t.Fatalf("code=%s", ae.Code)
		}
	})
}

func TestServer_WSEventsInjection(t *testing.T) {
	backend := NewMockBackend()
	hs := newTestServer(t, backend)

	rid, err := backend.Create(context.Background(), specWithUUID("evt-uuid"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 先注入(cursor=1),再以 since=0 訂閱補送,避免訂閱/注入競態。
	backend.InjectDie(rid, 42)

	conn, _, err := dialWS(t, hs, apiBase+"/events?since=0", testToken, "http://127.0.0.1")
	if err != nil {
		t.Fatalf("dial events ws: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ev protocol.RuntimeEvent
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if ev.Kind != protocol.RuntimeEventDie {
		t.Fatalf("event kind=%s want die", ev.Kind)
	}
	if ev.ExitCode == nil || *ev.ExitCode != 42 {
		t.Fatalf("event exit code=%v want 42", ev.ExitCode)
	}
	if ev.ID != rid {
		t.Fatalf("event id=%s want %s", ev.ID, rid)
	}
}

func TestServer_WSBadOrigin(t *testing.T) {
	hs := newTestServer(t, NewMockBackend())
	// 有效 token,但非 loopback Origin → 拒絕(升級前 403)。
	conn, resp, err := dialWS(t, hs, apiBase+"/events", testToken, "http://evil.example.com")
	if err == nil {
		conn.Close()
		t.Fatal("expected bad-origin dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got resp=%v", resp)
	}
}

func TestServer_WSUnauthorized(t *testing.T) {
	hs := newTestServer(t, NewMockBackend())
	conn, resp, err := dialWS(t, hs, apiBase+"/events", "", "http://127.0.0.1")
	if err == nil {
		conn.Close()
		t.Fatal("expected unauthenticated dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got resp=%v", resp)
	}
}

func TestServer_WSStats(t *testing.T) {
	backend := NewMockBackend()
	hs := newTestServer(t, backend)
	const uuid = "stats-uuid"
	if _, err := backend.Create(context.Background(), specWithUUID(uuid)); err != nil {
		t.Fatalf("create: %v", err)
	}

	conn, _, err := dialWS(t, hs, apiBase+"/instances/"+uuid+"/stats", testToken, "http://127.0.0.1")
	if err != nil {
		t.Fatalf("dial stats ws: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var stats protocol.ResourceStats
	if err := conn.ReadJSON(&stats); err != nil {
		t.Fatalf("read stats: %v", err)
	}
	if stats.MemoryBytes == 0 {
		t.Fatalf("expected non-zero stats, got %+v", stats)
	}
}

func TestServer_WSLogs(t *testing.T) {
	backend := NewMockBackend()
	hs := newTestServer(t, backend)
	const uuid = "logs-uuid"
	if _, err := backend.Create(context.Background(), specWithUUID(uuid)); err != nil {
		t.Fatalf("create: %v", err)
	}

	conn, _, err := dialWS(t, hs, apiBase+"/instances/"+uuid+"/logs", testToken, "http://127.0.0.1")
	if err != nil {
		t.Fatalf("dial logs ws: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var line LogLine
	if err := conn.ReadJSON(&line); err != nil {
		t.Fatalf("read log line: %v", err)
	}
	if line.Stream != "stdout" || line.Line == "" {
		t.Fatalf("unexpected log line: %+v", line)
	}
}

// 5xx(伺服器端暫時性失敗)不快取:同鍵下一次請求應重新執行,不卡整個 TTL;
// 2xx 成功仍重播不重執。
func TestServer_IdempotencyDoesNotCache5xx(t *testing.T) {
	backend := newWrapBackend()
	backend.startErr = errors.New("transient backend failure") // 映射為 500
	hs := newTestServer(t, backend)
	const uuid = "5xx-uuid"
	if _, err := backend.MockBackend.Create(context.Background(), specWithUUID(uuid)); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	path := apiBase + "/instances/" + uuid + "/start"

	// 首次:回 500。
	if s1, _ := request(t, hs, http.MethodPost, path, testToken, "k5xx", nil); s1 != http.StatusInternalServerError {
		t.Fatalf("first start status=%d want 500", s1)
	}
	// 同鍵第二次:5xx 未快取 → 重新執行(startCalls 累加)。
	if s2, _ := request(t, hs, http.MethodPost, path, testToken, "k5xx", nil); s2 != http.StatusInternalServerError {
		t.Fatalf("second start status=%d want 500", s2)
	}
	if _, sc := backend.counts(); sc != 2 {
		t.Fatalf("5xx 不應被快取: startCalls=%d want 2", sc)
	}

	// 對照:成功(2xx)仍重播,不重新執行。
	backend.startErr = nil
	if s3, _ := request(t, hs, http.MethodPost, path, testToken, "kok", nil); s3 != http.StatusNoContent {
		t.Fatalf("ok start status=%d want 204", s3)
	}
	_, scAfterOK := backend.counts()
	if s4, _ := request(t, hs, http.MethodPost, path, testToken, "kok", nil); s4 != http.StatusNoContent {
		t.Fatalf("replay ok start status=%d want 204", s4)
	}
	if _, sc := backend.counts(); sc != scAfterOK {
		t.Fatalf("2xx 應重播不重執: startCalls=%d want %d", sc, scAfterOK)
	}
}

func TestServer_IdempotencyTTLExpiry(t *testing.T) {
	backend := newWrapBackend()
	s, err := NewServer(Config{Backend: backend, Token: testToken, IdempotencyTTL: time.Minute})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// 注入可控時鐘。
	fake := time.Now()
	s.idem.now = func() time.Time { return fake }
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() { hs.Close(); _ = s.Close() })

	const uuid = "ttl-uuid"
	if _, err := backend.MockBackend.Create(context.Background(), specWithUUID(uuid)); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	path := apiBase + "/instances/" + uuid + "/start"

	request(t, hs, http.MethodPost, path, testToken, "ttlkey", nil)
	request(t, hs, http.MethodPost, path, testToken, "ttlkey", nil) // 窗口內重播
	if _, sc := backend.counts(); sc != 1 {
		t.Fatalf("within TTL: startCalls=%d want 1", sc)
	}

	// 時間前進超過 TTL → 同鍵可重用(重新執行)。
	fake = fake.Add(2 * time.Minute)
	request(t, hs, http.MethodPost, path, testToken, "ttlkey", nil)
	if _, sc := backend.counts(); sc != 2 {
		t.Fatalf("after TTL: startCalls=%d want 2", sc)
	}
}
