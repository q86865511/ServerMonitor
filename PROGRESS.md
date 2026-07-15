# ServerMonitor — 進度(PROGRESS.md)

## 目前狀態

已進入 /pipeline 實作。**T1–T14 完成並勾銷(14/16)**:T12/T13 經雙審(11 成立+1 部分)→ 使用者裁示全修 → 兩路並行修正 → 聚焦複審 11 項全關閉、四組交互通過;合併樹 build/test/-race/**真 Docker 全整合(含 backup-retention-restore 迴圈)**全綠。剩 T15 GUI、T16 E2E/文件。規格 rev.3。

## 已完成

- [2026-07-15] 🚀 R1 實作 T12+T13+雙審修正(/pipeline):**T12** BackupService(planned-stop 一致快照→原 desired 收斂、RetentionPolicy N=7 經 BackupDeleter 鏈+DELETE 端點、op-journal 獨立於建立 journal、RecoverInterrupted 掛鉤);**T13** AlertDispatcher(AlertSink 生產實作、dedup cooldown)+DiscordWebhookChannel(429 Retry-After/5xx/4xx 策略)+ThresholdMonitor(60s 窗口/遲滯/邊緣觸發)。雙審 11 成立全修:backupID 路徑遍歷雙層防禦(server 400+backend 容器斷言+findBackup 同補)、RecoverInterrupted 失敗保留 journal、收斂改依 desired(stopLocked 污染前擷取)、事件語意去矛盾、journal 記 newRuntimeID 認養、空清單不修剪、alertDispatchTimeout=90s、dedup 成功才佔 cooldown、Retry-After 小數/HTTP-date、URL 不洩漏、dedup 惰性清理。聚焦複審 11 項全關閉;真 Docker 全整合(backup→刪最舊→restore 迴圈 PASS)。中途兩代理撞用量上限,SendMessage 原地續跑完成。
- [2026-07-14] 🚀 R1 實作 T10+T11+雙審修正(/pipeline):**T10** MonitorHub(stats/logs WS 訂閱+退避重連、log 背壓丟舊留新+提示行、玩家數解析、探針/指令以消費端介面注入、Snapshot);**T11** HealthProber(docker/tcp/rcon/rest,兼 ReadinessProber)、HealthMonitor(15s/3 次判卡死)、RestartPolicy(5min3 次→giveup+AlertSink port)、Scheduler(UTC 自製格式免 cron、重啟先 announce、備份觸發)。雙審 9 成立+1 誤報→全修:MonitorHub 全程 h.mu 序列化(壓力測試 -race×20)、重試失敗續排至上限、卡死復原先 token+強停再交 hook(die 雙防線去重)、排程首 tick/fire goroutine 化/last_fired 持久化(schema v2)、rcon 探針判 Success、shutdown 取消未決計時器。聚焦複審 9 項全關閉、四組交互驗自洽。合併樹 -race×3 全綠。
- [2026-07-14] 🚀 R1 實作 T9+T14+雙審修正(/pipeline):**T9** RCON adapter(哨兵法多封包聚合+auth id/type 核對)、Palworld REST adapter(具名動作+Basic Auth)、dispatcher、CommandService(範本→協定→金鑰→CommandTarget)、hooks.stop/announce 接線(schema 化 args/message_key/username);**T14** modpack spike 落檔、loader 矩陣前置檢查、itzg MODRINTH/AUTO_CURSEFORGE 透傳(CF_API_KEY=SecretRef)、手動 mrpack/cfzip 驗格式(拒 `..`/索引須合法 JSON)+**Mounts 傳輸 seam**(InstanceSpec.Mounts+agent PUT mounts 端點+上傳失敗回滾;mounts 排除備份範圍);minecraft.toml 鎖 VERSION=1.21.1(itzg LATEST 已需 Java25,實測);CommandTarget String/GoString 遮罩。雙審 9 成立+1 誤報→全修→聚焦複審 9 項全關閉無新矛盾。真 Docker:RCON 哨兵路徑 PASS、掛載上傳端到端 PASS、Modrinth 模組包生效 PASS。
- [2026-07-14] 🚀 R1 實作 T8(/pipeline):狀態機(design 轉移表+非法轉移守衛)、生命週期編排 Orchestrator(Start 就緒輪詢/Stop planned-stop token/Restart 同容器斷言,per-instance lock 序列化)、事件消費迴圈(die 判死因——token 有效=planned、無/過期=Crashed+INSTANCE_CRASHED,stale 不遮蔽)、Reconciler(List 失敗≠空、孤兒/缺失/不一致、journal 未完成建立清理、summary)、single-instance lock(O_EXCL+PID stale 接管)。留 T11 縫(ReadinessProber/CrashHook)與 T15 縫(AcquireAppLock)。8 新檔;build/vet/test/-race/docker 整合全綠。
- [2026-07-14] 🔧 R1 雙審修正(/pipeline 第 3-5 步):reviewer(opus)+Codex 雙審 T1-T7,13 項發現經逐條讀碼裁決 9 成立;使用者裁示全修。已修:A 冪等鍵誤用(NodeClient 改每呼叫隨機鍵;server 5xx 不快取)、B Restore 無回滾(swap 可反向回滾、舊資料延至 Create 成功後刪)、C events 斷線漏失(重連帶 Since+發 Resync)、D 跨實例還原(uuid 已知不 fallback 掃描)、E secret 必填驗證(非 legacy 協定引用即必填)、F channel 關閉空轉、H untar symlink 驗證、I 必填參數不回退 default。新增 15 個測試(含反向驗證);build/vet/gofmt/test/-race 全綠;`-tags docker` 整合測試已於 Docker Desktop 重啟後補跑通過(2026-07-14,`TestDockerBackend_Integration` PASS)。
- [2026-07-13] 🚀 R1 實作 T1–T7(/pipeline):Go+Wails 骨架與版本鎖定、`internal/protocol` 全型別+SecretRef、持久化/事件基礎(SQLite/遷移/quarantine/port_reservations/EventLog/keyring)、`RuntimeBackend` 介面+MockBackend(含 Events)、`DockerBackend`(真 Docker 整合測試綠)、代理 HTTP/WS server(bearer/Origin/冪等)、核心骨幹(NodeClient/NodeRegistry/範本引擎+內建 minecraft·palworld/InstanceService 原子建立+wildcard 埠+分階段回滾)。三套件 build/vet/gofmt/test/`-race`/docker 全綠;5 依賴鎖版。Palworld spike(REST 12 端點、RCON legacy、映像 2.5.1)、備份 spike(停機 bind-mount 快照可行)落檔於 `specs/game-server-manager/spikes/`。
- [2026-07-13] 📄 R1 規格深修(rev.3):依 Codex 第二輪二審(3 解決/10 部分/9 高),把高+中嚴重補進規格——埠 host binding+預留、RuntimeBackend 補 Events、restart 單一所有者、SecretRef 型別、完整狀態轉移表、備份 BackupID/停機快照、事件封套+碼表、精確參數與版本下限、任務重排 + Palworld/備份/模組 spike 前置 gate。Codex 第三輪聚焦複審:9 高中 7 關閉、2 部分,修掉 2 阻擋項(啟動就緒條件四處對齊、備份 spike 掛入 T4 並修正 spike 任務編號)後判定可進 pipeline。
- [2026-07-13] 📄 R1 規格二審與修訂(rev.2):Codex 對三件套二審(15 高/12 中/2 低)。已修訂:修正非法 TOML schema、補全代理 API 契約與 `RuntimeBackend`(List/Inspect/Remove/Archive/Restore、ExecProcess 與遊戲指令分離)、驗收可測化、新增 R13 啟動對帳/單一實例與 R14 結構化事件記錄、代理 bearer 認證;並依裁決改 Palworld REST 為主/RCON legacy、CurseForge 改 itzg 原生。
- [2026-07-13] 📄 R1 規格與專案初始化:完成 game-server-manager 規格三件套(requirements / design / tasks)並核可落檔於 `specs/game-server-manager/`;初始化 PROGRESS/CLAUDE/README 與 git。

## 進行中

(無——T12/T13 批次已收尾,待指示續派 T15)

## 待辦

> 完整任務見 `specs/game-server-manager/tasks.md`(16 項)。**T1–T8 已完成。** 近期:

- [ ] T15 桌面 GUI(六子項)+ **生產組裝接線清單**:Orchestrator/CommandService/MonitorHub/HealthMonitor/Scheduler/RestartPolicy/AppLock/BackupService(scheduler backup callback+啟動後 RecoverInterrupted)/AlertDispatcher(含 INSTANCE_CRASHED producer 接線)/ThresholdMonitor;agent listener 綁 127.0.0.1 並斷言;op-journal 與建立 journal 目錄相異斷言;Restore 舊容器 GC(對帳孤兒處理)
- [ ] T16 E2E/效能基準/文件
- [ ] (審查遺留)動態埠 host_port=0 支援時補 runtime 埠查詢;Palworld shutdown waittime 字串型別待真機查證;Paper 外掛/AUTO_CURSEFORGE 真機驗證併入 T16;事件流停滯逾 token TTL 極端窗(token-TTL 固有);Restore 的 Upsert 回錯路徑也清 journal(複審低度存疑,與「失敗保留 journal」哲學不一致,僅本機 SQLite 寫入失敗觸發)
- [ ] (餘)T12 備份/還原、T13 告警、T14 模組+spike(含模組 spike)、T15 GUI、T16 E2E/文件
- [ ] (審查遺留 J)T15/T16 組裝時:production listener 必須綁 127.0.0.1 並斷言(目前僅提供 Handler(),無監聽保證)

## 已知問題

(無)

## 重要決策紀錄

- [2026-07-13] 技術選型:Go 全棧 + Wails 桌面。理由:Go 為容器工具母語(Docker SDK 原生)、單二進位利於未來遠端節點、資源占用低。
- [2026-07-13] 部署拓撲:先單機,但核心/節點代理以 localhost HTTP 分離(Pterodactyl 模式),預留多節點(R5)。
- [2026-07-13] 執行後端:Docker 為主,抽象 `RuntimeBackend` 介面,預留原生 SteamCMD(R4)。
- [2026-07-13] 生命週期:僅 GUI 開啟時運作,不裝自啟背景服務——關閉應用暫停自動化,遊戲容器仍由 Docker 維持。
- [2026-07-13] 首版取捨:告警只做 Discord webhook;模組包含 Modrinth/CurseForge 串接;Palworld REST+RCON 皆做。
- [2026-07-13] rev.2(依 Codex 二審修訂):Palworld 改「REST 具名動作為主、RCON 標 legacy(需查證)」;CurseForge 改「itzg 原生 AUTO_CURSEFORGE + 手動」,自建下載器延後;新增 R13 啟動對帳/單一實例、R14 結構化事件記錄;代理 API 加 bearer token 認證;備份改經 RuntimeBackend Archive/Restore;鎖定依賴版本(Wails v2 stable 等)。
- [2026-07-13] rev.3(依 Codex 兩輪二審深修):restart 由 core 單一編排(agent 不開 /restart);Docker 事件納入 `RuntimeBackend.Events`;敏感值改 `SecretRef` 型別(輸入即入金鑰庫+redaction);備份首版=停機快照+bind mount、agent 擁有備份根+opaque `BackupID`;首版不支援線上 update(改參數=重建);Palworld/備份/模組三個 spike 設為對應功能前置 gate;依賴版本鎖定給具體下限。
- [2026-07-13] 開發環境:本機經 winget 裝 Go 1.26.5、`go install` 裝 Wails v2.13.0(WebView2 / Node 24 已備、`wails doctor` 通過);Docker Desktop 29.4.1。**注意:winget 裝的 Go/Wails 不在本 session 既有 shell 的 PATH**,執行 go/wails 前需 `export PATH="/c/Program Files/Go/bin:$HOME/go/bin:$PATH"`。鎖版:docker v27.5.1(go-connections **必須** v0.5.0,v0.7.0 會壞 Windows 編譯)、gorilla/websocket v1.5.3、modernc/sqlite v1.34.5、go-keyring v0.2.8、go-toml/v2。
