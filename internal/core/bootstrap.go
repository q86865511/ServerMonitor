package core

import (
	"encoding/json"

	"servermonitor/internal/protocol"
)

// EventConfigRecovered 標記「一般設定檔毀損、以預設值降級啟動」(R12)。
//
// 定義於此(而非 protocol 的必備碼表)是因為:設定檔的「記錄」由 core 啟動流程負責——
// T2 的 LoadAppConfig 刻意只回 recovered 旗標、把記事件留給 bootstrap(此處)。它不在
// protocol.RequiredEventCodes 的必備集內,屬補充事件碼。
const EventConfigRecovered protocol.EventCode = "CONFIG_RECOVERED"

// EventConfigCurseForgeKeyMigrated 標記「舊版 config.json 的明文 CurseForge API 金鑰已遷入 OS 金鑰庫
// 並自設定檔清除」(#11)。與 EventConfigRecovered 同屬 core 補充事件碼(不在 protocol 必備碼表)。
const EventConfigCurseForgeKeyMigrated protocol.EventCode = "CONFIG_CF_KEY_MIGRATED"

// Bootstrap 執行啟動時的設定載入掛接(R12):讀一般設定檔,若因毀損而以預設值降級
// (recovered=true)則記一筆 CONFIG_RECOVERED 事件。回傳有效設定與 I/O 錯誤(如權限)。
//
// events 可為 nil(則僅載入、不記事件)。SQLite 毀損的 quarantine 與 DB_QUARANTINE 事件
// 由 Store.Open 自行處理,不在此;此處僅管「一般設定檔」的降級記錄。
func Bootstrap(cfgPath string, events *EventLog) (AppConfig, error) {
	cfg, recovered, err := LoadAppConfig(cfgPath)
	if err != nil {
		return cfg, err
	}
	if recovered && events != nil {
		details, _ := json.Marshal(map[string]string{"path": cfgPath})
		_ = events.Append(protocol.Event{
			Code:        EventConfigRecovered,
			Severity:    protocol.SeverityWarning,
			DetailsJSON: details,
		})
	}
	return cfg, nil
}
