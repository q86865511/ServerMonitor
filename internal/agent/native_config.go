package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"servermonitor/internal/protocol"
)

// native 設定檔具名編碼器(native-backend R3):不假裝單一泛型映射能涵蓋兩種格式。
//   - "properties":server.properties 風格,configKey=value 逐行。
//   - "palworld-ini":section 下單行 OptionSettings=(K=V,...) 打包(Palworld PalWorldSettings.ini)。
//
// 值來源:ConfigMapping.Map 的鍵(paramKey)索引進 InstanceSpec.Env,取得的 env 值即設定值;
// 只有 Env 中存在的 paramKey 才落檔(未提供的參數不寫,交由伺服器預設)。輸出以 configKey 排序,
// 使檔案位元組穩定(便於還原比對與測試斷言)。值格式化(如 Palworld 字串加引號)由參數值本身承載,
// 編碼器不臆測型別。

// writeConfigFile 依 cm.Format 選編碼器,把 env 中對應的參數值寫入實例根下的 cm.File。
func writeConfigFile(instanceRoot string, cm protocol.NativeConfigMap, env map[string]string) error {
	pairs := resolveConfigPairs(cm.Map, env)
	path := filepath.Join(instanceRoot, filepath.FromSlash(cm.File))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("建立設定檔目錄失敗: %w", err)
	}
	var content string
	switch cm.Format {
	case "properties":
		content = encodeProperties(pairs)
	case "palworld-ini":
		content = encodePalworldIni(cm.Section, pairs)
	default:
		return fmt.Errorf("agent: 未知設定檔格式 %q(檔 %s)", cm.Format, cm.File)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("寫入設定檔 %s 失敗: %w", cm.File, err)
	}
	return nil
}

// configPair 是一組已解析的設定鍵值(configKey=value)。
type configPair struct {
	key   string
	value string
}

// resolveConfigPairs 把 paramKey→configKey 映射與 env 值解析為 (configKey,value) 清單,依 configKey 排序。
func resolveConfigPairs(m map[string]string, env map[string]string) []configPair {
	pairs := make([]configPair, 0, len(m))
	for paramKey, configKey := range m {
		if v, ok := env[paramKey]; ok {
			pairs = append(pairs, configPair{key: configKey, value: v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	return pairs
}

// encodeProperties 編碼為 server.properties(configKey=value 逐行,LF 換行)。
func encodeProperties(pairs []configPair) string {
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(p.key)
		b.WriteByte('=')
		b.WriteString(p.value)
		b.WriteByte('\n')
	}
	return b.String()
}

// encodePalworldIni 編碼為 Palworld PalWorldSettings.ini:section 下單行 OptionSettings=(K=V,...)。
func encodePalworldIni(section string, pairs []configPair) string {
	inner := make([]string, 0, len(pairs))
	for _, p := range pairs {
		inner = append(inner, p.key+"="+p.value)
	}
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(section)
	b.WriteString("]\n")
	b.WriteString("OptionSettings=(")
	b.WriteString(strings.Join(inner, ","))
	b.WriteString(")\n")
	return b.String()
}
