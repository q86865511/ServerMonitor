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

// writeConfigFile 依 cm.Format 選編碼器,把 env 中對應的參數值(cm.Map)與固定/衍生值(cm.Set,
// 展開 {port:<name>} token)寫入實例根下的 cm.File。ports 供 Set 的埠 token 展開(native 無 docker
// 埠映射,伺服器須自 server.properties/ini 綁到與探針一致的埠)。
func writeConfigFile(instanceRoot string, cm protocol.NativeConfigMap, env map[string]string, ports []protocol.PortBinding) error {
	pairs, err := resolveConfigPairs(cm.Map, cm.Set, env, ports)
	if err != nil {
		return err
	}
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

// resolveConfigPairs 合併兩個來源為 (configKey,value) 清單並依 configKey 排序:
//   - m(paramKey→configKey):由 env 取值,只有 env 存在的 paramKey 才落檔(未提供者交伺服器預設)。
//   - set(configKey→字面值/{port:<name>} token):固定/衍生值,埠 token 以 ports 展開。
//
// 同一 configKey 同時來自 m 與 set 時,set 覆蓋(native 執行必需值優先於使用者參數,如埠)。
func resolveConfigPairs(m, set map[string]string, env map[string]string, ports []protocol.PortBinding) ([]configPair, error) {
	byKey := make(map[string]string, len(m)+len(set))
	for paramKey, configKey := range m {
		if v, ok := env[paramKey]; ok {
			byKey[configKey] = v
		}
	}
	if len(set) > 0 {
		portByName := make(map[string]int, len(ports))
		for _, p := range ports {
			hp := p.HostPort
			if hp == 0 {
				hp = p.Container
			}
			portByName[p.Name] = hp
		}
		for configKey, raw := range set {
			v, err := expandPortTokens(raw, portByName)
			if err != nil {
				return nil, fmt.Errorf("設定 %q 的值展開失敗: %w", configKey, err)
			}
			byKey[configKey] = v
		}
	}
	pairs := make([]configPair, 0, len(byKey))
	for k, v := range byKey {
		pairs = append(pairs, configPair{key: k, value: v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	return pairs, nil
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
