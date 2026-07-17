# gui-redesign — 需求規格(requirements.md)

> 建立日期:2026-07-16｜狀態:已核可(2026-07-17)
> 需求主體使用 EARS 句式。
> 背景裁決(2026-07-16/17):五項後端擴充納入本規格;基底=claude/session-c0092e 併入 master 後的程式碼;codex/graphite-ops-gui 分支棄用、僅作視覺參考;c0092e 新增的前端功能(精靈 runtime/資源/CF/供應進度、CurseForge 設定卡、卡片 runtime 標示)須功能等價吸收(見 R5/R8/R9)。

## 概述

依 4 張參考設計圖(深色專業桌面管理介面)重構 ServerMonitor 的完整前端 GUI:建立統一設計系統與 Application Shell,新增總覽儀表板、伺服器詳細頁、頁內主控台與四步驟建立精靈,並以五項最小後端擴充補齊畫面所需資料。為長時間操作多遊戲 dedicated server 的管理者,提供一致、專業、高資訊密度的桌面介面。

## 範圍(含明確不做)

- 包含:design tokens 與共用元件庫、Application Shell(側欄+頂欄)、hash 路由、總覽儀表板、伺服器詳細頁(概覽/主控台/備份/排程/設定分頁)、四步驟建立精靈、全域頁面(範本/節點/備份/排程/警報/事件/設定)、五項後端擴充(顯示名稱/uptime/連接埠/指標時序/範本 icon)。
- 明確不做:玩家延遲 ping、網路流量圖、Minecraft TPS、逐玩家清單(後端無資料來源且超出 game-server-manager 規格 R6 指標集);詳細頁「玩家」「檔案」分頁(後端無對應 API);i18n 語言切換與淺色主題(維持繁中深色);行動版版型(小視窗僅收合側欄)。

## 需求

### R1 設計系統與共用元件

系統應提供單一組 design tokens(色彩/間距/字階/圓角/陰影/動效時長)與共用 UI 元件庫,所有頁面經由它們構成。

驗收條件:
- `style.css` 的 `:root` 為唯一 token 定義點,沿用既有變數名(--bg-*/--fg-*/--accent/--ok/--busy/--err/--off)並擴充,無元件內硬編碼色值(SVG 圖表漸層等技術例外需註記)。
- 共用元件至少含:Button、IconButton、Card、Badge、StatusBadge、MetricCard、Tabs、Modal、Dropdown、Tooltip、SearchInput、Select、TextField、ProgressBar、EmptyState、ErrorState、Skeleton、ConfirmDialog、Toast、DataTable、LogViewer、Sparkline/TrendChart。
- 互動元件具 hover/focus/active/disabled 樣式;focus 可見(鍵盤可操作)。

### R2 Application Shell

系統應提供固定左側導覽(總覽/伺服器/遊戲範本/節點/備份/排程/警報/事件/設定)與頂部工具列(頁面標題、全域搜尋、通知入口、系統狀態、新增伺服器主鈕)的共用外框。

驗收條件:
- 視窗寬 1280px 時,側欄可收合為 icon 列,主內容區無關鍵操作被遮蔽。
- 全域搜尋輸入伺服器名稱片段時,可跳轉至符合的伺服器詳細頁。
- 節點離線時,shell 顯示可見的離線提示(沿用 NodeBanner 行為)。

### R3 路由與深連結

當使用者切換頁面或進入某伺服器詳細頁時,系統應以 hash 路由(`#/servers/:uuid/:tab` 等)反映位置,重新整理後停留在原頁。

驗收條件:
- 總覽、伺服器清單、伺服器詳細(5 個分頁)、範本、節點、備份、排程、警報、事件、設定各路由直接輸入 hash 可達。
- 不存在的 uuid 路由顯示錯誤態頁面而非白屏。

### R4 總覽儀表板

系統應提供總覽頁,聚合顯示:伺服器總數、運行中數量、線上玩家總和、平均 CPU、平均 RAM 五張統計卡,伺服器卡片網格,全體 CPU/RAM 歷史趨勢圖,最近事件時間軸,與節點連線狀態。

驗收條件:
- 統計值全部由既有 bindings(ListInstances/GetSnapshot/NodeStatus)與新時序查詢聚合而得;player_count 為 nil 的實例在加總時跳過,全部為 nil 時顯示「不適用」而非 0。
- 無任何實例時顯示 EmptyState 並引導建立;趨勢圖無歷史資料時顯示空態而非空白圖。
- 最近事件來自 QueryEvents(限 20 筆),時間軸按時間新到舊。

### R5 伺服器卡片(範本驅動)

系統應由實例資料+範本資料動態產生伺服器卡片(範本圖示、名稱、狀態、位址:連接埠、玩家數、運行時間、CPU/RAM、管理/啟停/更多操作),不得在 UI 硬編碼遊戲清單或以 template_id 分支呈現邏輯。

驗收條件:
- 前端程式碼中不存在以特定遊戲 id(minecraft/palworld)為條件的呈現分支(範本能力欄位驅動除外)。
- 新增第三個範本 toml 後,不改前端程式碼即可正常顯示其卡片與精靈欄位。
- 卡片操作(啟動/停止/重啟/移除)呼叫既有 bindings,操作進行中該卡按鈕鎖定並顯示 loading。
- 卡片顯示實例 runtime(docker/native,InstanceDTO.runtime,c0092e 已有),以 Badge 呈現。

### R6 伺服器詳細頁

當使用者點擊卡片「管理」或經路由進入時,系統應顯示伺服器詳細頁:頁首(名稱、範本、節點、玩家數、運行時間、狀態、啟動/停止/重啟/移除)與分頁(概覽/主控台/備份/排程/設定)。

驗收條件:
- 概覽分頁含:CPU/RAM 歷史趨勢(時序查詢回填+即時事件追加)、連接埠清單、磁碟用量(無法採集顯示「不適用」)、快速操作(建立備份/跳轉主控台)。
- 移除操作有 ConfirmDialog 且含 purge 勾選(沿用既有行為);成功後返回伺服器清單。
- 分頁切換不重建整頁;直接以 `#/servers/:uuid/console` 進入時主控台為作用分頁。

### R7 即時主控台

於伺服器詳細頁主控台分頁期間,系統應串流顯示日誌(前端解析 INFO/WARN/ERROR 等級並標色)、提供等級篩選、關鍵字搜尋、暫停/恢復自動捲動、清除、複製/匯出,並依 GetCommandCapability 提供指令輸入(rcon 自由輸入/rest 具名動作/none 停用+提示)。

驗收條件:
- 日誌 buffer 上限 3000 行/實例,超出丟舊留新;高頻輸出(≥100 行/秒)期間 UI 仍可操作(捲動、篩選、輸入不凍結)。
- stderr 行與含 ERROR/SEVERE/FATAL 的行標為 error 級,含 WARN 的標為 warn 級;等級篩選與搜尋可同時生效。
- 使用者上捲時暫停自動捲底並顯示「新日誌」提示,點擊回到底部。
- 切至其他分頁再返回,buffer 不丟失;離開詳細頁 30 秒後才真正取消訂閱(EventsOff+UnsubscribeLogs),無孤兒訂閱。
- 指令送出後回顯結果;連線失敗顯示明確錯誤,GUI 不卡死(對齊 game-server-manager R7)。

### R8 新增伺服器精靈(含 c0092e 功能等價吸收)

當使用者點擊「新增伺服器」時,系統應開啟四步驟精靈:①基本資訊(名稱、節點)→②選擇遊戲範本(圖卡+搜尋+runtime 標籤)→③資源與參數(runtime 選擇、記憶體/CPU 上限、params/secrets、模組包)→④確認建立;並功能等價保留 c0092e 版 CreateWizard 的全部新能力。

驗收條件:
- 範本清單來自 ListTemplates,含圖示(HasIcon 者載入 `/tpl-icons/{id}`,否則佔位圖);參數欄位由範本 params/secrets 宣告動態渲染(text/int/bool/EULA/SecretRef/模組包,邏輯等價 c0092e 版 CreateWizard)。
- runtime 選擇:選項來自 TemplateDTO.runtimes;DockerAvailable()=false 時 docker 選項置灰並提示;僅 1 項時不顯示選擇器。
- 資源上限:memory_mb/cpu_percent 輸入隨 CreateInstanceRequest 送出(c0092e 已有欄位)。
- 模組包 CF 流程等價 c0092e:docker 路徑 CF 來源需填 CF_API_KEY;native 路徑由 CurseForgeEnabled() 決定 CF 選項可用性(false 置灰+提示);ref placeholder 隨 runtime 切換(native=projectID:fileID 或 cfzip 連結;docker=slug 或整合頁 URL)。
- 建立期間訂閱 `provision` 事件顯示供應進度(stage/percent/detail);stage="blocked-mods" 時解析清單彈出對話框,列出被擋模組並可經 BrowserOpenURL 開啟下載頁(等價 c0092e 行為)。
- 每步「下一步」前驗證,缺漏必填時阻擋並指出缺項(對齊 game-server-manager R2);上一步保留已輸入值。
- 建立中鎖定送出鈕防重複送出;失敗留在確認步顯示可讀錯誤並可重試;成功後跳轉新實例詳細頁。
- 已有輸入時關閉精靈需確認;provision 事件訂閱於精靈關閉時解除(EventsOff)。

### R9 全域頁面(功能等價遷移)

系統應將既有 SettingsView 內的備份/排程/警報面板與 EventsView 遷移為獨立頁面(備份/排程/警報頁含實例選擇器),並新增範本頁(範本圖卡總覽)、節點頁(節點狀態、Docker 能力、RetryDocker)、設定頁(含 c0092e 的 CurseForge API key 卡片)。

驗收條件:
- 備份列表/立即備份/還原、排程 CRUD、警報設定讀寫、事件查詢篩選,功能與現況等價無迴歸。
- 詳細頁的備份/排程分頁與全域頁複用同一元件(單一實作)。
- 節點頁區分「節點在線」與「Docker 可用」兩種狀態顯示(NodeStatusDTO.docker_available,c0092e 已有);RetryDocker 語意為「僅重建 docker 子後端」。
- 設定頁含 CurseForge 卡片:CurseForgeKeyConfigured() 顯示已設定狀態、SetCurseForgeAPIKey() 寫入(金鑰庫直寫、熱生效),功能等價 c0092e 的 CurseForgeSettings.svelte。

### R10 實例顯示名稱(後端擴充)

系統應允許建立實例時指定顯示名稱,並於清單/卡片/詳細頁顯示。

驗收條件:
- instances 表以追加 migration(version 3)加 name 欄位;CreateInstanceRequest 與 InstanceDTO 加 name;既有實例 name 為空。
- name 為空時前端顯示「範本名 #uuid 前 8 碼」fallback,不顯示空字串。

### R11 運行時間(後端擴充)

系統應於 SnapshotDTO 暴露實例的 StartedAt(RFC3339),前端據此顯示運行時間並本地遞增。

驗收條件:
- 運行中實例的 GetSnapshot 回傳非空 StartedAt;停止或未知時為空,前端顯示「—」。
- 取得管道經 MonitorHub 既有輪詢(≤10s 更新),不新增獨立輪詢迴圈。

### R12 已分配連接埠(後端擴充)

系統應於 InstanceDTO 暴露該實例的已分配連接埠(bind_ip/protocol/host_port),供卡片與詳細頁顯示。

驗收條件:
- 資料來自既有 port_reservations 表;ListInstances 一次查詢分組,不做 N+1。
- 無保留埠的實例,卡片位址欄顯示「—」。

### R13 指標時序持久化(後端擴充)

系統應每 15 秒將各受監控實例的 CPU/記憶體/玩家數聚合寫入 metrics 資料表,保留 36 小時,並提供 QueryMetrics(uuid, since) 與 QueryMetricsSummary(since) 綁定供趨勢圖查詢。

驗收條件:
- 寫入掛在既有 MonitorHub stats 流(2s)之上做 15s 聚合;實例停止監控即停寫。
- QueryMetrics 回傳按時間升冪的點列;QueryMetricsSummary 回傳全體實例 15s bucket 平均。
- 超過 36 小時的資料由每小時清理批刪;`go test ./...` 含 store_metrics 與 recorder 的單元測試。

### R14 範本 icon(後端擴充)

系統應允許範本 toml 宣告選配 icon(範本目錄相對路徑),經 AssetServer Handler 以 `GET /tpl-icons/{id}` 服務;前端無圖時以「遊戲名首字+依 id 生成色塊」佔位。

驗收條件:
- icon 路徑經 Clean+前綴檢查,`../` 逃逸請求回 404;無 icon 或 rt 未就緒回 404,前端 onerror 換佔位。
- 內建 minecraft/palworld 範本各附一張圖;TemplateDTO 含 HasIcon。

### R15 資料誠實與空態

若後端未提供某項資料(nil/欄位不存在/採集不支援),則系統應顯示「不適用」「—」或 empty/unavailable 狀態,不得顯示捏造數值或永久假資料。

驗收條件:
- player_count/online 為 nil → 「不適用」;磁碟不可採集 → 「不適用」;StartedAt 空 → 「—」。
- 前端程式碼無寫死的示範數值(mock 常數)進入正式渲染路徑。

### R16 非同步狀態完整性

當任何頁面載入資料或執行操作時,系統應呈現對應的 loading(Skeleton/spinner)、empty、error(含重試)、disabled 狀態,操作類按鈕於進行中防重複點擊,完成後有成功/失敗 toast 回饋。

驗收條件:
- 逐頁巡檢清單(總覽/清單/詳細五分頁/精靈四步/六個全域頁)每頁四態齊備。
- 同一操作連點兩次只發出一次請求(以按鈕 disabled 或 in-flight 鎖驗證)。

## 非功能需求

- 視窗:最小 1280×720(main.go 設 MinWidth/MinHeight),預設 1440×900;於 1280×720/1440×900/1920×1080 三尺寸無版面破版。
- 效能:日誌高頻輸出不使前端事件迴圈阻塞逾 200ms(對齊 game-server-manager R6);LogViewer 用固定上限+content-visibility,不因日誌累積使 DOM 無限膨脹。
- 相依:不新增前端 runtime 依賴(圖表手刻 SVG、路由自製);不動已鎖定 Go 依賴版本。
- 型別:TypeScript 無 any;svelte-check 0 錯誤。
- 相容:`go test ./...`、`wails build`、`npm run build` 全綠;基底為 master 併入 claude/session-c0092e 之後,c0092e 新增的前端功能(精靈 runtime/資源/CF/供應進度、CurseForge 設定卡、卡片 runtime 標示)全部功能等價保留,不得迴歸。
- 語言與風格:繁中 UI;系統字型+日誌 monospace;深色主題;基本無障礙(focus 可見、對比達可讀水準、Esc 關閉 modal)。

## 開放問題

- 無(五項後端擴充歸屬、功能名、基底分支、codex 分支處置已由使用者裁決於 2026-07-16/17)。
