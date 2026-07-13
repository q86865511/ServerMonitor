# game-server-manager — 任務清單(tasks.md)

> 建立日期:2026-07-13｜狀態:已核可(2026-07-13),經 Codex 二審修訂(2026-07-13, rev.2)
> 本檔為 /pipeline 任務清單來源:pipeline 第 1 步直接採用本清單與 HARD/NORMAL 標記,不重新拆解。
> 勾選(`- [x]`)只由 pipeline 第 5 步收尾回寫,其他階段不動。

## 任務

- [ ] T1 [HARD] 專案骨架 + 共享型別 + 版本鎖定:Go module、Wails v2、前端框架、目錄(`internal/{protocol,agent,core}`、`app`、`frontend`);`internal/protocol` 定義版本化範本 schema、`InstanceSpec`、狀態/指標/事件型別、代理 API DTO;鎖定依賴版本(Go / Wails v2 / Docker client / modernc / go-keyring)(對應 R1,R4,R5;驗收:`wails dev` 起窗、`go build ./...`、protocol JSON 序列化 + 「原樣解析 design 範本範例」golden TOML 測試通過、go.mod 版本已鎖;依賴:無)
- [ ] T2 [HARD] `RuntimeBackend` 介面 + `MockBackend`:定義 Create/Start/Stop/Status/List/Inspect/Remove/Logs/ExecProcess/Stats/Archive/Restore,MockBackend 記憶體實作(對應 R4;驗收:MockBackend 單元測試跑通完整生命週期含 List/Inspect/Remove/Archive/Restore;依賴:T1)
- [ ] T3 [HARD] `DockerBackend`(官方 Docker SDK):實作介面全部方法,含 UUID label、優雅啟停、log 串流、stats、Docker events、Archive/Restore(bind mount 或 helper container 封存 + checksum)(對應 R2,R3,R4,R6,R9;驗收:真 Docker 整合建立/啟停/stats/log/archive+restore 通過;依賴:T2)
- [ ] T4 [HARD] 節點代理 HTTP/WS server(版本化 + bearer 認證 + Origin):端點 list/create/inspect/remove、start/stop/restart、status、stats WS、logs WS、events WS、command、backups/backup/restore、health;Docker 事件串到 events WS(對應 R5,R8,R14;驗收:未帶 token 拒絕、HTTP CRUD、WS 收 stats/log/events、kill 容器經 events 通報;依賴:T3)
- [ ] T5 [HARD] 管理核心骨幹:`NodeClient`(HTTP+bearer)+ `NodeRegistry`(離線標記)+ 範本引擎(載入/驗證/版本/adapter 標示)+ `InstanceService`(建立:必填/EULA/埠衝突鍵、原子回滾)+ `EventLog`(結構化事件)(對應 R1,R2,R5,R14;驗收:核心經 loopback+token 建立實例、缺參數/EULA/埠衝突擋、缺欄位範本拒載記事件、代理離線標記、事件可查詢;依賴:T4)
- [ ] T6 [HARD] 狀態機 + 啟動對帳 + 單一實例:desired/observed 狀態、planned-stop token、per-instance lock、`Reconciler`(啟動 List/Inspect 對帳 DB↔容器、孤兒處理)、single-instance lock(對應 R3,R8,R13;驗收:planned-stop 不被誤判為崩潰、對帳孤兒/不一致、拒絕第二應用實例;依賴:T5)
- [ ] T7 [NORMAL] 內建範本:minecraft(vanilla/paper/forge/fabric、`command_protocols` rcon、health tcp、players_query `list`、data_dirs `/data`、mods owner=image-native)、palworld(`command_protocols` rest 具名動作 + rcon legacy、data_dirs)(對應 R1,R7,R11;驗收:GUI 列兩款及變體、第三份測試範本不改核心即現身且跑通載入→建立→啟動→狀態;依賴:T5)
- [ ] T8 [HARD] 指令 adapter:`GameCommandAdapter` + `RconAdapter`(Minecraft)+ `PalworldRestAdapter`(具名動作);代理 command 端點依範本 `command_protocols` 選 adapter;Palworld RCON legacy 不啟用(對應 R7;驗收:Minecraft RCON `list` 回顯、Palworld REST 具名動作回顯、未啟用停用並提示、連線失敗明確報錯不卡死;依賴:T4,T7)
- [ ] T9 [HARD] 監控聚合 + 即時 log(背壓)+ 磁碟/線上:核心訂閱 agent stats/log/events WS 轉推(有界緩衝/批次/丟棄提示);資料磁碟用量、線上探針、玩家數(對應 R6;驗收:≤5s 更新 CPU/RAM/資料磁碟、探針線上狀態、玩家數或「不適用」、高流量 GUI 不卡死;依賴:T6,T8)
- [ ] T10 [HARD] 排程器 + 自動重啟/崩潰復原 + 健康探針:`RestartPolicy`(退避、窗內上限→停+經 `AlertSink` port 告警)、健康探針(rest/rcon/tcp)、排程重啟(UTC、重啟前公告)、排程備份觸發;定義 `AlertSink` port(實作於 T12)(對應 R8;驗收:崩潰自動重啟、窗內超上限停+觸發 AlertSink、探針卡死納入復原、排程重啟依 UTC 觸發;依賴:T6,T8)
- [ ] T11 [HARD] 備份/還原:經 RuntimeBackend Archive/Restore、一致快照(save hook/停機)、backups 目錄+checksum+清單、`RetentionPolicy`(保留 N)、還原(停機→驗證→原子切換→復原原 desired 狀態)(對應 R9;驗收:排程產備份+checksum、可列清單、超 N 刪最舊、原子還原一致且復原原狀態;依賴:T3,T10)
- [ ] T12 [NORMAL] 告警分派:`AlertDispatcher` + `DiscordWebhookChannel`(四類事件固定 payload)、門檻窗口/hysteresis/cooldown/去重、重試(429 `Retry-After`/5xx 退避);實作 T10 的 `AlertSink`(對應 R10;驗收:四類事件 mock Discord 收到 payload、資源門檻去重不轟炸、無管道僅記錄、送失敗重試;依賴:T10)
- [ ] T13 [NORMAL] 模組/模組包(itzg 原生):變體套用、itzg `MODRINTH`/`AUTO_CURSEFORGE` 環境透傳(`CF_API_KEY` 於設定)、手動受限檔案(mrpack/curseforge-zip)、支援矩陣前置檢查;工具不解壓 `/data`(對應 R11;驗收:Paper+外掛生效、itzg Modrinth 模組包生效、手動匯入生效、不相容前置報錯;依賴:T7)
- [ ] T14 [HARD] 持久化 + 安全儲存:SQLite(instances/schedules/settings/backups/events)+ 遷移 + `integrity_check` + quarantine 復原、`go-keyring` 存敏感值參照、遊戲 runtime 設定例外處理(對應 R12,R14;驗收:重啟後清單/排程/設定重現、DB 毀損隔離不覆寫、敏感值不明文於 DB、事件表可查詢;依賴:T5)
- [ ] T15 [HARD] 桌面 GUI(Wails 前端,子項可獨立勾選):(a) 實例列表/卡片(狀態/CPU/RAM/磁碟/玩家數)、(b) 建立精靈(範本/變體/參數/EULA/模組)、(c) 主控台(log+指令)、(d) 排程/備份/告警設定、(e) 事件檢視、(f) 錯誤狀態矩陣;經 Wails bindings 呼叫核心(對應 R2,R3,R6,R7,R9,R10,R11,R14;驗收:六子項各自可驗、GUI 走完建立→啟動→監控→下指令→備份→設告警一輪;依賴:T5,T6,T7,T8,T9,T10,T11,T12,T13,T14)
- [ ] T16 [HARD] Palworld spike + 端到端 + 文件:Palworld time-boxed spike(查證官方 REST/RCON 現況、鎖伺服器版本/映像 tag 或 digest);真 Docker E2E(Minecraft 全流程 + Palworld REST);重啟後對帳驗證;更新 `PROGRESS.md`、專案 `CLAUDE.md`、`README`(安裝 Docker/WSL2、範本撰寫指南、版本鎖定)(對應 R1–R14 整合驗證;驗收:E2E 通過、重啟對帳、Palworld spike 結論落檔、文件齊備;依賴:T15)

<!-- 雙向覆蓋檢查:R1→T1,T5,T7,T16｜R2→T3,T5,T15,T16｜R3→T3,T6,T15｜R4→T1,T2,T3｜R5→T1,T4,T5｜R6→T3,T9,T15｜R7→T7,T8,T15｜R8→T4,T6,T10｜R9→T3,T11,T15｜R10→T12,T15｜R11→T7,T13,T15｜R12→T14,T16｜R13→T6,T16｜R14→T4,T5,T14,T15。每條 R# 皆被覆蓋;每條任務皆對應至少一 R#。依賴為 DAG 無環;AlertSink 由 T10 定義 port、T12 實作(無循環)。 -->

## 完成定義

- 全部任務勾選完成,且各任務驗收條件有實跑證據(單元/整合/真 Docker 輸出);Palworld spike 結論落檔。
- requirements.md 每條 R#(R1–R14)的驗收條件通過 /pipeline 雙審對照。
- 文件(PROGRESS.md、README、專案 CLAUDE.md)依全域規則更新。
