# game-server-manager — 需求規格(requirements.md)

> 建立日期:2026-07-13｜狀態:已核可(2026-07-13),經 Codex 兩輪二審深修(rev.3)
> 需求主體使用 EARS 句式(速查表見 C:\Users\q86865511\.claude\skills\spec\SKILL.md)。
> rev.3 已將兩輪二審的高/中嚴重發現落實至可測驗收與 design 結構;實作前仍需完成「待查證」的三個 spike。

## 概述

為個人(未來可擴及朋友)提供一套可擴充的遊戲專用伺服器管理與監控桌面工具。透過「遊戲範本」抽象,使新增「沿用既有 runtime/指令/健康 adapter」的遊戲主要是加一份範本而非改核心;首版以 Docker 為執行後端,支援 Minecraft 與 Palworld 的一鍵建立、監控、指令、自動復原、備份與告警。

## 範圍(含明確不做)

**包含(首版):** 見概述;細節見各 R#。核心能力:範本擴充、一鍵建立、生命週期(含狀態機/對帳)、監控+log、指令(Minecraft RCON / Palworld REST)、自動復原、排程備份/還原、Discord 告警、模組(itzg 原生)、持久化(含毀損隔離)、結構化事件記錄。

**明確不做(首版,防蔓延):**
- 多節點遠端部署與跨機安全層(只做「有版本、有認證」的 loopback 契約;不做 `cmd/agent`、跨機 TLS、節點註冊)。
- 原生 SteamCMD 後端完整實作(只留抽象介面 + mock)。
- 自建 Modrinth/CurseForge 下載器(用 itzg 原生 + 手動受限檔案)。
- Palworld RCON(首版 legacy,不列驗收,待官方查證)。
- 實例設定的**更新(update/PUT)**:首版僅支援建立、啟停、移除、重建;修改參數=移除後重建(明確排除線上 update)。
- 雲端 VM 隨需開機、Web/行動版、多使用者權限、Windows-only 容器/反作弊。

## 關鍵常數預設(可設定;作為驗收基準)

| 常數 | 預設 | 用途 |
|---|---|---|
| 優雅停機寬限期 | 30 秒 | R3 停止:送停止指令後等待,逾時強停 |
| 啟動就緒逾時 | 60 秒 | R3 啟動:輪詢至 Running,逾時判失敗 |
| 崩潰迴圈上限 | 5 分鐘內 3 次 | R8 超過則停止自動重試 + 告警 |
| 健康探針:啟動寬限 / 週期 / 連續失敗門檻 | 60 秒 / 15 秒 / 3 次 | R8 判「running 但卡死」 |
| 告警 cooldown / 資源持續窗口 / 遲滯 | 5 分鐘 / 60 秒 / 門檻−10% 解除 | R10 去重與門檻 |
| 告警重試 | 3 次,退避 base 2s cap 60s,遵守 429 `Retry-After`,5xx 重試、4xx 不重試 | R10 |
| 備份保留 N | 7 份 | R9 保留策略 |

## 需求

### R1 遊戲範本抽象與擴充
常態:系統應以版本化「遊戲範本」定義每款遊戲的安裝來源、變體、埠(含 host binding)、參數、資料目錄、指令協定與動作、生命週期 hooks、健康探針,使新增「沿用既有 adapter」的遊戲僅需新增一份範本。

驗收條件:
- 範本 schema 有 `schema_version`;載入內建 Minecraft、Palworld 範本後,GUI 列出可建立的遊戲類型與變體。
- 以第三款測試範本(沿用既有 Docker runtime 與現有 RCON/health adapter)、不改核心,可完成「載入 → 建立 → 啟動 → 查詢狀態 → 停止」全鏈路。
- 範本缺必填欄位、`schema_version` 不支援、template ID 重複、或引用不存在的 adapter(runtime/protocol/health kind)時,拒絕載入該範本並寫入 `TEMPLATE_LOAD_FAILED` 事件(含原因與欄位),其餘範本不受影響。

### R2 一鍵建立伺服器實例(原子 + 埠預留)
事件驅動:當使用者選定範本、變體、填妥必要參數並確認建立時,系統應以原子方式建立一個 `Created` 狀態實例。

驗收條件:
- 成功後實例以穩定 UUID 登錄 DB(狀態 `Created`),記錄 runtime(容器)ID 與 Docker 標籤(`gsm.uuid`、`gsm.managed-by`、`gsm.node`、`gsm.schema`)。
- 必填參數缺漏於建立前阻擋並指出缺項;Minecraft 未接受 EULA 不得建立(不暗中預設)。
- **埠衝突**:以 `(bind_ip, protocol, host_port)` 為鍵,於 DB 以唯一約束**預留**;判定重疊時 `0.0.0.0` 與具體 IP 視為衝突(wildcard 覆蓋規則),IPv4/IPv6 各自判定;預留後再經 Docker/OS 二次檢查。兩個併發建立不得同時通過。
- **原子性**:建立分階段(預留埠 → 拉映像 → 建容器 → 寫 DB 完成),任一階段失敗回滾:釋放埠預留、移除已建容器、不留 DB 完成紀錄;以建立 journal 支援「建容器後、寫 DB 前崩潰」的重開清理(對帳時 R13 處理孤兒)。

### R3 伺服器生命週期管理(序列化 + 冪等)
常態:系統應提供啟動、停止、重建操作;同一實例操作序列化。

驗收條件:
- 啟動:輪詢至容器 `Running`;預設 60 秒逾時未達則標記 `Error` 並報明確錯誤。
- 停止(計畫性):標記 planned-stop token(含 operation generation)→ 送範本 `hooks.stop` 指令(若有)→ 寬限期 30 秒 → 逾時強停;狀態轉 `Stopped`。
- 重建(restart)= **core 層編排**的 Stop→Start,持 per-instance lock,斷言仍是同一 runtime ID 回到 `Running`;**agent 不提供獨立 restart 端點**(避免繞過 lock/planned-stop)。
- 對 `Running` 實例重複啟動為冪等:不重建容器,回報「已在執行」。

### R4 執行後端抽象(RuntimeBackend + 事件來源)
常態:系統應透過抽象 RuntimeBackend 介面操作容器生命週期、資料封存與**執行事件來源**,首版 Docker 實作,預留原生 SteamCMD。

驗收條件:
- 核心對容器的所有操作(建立/啟停/查狀態/列舉/查詢/移除/log/程序 exec/資源指標/封存/還原/**事件監看**)皆經介面,不散落直呼 Docker SDK。
- 介面涵蓋:Create、Start、Stop、Status、List、Inspect、Remove、Logs、ExecProcess、Stats、Archive、Restore(回傳新 runtime ID)、**Events(串流,含 reconnect/cursor/漏事件後對帳語意)**。
- 遊戲內指令**不經** ExecProcess,經獨立 `GameCommandAdapter`(見 R7)。
- 以 MockBackend 替換 Docker,核心生命週期與**崩潰事件路徑**於「core → HTTP agent → mock」整合測試中跑通。

### R5 控制核心與節點代理分離(版本化 + 認證)
常態:系統應以「有版本、有認證」的 API 契約分離核心與代理;首版單機走 loopback,核心對代理經該 API。

驗收條件:
- 版本化契約(OpenAPI 或等價)涵蓋:實例 建立/列舉/查詢/移除、啟停(無 restart 端點)、狀態、指標串流、事件串流、log 串流、遊戲指令、備份清單/建立/還原、health;各端點定義 request/response、錯誤碼、冪等鍵(含 TTL 與重播規則);`{id}` 明訂為 instance UUID。首版不支援 update(明確排除)。
- 代理僅綁 loopback + 每次啟動的記憶體 bearer token;未帶正確 token 的 HTTP/WS 一律拒絕;WS 檢查 Origin。
- 呼叫路徑可追蹤;代理無回應時核心標該節點離線並於 GUI 顯示,不整體當掉。

### R6 資源與狀態監控 + 即時 log
持續狀態:於執行期間持續採集 CPU%、記憶體、**資料磁碟用量**、**線上狀態**並即時顯示;開啟主控台時串流即時 log。

驗收條件:
- CPU%(定義正規化基準:對容器 CPU 使用率 / 可用核心)、記憶體用量於 GUI ≤5 秒更新。
- 資料磁碟用量以 `data_dirs` 對應宿主路徑或 helper 採集已用量(明確與 Docker 累積 block I/O 區分);無法採集顯示「不適用」。
- 線上狀態依範本探針(rest/rcon/tcp)判定;不支援顯示「不適用」。
- log 串流有背壓(有界緩衝、批次推送、丟棄提示),高流量下 GUI 不卡死(定義「不卡死」= 前端事件迴圈不因 log 阻塞逾 200ms)。
- 玩家數:範本支援查詢者顯示;否則「不適用」。

### R7 指令主控台(協定/動作 tagged union + 生命週期 hooks)
事件驅動:當使用者於主控台送出指令時,系統應依範本 `command_protocols` 送達並回顯。

驗收條件:
- 範本以 `command_protocols[]` 表達;每項有 `protocol_id` 與 `kind`:`rcon`(raw console,自由字串)或 `rest`(具名 `actions[]`,各定義 `action_id`、method、path、typed input/output、憑證參照、埠參照)。
- 生命週期 hooks(`hooks.stop`、`hooks.announce`、`hooks.quiesce`/`hooks.resume`)以 `(protocol_id, action_id 或 raw command)` 映射,使核心不需硬編遊戲語意、RuntimeBackend 不需理解遊戲。
- 對 Minecraft 送 RCON `list` 回顯;對 Palworld 以 REST 具名動作(查玩家/公告)可執行並回顯;**Palworld RCON 首版列 legacy,不列驗收**。
- 未啟用協定的實例輸入停用並提示;連線失敗明確報錯,GUI 不卡死。

### R8 自動重啟 / 崩潰復原(完整狀態機 + 探針)
事件驅動:當實例「非計畫性」結束或健康探針判定卡死時,系統應依設定自動重啟並記錄;常態:支援排程重啟。

驗收條件:
- 維護每實例狀態機(狀態集與轉移表見 design);容器 `die` 事件需比對 planned-stop token(含 operation generation、TTL、consume、失敗清理):計畫停止/排程重啟/還原前停機**不**觸發自動重啟。
- 「running 但卡死」由健康探針(啟動寬限 60s、週期 15s、連續失敗 3 次)判定並納入復原;單次瞬斷不誤判。
- 崩潰後於重試間隔重啟;5 分鐘內 3 次達上限→停止自動重試 + `RESTART_GIVEUP` 告警,不無限重啟。
- 排程重啟以 UTC 記錄(處理時區/DST);範本支援公告時,重啟前經 `hooks.announce` 發公告;同實例操作序列化,不與備份/還原互撞。
- GUI 於排程重啟中途關閉後重開:依 desired 狀態收斂(定義:desired=Running 則對帳後補啟動)。

### R9 排程備份 + 還原(一致 + 原子 + 所有權)
條件:若啟用排程備份,系統應於設定時點經 RuntimeBackend Archive 打包資料並套用保留策略;事件驅動:選擇還原時,以驗證後原子方式還原並恢復原 desired 狀態。

驗收條件:
- **首版策略**:資料以**同檔案系統 bind mount**;備份採**停機一致快照**(先 planned-stop → Archive → 視原 desired 決定是否重啟)。named volume 原子交換列為待查證 spike。
- 備份由 **agent 擁有儲存根**;對外以 opaque `BackupID` 表示;core DB 存 `BackupID` 與中繼(時間戳 UTC、實例 UUID、遊戲、checksum),不存原始路徑;GUI 可列某實例備份清單。
- 保留 N(預設 7)超過自動刪最舊;刪除失敗記 `BACKUP_FAILED` 不中斷主流程。
- 還原:planned-stop → staging 驗證 checksum → 原子切換 → 依原 desired 狀態恢復(原停止則保持停止),`Restore` 回傳新 runtime ID;還原後資料與備份一致。
- 中斷邊界:還原/備份進行中,GUI 關閉應阻擋關閉或提供 crash-safe rollback(以 operation journal),不留半套狀態。

### R10 告警通知(參數化 + 窗口 + 去重)
事件驅動:當崩潰、連續重啟失敗、備份失敗、或資源持續超門檻時,經已設定管道發送。

驗收條件:
- 四類事件各有固定觸發輸入與可斷言 payload(event code、實例名、時間 UTC、細節);payload schema 明確。
- 資源門檻:持續窗口 60 秒 + 遲滯(門檻−10% 解除)+ dedup key `(instance_uuid, metric)` 於 cooldown 5 分鐘內去重,不每 5 秒轟炸;以 fake clock 可驗窗口/解除/cooldown。
- 未設定任何管道時僅寫事件記錄且不報錯。
- 送出失敗依常數表重試(3 次、退避 base 2s cap 60s、遵守 429 `Retry-After`、5xx 重試、4xx 不重試),失敗記 `ALERT_FAILED` 不阻塞主流程。

### R11 模組 / 模組包支援(itzg 為唯一安裝擁有者)
常態:系統應允許以變體選型別(Vanilla/Paper/Forge/Fabric),經 itzg 映像原生機制安裝模組包;工具不與 itzg 爭奪 `/data` 安裝擁有權。

驗收條件:
- 範本列 loader/遊戲版本支援矩陣;以 Paper 變體建立可啟動,放入外掛目錄的外掛啟動後生效。
- 以 itzg 原生 `MODRINTH`/`AUTO_CURSEFORGE`(CurseForge 需 `CF_API_KEY`)指定模組包建立,啟動後生效;工具不另解壓 `/data`。
- 手動受限檔案:格式統一為 **`mrpack` / `curseforge-zip`**(命名一致);定義手動檔如何交給 itzg(掛載路徑 + 對應 env),匯入後啟動生效。
- 不相容變體/loader/版本組合於建立前依矩陣給可理解錯誤;Minecraft/Palworld 映像鎖 tag 或 digest。

### R12 設定與資料持久化(毀損隔離 + SecretRef)
常態:系統應以 UTF-8 儲存設定與中繼資料;重啟後清單/範本/排程/告警設定皆保留。

驗收條件:
- 建立數實例並設定排程後重啟,清單與設定完整重現;DB 具 schema 版本與遷移。
- SQLite 毀損:進 quarantine(改名保留原檔)+ 降級復原,**不以空庫覆寫**;寫 `DB_QUARANTINE` 事件到 **fallback 診斷媒介**(當 DB 本身不可用時寫入 fallback 日誌檔);一般設定檔毀損以預設值啟動並記錄。
- 敏感值以 **`SecretRef` 型別**建模:輸入即寫 OS 金鑰庫、DB 只存參照、DTO/event/log 統一 redaction(不可回顯);`RCON_PASSWORD`、Palworld AdminPassword、`CF_API_KEY` 皆為 SecretRef。
- 遊戲 runtime 本身若需把密碼寫入其設定檔/容器環境,列明確例外並定義:其檔案 ACL、備份時的加密/排除/警告政策、secret 刪除與輪替。

### R13 啟動對帳與單一實例
常態:系統啟動時應以 Docker 標籤/runtime ID 對帳 DB 與實際容器,並確保同時只有一個應用程序實例管理同一組容器。

驗收條件:
- 啟動時 List/Inspect:**區分「Docker daemon 不可用」與「容器清單為空」**(前者標節點離線並提示,不誤刪);以 `gsm.uuid`/`gsm.managed-by`/`gsm.node` 標籤關聯。
- 對帳:DB 有而容器不存在→標記;容器有 `gsm.uuid` 而 DB 無→標孤兒並提示;狀態不一致→以實際更新 observed 並記 `RECONCILE_*` 事件。
- 單一實例:以明訂資料根的 lockfile/named mutex(定義作用域),偵測第二實例則後啟者拒絕接管並提示。
- GUI 關閉期間的變化,重開時經對帳反映並重建事件訂閱;離線期間至少產生 reconciliation summary(完整歷史 backfill 為 best-effort,不保證)。

### R14 結構化事件記錄
常態:系統應將關鍵事件寫入結構化、可查詢的事件記錄。

驗收條件:
- 統一 event envelope:`code`、`ts_utc`、`severity`、`instance_uuid`(nullable)、`node`(nullable)、`template_id`(nullable)、`details_json`;非僅寫 stdout。
- 定義必備 event code 目錄(見 design);其他需求(R1/R2/R8/R9/R10/R12/R13)的「已記錄」驗收以對應 code + 欄位判定。
- GUI/介面可依實例/類型/時間查詢(filter 驗收);有保留上限與輪替(避免無限增長);DB 不可用時有 fallback 診斷媒介。

## 非功能需求

- **相容性**:Windows 11;Docker 經 WSL2/Docker Desktop;桌面應用 Windows 原生可執行檔;最低 Docker Engine API 版本見 design(建議 ≥1.44)。
- **相依版本鎖定**:Go、Wails v2、Docker client 模組、`modernc.org/sqlite`、`go-keyring` 之版本於 design 給具體下限,並於 T1 鎖定於 `go.mod`。
- **效能(可量測)**:於指定量測環境(見 design 基準:某參考硬體 + 取樣窗 60 秒)下,閒置時工具 CPU 與 working set 有上限基準;每實例監控開銷與高流量 log 基準以 benchmark 任務量測且遠低於單一遊戲伺服器負載。
- **安全**:代理 loopback + 每次啟動 bearer token + WS Origin 檢查;所有 SecretRef 存 OS 金鑰庫,不明文;DTO/event/log redaction。
- **可靠**:核心崩潰不停止遊戲容器(生命週期解耦);操作以 journal/lock 具 crash-safe 邊界。
- **生命週期**:自動化僅 GUI 開啟時運作;關閉不停容器(Docker 維持),重開經 R13 對帳恢復。
- **擴充**:新增「沿用既有 adapter」遊戲以加範本為主;新增執行後端以實作 RuntimeBackend 為主。

## 已裁決(原開放問題)

- **OQ1(R10)**:告警首版只做 Discord webhook;Email/SMTP 次期。
- **OQ2(R11)**:模組包用 itzg 原生 `MODRINTH`/`AUTO_CURSEFORGE` + 手動受限檔案;自建下載器延後(rev.2)。
- **OQ3(R7)**:Palworld REST 具名動作為主;RCON legacy、待查證(rev.2)。

## 待查證(實作前以 spike 完成,不作為既定事實)

- **Palworld spike(T7 前置 gate)**:官方 REST 端點/認證與 RCON 現況,鎖伺服器版本/映像 tag 或 digest。
- **備份 spike**:Windows/Docker Desktop 下 bind mount 停機快照的一致性與原子切換;named volume 策略是否需要。
- **模組 spike(T13 前置)**:itzg 各 loader/遊戲版本支援矩陣、`AUTO_CURSEFORGE` 實際 env 行為、CurseForge API 條款/金鑰/attribution。
