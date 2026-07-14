package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// ---- 假 RCON server(測試內以標準庫實作 Source RCON 協定)----

type fakeRconMode int

const (
	fakeRconNormal      fakeRconMode = iota // 正常認證 + 單封包回顯
	fakeRconTimeout                         // 讀取 AUTH 後不回應、不關閉,逼 adapter 逾時
	fakeRconMultiPacket                     // exec 回應拆成 3 個封包(同 execID)再回哨兵 echo
	fakeRconLarge                           // exec 回應總長 >8KB,拆成多個 ≤4KB 封包
	fakeRconBadAuthID                       // AUTH_RESPONSE 回非 authID 也非 -1 的 id → 協定錯誤
)

type fakeRcon struct {
	ln       net.Listener
	password string
	mode     fakeRconMode

	mu    sync.Mutex
	execs []string
}

func startFakeRcon(t *testing.T, password string, mode fakeRconMode) *fakeRcon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRcon{ln: ln, password: password, mode: mode}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeRcon) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

// splitN 把 s 切成 n 段(近乎等長),供多封包測試把回應拆成數個封包。
func splitN(s string, n int) []string {
	if n <= 1 || len(s) < n {
		return []string{s}
	}
	size := (len(s) + n - 1) / n
	var parts []string
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		parts = append(parts, s[i:end])
	}
	return parts
}

func (f *fakeRcon) lastExec() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execs) == 0 {
		return "", false
	}
	return f.execs[len(f.execs)-1], true
}

func (f *fakeRcon) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRcon) handle(conn net.Conn) {
	defer conn.Close()
	for {
		id, typ, body, err := readRconPacket(conn)
		if err != nil {
			return
		}
		switch typ {
		case rconTypeAuth:
			if f.mode == fakeRconTimeout {
				// 不回應:阻塞至 adapter 逾時關閉連線,readRconPacket 隨即回錯結束。
				_, _, _, _ = readRconPacket(conn)
				return
			}
			_ = writeRconPacket(conn, id, rconTypeResponse, "") // 前導空回應
			if f.mode == fakeRconBadAuthID {
				// 回一個既非 authID(=id)也非 -1 的 id,測試 adapter 的協定核對。
				_ = writeRconPacket(conn, id+100, rconTypeAuthResponse, "")
				continue
			}
			respID := id
			if body != f.password {
				respID = rconAuthFailID
			}
			_ = writeRconPacket(conn, respID, rconTypeAuthResponse, "") // 認證結果
		case rconTypeExecCommand:
			f.mu.Lock()
			f.execs = append(f.execs, body)
			f.mu.Unlock()
			switch f.mode {
			case fakeRconMultiPacket:
				// 把回應拆成 3 個封包(同 execID),測試哨兵法聚合。
				full := "There are 0 of a max of 20 players online: [" + body + "]"
				for _, part := range splitN(full, 3) {
					_ = writeRconPacket(conn, id, rconTypeResponse, part)
				}
			case fakeRconLarge:
				// 回應總長 >8KB(3×3500=10500),每封包 ≤4KB,測試大回應聚合。
				chunk := strings.Repeat("A", 3500)
				for i := 0; i < 3; i++ {
					_ = writeRconPacket(conn, id, rconTypeResponse, chunk)
				}
			default:
				_ = writeRconPacket(conn, id, rconTypeResponse, "There are 0 of a max of 20 players online: ["+body+"]")
			}
		case rconTypeResponse:
			// 哨兵請求(空 body、id=sentinelID)→ echo 回同 id 的空回應,標誌 exec 回應完結。
			_ = writeRconPacket(conn, id, rconTypeResponse, "")
		}
	}
}

func TestRconAdapter_ListEcho(t *testing.T) {
	f := startFakeRcon(t, "secret", fakeRconNormal)
	a := RconAdapter{Timeout: 2 * time.Second}
	target := protocol.CommandTarget{Kind: "rcon", Host: "127.0.0.1", Port: f.port(), Password: "secret"}

	res, err := a.Send(context.Background(), target, protocol.GameCommand{Raw: "list"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !res.Success {
		t.Fatalf("期望 success,得 %+v", res)
	}
	if !strings.Contains(res.Output, "list") {
		t.Fatalf("回顯應含指令 list,得 %q", res.Output)
	}
	if got, _ := f.lastExec(); got != "list" {
		t.Fatalf("server 應收到 list,得 %q", got)
	}
}

func TestRconAdapter_AuthFailure(t *testing.T) {
	f := startFakeRcon(t, "correct", fakeRconNormal)
	a := RconAdapter{Timeout: 2 * time.Second}
	target := protocol.CommandTarget{Kind: "rcon", Host: "127.0.0.1", Port: f.port(), Password: "wrong"}

	_, err := a.Send(context.Background(), target, protocol.GameCommand{Raw: "list"})
	if err == nil {
		t.Fatalf("密碼錯誤應回錯誤")
	}
	if !strings.Contains(err.Error(), "認證") {
		t.Fatalf("錯誤應指認證失敗,得 %v", err)
	}
}

func TestRconAdapter_ConnectionRefused(t *testing.T) {
	// 開一個 listener 取得空閒埠後立即關閉,使該埠無人監聽 → 連線被拒。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	a := RconAdapter{Timeout: time.Second}
	target := protocol.CommandTarget{Kind: "rcon", Host: "127.0.0.1", Port: port, Password: "x"}

	done := make(chan error, 1)
	go func() {
		_, e := a.Send(context.Background(), target, protocol.GameCommand{Raw: "list"})
		done <- e
	}()
	select {
	case e := <-done:
		if e == nil {
			t.Fatalf("連線被拒應回錯誤")
		}
		if !strings.Contains(e.Error(), "連線") {
			t.Fatalf("錯誤應指連線失敗,得 %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("連線被拒不應掛死")
	}
}

func TestRconAdapter_Timeout(t *testing.T) {
	f := startFakeRcon(t, "secret", fakeRconTimeout)
	a := RconAdapter{Timeout: 200 * time.Millisecond}
	target := protocol.CommandTarget{Kind: "rcon", Host: "127.0.0.1", Port: f.port(), Password: "secret"}

	done := make(chan error, 1)
	go func() {
		_, e := a.Send(context.Background(), target, protocol.GameCommand{Raw: "list"})
		done <- e
	}()
	select {
	case e := <-done:
		if e == nil {
			t.Fatalf("無回應應逾時回錯誤")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("逾時保護失效:Send 掛死")
	}
}

func TestRconAdapter_MultiPacketAggregation(t *testing.T) {
	f := startFakeRcon(t, "secret", fakeRconMultiPacket)
	a := RconAdapter{Timeout: 2 * time.Second}
	target := protocol.CommandTarget{Kind: "rcon", Host: "127.0.0.1", Port: f.port(), Password: "secret"}

	res, err := a.Send(context.Background(), target, protocol.GameCommand{Raw: "list"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// 3 個封包串接後應還原完整回應(哨兵前的多封包全數聚合)。
	want := "There are 0 of a max of 20 players online: [list]"
	if res.Output != want {
		t.Fatalf("多封包聚合結果不符\n want %q\n got  %q", want, res.Output)
	}
}

func TestRconAdapter_LargeResponse(t *testing.T) {
	f := startFakeRcon(t, "secret", fakeRconLarge)
	a := RconAdapter{Timeout: 2 * time.Second}
	target := protocol.CommandTarget{Kind: "rcon", Host: "127.0.0.1", Port: f.port(), Password: "secret"}

	res, err := a.Send(context.Background(), target, protocol.GameCommand{Raw: "list"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(res.Output) != 3*3500 {
		t.Fatalf("大回應應完整聚合 %d bytes,得 %d", 3*3500, len(res.Output))
	}
	if len(res.Output) <= 8<<10 {
		t.Fatalf("測試前提:回應應 >8KB,得 %d", len(res.Output))
	}
}

func TestRconAdapter_AuthBadID(t *testing.T) {
	f := startFakeRcon(t, "secret", fakeRconBadAuthID)
	a := RconAdapter{Timeout: 2 * time.Second}
	target := protocol.CommandTarget{Kind: "rcon", Host: "127.0.0.1", Port: f.port(), Password: "secret"}

	_, err := a.Send(context.Background(), target, protocol.GameCommand{Raw: "list"})
	if err == nil {
		t.Fatalf("AUTH_RESPONSE id 不符應回協定錯誤")
	}
	if !strings.Contains(err.Error(), "id 不符") {
		t.Fatalf("錯誤應指 id 不符,得 %v", err)
	}
}

// ---- 假 Palworld REST server(httptest,依 spike 端點)----

type restCapture struct {
	mu       sync.Mutex
	authUser string
	authPass string
	authOK   bool
	method   string
	path     string
	body     map[string]string
}

func restTarget(t *testing.T, hs *httptest.Server, actions []protocol.RestActionSpec) protocol.CommandTarget {
	t.Helper()
	u, err := url.Parse(hs.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)
	return protocol.CommandTarget{
		Kind:     "rest",
		Host:     host,
		Port:     port,
		Username: "admin",
		Password: "adminpw",
		Actions:  actions,
	}
}

func palworldActions() []protocol.RestActionSpec {
	return []protocol.RestActionSpec{
		{ActionID: "players", Method: "GET", Path: "/v1/api/players"},
		{ActionID: "announce", Method: "POST", Path: "/v1/api/announce"},
	}
}

func TestPalworldRestAdapter_Players(t *testing.T) {
	cap := &restCapture{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		cap.mu.Lock()
		cap.authUser, cap.authPass, cap.authOK = u, p, ok
		cap.method, cap.path = r.Method, r.URL.Path
		cap.mu.Unlock()
		_, _ = w.Write([]byte(`{"players":[]}`))
	}))
	t.Cleanup(hs.Close)

	a := PalworldRestAdapter{Timeout: 2 * time.Second}
	res, err := a.Send(context.Background(), restTarget(t, hs, palworldActions()),
		protocol.GameCommand{ActionID: "players"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !res.Success || !strings.Contains(res.Output, "players") {
		t.Fatalf("players 回應異常: %+v", res)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if !cap.authOK || cap.authUser != "admin" || cap.authPass != "adminpw" {
		t.Fatalf("Basic Auth 標頭錯誤: ok=%v user=%q pass=%q", cap.authOK, cap.authUser, cap.authPass)
	}
	if cap.method != http.MethodGet || cap.path != "/v1/api/players" {
		t.Fatalf("method/path 錯誤: %s %s", cap.method, cap.path)
	}
}

func TestPalworldRestAdapter_Announce(t *testing.T) {
	cap := &restCapture{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		cap.mu.Lock()
		cap.authUser, cap.authPass, cap.authOK = u, p, ok
		cap.method, cap.path, cap.body = r.Method, r.URL.Path, body
		cap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hs.Close)

	a := PalworldRestAdapter{Timeout: 2 * time.Second}
	_, err := a.Send(context.Background(), restTarget(t, hs, palworldActions()),
		protocol.GameCommand{ActionID: "announce", Args: map[string]string{"message": "restart in 5m"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if !cap.authOK || cap.authUser != "admin" || cap.authPass != "adminpw" {
		t.Fatalf("Basic Auth 標頭錯誤: ok=%v user=%q", cap.authOK, cap.authUser)
	}
	if cap.method != http.MethodPost || cap.path != "/v1/api/announce" {
		t.Fatalf("method/path 錯誤: %s %s", cap.method, cap.path)
	}
	if cap.body["message"] != "restart in 5m" {
		t.Fatalf("announce body 未帶 message: %+v", cap.body)
	}
}

func TestPalworldRestAdapter_UnknownAction(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("不存在的動作不應發出 HTTP 請求")
	}))
	t.Cleanup(hs.Close)

	a := PalworldRestAdapter{Timeout: time.Second}
	_, err := a.Send(context.Background(), restTarget(t, hs, palworldActions()),
		protocol.GameCommand{ActionID: "nope"})
	if err == nil {
		t.Fatalf("不存在的動作應回錯誤")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("錯誤應指動作不存在,得 %v", err)
	}
}

func TestPalworldRestAdapter_ErrorStatus(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(hs.Close)

	a := PalworldRestAdapter{Timeout: time.Second}
	_, err := a.Send(context.Background(), restTarget(t, hs, palworldActions()),
		protocol.GameCommand{ActionID: "players"})
	if err == nil {
		t.Fatalf("非 2xx 應回錯誤")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("錯誤應含狀態碼,得 %v", err)
	}
}

func TestPalworldRestAdapter_LargeBody(t *testing.T) {
	// 回應 >4096(舊上限)的 body,驗證完整讀取不截斷。
	const n = 20000
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("A", n)))
	}))
	t.Cleanup(hs.Close)

	a := PalworldRestAdapter{Timeout: 2 * time.Second}
	res, err := a.Send(context.Background(), restTarget(t, hs, palworldActions()),
		protocol.GameCommand{ActionID: "players"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(res.Output) != n {
		t.Fatalf("大 body 應完整讀取 %d bytes,得 %d", n, len(res.Output))
	}
}

func TestPalworldRestAdapter_BodyTooLarge(t *testing.T) {
	// 回應超過 restMaxBody(1MiB)→ 明確錯誤,不回半截 JSON。
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("A", (1<<20)+1024)))
	}))
	t.Cleanup(hs.Close)

	a := PalworldRestAdapter{Timeout: 2 * time.Second}
	_, err := a.Send(context.Background(), restTarget(t, hs, palworldActions()),
		protocol.GameCommand{ActionID: "players"})
	if err == nil {
		t.Fatalf("超過上限的 body 應回錯誤")
	}
	if !strings.Contains(err.Error(), "過長") {
		t.Fatalf("錯誤應指回應過長,得 %v", err)
	}
}

func TestCommandDispatcher_SelectsByKind(t *testing.T) {
	d := NewCommandDispatcher()

	// 未知/未啟用(空)kind → 明確錯誤(legacy 協定在 core 端已不選,永不帶 kind 到此)。
	_, err := d.Send(context.Background(), protocol.CommandTarget{Kind: ""}, protocol.GameCommand{})
	if err == nil || !strings.Contains(err.Error(), "未知或未啟用") {
		t.Fatalf("空 kind 應回明確錯誤,得 %v", err)
	}
	_, err = d.Send(context.Background(), protocol.CommandTarget{Kind: "smtp"}, protocol.GameCommand{})
	if err == nil {
		t.Fatalf("未知 kind 應回錯誤")
	}
}
