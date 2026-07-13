package agent

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"

	"servermonitor/internal/protocol"
)

const wsWriteTimeout = 10 * time.Second

// wsUpgrader 無 per-server 狀態(Origin 檢查為純函式),故以套件層變數共用。
var wsUpgrader = websocket.Upgrader{
	CheckOrigin:      checkLoopbackOrigin,
	HandshakeTimeout: 10 * time.Second,
}

// checkLoopbackOrigin 允許無 Origin(非瀏覽器客戶端如核心不送 Origin)或 Origin 主機為 loopback
// 的握手;其餘拒絕(防跨站 WebSocket 劫持,R5/安全)。
func checkLoopbackOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serveWS 檢查 Origin、升級連線,並以「讀取幫浦偵測關閉、baseCtx 綁 server 生命週期」的 ctx
// 執行推送迴圈 run。run 返回即關閉連線。
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request, run func(ctx context.Context, conn *websocket.Conn)) {
	if !checkLoopbackOrigin(r) {
		writeError(w, http.StatusForbidden, protocol.APIError{
			Code: protocol.ErrUnauthorized, Message: "websocket origin not allowed",
		})
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade 失敗時已寫回 HTTP 錯誤
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel()
	// 讀取幫浦:消化控制訊息並在客戶端關閉/錯誤時取消 ctx,收束推送迴圈。
	go func() {
		for {
			if _, _, rerr := conn.ReadMessage(); rerr != nil {
				cancel()
				return
			}
		}
	}()
	run(ctx, conn)
}

// handleStatsWS 週期推送資源取樣(R6)。
func (s *Server) handleStatsWS(w http.ResponseWriter, r *http.Request) {
	rid, err := s.resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.serveWS(w, r, func(ctx context.Context, conn *websocket.Conn) { s.pumpStats(ctx, conn, rid) })
}

func (s *Server) pumpStats(ctx context.Context, conn *websocket.Conn, rid protocol.RuntimeID) {
	ticker := time.NewTicker(s.statsInterval)
	defer ticker.Stop()
	for {
		stats, err := s.backend.Stats(ctx, rid)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			writeWSError(conn, err)
			return
		}
		if err := writeJSONFrame(conn, stats); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// handleLogsWS 轉發即時 log 串流(R6)。
func (s *Server) handleLogsWS(w http.ResponseWriter, r *http.Request) {
	rid, err := s.resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.serveWS(w, r, func(ctx context.Context, conn *websocket.Conn) { s.pumpLogs(ctx, conn, rid) })
}

func (s *Server) pumpLogs(ctx context.Context, conn *websocket.Conn, rid protocol.RuntimeID) {
	stream, err := s.backend.Logs(ctx, rid, LogOpts{Follow: true})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		writeWSError(conn, err)
		return
	}
	defer stream.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-stream.Lines():
			if !ok {
				return
			}
			if err := writeJSONFrame(conn, line); err != nil {
				return
			}
		}
	}
}

// handleEventsWS 轉發執行事件串流(R8/R13);?since=<cursor> 支援 reconnect 對帳。
func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	since := protocol.Cursor(r.URL.Query().Get("since"))
	s.serveWS(w, r, func(ctx context.Context, conn *websocket.Conn) { s.pumpEvents(ctx, conn, since) })
}

func (s *Server) pumpEvents(ctx context.Context, conn *websocket.Conn, since protocol.Cursor) {
	stream, err := s.backend.Events(ctx, since)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		writeWSError(conn, err)
		return
	}
	defer stream.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-stream.Events():
			if !ok {
				return
			}
			if err := writeJSONFrame(conn, ev); err != nil {
				return
			}
		}
	}
}

// ---- WS 寫入輔助 ----

func writeJSONFrame(conn *websocket.Conn, v any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return conn.WriteJSON(v)
}

// writeWSError 在升級後送出一則結構化錯誤 frame,再以 close frame 收束連線。
func writeWSError(conn *websocket.Conn, err error) {
	_, apiErr := apiErrorFor(err)
	_ = writeJSONFrame(conn, apiErr)
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	_ = conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseInternalServerErr, string(apiErr.Code)),
	)
}
