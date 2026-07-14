package agent

import (
	"bytes"
	"net/http"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// idempotencyCache 是寫入端點的記憶體冪等快取(R5):以 Idempotency-Key 記錄首次完成的回應,
// 於 TTL 內重播回原結果而不重複執行後端動作;TTL 過期後同鍵可重用。併發安全,且對「同鍵並發」
// 保證僅執行一次(後到者等待首次完成再取回原結果)。now 可注入以測試 TTL。
type idempotencyCache struct {
	mu      sync.Mutex
	entries map[string]*idemEntry
	ttl     time.Duration
	now     func() time.Time
}

type idemEntry struct {
	done    chan struct{}     // 首次執行完成時關閉
	resp    *capturedResponse // 完成後的回應快照
	expires time.Time         // 完成後的到期時間
}

type capturedResponse struct {
	status int
	header http.Header
	body   []byte
}

func newIdempotencyCache(ttl time.Duration) *idempotencyCache {
	return &idempotencyCache{
		entries: make(map[string]*idemEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// begin 取得既有(進行中或未過期已完成)條目並回 first=false,或預約一個新條目回 first=true。
func (c *idempotencyCache) begin(key string) (*idemEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	if e, ok := c.entries[key]; ok {
		return e, false
	}
	e := &idemEntry{done: make(chan struct{})}
	c.entries[key] = e
	return e, true
}

// complete 記錄回應快照、設定到期並喚醒等待者。狀態 >=500 時不留存快取:仍完成 entry
// 讓已在等待的同鍵請求收到本次結果,但隨即從 map 移除,使下一個同鍵請求重新執行——
// 暫時性(伺服器端)失敗不應卡住整個 TTL。4xx 屬語意性失敗,維持快取重播。
func (c *idempotencyCache) complete(key string, e *idemEntry, resp *capturedResponse) {
	c.mu.Lock()
	e.resp = resp
	e.expires = c.now().Add(c.ttl)
	if resp != nil && resp.status >= 500 {
		if c.entries[key] == e {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()
	close(e.done)
}

// purgeLocked 移除已完成且過期的條目(進行中的保留)。呼叫端須持 c.mu。
func (c *idempotencyCache) purgeLocked() {
	now := c.now()
	for k, e := range c.entries {
		select {
		case <-e.done:
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		default:
			// 進行中,保留
		}
	}
}

// idempotent 是寫入端點的中介層:帶 Idempotency-Key 時記錄/重播回應;不帶則照常執行。
func (s *Server) idempotent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(protocol.IdempotencyKeyHeader)
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		// 以 method+path 為快取鍵前綴,避免同一鍵誤用於不同端點時交叉污染回應。
		cacheKey := r.Method + " " + r.URL.Path + "\x00" + key
		entry, first := s.idem.begin(cacheKey)
		if !first {
			// 等待首次完成(或請求取消)後重播原結果。
			select {
			case <-entry.done:
				writeCaptured(w, entry.resp)
			case <-r.Context().Done():
			}
			return
		}
		rec := newResponseRecorder()
		serveSafely(next, rec, r)
		resp := rec.captured()
		s.idem.complete(cacheKey, entry, resp)
		writeCaptured(w, resp)
	})
}

// serveSafely 執行 handler,並把 panic 轉為 500 寫進 recorder,確保冪等條目必被完成、不使等待者卡死。
func serveSafely(h http.Handler, w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rc := recover(); rc != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()
	h.ServeHTTP(w, r)
}

// responseRecorder 緩衝 handler 的回應以供冪等快取;不直寫客戶端。
type responseRecorder struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header), status: http.StatusOK}
}

func (rr *responseRecorder) Header() http.Header { return rr.header }

func (rr *responseRecorder) WriteHeader(code int) {
	if rr.wroteHeader {
		return
	}
	rr.status = code
	rr.wroteHeader = true
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if !rr.wroteHeader {
		rr.WriteHeader(http.StatusOK)
	}
	return rr.body.Write(b)
}

func (rr *responseRecorder) captured() *capturedResponse {
	h := make(http.Header, len(rr.header))
	for k, v := range rr.header {
		h[k] = append([]string(nil), v...)
	}
	return &capturedResponse{
		status: rr.status,
		header: h,
		body:   append([]byte(nil), rr.body.Bytes()...),
	}
}

// writeCaptured 把快取的回應寫給客戶端。
func writeCaptured(w http.ResponseWriter, resp *capturedResponse) {
	if resp == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	h := w.Header()
	for k, v := range resp.header {
		h[k] = v
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.body)
}
