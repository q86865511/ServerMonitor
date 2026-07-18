package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// agentConfig 是獨立節點代理(agent)的設定檔(agent.toml)內容。所有相對路徑於載入時解析為
// 相對設定檔所在目錄的絕對路徑,使「把 agent 執行檔、agent.toml 與資料目錄放同一資料夾」即可運行。
type agentConfig struct {
	Bind       string    `toml:"bind"`        // 監聽位址(host:port);預設 0.0.0.0:9444
	DataRoot   string    `toml:"data_root"`   // 每實例資料根(<data_root>/<uuid>/)
	BackupRoot string    `toml:"backup_root"` // 備份根(與 in-process agent 互通)
	CacheRoot  string    `toml:"cache_root"`  // native 供應共用快取(JRE/SteamCMD)
	Node       string    `toml:"node"`        // gsm.node 標籤值;預設為主機名
	TokenFile  string    `toml:"token_file"`  // 持久 bearer token 檔;不存在則首啟生成
	TLS        tlsConfig `toml:"tls"`
}

// tlsConfig 是 TLS 設定。Mode:
//   - "auto"(預設):自簽 ECDSA P-256 憑證(不存在則生成),SAN 含本機 hostname 與 IP。
//   - "custom":使用 CertFile/KeyFile 指定的正式憑證。
//   - "off":純 HTTP 明文(僅限內網/VPN,啟動時明示風險)。
type tlsConfig struct {
	Mode     string `toml:"mode"`
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

// TLS 模式常數。
const (
	tlsModeAuto   = "auto"
	tlsModeCustom = "custom"
	tlsModeOff    = "off"
)

const (
	defaultBind       = "0.0.0.0:9444"
	defaultConfigName = "agent.toml"
)

// loadAgentConfig 載入設定檔並套用預設值。configPath 為空時預設為執行檔旁的 agent.toml。設定檔
// 不存在時以全預設啟動(首啟即可零設定運行);存在但格式錯則回錯。回傳的路徑欄位皆為絕對路徑。
func loadAgentConfig(configPath string) (agentConfig, error) {
	if configPath == "" {
		configPath = filepath.Join(exeDir(), defaultConfigName)
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return agentConfig{}, fmt.Errorf("解析設定檔路徑失敗: %w", err)
	}
	baseDir := filepath.Dir(configPath)

	var cfg agentConfig
	data, err := os.ReadFile(configPath)
	switch {
	case err == nil:
		if uerr := toml.Unmarshal(data, &cfg); uerr != nil {
			return agentConfig{}, fmt.Errorf("解析設定檔 %s 失敗: %w", configPath, uerr)
		}
	case errors.Is(err, fs.ErrNotExist):
		// 無設定檔:全預設啟動。
	default:
		return agentConfig{}, fmt.Errorf("讀取設定檔 %s 失敗: %w", configPath, err)
	}

	applyDefaults(&cfg, baseDir)
	return cfg, nil
}

// applyDefaults 補齊空欄位的預設值,並把相對路徑解析為相對 baseDir 的絕對路徑。
func applyDefaults(cfg *agentConfig, baseDir string) {
	if cfg.Bind == "" {
		cfg.Bind = defaultBind
	}
	if cfg.Node == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			cfg.Node = h
		} else {
			cfg.Node = "agent"
		}
	}
	if cfg.TLS.Mode == "" {
		cfg.TLS.Mode = tlsModeAuto
	}
	if cfg.DataRoot == "" {
		cfg.DataRoot = "data"
	}
	if cfg.BackupRoot == "" {
		cfg.BackupRoot = "backups"
	}
	if cfg.CacheRoot == "" {
		cfg.CacheRoot = "cache"
	}
	if cfg.TokenFile == "" {
		cfg.TokenFile = "token"
	}
	if cfg.TLS.CertFile == "" {
		cfg.TLS.CertFile = "cert.pem"
	}
	if cfg.TLS.KeyFile == "" {
		cfg.TLS.KeyFile = "key.pem"
	}

	cfg.DataRoot = resolvePath(baseDir, cfg.DataRoot)
	cfg.BackupRoot = resolvePath(baseDir, cfg.BackupRoot)
	cfg.CacheRoot = resolvePath(baseDir, cfg.CacheRoot)
	cfg.TokenFile = resolvePath(baseDir, cfg.TokenFile)
	cfg.TLS.CertFile = resolvePath(baseDir, cfg.TLS.CertFile)
	cfg.TLS.KeyFile = resolvePath(baseDir, cfg.TLS.KeyFile)
}

// resolvePath 把相對路徑解析為相對 baseDir 的絕對路徑;已是絕對路徑則原樣返回。
func resolvePath(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

// exeDir 回傳執行檔所在目錄(取不到時退回當前工作目錄)。
func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
