package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"servermonitor/internal/core"
)

// tokenHexLen 是持久 token 的十六進位長度(32 bytes → 64 hex chars)。載入時據此驗證,避免半寫入
// (首啟生成途中斷電)留下的低熵殘檔被當成有效 token 沿用而遭暴力破解。
const tokenHexLen = 64

// validTokenHex 回報字串是否為長度正確的十六進位 token(排除半寫入/被竄改的殘檔)。
func validTokenHex(t string) bool {
	if len(t) != tokenHexLen {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

// loadOrCreateToken 讀取持久 bearer token;不存在、空、或格式不合法(半寫入殘檔)則以 crypto/rand
// 生成 32 bytes(hex 編碼)、原子寫檔(temp+rename)並收斂權限(0600;Windows 不轉 ACL,見 secureFilePerm)。
// 既有合法 token 於 unix 上一併盡力收斂權限。回傳 token 與是否為本次新生成(供啟動訊息提示)。
func loadOrCreateToken(path string) (token string, generated bool, err error) {
	data, rerr := os.ReadFile(path)
	if rerr == nil {
		if t := strings.TrimSpace(string(data)); validTokenHex(t) {
			secureFilePerm(path) // 既有檔盡力收斂 0600(unix);OS.WriteFile 不改既存檔權限
			return t, false, nil
		}
		// 檔存在但內容不合法(空/半寫入/被竄改):不沿用,重新生成覆蓋。
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return "", false, fmt.Errorf("讀取 token 檔失敗: %w", rerr)
	}

	var raw [32]byte
	if _, gerr := rand.Read(raw[:]); gerr != nil {
		return "", false, fmt.Errorf("生成 token 失敗: %w", gerr)
	}
	token = hex.EncodeToString(raw[:])
	if werr := atomicWriteFile(path, []byte(token+"\n"), 0o600); werr != nil {
		return "", false, fmt.Errorf("寫入 token 檔失敗: %w", werr)
	}
	return token, true, nil
}

// loadOrCreateCert 依 mode 準備 TLS 憑證:
//   - auto:cert/key 皆存在則載入;否則生成自簽 ECDSA P-256(SAN 含 hostname 與本機 IP,效期 10 年)。
//   - custom:載入 certFile/keyFile(缺檔即錯)。
//
// 回傳憑證、葉憑證 SHA-256 指紋(冒號分隔大寫 hex)、以及是否為本次新生成。mode=off 不呼叫本函式。
func loadOrCreateCert(mode, certFile, keyFile string) (cert tls.Certificate, fingerprint string, generated bool, err error) {
	certExists := fileExists(certFile)
	keyExists := fileExists(keyFile)

	if mode == tlsModeCustom || (certExists && keyExists) {
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return tls.Certificate{}, "", false, fmt.Errorf("載入 TLS 憑證失敗: %w", err)
		}
		if mode != tlsModeCustom {
			secureFilePerm(keyFile) // auto 模式沿用既有私鑰時,於 unix 盡力收斂 0600(custom 由使用者自管)
		}
		fingerprint = leafFingerprint(cert)
		return cert, fingerprint, false, nil
	}

	// auto 且憑證不齊 → 生成自簽。
	certPEM, keyPEM, gerr := generateSelfSignedCert()
	if gerr != nil {
		return tls.Certificate{}, "", false, gerr
	}
	if werr := atomicWriteFile(certFile, certPEM, 0o644); werr != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("寫入憑證檔失敗: %w", werr)
	}
	if werr := atomicWriteFile(keyFile, keyPEM, 0o600); werr != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("寫入私鑰檔失敗: %w", werr)
	}
	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("載入新生成憑證失敗: %w", err)
	}
	return cert, leafFingerprint(cert), true, nil
}

// generateSelfSignedCert 生成自簽 ECDSA P-256 憑證(效期 10 年),SAN 含本機 hostname、localhost
// 與所有本機 IP(loopback + 對外介面),回傳 PEM 編碼的憑證與私鑰。
func generateSelfSignedCert() (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 ECDSA 私鑰失敗: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("生成憑證序號失敗: %w", err)
	}
	host, _ := os.Hostname()
	cn := host
	if cn == "" {
		cn = "servermonitor-agent"
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"ServerMonitor Agent"}},
		NotBefore:             now.Add(-1 * time.Hour), // 容忍些微時鐘偏差
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	tmpl.DNSNames = dedupDNS(host)
	tmpl.IPAddresses = localIPs()

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("建立自簽憑證失敗: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("序列化私鑰失敗: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// dedupDNS 回傳 SAN 的 DNS 名稱清單:恆含 "localhost",主機名非空且相異時併入。
func dedupDNS(host string) []string {
	dns := []string{"localhost"}
	if host != "" && host != "localhost" {
		dns = append(dns, host)
	}
	return dns
}

// localIPs 蒐集本機所有 IP(loopback 恆含 127.0.0.1 與 ::1,另加對外介面位址)供憑證 SAN。
func localIPs() []net.IP {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	seen := map[string]bool{"127.0.0.1": true, "::1": true}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		s := ip.String()
		if !seen[s] {
			seen[s] = true
			ips = append(ips, ip)
		}
	}
	return ips
}

// leafFingerprint 回傳一份 tls.Certificate 葉憑證(Certificate[0] DER)的 SHA-256 指紋,格式與
// core.CertFingerprint(冒號分隔大寫 hex)一致,便於使用者與 GUI 顯示值肉眼比對。
func leafFingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	return core.CertFingerprint(cert.Certificate[0])
}

// fileExists 回報路徑是否為既存的一般檔案。
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// atomicWriteFile 以「寫暫存檔→rename」原子替換目標,避免半寫入殘檔(斷電/磁碟滿)被後續啟動採用。
// 暫存檔與目標同目錄(確保 rename 為同檔系統的原子操作)。perm 於建立暫存檔時套用(unix 生效)。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_ = f.Chmod(perm)
	if _, werr := f.Write(data); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return werr
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return rerr
	}
	return nil
}
