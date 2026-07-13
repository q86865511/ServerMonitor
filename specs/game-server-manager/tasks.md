# game-server-manager — 任務清單(tasks.md)

> 建立日期:2026-07-13｜狀態:已核可(2026-07-13)
> 本檔為 /pipeline 任務清單來源:pipeline 第 1 步直接採用本清單與 HARD/NORMAL 標記,不重新拆解。
> 勾選(`- [x]`)只由 pipeline 第 5 步收尾回寫,其他階段不動。

## 任務

- [ ] T1 [HARD] 專案骨架 + 共享型別:建立 Go module、Wails 專案、前端框架(Svelte/React)、目錄(`internal/{protocol,agent,core}`、`app`、`frontend`);於 `internal/protocol` 定義範本 schema、`InstanceSpec`、`RuntimeStatus/Stats`、代理 API DTO(對應 R1,R4,R5;驗收:`wails dev` 起空視窗、`go build ./...` 通過、protocol 型別 JSON 序列化單元測試通過;依賴:無)
- [ ] T2 [HARD] `RuntimeBackend` 介面 + `MockBackend`(記憶體模擬):定義 create/start/stop/status/logs/exec/stats,MockBackend 實作供測試(對應 R4;驗收:MockBackend 單元測試跑通 create→start→status→stop→logs;依賴:T1)
- [ ] T3 [HARD] `DockerBackend`(官方 Docker SDK):實作介面全部方法——建容器、優雅啟停、狀態、log 串流、exec、stats(對應 R2,R3,R4,R6;驗收:真 Docker 整合測試建立並啟停容器、讀 stats、串 log;依賴:T2)
- [ ] T4 [HARD] 節點代理 HTTP/WS server + Docker 事件監看:把 `RuntimeBackend` 包成代理 API(instances CRUD、start/stop/restart、status、stats、logs WS、command、backup、health),監看 Docker events 供崩潰偵測(對應 R5,R8;驗收:HTTP 建立/啟停、WS 收 log/stats、kill 容器時 events 通報;依賴:T3)
- [ ] T5 [HARD] 管理核心:`NodeClient`(HTTP)+ `NodeRegistry`(離線標記)+ 範本引擎(載入/驗證 `templates/*.toml`)+ `InstanceService`(建立流程:必填參數/埠衝突驗證)(對應 R1,R2,R5;驗收:核心經 localhost HTTP 呼叫代理建立實例、缺參數/埠衝突被擋、缺欄位範本拒載並記 log、代理離線→標記;依賴:T4)
- [ ] T6 [NORMAL] 內建範本:minecraft(vanilla/paper/forge/fabric 變體、RCON、players_query `list`、data_dirs `/data`、mods 目錄)、palworld(REST+RCON、data_dirs)(對應 R1,R7,R11;驗收:GUI 列兩款遊戲及變體、放入第三份測試範本不改核心即現身;依賴:T5)
- [ ] T7 [HARD] 指令協定通道:`RconChannel`(Minecraft/Palworld)+ `PalworldRestChannel`;代理 command 端點依範本 `command_protocol` 選通道(對應 R7;驗收:真 Minecraft RCON `list` 回顯、Palworld REST 與 RCON 皆可送指令、未啟用時停用並提示、連線失敗明確報錯不卡死;依賴:T4,T6)
- [ ] T8 [HARD] 監控聚合 + 即時 log 串流到 GUI:核心訂閱代理 stats/log WS 並轉推前端,玩家數走 `players_query`(對應 R6;驗收:GUI 每 ≤5s 更新 CPU/RAM、主控台即時 log、玩家數顯示或「不適用」;依賴:T5,T6)
- [ ] T9 [HARD] 排程器 + 自動重啟/崩潰復原:`RestartPolicy`(退避重試、窗內上限→停+告警)、排程重啟(重啟前公告若支援)、排程備份觸發(對應 R8,R9;驗收:kill 容器→自動重啟、窗內超上限停+觸發告警、排程重啟依時間觸發;依賴:T4,T5)
- [ ] T10 [NORMAL] 備份/還原:`BackupRunner`(打包 data_dirs→備份區,含時間戳/實例/遊戲)、`RetentionPolicy`(保留 N)、還原(停機→還原→啟動)(對應 R9;驗收:排程產備份檔、超 N 刪最舊、還原前先停機且還原後資料一致;依賴:T3,T9)
- [ ] T11 [NORMAL] 告警分派:`AlertDispatcher` + `DiscordWebhookChannel`(崩潰/重啟失敗/備份失敗/資源超標)(對應 R10;驗收:mock Discord 端點收到含實例名+事件類型、無管道僅記錄不報錯、送失敗有限重試;依賴:T9)
- [ ] T12 [HARD] 模組/模組包:`ModpackProvider`(`ModrinthProvider` 開放 API、`CurseForgeProvider` 需 key)搜尋/下載、變體套用、手動壓縮檔匯入、不相容組合靜態檢查(對應 R11;驗收:Paper 變體+外掛生效、Modrinth 搜尋下載套用、手動匯入生效、不相容給可理解錯誤;依賴:T6)
- [ ] T13 [HARD] 持久化 + 安全儲存:SQLite(instances/schedules/settings/events)、`go-keyring` 存敏感值、毀損容錯以預設值啟動(對應 R12;驗收:重啟應用後清單/排程/設定重現、毀損設定容錯、RCON/webhook 不明文落檔;依賴:T5)
- [ ] T14 [HARD] 桌面 GUI(Wails 前端):實例列表/卡片(狀態、CPU/RAM、玩家數)、建立精靈(範本/變體/參數/模組)、主控台(log+指令輸入)、排程/備份/告警設定頁,經 Wails bindings 呼叫核心(對應 R2,R3,R6,R7,R9,R10,R11;驗收:GUI 走完建立→啟動→監控→下指令→備份→設告警一輪;依賴:T5,T6,T7,T8,T9,T10,T11,T12,T13)
- [ ] T15 [NORMAL] 端到端驗證 + 文件:真 Docker 端到端(Minecraft+Palworld);建立 `PROGRESS.md`、專案 `CLAUDE.md`、`README`(安裝需 Docker/WSL2、用法、範本撰寫指南)(對應 R1–R12 整合驗證;驗收:E2E 通過、重啟後狀態重現、文件齊備;依賴:T14)

<!-- 雙向覆蓋檢查:R1→T1,T5,T6,T15｜R2→T3,T5,T14｜R3→T3,T14｜R4→T1,T2,T3｜R5→T1,T4,T5｜R6→T3,T8,T14｜R7→T6,T7,T14｜R8→T4,T9｜R9→T9,T10,T14｜R10→T11,T14｜R11→T6,T12,T14｜R12→T13,T15。每條 R# 皆被覆蓋;每條任務皆對應至少一 R#。 -->

## 完成定義

- 全部任務勾選完成,且各任務驗收條件有實跑證據(單元/整合/真 Docker 輸出)。
- requirements.md 每條 R# 的驗收條件通過 /pipeline 雙審對照。
- 文件(PROGRESS.md、README、專案 CLAUDE.md)依全域規則更新。
