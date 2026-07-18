package core

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// 遠端節點傳輸層錯誤哨符(R5 多節點)。有別於 nodeclient.go 的 API/傳輸哨符,這些是「建立連線
// 之前」的設定/信任判定失敗,供綁定層轉為可辨識的繁中錯誤。
var (
	// ErrNodeFingerprintMismatch 表示對端葉憑證的 SHA-256 指紋與 pin 值不符(TOFU 失敗)。
	ErrNodeFingerprintMismatch = errors.New("core: 節點 TLS 憑證指紋不符")
	// ErrInsecureHTTPNotAllowed 表示 base URL 為明文 http 但未顯式允許 insecure_http。
	ErrInsecureHTTPNotAllowed = errors.New("core: 拒絕明文 HTTP 連線(未允許 insecure_http)")
	// ErrNoPeerCert 表示 TLS 握手未取得對端憑證(理論上不會發生)。
	ErrNoPeerCert = errors.New("core: TLS 握手未取得對端憑證")
)

// defaultNodeConnectTimeout 是遠端節點的連線/TLS 握手逾時(WAN 場景;有別於請求逾時,見
// nodeclient.go 的 defaultNodeHTTPTimeout)。loopback in-process 節點不套此值(其 hc 為預設)。
var defaultNodeConnectTimeout = 10 * time.Second

// NodeClientOptions 是遠端 NodeClient 的傳輸設定(R5 多節點)。
type NodeClientOptions struct {
	// TLSFingerprint 非空:pin 對端葉憑證的 SHA-256 指紋(大小寫/冒號不敏感;自簽 TOFU 模式)。
	// 空且 scheme=https:走系統 CA 驗證(正式憑證)。scheme=http 時忽略。
	TLSFingerprint string
	// InsecureHTTP true 時允許 base URL 為明文 http(僅限內網/VPN;呼叫端須向使用者明示風險)。
	InsecureHTTP bool
	// ConnectTimeout 覆寫連線/握手逾時;<=0 用 defaultNodeConnectTimeout(10s)。
	ConnectTimeout time.Duration
}

// NewRemoteNodeClient 依 opts 建立連往遠端節點代理的 NodeClient(R5 多節點)。相較 loopback 用的
// NewNodeClient,本建構子多做:(1) 依 scheme 與 fingerprint 組出 TLS 設定(指紋 pin 或系統 CA);
// (2) 給 HTTP transport 與 WS dialer 套用連線/握手逾時。明文 http 未顯式允許則回 ErrInsecureHTTPNotAllowed。
func NewRemoteNodeClient(baseURL, token string, opts NodeClientOptions) (*NodeClient, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("解析節點位址失敗: %w", err)
	}
	switch u.Scheme {
	case "https":
		// ok
	case "http":
		if !opts.InsecureHTTP {
			return nil, fmt.Errorf("%w: %s", ErrInsecureHTTPNotAllowed, baseURL)
		}
	default:
		return nil, fmt.Errorf("節點位址須為 http 或 https: %s", baseURL)
	}

	connectTimeout := opts.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultNodeConnectTimeout
	}

	var tlsCfg *tls.Config
	if u.Scheme == "https" && opts.TLSFingerprint != "" {
		tlsCfg = pinnedTLSConfig(opts.TLSFingerprint)
	}

	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSClientConfig:       tlsCfg, // nil=系統 CA 預設驗證
		TLSHandshakeTimeout:   connectTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	c := NewNodeClient(baseURL, token, &http.Client{Transport: transport})
	// WS 走 gorilla dialer;沿用同一 TLS 設定(指紋 pin 或系統 CA)與連線逾時。
	c.wsDialer = &websocket.Dialer{
		NetDialContext:   (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSClientConfig:  tlsCfg,
		HandshakeTimeout: connectTimeout,
	}
	return c, nil
}

// pinnedTLSConfig 回傳「以指紋自驗葉憑證」的 TLS 設定(TOFU 自簽模式):跳過系統 CA 鏈驗證,改以
// VerifyPeerCertificate 比對葉憑證 DER 的 SHA-256 指紋(常數時間、大小寫/冒號不敏感)。
func pinnedTLSConfig(fingerprint string) *tls.Config {
	want := normalizeFingerprint(fingerprint)
	return &tls.Config{
		// 我們以指紋自驗,不依賴系統 CA 對自簽憑證的鏈驗(否則自簽必失敗)。
		InsecureSkipVerify: true, //nolint:gosec // 指紋 pin 取代 CA 鏈,見 VerifyPeerCertificate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return ErrNoPeerCert
			}
			got := normalizeFingerprint(CertFingerprint(rawCerts[0]))
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("%w: 期望 %s,實得 %s", ErrNodeFingerprintMismatch, want, got)
			}
			return nil
		},
	}
}

// FetchLeafFingerprint 撥一條 TLS 連線(不驗證憑證鏈,僅為擷取)取回對端葉憑證的 SHA-256 指紋
// (TOFU 撥測:供 GUI 顯示讓使用者確認)。addr 取自 base URL 的 host[:port](https 預設 443)。
func FetchLeafFingerprint(ctx context.Context, baseURL string, connectTimeout time.Duration) (string, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("解析節點位址失敗: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("僅 https 位址可取回憑證指紋: %s", baseURL)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	if connectTimeout <= 0 {
		connectTimeout = defaultNodeConnectTimeout
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: connectTimeout},
		Config:    &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 僅為擷取指紋,不建立信任
	}
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := dialer.DialContext(dctx, "tcp", host)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNodeUnreachable, err)
	}
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return "", ErrNoPeerCert
	}
	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", ErrNoPeerCert
	}
	return CertFingerprint(certs[0].Raw), nil
}

// CertFingerprint 回傳一份 DER 憑證的 SHA-256 指紋,格式為冒號分隔大寫 hex(如 "AB:CD:...")。
// 與 cmd/agent 啟動時印出的格式一致,便於使用者肉眼比對。
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	var sb strings.Builder
	sb.Grow(len(sum)*3 - 1)
	const hexDigits = "0123456789ABCDEF"
	for i, b := range sum {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteByte(hexDigits[b>>4])
		sb.WriteByte(hexDigits[b&0x0f])
	}
	return sb.String()
}

// normalizeFingerprint 正規化指紋以供比對:去除冒號與空白、轉大寫。使 "ab:cd" 與 "ABCD" 視為相等。
func normalizeFingerprint(s string) string {
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, " ", "")
	return strings.ToUpper(strings.TrimSpace(s))
}
