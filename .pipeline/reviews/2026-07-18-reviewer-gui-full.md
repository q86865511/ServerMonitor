# gui-redesign 全量審查(第一審 / reviewer)— 2026-07-18

範圍:`git diff af8f3bd..HEAD`(107 檔 / +10713 / -2580),對照 specs/gui-redesign/{requirements,design}.md。
審查重點:後端 .go(最高優先)→ stores 層 → 跨任務接縫 → 規格對照。波6(T10/T11/T12)5 條已知低發現不重列。
驗證:`go test -count=1 ./internal/core/ -run 'TestMetric|Recorder|TestMonitor|Icon'` → ok(非快取,1.375s)。

## 發現表(最嚴重在前)

| 編號 | 嚴重度 | 檔案:行號 | 問題 + 失敗情境 |
|---|---|---|---|
| 1 | 高 | frontend/src/lib/pages/server/ConsoleTab.svelte:31-40;frontend/src/lib/pages/server/OverviewTab.svelte:41-58 | 訂閱釋放讀到「新」uuid → 舊實例訂閱洩漏(詳下) |
| 2 | 中 | frontend/src/lib/stores/logs.ts:135-149;frontend/src/lib/stores/metrics.ts:169-183 | teardown await 後未重查是否被 re-acquire 取代 → 誤拆新訂閱(詳下) |
| 3 | 低 | frontend/src/lib/stores/metrics.ts:210,215 | `playersTrend`/`ramBytesTrend` 為死匯出(無元件使用);清潔度 |
| 4 | 低(存疑) | frontend/src/lib/stores/metrics.ts:116 | 即時 bucket 玩家數 `s.player_count ?? null`:串流若送 0 則存 0 非 null;但目前無圖表渲染 playersTrend,不可見 |
| 5 | 低 | internal/core/metricsrecorder.go:262,278 | InsertMetric/PruneMetrics 錯誤全吞且無任何 log/event → DB 持續寫失敗時指標靜默消失、零可觀測性 |

---

## 詳述

### 1【高】訂閱釋放讀到反應式 `uuid`,舊實例 log/stats 訂閱洩漏

ConsoleTab 的日誌 `$effect`(31-40)與 OverviewTab 的指標 `$effect`(41-58)在 cleanup 內
直接讀反應式 `uuid` 呼叫 `releaseLogs(uuid)` / `releaseMetrics(uuid)`,而非捕捉區域副本。
Svelte 5 的 effect cleanup 於「依賴變更觸發 re-run / 卸載」時執行,此時 `uuid` prop 已前進到新值。

**確定可觸發路徑**(非邊角):Topbar 全域搜尋在 shell 內、任何頁面(含詳細頁)皆可用。
於 `/servers/A/console` 用搜尋跳到 B → `navigate('/servers/B')` → route 變 uuid A→B、tab console→overview:
- page 仍為 `server-detail`,`ServerDetailPage` **不重建**(App.svelte:58-59 未 `{#key uuid}`),`uuid` prop A→B;
- ConsoleTab(A)因 tab 改變而卸載,cleanup 讀到的 `uuid` 已是 B → `releaseLogs(B)`(B 無 entry,no-op),
  而先前 `acquireLogs(A)` 的 refCount[A] **永不歸零**;
- 同理 OverviewTab(overview→overview,元件不卸載只 re-run):cleanup `releaseMetrics(B)`、body `acquireMetrics(B)`,A 洩漏。

**後果**:舊實例 A 的訂閱全鏈路洩漏 —— 前端 `EventsOn(logs:A)` 監聽器與 16ms flush 計時器續活、
後端 MonitorHub 保留 A 的訂閱者 channel、app.go 的轉推 goroutine 續跑、`EventsEmit("logs:A"/"stats:A")` 對
無人顯示的實例持續發送。每次「進入某伺服器詳細頁再跳到別台」累積一份,長 session 逐步劣化(正是本次
重構主打的監控 UX)。

**佐證非誤判**:同兩檔的其他 `$effect`(ConsoleTab:63-86 能力、169-185 排程;皆 `const id=uuid` + `if(id!==uuid)return`)
正確捕捉區域副本,證明作者已知 uuid 會在掛載中變動 —— 唯獨這兩個訂閱釋放 effect 漏捕捉。

**另一條(較高機率、非 100% 確定)**:自 `/servers/A/overview` 按返回到 `/servers`,`ServerDetailPage` 卸載,
cleanup 讀 `uuid` 時 route.params.uuid 已 undefined → `releaseMetrics('')` no-op、A 洩漏。此路徑取決於 Svelte 5
卸載期對 derived 的求值時機(本審無法實跑驗證),但修法相同即可一併消除。

**修法**:effect 開頭 `const id = uuid;`,body 與 cleanup 全用 `id`(對齊同檔既有 async guard 寫法)。

### 2【中】teardown 於 await 後未重查,誤拆被 re-acquire 的新訂閱

logs.ts:135-149 / metrics.ts:169-183 的 `teardown`:同步段設 `subscribed=false`、`EventsOff`,
接著 `await subscribePromise` 後**無條件** `UnsubscribeLogs/Stats` 並(logs)設 conn=idle。
若在 await 讓出期間發生 re-acquire(acquire 見 subscribed=false → 重新 subscribe:subscribed=true、
新 unlisten、新 subscribePromise),teardown 恢復後 await 的是**新** promise,隨即 Unsubscribe 掉剛建立的訂閱。
結果:前端自認已訂閱(subscribed=true、listener 在),後端已 UnsubscribeStats/Logs 不再串流 →
log/stats **靜默停流且不重連**。metrics 無 30s 延遲,快速 unmount→remount 同 uuid 時窗口更易命中;
logs 因 30s 延遲較難但 await 期間 re-acquire 仍可觸發。
誠實限定:需在 await 讓出點精確交錯,窗口窄;JS 單執行緒下多由 microtask(如 Svelte effect flush)命中。
**修法**:teardown 進入前記 generation token,await 後若 `subscribed` 已被重設或 token 變更則放棄拆除。

### 3【低】死匯出
`playersTrend`/`ramBytesTrend` 無任何元件引用(OverviewTab 僅用 cpuTrend/ramPercentTrend)。清潔度,無行為影響。

### 4【低,存疑】即時 bucket 玩家數 0 vs null
metrics.ts:116 `e.open.players = s.player_count ?? null`。models.ts:586 ResourceStats 帶 `player_count?: number`。
若 agent stats frame 送 Go 零值 0(而非省略欄位),`0 ?? null`=0 → 即時 bucket 記 players=0(非「不適用」)。
但目前無圖表渲染 playersTrend,不可見;且回填由後端快照(權威)覆寫。存疑點:取決於 agent 是否於 stats 填該欄。
若日後接上玩家數趨勢圖,建議改 `s.player_count && s.player_count > 0 ? ... : null` 或明確判 undefined。

### 5【低】指標寫入錯誤零可觀測性
metricsrecorder.go flush(262)`_ = r.store.InsertMetric(p)`、runPrune(278)`_ = r.store.PruneMetrics(...)` 全吞錯,
全檔無 log/event。DB 持續寫失敗時 R13 指標靜默消失且無告警。R13 未強制外露,故低;建議至少計數或降頻記一次事件。

---

## 未發現問題的面向(明確結論)

- **後端 .go(最高優先)**:未發現正確性 bug。
  - metricsrecorder:15s bucket 語意符 R13(cpu=均值 247、memory=末樣本且 ==0→NULL 249-255、玩家數自快照 257-260);
    生命週期硬保證正確(closedFlag 於 Wait 前短路 flush 241 / runPrune 275;lock 序 h.mu→mi.mu;wg.Add 在 map 內)。
  - monitor.StatusFetcher:僅 `st.Running && StartedAt!=nil` 才回(fetchStartedAt 426),符 R11;NewMonitorHub 簽章更新完整(bootstrap.go:395-402 注入齊全)。
  - IconPath 路徑拘束穩健:`filepath.Rel` 擋 sibling-prefix + `EvalSymlinks` 雙邊界判定擋 symlink 逃逸 + `IsRegular` 拒目錄/裝置(template_engine.go:272-304);main.go:29-34 handler `app.rt==nil` nil-check 在請求時判定。
  - schema v3 遷移單交易、name 透傳(store_ports/records)、app.go DTO nullable 正確、PortsByInstance 免 N+1(api.go:34)。
  - lifecycle 收束序正確:metrics.Close 先於 monitor.Close(93-99);startMonitoring 先 StartMonitoring 再 Watch。
- **跨任務接縫**:元件無自行 EventsOn(僅 stores 與 wizard provision 例外,CreateWizardModal onDestroy+finally 雙重解除);
  SettingsTab 機密只顯宣告 + `••••••••`,值不入 DTO 也不入 DOM(符 R6)。
- **stores/instances.ts**:refreshSeq 防競態正確(30 檢查於雙 await 後)。
- **規格對照**:R7 主控台 3000 行上限(logs.ts:17)+ 等級解析(35-41)、R14 icon、R15 nil→「不適用/—」、R16 loading/empty/error+防重複(busyAction/sending/submitting)均落實。

## 結論

**可進裁決,但有 1 條建議修的阻擋級項(發現 1)**。發現 1 為確定可觸發(Topbar 搜尋跨伺服器)的訂閱洩漏,
屬本次重構核心監控鏈路,且修法極小(捕捉區域 uuid);建議此條先修再併。發現 2 為窄窗競態、建議一併補強。
其餘為低/清潔度。後端五項擴充(T1-T5)未發現正確性問題,測試綠(非快取)。
