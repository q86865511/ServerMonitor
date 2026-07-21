# gui-redesign — 任務清單(tasks.md)

> 建立日期:2026-07-16｜狀態:已核可(2026-07-17;同日依 Codex 二審 18 條修訂,rev.2);實作完成 2026-07(任務全數收官,見 PROGRESS.md)
> 本檔為 /pipeline 任務清單來源:pipeline 第 1 步直接採用本清單與 HARD/NORMAL 標記,不重新拆解。
> 勾選(`- [x]`)只由 pipeline 第 5 步收尾回寫,其他階段不動。
> 前置注意:開工前提=claude/session-c0092e 已併入 master(併入經使用者確認執行),重構分支自併入後的 master 開出;主 repo 工作樹目前 checkout 於 codex/graphite-ops-gui(棄用分支),實作 session 用獨立 worktree,勿基於該分支。tasks 內後端行號引用以併入後實際碼重新定位。

## 任務

- [x] T1 [HARD] 後端擴充—名稱與連接埠:schema.go 追加 migration v3(instances.name+port_reservations.name+metrics 表一次到位)、store_records/instance_service/CreateInstanceRequest/InstanceDTO 透傳 name、建立實例時把範本 PortSpec.Name 寫入埠預留、api.go PortsByInstance()+InstanceDTO.Ports(含 name;一次查詢分組免 N+1)(對應 R10、R12;驗收:migration 套用後欄位與表存在之單測綠、埠預留含 name 之測試綠、`go test ./...` 全綠;依賴:無)
- [x] T2 [HARD] 後端擴充—指標時序:store_metrics.go(Insert/QueryMetrics/QueryMetricsSummary/Prune;記憶體欄 nullable)、metricsrecorder.go(CPU/RAM 仿 threshold statsSource 訂閱 stats 流、玩家數自 MonitorHub.Snapshot 取、2s→15s 聚合:cpu=均值/memory=末樣本/memory_bytes==0 存 NULL、36h 保留)、生命週期:bootstrap 建構+lifecycle start/stopMonitoring 掛 Watch/Stop+Runtime.Shutdown 收束、Prune ticker 每小時且獨立於 watcher、app.go 兩個查詢綁定(Summary 分母僅計有樣本實例)(對應 R13;驗收:餵假 stats 流驗聚合語意/NULL 樣本/玩家數自快照/Close 後不寫入/Prune 之單測綠;依賴:T1)
- [x] T3 [NORMAL] 後端擴充—uptime:monitor.go MonitorSnapshot 加 StartedAt、pollOnce 經注入 statusFetcher 取值、bootstrap 注入、SnapshotDTO 透傳 RFC3339(對應 R11 後端半部;驗收:運行中實例 GetSnapshot 回非空 StartedAt、停止為空之測試綠;依賴:無)
- [x] T4 [HARD] 後端擴充—範本 icon:GameTemplate.Icon、template_engine 記 sourceDir+IconPath(**filepath.Rel 判定不逃逸+regular file 檢查,禁字串前綴法**)、main.go AssetServer Handler(`/tpl-icons/{id}`,rt nil 或無圖 404)、TemplateDTO.HasIcon、內建兩範本附圖(對應 R14;驗收:有圖 200/無圖 404/`../` 404/**sibling-prefix 目錄 404**/symlink 外逸 404(無權建 symlink 可 skip 註明)之 handler 單測綠;依賴:無)
- [x] T5 [NORMAL] wailsjs 重生成與型別校對:`wails build` 重生成 frontend/wailsjs/,校對 App.d.ts 出現 Name/Ports/StartedAt/HasIcon/QueryMetrics*,並確認 c0092e 已併入的成員仍在(DockerAvailable/CurseForge*/runtime/runtimes/docker_available/memory_mb/cpu_percent)(對應 R10–R14 交付、design 相容基準;驗收:App.d.ts 含上列全部成員、`npm run check` 對 wailsjs 無型別錯;依賴:T1–T4)
- [x] T6 [NORMAL] design tokens+基礎 UI 元件:style.css :root 擴充(沿用既有變數名),ui/ 建 Button/IconButton/Card/Badge/StatusBadge/MetricCard/Tabs/Modal/Dropdown/Tooltip/SearchInput/Select/TextField/ProgressBar/EmptyState/ErrorState/Skeleton/ConfirmDialog/Toast(改造遷入 Modal/ConfirmDialog/ToastHost/StatePill,全 runes)(對應 R1;驗收:`npm run check` 0 錯、互動元件四態樣式齊、無元件內硬編碼色值;依賴:無)
- [x] T7 [HARD] 複雜 UI 元件:LogViewer(keyed each+content-visibility+3000 行上限+暫停捲動/新日誌提示/篩選搜尋/複製匯出)、TrendChart+Sparkline(手刻 SVG)、DataTable(對應 R1、R7 部分;驗收:灌 ≥100 行/秒模擬資料 UI 不凍結、check 0 錯;依賴:T6)
- [x] T8 [HARD] stores 層:instances.ts(5s 輪詢+refreshSeq 上移)、metrics.ts(QueryMetrics 回填+stats 事件 15s 降採樣入 ring 720 點)、logs.ts(buffer 3000 行+等級正規解析+訂閱引用計數+30s 延遲釋放+防孤兒訂閱)、toasts.ts;元件一律不得自行 EventsOn(對應 R4/R7 資料層;驗收:check 0 錯、切頁往返 DevTools 驗無殘留訂閱;依賴:T5)
- [x] T9 [NORMAL] 路由+Shell:router.ts(hash,11 路由+navigate)、Sidebar 重寫(9 項導覽+收合 56px)、Topbar(標題/全域搜尋/通知/系統狀態/新增鈕)、NodeBanner 遷入、App.svelte 重組為 shell+outlet、main.go 視窗 1440×900+Min 1280×720(對應 R2、R3;驗收:11 條 hash 直達、壞 uuid 顯錯誤態、1280×720 收合可操作;依賴:T6、T8(全域搜尋與壞 uuid 判斷消費 instances store))
- [x] T10 [NORMAL] 總覽儀表板:DashboardPage(5 統計卡 nil 跳過/ServerCard 網格範本驅動(名稱含 name fallback「範本名 #uuid8」、uptime、位址=第一個 required 埠、runtime Badge)/QueryMetricsSummary 全體趨勢/QueryEvents 20 筆時間軸/節點狀態),icon+onerror 佔位(對應 R4、R5、R10/R11 前端半部、R15;驗收:空清單 EmptyState、統計全 nil 顯「不適用」、name 空顯 fallback 不顯空白、卡片啟停/重啟/移除經 bindings 且 per-card in-flight 鎖(運行中顯停止/重啟,停止顯啟動)、臨時加第三範本 toml 免改碼正常顯示;依賴:T7、T8、T9)
- [x] T11 [HARD] 伺服器詳細頁+主控台:ServerDetailPage(頁首操作+路由驅動 Tabs+uptime 本地遞增)、OverviewTab(趨勢回填+即時/埠(含 name)/磁碟/快速操作)、ConsoleTab(LogViewer 組合+指令三態搬移+右欄資訊/排程倒數/告警門檻)、**BackupsPanel/SchedulesPanel 共用元件產出**(uuid 為 prop 的單一實作,BackupsTab/SchedulesTab 組裝)、SettingsTab(唯讀組態檢視:範本/變體/參數(secrets 遮罩)/埠/runtime+「修改參數需重建」提示,不做編輯)(對應 R6、R7、R11 前端半部;驗收:分頁往返 buffer 不丟、`#/servers/:uuid/console` 直達、移除含 purge 確認後返清單、**輪詢發現實例被外部移除→轉錯誤態/返清單並釋放訂閱引用**;依賴:T7、T8、T9)
- [x] T12 [HARD] 建立精靈:wizard/ 四步(基本資訊→範本圖卡+搜尋→資源與參數→確認),自 c0092e 版 CreateWizard 功能等價搬移全部邏輯:runtime 選擇(DockerAvailable 置灰)、memory_mb/cpu_percent、params/secrets/EULA/SecretRef 欄位渲染、模組包 CF 金鑰三態(needsCFKey/cfNativeUnavailable/placeholder 隨 runtime)、provision 進度(EventsOn/Off 隨精靈生命週期)、blocked-mods 對話框+BrowserOpenURL;逐步驗證/in-flight 鎖/失敗重試/關閉確認,成功跳轉詳細頁(對應 R8;驗收:缺必填被阻擋並指出缺項、連點送出僅一請求、EULA 未接受不得建立、native+CF 未啟用時 CF 選項置灰、provision 進度與 blocked-mods 對話框以模擬事件驗證、docker-only 範本+Docker 不可用顯「無可用執行後端」阻擋、資源欄位僅 native 顯示送值、第三範本(含 int/bool/secret 欄位)在精靈正確渲染、name 輸入隨 request 送出;依賴:T5、T6、T9)
- [x] T13 [NORMAL] 全域頁面遷移:Templates/Nodes(在線與 Docker 可用雙狀態+RetryDocker)/Backups/Schedules/Alerts(實例選擇器+與詳細頁分頁複用同一元件)/Events(DataTable+篩選)/Settings(含 CurseForge API key 卡,自 c0092e 的 CurseForgeSettings.svelte 搬移)各頁;舊 SettingsView/三 Panel/EventsView/CurseForgeSettings 搬移後刪除;備份/排程頁**複用 T11 產出的 BackupsPanel/SchedulesPanel**(單一實作,不另寫)(對應 R9;驗收:備份/還原/排程 CRUD/警報/事件篩選/CF key 設定與現況等價無迴歸;依賴:T6、T9、T11)
- [x] T14 [NORMAL] 四態補齊與防重複:先列「頁 × 適用狀態」矩陣(總覽/伺服器清單/詳細五分頁/精靈四步/**七個全域頁**;不適用格註明理由,如精靈步驟①無 empty/loading 資料態),再逐格勾驗 loading/empty/error/disabled;操作按鈕 in-flight 鎖+成功失敗 toast;渲染路徑無 mock 常數;趨勢圖 NULL 畫缺口不補 0(對應 R15、R16;驗收:矩陣逐格勾畢(附矩陣)、同操作連點僅一請求;依賴:T10–T13)
- [x] T15 [NORMAL] 驗證收尾:`go build ./...`+`go test ./...`+`npm run check`+`npm run build`+`wails build` 全綠;手寫碼 any 掃描(`grep -rnE ": any|as any|<any>" frontend/src` 排除 wailsjs,為空);`wails dev` 三尺寸(1280×720/1440×900/1920×1080)實跑巡檢;高頻日誌量化實測(≥100 行/秒持續 ≥60s,PerformanceObserver longtask p95 <200ms 且篩選輸入即時回應);文件更新(PROGRESS.md/README GUI 章節/專案 CLAUDE.md 前端慣例)(對應 非功能需求、完成定義;驗收:各指令實際輸出貼於回報、三尺寸無關鍵操作遮蔽、longtask 量測數據附上;依賴:T1–T14)

<!-- 覆蓋檢查:R1→T6/T7;R2,R3→T9;R4→T8/T10;R5→T10;R6→T11;R7→T7/T8/T11;R8→T12;R9→T13;R10→T1/T10/T12;R11→T3/T10/T11;R12→T1/T10/T11;R13→T2;R14→T4;R15→T10/T14;R16→T14;非功能→T15。每任務皆有 R# 或非功能對應。依賴 DAG:T9→{T6,T8};T12→{T5,T6,T9};T13→{T6,T9,T11}。 -->

## 完成定義

- 全部任務勾選完成,且各任務驗收條件有實跑證據。
- requirements.md 每條 R# 的驗收條件通過 pipeline 雙審對照。
- 相關文件(PROGRESS.md、README)依全域規則更新。
- 實作報告列出「參考圖有但依規格排除」項:玩家延遲 ping、網路流量圖、TPS、玩家清單、檔案管理。
