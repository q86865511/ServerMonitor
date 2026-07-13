package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// AppConfig 是應用層的「一般設定檔」內容(UTF-8 JSON),有別於 DB 內的 settings 表。
// 這類檔案毀損時以預設值降級啟動(R12),不像 SQLite 毀損需 quarantine。
type AppConfig struct {
	// DataRoot 是資料根目錄(DB、fallback 診斷檔、單一實例 lock 的作用域)。
	DataRoot string `json:"data_root"`
	// Node 是本機節點識別(單機首版固定 "local")。
	Node string `json:"node"`
	// EventRetentionMax 是事件保留上限(0 = 用內建預設)。
	EventRetentionMax int `json:"event_retention_max"`
}

// DefaultAppConfig 回傳內建預設設定。
func DefaultAppConfig() AppConfig {
	return AppConfig{
		DataRoot:          "",
		Node:              "local",
		EventRetentionMax: 10000,
	}
}

// LoadAppConfig 讀取一般設定檔。
//
// 回傳語意:
//   - 檔案不存在:回預設值,recovered=false,err=nil(首次啟動視為正常)。
//   - 檔案存在但無法解析(毀損):回預設值,recovered=true,err=nil
//     (毀損被容忍;呼叫端據 recovered 記錄一筆事件並可重寫預設檔)。
//   - 其他 I/O 錯誤(如權限):回預設值,recovered=false,err!=nil。
func LoadAppConfig(path string) (cfg AppConfig, recovered bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultAppConfig(), false, nil
		}
		return DefaultAppConfig(), false, fmt.Errorf("讀取設定檔失敗: %w", err)
	}

	cfg = DefaultAppConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		// 毀損:以預設值降級啟動並回報 recovered。
		return DefaultAppConfig(), true, nil
	}
	return cfg, false, nil
}

// SaveAppConfig 以 UTF-8 JSON(縮排、易讀)寫出設定檔。
func SaveAppConfig(path string, cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化設定失敗: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("寫入設定檔失敗: %w", err)
	}
	return nil
}
