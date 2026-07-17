# ServerMonitor — 進度(PROGRESS.md)

## 目前狀態

**🏁 gui-redesign 實作 T1–T15 全數完成(/pipeline,分支 claude/gui-redesign,9 個 commit)**;全套件驗證綠(go test 6 套件非快取全 ok、wails build 成功、svelte-check 170 檔 0 錯 0 警告、手寫碼零 any)。**待雙審裁決**(reviewer 總審進行中;Codex 第二審因額度未重置(7/23)暫緩)。已累積待裁決:波6一審 5 條(全低)+ T13/T14 偏離註記。留給使用者:GUI 視窗真機一輪(三尺寸目視+真實資料互動;自動化已驗 shell 版面/hash 路由/四態呈現,截圖管線在此環境 timeout 無法目視像素版面)、native-backend 的 CurseForge 真 key E2E、Palworld native E2E。

## 已完成

- [2026-07-18] 🚀 gui-redesign 實作 T1–T15(/pipeline,分支 claude/gui-redesign):依 4 張參考圖重構整套前端。**後端五擴充**:T1 顯示名稱+連接埠(schema v3 遷移 instances.name/port_reservations.name)、T2 指標時序(metrics 表 nullable 記憶體欄+MetricsRecorder 15s 聚合/末樣本 NULL/玩家數自快照/36h Prune+2 綁定)、T3 uptime(StatusFetcher 注入)、T4 範本 icon(filepath.Rel+EvalSymlinks 路徑拘束+AssetServer Handler+程式生成識別圖)、T5 wailsjs 重生成。**前端**:T6 tokens+20 UI 元件(runes)、T7 LogViewer(content-visibility,T7 真機實測 92s longtask=0)/TrendChart(null 畫缺口)/DataTable、T8 stores(instances 輪詢上移/metrics ring/logs 引用計數+30s 延遲釋放)、T9 hash 路由+Shell(可收合側欄/頂欄搜尋)、T10 總覽+ServerCard(範本驅動)、T11 詳細頁五分頁+主控台(共用 Backups/Schedules/AlertsPanel)、T12 四步精靈(runtime/資源/CF 三態/provision 進度/blocked-mods 等價吸收)、T13 全域七頁遷移+清理 16 舊元件、T14 四態巡檢+防重複、T15 驗證收尾。每波經主迴圈非快取抽驗。
- [2026-07-17] 📄 gui-redesign 規格二審+修訂(rev.2):Codex 第二審提 18 條(4 高/14 中),逐條驗證(agent stats 無玩家數/port_reservations 無 name 欄/native 降級零值/sibling-prefix 逃逸等皆屬實)後全數採納修訂;Codex 額度中途用罄,聚焦複審改由獨立 reviewer(opus)執行——18 條全關閉、三件套一致、判定可進實作;複審附帶 3 條低階瑕疵亦已修(覆蓋表 R12 補 T11、測試策略對齊矩陣語意、memory NULL 判定釘死為末樣本)。T4 升 HARD(共 7 HARD)。
- [2026-07-17] 📄 gui-redesign 規格三件套核可落檔(`specs/gui-redesign/`):依 4 張 Codex 參考圖重構完整前端 GUI 的 spec(R1–R16:設計系統/Shell/hash 路由/總覽儀表板/詳細頁/主控台/四步精靈/全域頁+五項後端擴充);15 任務(5 HARD)。已對齊 c0092e(native-backend)落地後的 bindings 基準,c0092e 前端新功能(精靈 runtime/資源/CF/供應進度、CurseForge 設定卡)納入功能等價吸收要求。
- [2026-07-17] 功能: GUI 設定 CurseForge API key——設定頁新增 CurseForge 卡(password 輸入/儲存/清除/三態徽章),金鑰直寫 OS 金鑰庫不落檔,經 dispatch→native→adapter→Provisioner 四層委派**熱生效免重啟**(RWMutex 收斂併發,-race 綠);綁定加法 SetCurseForgeAPIKey/CurseForgeKeyConfigured;README 同步。
- [2026-07-17] 🔧 native-backend 雙審修正輪(17+1 項全修)＋聚焦複審關閉:雙審(reviewer/Opus 7 條＋Codex 13 條,3 重疊)裁決 17 條成立、使用者核可全修,分三批並行修正——批A core/GUI(Create 改 30min 逾時修 GUI 首建必逾時、CF key 驗證依 runtime 分流、覆蓋 key 自動遷入金鑰庫、GOOS 過濾 native 選項、CF 文案 runtime-aware)、批B agent(模組包 TargetDir 修雙層巢狀、native.json 兩段式寫入修回滾 404、Job 超限即 TerminateJobObject、BlockedMods cf-imports 接線、Remove ctx 解永久阻塞、259 判活/proc.json 吞錯/並發 Start 三低項)、批C provision/範本(Palworld 改 PalServer.exe＋ini 改 Pal/Saved/Config/WindowsServer 路徑、MC native 佈局對齊 data/ 修備份互轉、sha1 旁檔 404 硬失敗＋CF 直鏈強制 https)。聚焦複審確認 17+1 修到位,另抓到交互問題 A(#2×#7:模組落位須跟隨 working_dir)→主迴圈修正＋迴歸鎖。E2E 重跑 50s PASS、全套件非快取綠、tasks.md 14/14 回寫。
- [2026-07-17] 🚀 native-backend T1–T14 實作完成(/pipeline 波1–5＋收官),native MC 全流程 E2E 實跑通過(108s,就緒 11s:建立→啟動→RCON→統計→備份→還原→移除):T1 protocol/範本層、T2 provision 基礎+Adoptium JRE、T3/T4 五 loader 安裝器(PaperMC 實測改接 v3 Fill API,v2 已 sunset;Forge/NeoForge 走官方 installer 產出腳本不自解析)、T5 SteamCMD、T6 NativeBackend 13 方法+契約測試+備份 tar 抽共用、T7 Job Objects(oom 事件/CPU cap 實測/agent 退出行程續活三真機驗收)、T8 dispatchBackend 前綴路由+bootstrap 泛化+RetryDocker 熱替換、T9 runtime 選擇鏈+itzg gate、T10 Modrinth/mrpack+ModProvider 擴充點、T11 行程收養(PID+start-time+Job 重掛)+對帳整合、T12 GUI(bindings 零破壞加法+建立精靈 runtime 選擇+供應進度)、T14 CurseForge(key ldflags 注入+設定覆蓋+BlockedMods 降級流程)、T13 範本校正(forge run.bat 改寫/config Set 欄位/per-instance temp 修 netty AF_UNIX 崩潰)+文件。待雙審裁決。
- [2026-07-16] 📄 native-backend 規格三件套核可落檔於 `specs/native-backend/`(R1–R14,T1–T14):免 Docker 本機行程後端——dispatchBackend 分派(native: 前綴、docker 裸穿零遷移)、供應子系統(Adoptium JRE/五 loader 安裝器/SteamCMD/Modrinth)、Job Objects 資源上限+行程樹統計、收養(proc.json+日誌落檔)、CurseForge R14 可獨立延後(比照 Prism 專案 key 嵌入,經研究查證合規)。既有 game-server-manager 三件套完成 Docker 語境加註修訂(不改原文語意)。
- [2026-07-16] 🏁 R1 實作 T16+雙審修正+16/16 收官(/pipeline):E2E(Minecraft 全流程/重啟對帳/Palworld REST)、效能基準回填、文件完稿。雙審 8 成立全修:**Minecraft 就緒探針 tcp→rcon**(Docker proxy 假陽性,真產品 bug)、**Palworld rest 探針帶認證**(health 加 action_id 經 CommandService,原裸 GET 對全認證 API 永遠失敗——15 分逾時實測抓到)、**palworld 映像 tag 2.5.1→v2.5.1**(Hub 命名帶 v,pull 實測抓到)、NodeClient 逾時分級(Stop 90s/備份還原 30min)、perf 測試 atomic+緩衝、E2E 失敗路徑清理、README REST 欄位/wails 鎖版。全部修正由真機 E2E 或單元測試實證關閉。
- [2026-07-15] 🚀 R1 實作 T15+雙審修正(/pipeline):**波1** `internal/app` 生產組裝(bootstrap 全元件接線/CrashHook 組合接 INSTANCE_CRASHED 告警/journal 目錄與 loopback 斷言/優雅關閉)+25 Wails bindings+`logs:/stats:` 事件推送;**波2** Svelte 六子項(卡片/建立精靈/主控台/排程備份告警/事件檢視/錯誤矩陣)。雙審 11 成立全修:registry 委派 dialer(解 nil panic+RetryDocker 舊連線)、Palworld REST 主控台(Capability DTO+動作下拉)、關閉收束(rootCtx+inflight 排空+readEvents closeOnCancel 解死鎖)、前端數值綁定字串化、Remove 埠失敗中止+機密鍵清單持久化、OnlineProber 適配、訂閱競態/輪詢序號/purge 勾選。聚焦複審 11 項全關閉。GUI 真機一輪待使用者。
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

- gui-redesign(/pipeline 分支 claude/gui-redesign):實作 T1–T15 全完成,**現於 pipeline 第 3-4 步:reviewer 對整個分支 diff 做總審 → 彙整裁決清單交使用者**(停點)。Codex 第二審因額度 7/23 才重置,依 pipeline 規則本輪註明「第二審未執行」。裁決後才做第 5 步修正+回寫 tasks.md 勾選+文件關卡(README GUI 章節/CLAUDE 前端慣例)+push/PR。

## 待辦

> 完整任務見 `specs/game-server-manager/tasks.md`——**16/16 全數勾銷**。以下為後續:

- [ ] GUI 重構:以 `/pipeline gui-redesign` 依 `specs/gui-redesign/tasks.md` 執行(c0092e 已於 2026-07-17 經 PR #1 併入 master,規格分支已 rebase 至其上);codex/graphite-ops-gui 分支棄用不併(僅視覺參考)
- [ ] 使用者 GUI 視窗真機一輪(懶人包見 README 快速開始)
- [ ] 次期候選:動態埠 host_port=0、Email 告警、Palworld waittime 型別真機查證、Paper 外掛/AUTO_CURSEFORGE 真機驗證(NativeBackend 已升格為 specs/native-backend 進行中)
- [ ] CurseForge API key:使用者已持有(2026-07-16 口頭確認);T14 於本輪執行,key 以 build-time 注入/本機設定提供,不 commit 進 repo
- [ ] 打包:`wails build` 不自動隨附 `templates/`(exe 找「執行檔目錄/templates」);發佈需複製隨附或改 go:embed(本機 build/bin 已手動補)
- [ ] (審查遺留,低)事件流停滯逾 token TTL 極端窗;Restore 的 Upsert 回錯路徑清 journal;Restore 舊容器 GC(靠對帳);Console 就地換 uuid 需 {#key};dispatchCrashAlert 不入 inflight 記帳(唯讀無害)

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
- [2026-07-16] native-backend 規格裁決:新開 specs/native-backend(不改寫已交付規格,僅加註);逐實例選 runtime、Windows 預設 native、Linux 強制 Docker;資源上限首版即用 Windows Job Objects;涵蓋 MC 全 loader+Palworld+Modrinth;CurseForge 納入為 R14 可獨立延後(採 Prism 模式:專案專屬 key 嵌入+opted-out 手動下載 fallback;CDN 直連/社群代理不合規不採用);唯一依賴變動=x/sys 提為直接相依(版本不變)。
- [2026-07-17] gui-redesign 規格裁決:五項後端擴充全納入(實例顯示名稱/uptime/連接埠/指標時序 15s×36h/範本 icon 欄位);趨勢圖走後端時序持久化;前端零新增 runtime 依賴(hash 路由自製、圖表手刻 SVG、LogViewer 固定上限+content-visibility 不做虛擬列表);新元件一律 Svelte 5 runes;基底=c0092e 併入 master 後;codex/graphite-ops-gui 棄用僅作視覺參考。
- [2026-07-13] 開發環境:本機經 winget 裝 Go 1.26.5、`go install` 裝 Wails v2.13.0(WebView2 / Node 24 已備、`wails doctor` 通過);Docker Desktop 29.4.1。**注意:winget 裝的 Go/Wails 不在本 session 既有 shell 的 PATH**,執行 go/wails 前需 `export PATH="/c/Program Files/Go/bin:$HOME/go/bin:$PATH"`。鎖版:docker v27.5.1(go-connections **必須** v0.5.0,v0.7.0 會壞 Windows 編譯)、gorilla/websocket v1.5.3、modernc/sqlite v1.34.5、go-keyring v0.2.8、go-toml/v2。
