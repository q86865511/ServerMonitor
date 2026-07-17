# gui-redesign — 技術設計(design.md)

> 建立日期:2026-07-16(2026-07-17 依 c0092e 落地內容修訂)｜狀態:已核可(2026-07-17)
> 對照 requirements.md;每條 R# 見需求對應表。
> bindings 基準:**master 併入 claude/session-c0092e 後**的 app.go——既有方法+新增 DockerAvailable/CurseForgeEnabled/CurseForgeKeyConfigured/SetCurseForgeAPIKey;事件 `logs:<uuid>`/`stats:<uuid>`/`provision`(全域,payload {stage,percent,detail},含 stage="blocked-mods");DTO 新欄位 runtime/runtimes/docker_available/memory_mb/cpu_percent(已實測存在於 c0092e 的 wailsjs)。本檔引用的檔案:行號以撰寫時的 master 為準,實作時以併入後實際碼重新定位。

## 架構概述

前端整體重寫為「tokens → ui/ 元件庫 → shell(側欄+頂欄)+ 自製 hash 路由 → pages/」四層,狀態集中於 stores/(instances 輪詢、metrics ring、logs buffer 皆在 store 層管理訂閱,元件不直接 EventsOn)。後端做五項最小擴充(schema v3 遷移:instances.name+metrics 表;SnapshotDTO.StartedAt;InstanceDTO.Ports;MetricsRecorder+2 個查詢綁定;template icon+AssetServer Handler),完成後以 `wails build` 重生成 wailsjs 型別,前端全部走真實 bindings。零新增前端 runtime 依賴:路由自製(~60 行)、圖表手刻 SVG、日誌用固定上限+content-visibility。

## 需求對應表

| 需求 | 設計元素 | 說明 |
|---|---|---|
| R1 | `frontend/src/style.css` :root 擴充;`frontend/src/lib/ui/`(22 元件,Svelte 5 runes) | tokens 沿用既有變數名擴充;元件統一互動狀態 |
| R2 | `lib/shell/`(Sidebar 重寫/Topbar 新增/NodeBanner 改造);App.svelte 重寫為 shell+outlet | 收合側欄 56px icon 列;搜尋比對 instances store |
| R3 | `lib/router.ts`(hashchange→{page,params} store+navigate()) | 11 條路由;壞 uuid → ErrorState 頁 |
| R4 | `lib/pages/DashboardPage.svelte`+`stores/instances.ts`+QueryMetricsSummary+QueryEvents | 聚合統計 nil 跳過;EmptyState 引導建立 |
| R5 | `ui/ServerCard.svelte`(消費 InstanceDTO+TemplateDTO+SnapshotDTO) | 一切由範本欄位驅動;icon 走 /tpl-icons/{id}+onerror 佔位;runtime Badge |
| R6 | `lib/pages/server/ServerDetailPage.svelte`+OverviewTab/ConsoleTab/BackupsTab/SchedulesTab/SettingsTab | 頁首操作用既有 Start/Stop/Restart/Remove;Tabs 由路由 :tab 驅動 |
| R7 | `ui/LogViewer.svelte`+`stores/logs.ts`+ConsoleTab(指令列邏輯自 Console.svelte:119-163 搬移) | 等級入 buffer 時解析一次;3000 行上限;引用計數訂閱+30s 延遲釋放 |
| R8 | `lib/pages/wizard/`(CreateWizard+Step1~4);全部邏輯自 **c0092e 版** CreateWizard 搬移(runtime 選擇/DockerAvailable 置灰/memory_mb+cpu_percent/CF 金鑰三態/provision 進度/blocked-mods 對話框/BrowserOpenURL) | 逐步驗證;in-flight 鎖;provision 訂閱隨精靈生命週期(關閉 EventsOff) |
| R9 | `lib/pages/`(Templates/Nodes/Backups/Schedules/Alerts/Events/Settings Page);Backups/Schedules/Alerts 邏輯自既有 Panel 搬移;CurseForge 卡自 c0092e 的 CurseForgeSettings.svelte 搬移 | 詳細頁分頁與全域頁複用同一元件;節點頁雙狀態(在線/Docker 可用) |
| R10 | schema.go 追加 version 3(`ALTER TABLE instances ADD COLUMN name`);store_records.go;instance_service.go CreateOptions;app.go 兩個 DTO | 遷移慣例:只追加不改舊項(schema.go:14-15) |
| R11 | monitor.go MonitorSnapshot(:95)加 StartedAt+pollOnce(:378)經注入 statusFetcher 取 RuntimeStatus.StartedAt(dto.go:89);bootstrap.go:326 注入;app.go SnapshotDTO | 掛既有 10s 輪詢,不加迴圈 |
| R12 | api.go 加 Runtime.PortsByInstance()(讀 store_ports.go:55 ListPortReservations 一次分組);app.go InstanceDTO.Ports | 免遷移;避免 N+1 |
| R13 | schema v3 加 metrics 表;新檔 store_metrics.go(Insert/QueryMetrics/QueryMetricsSummary/Prune);新檔 metricsrecorder.go(仿 threshold.go:66,163 statsSource 訂閱模式);lifecycle.go start/stopMonitoring 掛 Watch/Stop;app.go 兩個查詢綁定 | 2s 流聚合 15s 入庫;36h 保留每小時 Prune;單連線序列化無寫入壓力 |
| R14 | protocol/template.go:15 加 Icon;template_engine.go 記 sourceDir+IconPath(id)(Clean+前綴檢查);main.go:23 assetserver.Options 加 Handler(embedded 查無時 fallback);app.go HasIcon;templates/ 兩張圖 | rt 未就緒 nil-check 回 404;前端 onerror 佔位兜底 |
| R15 | 各頁渲染規則+format.ts helper(fmtMaybe/fmtUptime/fmtBytes) | nil→「不適用」/「—」;無 mock 常數進渲染路徑 |
| R16 | ui/ 的 Skeleton/EmptyState/ErrorState/Toast+`api.ts` call() 包裝+各操作 in-flight state | 既有 call()→toast 錯誤慣例沿用(api.ts:22) |

## 介面與資料模型

**新增/變更 Go 綁定**(既有方法不動):
- `CreateInstanceRequest` +`Name string`(app.go:136);`InstanceDTO` +`Name string, Ports []InstancePortDTO`(app.go:115);`InstancePortDTO{BindIP, Protocol string, HostPort int}`(新)。
- `SnapshotDTO` +`StartedAt string`(RFC3339,空=不適用)(app.go:125)。
- `TemplateDTO` +`HasIcon bool`(app.go:73)。
- 新方法:`QueryMetrics(uuid string, sinceUnix int64) ([]MetricPointDTO, error)`、`QueryMetricsSummary(sinceUnix int64) ([]MetricPointDTO, error)`;`MetricPointDTO{TsUTC string, CPUPercent float64, MemoryBytes, MemoryLimit int64, PlayerCount *int}`。
- HTTP(非綁定):`GET /tpl-icons/{id}` → image bytes 或 404。

**DB**(schema.go 追加 version 3,一次遷移含兩件事):
```sql
ALTER TABLE instances ADD COLUMN name TEXT NOT NULL DEFAULT '';
CREATE TABLE metrics (instance_uuid TEXT NOT NULL, ts_utc TEXT NOT NULL,
  cpu_percent REAL NOT NULL, memory_bytes INTEGER NOT NULL,
  memory_limit INTEGER NOT NULL, player_count INTEGER,
  PRIMARY KEY (instance_uuid, ts_utc));
CREATE INDEX idx_metrics_ts ON metrics(ts_utc);
```

**templates schema**:`GameTemplate` +`Icon string`(toml:"icon",範本目錄相對路徑,選配)。

**前端目錄**:
```
frontend/src/lib/
├── router.ts                      # hash 路由(新)
├── api.ts / format.ts             # 保留;format 補 helper
├── stores/  instances.ts(5s 輪詢+refreshSeq 上移) metrics.ts(ring+回填) logs.ts(buffer+等級+引用計數) toasts.ts
├── ui/      22 個共用元件(全 runes)
├── shell/   Sidebar Topbar NodeBanner
└── pages/   DashboardPage ServersPage server/(DetailPage+5 Tab) wizard/(4 步) Templates/Nodes/Backups/Schedules/Alerts/Events/Settings Page
```
舊檔處置:Modal/ConfirmDialog/ToastHost/StatePill/NodeBanner 改造遷入;Sidebar/App/InstanceList/InstanceCard/Console/CreateWizard/SettingsView 重寫後刪除;EventsView/三 Panel/CurseForgeSettings 邏輯搬移後刪除;stores.ts 拆散。

## 關鍵流程

1. **趨勢圖資料流**:MonitorHub stats(2s)→ MetricsRecorder 聚合 15s → SQLite metrics 表(36h)。前端進頁 → QueryMetrics(uuid, now-3h) 回填 ring(720 點上限)→ `stats:${uuid}` 事件即時 15s 降採樣追加 → TrendChart(SVG) 重繪。總覽走 QueryMetricsSummary。
2. **日誌流**:進主控台分頁 → logs store `acquire(uuid)`(首個引用才 SubscribeLogs+EventsOn;等 subscribe promise resolve 才允許 unsubscribe,沿用 Console.svelte:49-51 防孤兒)→ 行入 buffer 時正規解析等級(stderr→error;/ERROR|SEVERE|FATAL/i→error;/WARN(ING)?/i→warn)→ LogViewer 渲染(keyed each+content-visibility)→ 離開 `release(uuid)` 30s 後真正 Unsubscribe。
3. **建立流程**:精靈四步收集 → 開始建立時 EventsOn('provision') → CreateInstance({name, template_id, variant, params, secrets, node, modpack, runtime, memory_mb, cpu_percent}) → 進度列顯示 stage/percent/detail;stage="blocked-mods" 解析清單彈對話框(BrowserOpenURL 開下載頁)→ 成功 navigate 到 `#/servers/{uuid}`;失敗 inline 顯示留在確認步;精靈關閉 EventsOff('provision')。訂閱集中規則的例外:provision 為全域事件且僅精靈消費,隨精靈生命週期管理,不入 stores。
4. **icon 流**:toml `icon="icons/x.png"` → LoadDir 記 sourceDir → 前端 `<img src="/tpl-icons/{id}">` → AssetServer Handler → IconPath 安全檢查 → 檔案 bytes;任何失敗 → 404 → onerror → 佔位(首字+id-hash HSL 色塊)。

## 取捨與替代方案

- 採用 **自製 hash 路由**:11 條路由 60 行可解,免依賴且 WebView2 歷史/深連結免費。捨棄 svelte-spa-router(新依賴)與純 store 路由(重新整理丟位置)。
- 採用 **固定上限+content-visibility** 的 LogViewer:pre-wrap 不定行高使虛擬列表需量測高度,自製成本與 bug 面大。捨棄虛擬列表。
- 採用 **後端 15s 聚合時序**(使用者裁決):跨重啟保留、總覽可做全體平均。捨棄前端 session ring(重啟即清)與不做趨勢。
- 採用 **AssetServer Handler** 服務 icon:瀏覽器可快取、`<img>` 語意自然。捨棄 base64 經 binding(JSON 膨脹、無快取)。
- 採用 **Svelte 5 runes**(新元件一律):官方方向;跨頁狀態仍用 svelte/store(interop 官方支援)。捨棄全面沿用傳統 class API。
- 採用 **metrics 與 name 同一個 migration v3**:同批發布,避免版本碎片。

## 風險

- 基底時序:開工前提是 c0092e 已併入 master(使用者確認後執行併入);T1-T4 的後端落點(app.go/bootstrap.go/lifecycle.go/monitor.go)在 c0092e 上有變動,任務內引用的行號以併入後實際碼為準重新定位。
- codex/graphite-ops-gui 分支不併入(使用者裁決棄用):重構期間主 repo 工作樹 checkout 於該分支,實作 session 需在獨立 worktree 或切回 master 後進行,避免誤基於 codex 分支。
- wailsjs 型別漂移:後端擴充完成即 `wails build` 重生成;之後任何 Go 簽章變動重生成,`wailsjs/` 不手改。
- Svelte 5 runes 與傳統 API 同檔混用會編譯錯:改造元件整檔一次轉。
- icon Handler 在 OnStartup 前 rt 為 nil:Handler nil-check 回 404。
- SQLite 寫入頻率:每實例 15s 一筆經單連線序列化,10 實例 0.67 筆/s,安全;Prune 走 idx_metrics_ts 批刪。

## 測試策略

- R10-R13(Go):單元測試——migration v3 套用後欄位/表存在、store_metrics 插入/查詢/Prune、MetricsRecorder 聚合正確性(餵假 stats 流驗 15s 聚合)、IconPath 的 `../` 逃逸擋下;`go test ./...` 全綠。
- R14(HTTP):handler 單測(有圖 200/無圖 404/逃逸 404)。
- R1-R9、R15-R16(前端):`npm run check`(svelte-check 0 錯誤、無 any)+`npm run build`;無前端測試框架(現況),行為以 `wails dev` 實跑巡檢矩陣驗收:三尺寸(1280×720/1440×900/1920×1080)×每頁四態(loading/empty/error/正常)+R7 高頻日誌(以測試實例灌 log)+R5 第三範本實驗(臨時加一份 toml 驗證免改碼)。
- 迴歸:`go build ./...`、`go test ./...`、`wails build` 全綠;R9 逐項對照舊功能清單(備份/還原/排程 CRUD/警報/事件篩選/CF key 設定)。
