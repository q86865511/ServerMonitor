package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"servermonitor/internal/protocol"
)

// newFakeAgentTLS 起一個假 agent 的 TLS server:對 GET /v1/health 驗 bearer token 後回 HealthResponse;
// token 不符回 401 ERR_UNAUTHORIZED。回傳 server 與其葉憑證 SHA-256 指紋(即客戶端應 pin 的值)。
func newFakeAgentTLS(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(protocol.APIError{Code: protocol.ErrUnauthorized, Message: "bad token"})
			return
		}
		if r.URL.Path == "/"+protocol.AgentAPIVersion+"/health" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(protocol.HealthResponse{Status: "ok", Version: protocol.AgentAPIVersion})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	return ts, CertFingerprint(ts.Certificate().Raw)
}

// TestRemoteNodeClient_PinnedFingerprintOK 驗證 pin 正確指紋時 Health 成功(TLS 自驗通過)。
func TestRemoteNodeClient_PinnedFingerprintOK(t *testing.T) {
	const token = "tok-abc"
	ts, fp := newFakeAgentTLS(t, token)

	c, err := NewRemoteNodeClient(ts.URL, token, NodeClientOptions{TLSFingerprint: fp})
	if err != nil {
		t.Fatalf("NewRemoteNodeClient: %v", err)
	}
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health(pin 正確指紋)應成功: %v", err)
	}
	if h.Version != protocol.AgentAPIVersion {
		t.Errorf("version 非預期: %q", h.Version)
	}
}

// TestRemoteNodeClient_LowercaseColonlessFingerprint 驗證指紋比對大小寫/冒號不敏感。
func TestRemoteNodeClient_LowercaseColonlessFingerprint(t *testing.T) {
	const token = "tok-abc"
	ts, fp := newFakeAgentTLS(t, token)
	munged := strings.ToLower(strings.ReplaceAll(fp, ":", ""))

	c, err := NewRemoteNodeClient(ts.URL, token, NodeClientOptions{TLSFingerprint: munged})
	if err != nil {
		t.Fatalf("NewRemoteNodeClient: %v", err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health(小寫無冒號指紋)應成功: %v", err)
	}
}

// TestRemoteNodeClient_FingerprintMismatch 驗證 pin 錯誤指紋時連線被拒,錯誤可辨識為指紋不符。
func TestRemoteNodeClient_FingerprintMismatch(t *testing.T) {
	const token = "tok-abc"
	ts, _ := newFakeAgentTLS(t, token)
	bogus := "00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF"

	c, err := NewRemoteNodeClient(ts.URL, token, NodeClientOptions{TLSFingerprint: bogus})
	if err != nil {
		t.Fatalf("NewRemoteNodeClient: %v", err)
	}
	_, err = c.Health(context.Background())
	if err == nil {
		t.Fatal("pin 錯誤指紋應連線失敗")
	}
	if !errors.Is(err, ErrNodeFingerprintMismatch) {
		t.Errorf("錯誤應可辨識為指紋不符, got %v", err)
	}
}

// TestRemoteNodeClient_Unauthorized 驗證 token 錯誤時回 ErrNodeUnauthorized(401)。
func TestRemoteNodeClient_Unauthorized(t *testing.T) {
	ts, fp := newFakeAgentTLS(t, "correct-token")

	c, err := NewRemoteNodeClient(ts.URL, "wrong-token", NodeClientOptions{TLSFingerprint: fp})
	if err != nil {
		t.Fatalf("NewRemoteNodeClient: %v", err)
	}
	_, err = c.Health(context.Background())
	if !errors.Is(err, ErrNodeUnauthorized) {
		t.Errorf("token 錯誤應回 ErrNodeUnauthorized, got %v", err)
	}
}

// TestFetchLeafFingerprint 驗證撥測取回的葉憑證指紋與伺服器實際憑證一致(供 TOFU 顯示)。
func TestFetchLeafFingerprint(t *testing.T) {
	ts, fp := newFakeAgentTLS(t, "tok")

	got, err := FetchLeafFingerprint(context.Background(), ts.URL, 0)
	if err != nil {
		t.Fatalf("FetchLeafFingerprint: %v", err)
	}
	if got != fp {
		t.Errorf("指紋不符: got=%s want=%s", got, fp)
	}
}

// TestNewRemoteNodeClient_RejectsPlainHTTP 驗證明文 http 未允許時拒絕建立;允許則放行。
func TestNewRemoteNodeClient_RejectsPlainHTTP(t *testing.T) {
	if _, err := NewRemoteNodeClient("http://192.168.1.5:9444", "tok", NodeClientOptions{}); !errors.Is(err, ErrInsecureHTTPNotAllowed) {
		t.Errorf("明文 http 未允許應回 ErrInsecureHTTPNotAllowed, got %v", err)
	}
	if _, err := NewRemoteNodeClient("http://192.168.1.5:9444", "tok", NodeClientOptions{InsecureHTTP: true}); err != nil {
		t.Errorf("明文 http 已允許不應報錯: %v", err)
	}
}

// TestCertFingerprintFormat 驗證指紋格式為冒號分隔大寫 hex、長度正確(SHA-256=32 bytes)。
func TestCertFingerprintFormat(t *testing.T) {
	fp := CertFingerprint([]byte("hello"))
	parts := strings.Split(fp, ":")
	if len(parts) != 32 {
		t.Fatalf("SHA-256 指紋應為 32 段, got %d (%s)", len(parts), fp)
	}
	if fp != strings.ToUpper(fp) {
		t.Errorf("指紋應為大寫 hex: %s", fp)
	}
}
