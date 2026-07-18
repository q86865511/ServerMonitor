package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOrCreateToken_FirstAndSecond 驗證首啟生成、二次啟動沿用同一 token。
func TestLoadOrCreateToken_FirstAndSecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")

	tok1, gen1, err := loadOrCreateToken(path)
	if err != nil {
		t.Fatalf("首次: %v", err)
	}
	if !gen1 {
		t.Error("首次應標記為新生成")
	}
	if len(tok1) < 32 {
		t.Errorf("token 過短: %q", tok1)
	}

	tok2, gen2, err := loadOrCreateToken(path)
	if err != nil {
		t.Fatalf("二次: %v", err)
	}
	if gen2 {
		t.Error("二次不應再生成")
	}
	if tok1 != tok2 {
		t.Errorf("token 二次應不變: %q vs %q", tok1, tok2)
	}
}

// TestLoadOrCreateCert_AutoStable 驗證 auto 首啟生成自簽憑證、二次啟動載入同一憑證(指紋不變)。
func TestLoadOrCreateCert_AutoStable(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")

	cert1, fp1, gen1, err := loadOrCreateCert(tlsModeAuto, certFile, keyFile)
	if err != nil {
		t.Fatalf("首次: %v", err)
	}
	if !gen1 {
		t.Error("首次應生成")
	}
	if fp1 == "" || len(cert1.Certificate) == 0 {
		t.Fatal("應回有效憑證與指紋")
	}
	if strings.Count(fp1, ":") != 31 {
		t.Errorf("指紋格式非預期(應 32 段): %s", fp1)
	}

	_, fp2, gen2, err := loadOrCreateCert(tlsModeAuto, certFile, keyFile)
	if err != nil {
		t.Fatalf("二次: %v", err)
	}
	if gen2 {
		t.Error("二次不應再生成(既有憑證應載入)")
	}
	if fp1 != fp2 {
		t.Errorf("指紋二次應不變: %s vs %s", fp1, fp2)
	}
}

// TestLoadAgentConfig_Defaults 驗證無設定檔時套用預設值並把相對路徑解析為絕對。
func TestLoadAgentConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadAgentConfig(filepath.Join(dir, "agent.toml"))
	if err != nil {
		t.Fatalf("loadAgentConfig: %v", err)
	}
	if cfg.Bind != defaultBind {
		t.Errorf("預設 bind 非預期: %s", cfg.Bind)
	}
	if cfg.TLS.Mode != tlsModeAuto {
		t.Errorf("預設 tls.mode 應為 auto: %s", cfg.TLS.Mode)
	}
	if !filepath.IsAbs(cfg.DataRoot) || !filepath.IsAbs(cfg.TokenFile) {
		t.Errorf("路徑應解析為絕對: dataRoot=%s tokenFile=%s", cfg.DataRoot, cfg.TokenFile)
	}
	if filepath.Dir(cfg.DataRoot) != dir {
		t.Errorf("相對路徑應相對設定檔目錄: %s", cfg.DataRoot)
	}
}
