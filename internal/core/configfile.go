package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"servermonitor/internal/protocol"
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
	// CurseForgeAPIKey 是使用者自填的 CurseForge API 金鑰覆蓋值(native-backend R14)。
	//
	// #11:金鑰不再明文常駐 config.json——實值存 OS 金鑰庫(見 ResolveCurseForgeOverrideKey)。
	// 本欄位僅作「遷移入口」:使用者(或舊版)以編輯設定檔填入明文時,啟動時遷入金鑰庫並清空回寫
	// (欄位 omitempty,清空後不再序列化)。故正常運行的 config.json 不含此欄位;讀到值即代表待遷移。
	CurseForgeAPIKey string `json:"curseforge_api_key,omitempty"`
	// Nodes 是使用者新增的遠端節點清單(R5 多節點)。本機 in-process 節點("local")不列於此——
	// 它由 Bootstrap 恆註冊,不持久化。每個節點的 bearer token **不落 config**,以 SecretStore
	// 存(鍵含節點名命名空間,見 NodeTokenRef);config 只保存非敏感的連線與信任設定。
	Nodes []NodeConfig `json:"nodes,omitempty"`
}

// NodeConfig 是一個遠端節點的持久化連線/信任設定(R5 多節點)。敏感的 bearer token 不在此——
// 存 OS 金鑰庫(NodeTokenRef(Name))。
type NodeConfig struct {
	// Name 是節點在核心中的唯一識別(gsm.node 命名空間;不可為 "local")。
	Name string `json:"name"`
	// BaseURL 是節點代理根位址(如 https://vps.example.com:9444;不含 API 版本前綴)。
	BaseURL string `json:"base_url"`
	// TLSFingerprint 是對端葉憑證的 SHA-256 指紋(冒號分隔 hex;自簽 TOFU 模式 pin 用)。
	// 空=走系統 CA 驗證(對端為正式憑證時)。
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`
	// InsecureHTTP true 時允許 BaseURL 為明文 http(僅限內網/VPN)。
	InsecureHTTP bool `json:"insecure_http,omitempty"`
}

// NodeTokenRef 回傳某節點 bearer token 在 OS 金鑰庫的參照(R5 多節點)。以節點名命名空間,
// 避免與逐實例機密(instanceSecretKey)或節點層單一覆蓋值(curseForgeOverrideRef)相撞。
func NodeTokenRef(name string) protocol.SecretRef {
	return protocol.SecretRef{Key: "node-token/" + name}
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

// curseForgeOverrideRef 是「使用者設定覆蓋的 CurseForge API 金鑰」在 OS 金鑰庫的固定參照(#11)。
// 有別於逐實例機密(以 UUID 命名空間,見 instanceSecretKey),此為節點層級的單一覆蓋值,故用固定鍵名。
var curseForgeOverrideRef = protocol.SecretRef{Key: "curseforge-api-key"}

// ResolveCurseForgeOverrideKey 取回使用者設定覆蓋的 CurseForge API 金鑰實值,並在必要時把舊版
// 明文設定遷入 OS 金鑰庫(#11:金鑰不再明文落 config.json)。cfgPath 為設定檔路徑(遷移後回寫用);
// secrets 為 nil 時退化為只讀 cfg 明文欄位(不遷移),供無金鑰庫的降級路徑。行為:
//   - cfg.CurseForgeAPIKey 非空(舊版明文,或使用者以編輯設定檔新填)→ 寫入金鑰庫、清空該欄位並
//     回寫 config.json(欄位 omitempty,清空後不再序列化),回傳 (值, migrated=true, nil)。
//   - 否則自金鑰庫取回既存覆蓋值(查無回空字串),回傳 (值, false, nil)。
//
// 遷移成功後 cfg.CurseForgeAPIKey 就地清空(呼叫端後續使用同一 cfg 時不再見明文)。
func ResolveCurseForgeOverrideKey(secrets *SecretStore, cfgPath string, cfg *AppConfig) (key string, migrated bool, err error) {
	plain := strings.TrimSpace(cfg.CurseForgeAPIKey)
	if secrets == nil {
		return plain, false, nil
	}
	if plain != "" {
		if serr := secrets.Set(curseForgeOverrideRef, plain); serr != nil {
			return "", false, fmt.Errorf("遷移 CurseForge 金鑰入金鑰庫失敗: %w", serr)
		}
		cfg.CurseForgeAPIKey = ""
		if werr := SaveAppConfig(cfgPath, *cfg); werr != nil {
			return "", false, fmt.Errorf("清除 config.json 明文 CurseForge 金鑰失敗: %w", werr)
		}
		return plain, true, nil
	}
	v, gerr := secrets.Get(curseForgeOverrideRef)
	if errors.Is(gerr, ErrSecretNotFound) {
		return "", false, nil
	}
	if gerr != nil {
		return "", false, fmt.Errorf("讀取 CurseForge 覆蓋金鑰失敗: %w", gerr)
	}
	return v, false, nil
}

// SetCurseForgeOverrideKey 設定(或清除)使用者覆蓋的 CurseForge API 金鑰於 OS 金鑰庫(#11:實值
// 只落金鑰庫,永不明文寫 config.json、log 或事件)。key 為空字串(去空白後)=清除該項(冪等,查無
// 亦視為成功);非空=寫入/覆寫。secrets 為 nil(無金鑰庫)時回錯,由呼叫端決定如何回報。
// 供 GUI 綁定(app.Runtime.SetCurseForgeOverrideKey)於使用者於設定填入/清除金鑰時呼叫。
func SetCurseForgeOverrideKey(secrets *SecretStore, key string) error {
	if secrets == nil {
		return errors.New("core: 金鑰庫不可用,無法設定 CurseForge 覆蓋金鑰")
	}
	if strings.TrimSpace(key) == "" {
		return secrets.Delete(curseForgeOverrideRef)
	}
	return secrets.Set(curseForgeOverrideRef, key)
}

// CurseForgeOverrideKeySet 回報 OS 金鑰庫是否已存有使用者覆蓋的 CurseForge API 金鑰(#11)。只回
// 「有無」布林,不回明文——供 GUI 顯示「已設定/未設定」狀態。secrets 為 nil 或查無/讀取失敗一律回
// false(保守視為未設定)。有別於 CurseForgeEnabled(後者含建置內嵌 key),本函式只反映「使用者覆蓋」。
func CurseForgeOverrideKeySet(secrets *SecretStore) bool {
	if secrets == nil {
		return false
	}
	v, err := secrets.Get(curseForgeOverrideRef)
	if err != nil {
		return false
	}
	return strings.TrimSpace(v) != ""
}

// SaveAppConfig 以 UTF-8 JSON(縮排、易讀)寫出設定檔。採「寫暫存檔→rename」原子替換,避免寫入
// 途中崩潰/磁碟滿留下半份 config.json——毀損檔會使 LoadAppConfig 降級為預設值,靜默丟失所有已設
// 定的遠端節點(其 token 亦成金鑰庫孤兒)。暫存檔與目標同目錄以確保 rename 為同檔系統原子操作。
func SaveAppConfig(path string, cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化設定失敗: %w", err)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-config-*.json")
	if err != nil {
		return fmt.Errorf("建立設定暫存檔失敗: %w", err)
	}
	tmp := f.Name()
	if _, werr := f.Write(append(data, '\n')); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("寫入設定暫存檔失敗: %w", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("同步設定暫存檔失敗: %w", serr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("關閉設定暫存檔失敗: %w", cerr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替換設定檔失敗: %w", rerr)
	}
	return nil
}
