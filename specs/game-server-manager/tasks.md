# game-server-manager — 任務清單(tasks.md)

> 建立日期:2026-07-13｜狀態:已核可(2026-07-13),經 Codex 兩輪二審深修(rev.3);實作完成 2026-07(任務全數收官,見 PROGRESS.md)
> 本檔為 /pipeline 任務清單來源:pipeline 第 1 步直接採用本清單與 HARD/NORMAL 標記,不重新拆解。
> 勾選(`- [x]`)只由 pipeline 第 5 步收尾回寫,其他階段不動。
> 依賴為 DAG 無環;spike(T6/T14 內含)為對應功能的前置 gate。持久化/事件基礎(T2)排在需要它的核心之前。
> 2026-07-16:後續功能 native-backend(免 Docker 本機行程後端)見 specs/native-backend/tasks.md。

## 任務

- [x] T1 [HARD] 骨架 + 共享型別 + 版本鎖定 + SecretRef:Go module、Wails v2、前端、目錄;`internal/protocol` 定義版本化範本 schema(host binding、command_protocols tagged union、hooks、secrets)、DTO、事件 envelope 型別、`SecretRef`(String/Marshal 一律 redact);`go.mod` 鎖定依賴版本(Go/Wails v2/Docker client+Engine API/modernc/go-keyring,並驗 modernc 對 Go 版本要求)(對應 R1,R4,R5,R12,R14;驗收:`wails dev` 起窗、`go build ./...`、「原樣解析 design 範本範例」golden TOML 測試通過、SecretRef redaction 單元測試、go.mod 版本已鎖;依賴:無)
- [x] T2 [HARD] 持久化 + 事件基礎:SQLite(`modernc.org/sqlite`)+ 遷移 + `integrity_check` + quarantine(改名不覆寫)+ `port_reservations`(唯一約束)+ `EventLog`(envelope/必備 codes/retention/DB 不可用時 fallback NDJSON)+ `go-keyring` 存取 SecretRef(對應 R2,R12,R14;驗收:遷移、毀損隔離不以空庫覆寫、一般設定毀損以預設啟動、SecretRef 不明文於 DB、事件可依實例/類型/時間 filter、唯一約束擋重複埠、DB 毀損時事件落 fallback;依賴:T1)
- [x] T3 [HARD] `RuntimeBackend` 介面 + `MockBackend`(含 Events):定義全介面(Create/Start/Stop/Status/List/Inspect/Remove/Logs/ExecProcess/Stats/Archive/Restore→回 runtime ID/Events);Mock 記憶體實作含事件注入(對應 R4;驗收:Mock 跑通生命週期 + List/Inspect/Remove/Archive/Restore + Events 崩潰事件注入;依賴:T1)
- [x] T4 [HARD] `DockerBackend`(官方 SDK)+ 備份 spike(前置):先驗 Windows/Docker Desktop 下 bind mount 停機快照的一致性與原子切換(結論落檔);再實作全介面,含 `gsm.*` 標籤、優雅停機、log 串流、stats、Events(die/health 監看,reconnect/cursor/漏事件對帳)、Archive(停機 bind mount 一致快照+checksum)、Restore(原子切換回新 runtime ID)(對應 R2,R3,R4,R6,R9;驗收:備份 spike 結論落檔、真 Docker 建立/啟停/stats/log/events/archive+restore 通過;依賴:T3)
- [x] T5 [HARD] 節點代理 server(版本化 OpenAPI + bearer + Origin + 冪等):端點 list/create/inspect/remove、start/stop(**無 restart**)、status、stats WS、logs WS、events WS、command、backups/backup/restore、health;統一錯誤碼表;`Idempotency-Key` TTL 重播(對應 R5,R4,R14;驗收:未帶 token 拒絕、WS Origin 檢查、冪等鍵重播回原結果、events WS 通報、錯誤碼一致、`{id}`=UUID;依賴:T2,T4)
- [x] T6 [HARD] Palworld spike(T9 前置 gate):查證官方 REST 端點/認證與 RCON 現況,鎖 Palworld 伺服器版本/映像 tag 或 digest;結論落檔(對應 R7 待查證;驗收:spike 報告含可用 REST 具名動作清單、RCON 結論、鎖定映像;依賴:T1,可與 T2–T5 並行)
- [x] T7 [HARD] 管理核心骨幹:`NodeClient`(HTTP+bearer)+ `NodeRegistry`(離線)+ 範本引擎(載入/驗證/版本/adapter 註冊表)+ `InstanceService`(建立:必填/EULA/SecretRef 入庫/埠預留+wildcard 規則/建立 journal/分階段原子回滾)(對應 R1,R2,R5;驗收:core→agent→mock 建立、缺參數/EULA/埠衝突含 wildcard/併發擋、各階段 failure injection 皆回滾不留孤兒、缺欄位/adapter 不存在範本拒載記事件、代理離線標記;依賴:T5)
- [x] T8 [HARD] 狀態機 + 對帳 + 單一實例:desired/observed + 狀態轉移表 + planned-stop token(generation/TTL/consume/失敗清除)+ per-instance lock + `Reconciler`(daemon 不可用≠空、`gsm.*` 標籤、孤兒/不一致/journal 未完成建立、重建事件訂閱、離線 summary)+ single-instance lock(明訂作用域)(對應 R3,R8,R13;驗收:planned-stop 不誤判崩潰、探針卡死納入、對帳四情境、拒絕第二應用實例、重開重建訂閱;依賴:T7)
- [x] T9 [HARD] 指令 adapter + 生命週期 hooks:`GameCommandAdapter` + `RconAdapter`(Minecraft)+ `PalworldRestAdapter`(依 T6 具名動作);`hooks.stop`/`hooks.announce` 映射;agent command 端點依 `command_protocols` 選 adapter;Palworld RCON legacy 不啟用(對應 R7;驗收:Minecraft RCON `list`、Palworld REST 具名動作、`hooks.stop` 用於優雅停機、未啟用停用提示、連線失敗明確報錯;核心/agent 層驗收,GUI 呈現於 T15;依賴:T5,T6)
- [x] T10 [HARD] 監控聚合 + log 背壓 + 磁碟/線上:core 訂閱 agent stats/log/events WS 轉推(有界緩衝/批次/丟棄提示,核心層阻塞<200ms);資料磁碟宿主路徑;線上探針;玩家數(對應 R6;驗收:≤5s CPU/RAM/資料磁碟、探針線上狀態、玩家數或「不適用」、高流量核心層不阻塞;依賴:T8,T9)
- [x] T11 [HARD] 排程 + 自動重啟/崩潰復原 + 健康探針:`RestartPolicy`(退避、5min 3 次→停+經 `AlertSink` port)、健康探針(rest/rcon/tcp,60/15/3)、排程重啟(UTC、`hooks.announce`)、排程備份觸發;定義 `AlertSink` port(實作於 T13)(對應 R8;驗收:崩潰自動重啟、窗內超上限停+觸發 AlertSink(fake sink 驗)、探針卡死納入、排程重啟依 UTC;依賴:T8,T9)
- [x] T12 [HARD] 備份/還原:經 RuntimeBackend Archive/Restore、停機一致快照、`BackupID`(agent 擁有根)、DB 存 BackupID+checksum、`RetentionPolicy`(7)、還原(停機→驗證→原子切換→回新 runtime ID→依原 desired 收斂)、中斷 journal crash-safe rollback(對應 R9;驗收:排程產備份+checksum、可列清單、超 N 刪最舊、原子還原一致且復原原 desired、備份/還原中斷可 rollback;依賴:T4,T11)
- [x] T13 [NORMAL] 告警分派:`AlertDispatcher` + `DiscordWebhookChannel`(四類事件固定 payload)+ 窗口/hysteresis/cooldown/dedup(fake clock)+ 重試(429 `Retry-After`/5xx 退避);實作 T11 的 `AlertSink`;producer→AlertSink→dispatcher 跨層測試(對應 R10;驗收:四類 payload、資源門檻去重不轟炸、無管道僅記錄、重試策略、跨層接線驗收;依賴:T11)
- [x] T14 [HARD] 模組 spike(前置)+ 模組/模組包(itzg 原生):先查 itzg 支援矩陣/`AUTO_CURSEFORGE` env/CurseForge 條款(spike 落檔);變體套用、itzg `MODRINTH`/`AUTO_CURSEFORGE` 透傳(`CF_API_KEY`=SecretRef)、手動格式(`mrpack`/`curseforge-zip`)經 `manual_mount` 交付、支援矩陣前置檢查;工具不解壓 `/data`(對應 R11;驗收:spike 結論、Paper+外掛、itzg Modrinth 模組包、`AUTO_CURSEFORGE`(有 key)、手動匯入、不相容前置報錯;依賴:T7)〔勾選註:Modrinth 真 Docker 生效已驗;Paper 外掛與 AUTO_CURSEFORGE 為單元層驗證,真機驗證併入 T16 E2E〕
- [x] T15 [HARD] 桌面 GUI(Wails 前端,子項可獨立勾選):(a) 列表/卡片(狀態/CPU/RAM/磁碟/玩家數)、(b) 建立精靈(範本/變體/參數/EULA/SecretRef 輸入/模組)、(c) 主控台(log+指令)、(d) 排程/備份/告警設定、(e) 事件檢視(filter)、(f) 錯誤狀態矩陣;經 Wails bindings(對應 R2,R3,R6,R7,R9,R10,R11,R14;驗收:六子項各自可驗、GUI 走完建立→啟動→監控→下指令→備份→設告警一輪;依賴:T7,T8,T9,T10,T11,T12,T13,T14)〔勾選註:六子項與生產組裝已實作並經雙審+11 項修正全關閉(build/-race/svelte-check/wails build 綠);GUI 視窗真機一輪無法自動化,由使用者實測/併入 T16 E2E〕
- [x] T16 [HARD] 端到端 + 效能基準 + 文件:真 Docker E2E(Minecraft 全流程 + Palworld REST);重啟後 R12/R13 對帳驗證;效能 benchmark 回填 NFR 門檻(閒置 CPU/working set、每實例開銷、log 高流量);更新 `PROGRESS.md`、專案 `CLAUDE.md`、`README`(安裝 Docker/WSL2、範本撰寫指南、版本鎖定、spike 結論索引)(對應 R1,R6,R12,R13 整合驗證;驗收:E2E 通過、重啟對帳、benchmark 落定門檻、文件齊備;依賴:T15)

<!-- 雙向覆蓋檢查:
R1→T1,T7,T16｜R2→T2,T4,T7,T15｜R3→T4,T8,T9,T15｜R4→T1,T3,T4,T5｜R5→T1,T5,T7｜R6→T4,T10,T15,T16｜R7→T6,T9,T15｜R8→T4,T8,T11｜R9→T4,T12,T15｜R10→T11,T13,T15｜R11→T14,T15｜R12→T1,T2,T16｜R13→T8,T16｜R14→T1,T2,T5,T15。
每條 R#(R1–R14)皆被覆蓋;每條任務皆對應至少一 R#。
依賴 DAG:T1→(T2,T3,T6);T2,T4→T5;T3→T4;T5→T7;T7→T8,T14;T8,T9→T10,T11;(T5,T6)→T9;T4,T11→T12;T11→T13;T7..T14→T15→T16。AlertSink 由 T11 定義、T13 實作(無環)。 -->

## 完成定義

- 全部任務勾選完成,且各任務驗收條件有實跑證據(單元/整合/真 Docker 輸出);三個 spike(Palworld/備份/模組)結論落檔。
- requirements.md 每條 R#(R1–R14)的驗收條件通過 /pipeline 雙審對照。
- 文件(PROGRESS.md、README、專案 CLAUDE.md)依全域規則更新;效能 NFR 門檻由 T16 benchmark 回填。
