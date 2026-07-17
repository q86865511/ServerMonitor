# game-server-manager — 技術設計(design.md)

> 建立日期:2026-07-13｜狀態:已核可(2026-07-13),經 Codex 兩輪二審深修(rev.3)
> 對照 requirements.md;每條 R# 出現在需求對應表。

## 架構概述

四層,收斂為單一 **Wails v2 應用(Go 後端 + web 前端)**。Go 後端內含 **管理核心**(實例登錄、狀態機、對帳、排程器、監控聚合、告警、範本引擎、事件記錄)與 **本機節點代理**(代理 API 綁 127.0.0.1 + 每次啟動 bearer token);核心以 `NodeClient`(HTTP)呼叫代理。代理以 `RuntimeBackend`(Docker 官方 SDK)操作容器/封存/**事件監看**,以 `GameCommandAdapter` 執行遊戲指令。前端經 Wails bindings 呼叫。範本為版本化宣告式檔案。

**生命週期(僅 GUI 開啟時運作):** 自動化僅在應用開啟時運作;關閉暫停自動化但容器由 Docker 維持,重開經 **R13 對帳** 恢復。遠端節點為未來 `cmd/agent`,首版只做版本化 loopback 契約。

**後續擴充(2026-07-16):** 免 Docker 的本機行程後端(NativeBackend + dispatchBackend 分派、供應子系統、Job Objects)已由 `specs/native-backend/design.md` 規格化;本檔描述的 Docker 路徑行為不變,agent 端由該設計以組合方式擴充。

**建置形態(Go module + Wails v2,單一 repo):** `internal/protocol`(版本化 schema、DTO、事件型別、SecretRef)、`internal/agent`(RuntimeBackend、GameCommandAdapter、備份、事件監看、HTTP/WS server+認證)、`internal/core`(登錄、狀態機、對帳、範本引擎、排程、監控、告警、EventLog、NodeClient)、`app`(Wails 根)+`frontend/`、(未來)`cmd/agent`。

## 相依版本鎖定(NFR;T1 於 go.mod 鎖定實際 patch)

> 以下為**建議下限**;T1 對照上游當下穩定版與彼此相容性鎖定確切版本(尤其 modernc 對 Go 版本的要求)。

- **Go**:1.23+(以 modernc 選定版之 go.mod 要求為準,必要時上調)。
- **Wails**:v2.x stable(**非 v3 alpha**)。
- **Docker client**:`github.com/docker/docker`(Moby)v27.x;**最低 Docker Engine API 1.44**。
- **SQLite**:`modernc.org/sqlite` v1.34.x(純 Go,免 CGO)。
- **金鑰庫**:`github.com/zalando/go-keyring` v0.2.x(Windows Credential Manager backend)。

## 效能基準(NFR 可量測)

> **量測環境**:Intel Core i5-13600K(20 執行緒)/ 32GB RAM / Windows 11 / Docker Desktop 29.4.1(WSL2 後端)。
> **量測方法**:工具自身程序的 Windows 計數器——`GetProcessTimes`(累計 kernel+user CPU 時間)、`GetProcessMemoryInfo`(工作集),取樣窗 60 秒;log 吞吐於核心 fanout 層白箱量測。量測程式:`internal/app/e2e_docker_test.go`(`TestE2E_PerfBaseline`)、`internal/core/monitor_perf_test.go`(`TestPerf_LogThroughput`),以 `-tags docker` 執行。**量測日期:2026-07-15**。
> 註:headless(無 GUI)Runtime 量測;Wails GUI 視窗(WebView2)另計的工作集屬桌面殼層,**未量測/待人工**(以工作管理員實測補記)。
> 註:下表為**單次觀測(n=1)快照**,非多次取樣的統計值;數值供量級參考,絕對值會隨機器/負載波動。

| 指標 | 量測值 | 說明 |
|---|---|---|
| 閒置(0 實例)CPU | 全核 0.000% / 單核當量 0.000% | Bootstrap+Start、無實例,60s 窗平均低於量測解析度(僅背景事件 WS 連線 + 排程/存活迴圈的閒置週期) |
| 閒置工作集 | **17.8 MB** | headless Runtime 程序工作集 |
| 每執行中實例監控開銷 CPU | Δ 全核 +0.001%(單核當量 +0.026%) | 1 個執行中實例 + stats/log/poll 監控 + log 訂閱 vs 閒置增量 |
| 每執行中實例監控開銷 工作集 | Δ **+5.4 MB**(17.8→23.1 MB) | 同上 |
| log 常態流量(NFR 示例 1000 行/秒 × 10s) | 注入 10000、**丟棄 0**(無損轉推) | 健康消費者下核心 fanout 零丟棄 |
| log fanout 吞吐上限 | **~7.5×10⁶ 行/秒**(20 萬行 / 26.6ms) | 消費者跟不上時丟最舊留最新 + 注入「已丟棄 N 行」提示,**絕不阻塞**(既有背壓單元測試另證 500 行 <200ms) |

**NFR 結論(工具開銷遠低於單一遊戲伺服器)**:同機同批量測到的單一 Minecraft Paper 伺服器容器工作集約 **2.9 GB**(啟動期 CPU 數十%);相對之下,本工具閒置工作集 ~18 MB、每監控一個執行中實例僅 +~5 MB 與 <0.03%(單核當量)CPU,log 高流量下核心 fanout 吞吐達數百萬行/秒且常態零丟棄。工具總開銷相對遊戲伺服器自身數 GB/多核負載可忽略,**符合 NFR「遠低於單一遊戲伺服器」**。

## 需求對應表

| 需求 | 設計元素 | 說明 |
|---|---|---|
| R1 | `core` 範本引擎 + `protocol.GameTemplate`(`schema_version`)+ `templates/*.toml` + adapter 註冊表 | 版本化宣告式;缺欄位/版本/ID/adapter 不存在→拒載記事件 |
| R2 | `core.InstanceService.Create`(分階段+journal+回滾)+ `port_reservations`(唯一約束)+ Docker 標籤 | 埠鍵 (bind_ip,protocol,host_port)+wildcard 規則;併發不同時過;失敗回滾 |
| R3 | `RuntimeBackend.{Start,Stop,Status}` + core Restart 編排 + per-instance lock + planned-stop token | 停機 hooks.stop+30s 寬限;啟動 60s 逾時;restart=Stop+Start 同容器;agent 無 /restart |
| R4 | `agent.RuntimeBackend`(+List/Inspect/Remove/Archive/Restore/**Events**)+ `MockBackend` | 事件納入介面;Restore 回傳新 runtime ID;core→agent→mock 崩潰路徑可測 |
| R5 | `core.NodeClient`(HTTP+bearer)+ agent server(loopback+token+Origin)+ OpenAPI 契約 | {id}=UUID;無 restart/無 update;冪等鍵+TTL;錯誤碼表;未授權拒絕 |
| R6 | agent stats+資料磁碟採集+探針 + `MetricsHub`(WS,背壓) | CPU% 正規化;資料磁碟走宿主路徑;log 有界緩衝<200ms |
| R7 | 範本 `command_protocols[]`(tagged union)+`hooks` + `GameCommandAdapter`(`RconAdapter`/`PalworldRestAdapter`) | rcon=raw、rest=具名 actions;hooks 映射;Palworld RCON legacy |
| R8 | `core` 狀態機(轉移表)+ planned-stop token(generation/TTL)+ `RestartPolicy` + 探針 + `Scheduler` | 區分死因;卡死經探針(60/15/3);超上限停+告警;排程 UTC;GUI 關閉後收斂 |
| R9 | `RuntimeBackend.{Archive,Restore}` + `BackupID`(agent 擁有根)+ `RetentionPolicy` + journal | 首版停機快照+bind mount;checksum;原子還原回傳新 runtime ID;中斷 crash-safe |
| R10 | `core.AlertDispatcher` + `DiscordWebhookChannel` + 窗口/hysteresis/cooldown/dedup + 重試表 | 四類事件固定 payload;fake clock 可測;429/5xx 策略 |
| R11 | 範本 `variants`/`mods(owner=image-native)` + itzg env + 手動格式 + 支援矩陣 | itzg 唯一擁有者;工具不解壓 /data;映像鎖 tag/digest |
| R12 | `core` SQLite+遷移+integrity+quarantine + `protocol.SecretRef` + fallback 診斷 | 毀損不覆寫;SecretRef 金鑰庫+redaction;runtime 明文例外政策 |
| R13 | `core.Reconciler`(daemon-unavailable≠empty)+ 標籤 + single-instance lock | 孤兒/不一致;離線 summary;重建訂閱 |
| R14 | `core.EventLog`(envelope+code 目錄+retention)+ 查詢 + fallback | envelope 欄位;必備 codes;filter/rotation 可測 |

## 介面與資料模型

**遊戲範本 schema(合法 TOML;含 host binding、tagged-union 協定、hooks、SecretRef 參照):**
```toml
schema_version = 1
id = "minecraft"
name = "Minecraft: Java Edition"
runtime = "docker"
data_dirs = ["/data"]                 # R9 備份範圍(頂層)

[docker]
image = "itzg/minecraft-server"
image_digest = "sha256:..."           # R11 鎖 digest(或 tag)

[[variants]]
id = "paper"
[variants.env]
TYPE = "PAPER"

[[ports]]                              # R2 衝突鍵 (bind_ip, protocol, host_port)
name = "game"
container = 25565
host_port = 25565                      # 可為 0=動態
bind_ip = "0.0.0.0"                    # wildcard 與具體 IP 視為重疊
protocol = "tcp"
required = true

[[ports]]
name = "rcon"
container = 25575
host_port = 25575
bind_ip = "127.0.0.1"
protocol = "tcp"

[[params]]
key = "MEMORY"
label = "記憶體上限"
type = "string"
default = "2G"

[[params]]
key = "EULA"
label = "接受 Minecraft EULA"
type = "bool"
required = true                        # R2 未接受不得建立

[[secrets]]                            # R12 SecretRef:輸入即入金鑰庫,不進 params_json
key = "RCON_PASSWORD"
label = "RCON 密碼"

[[command_protocols]]                  # R7 tagged union
protocol_id = "mc-rcon"
kind = "rcon"                          # raw console
host_port_ref = "rcon"
password_ref = "RCON_PASSWORD"         # 指向 [[secrets]]

[hooks]                                # R3/R8/R9 生命週期映射
stop = { protocol_id = "mc-rcon", command = "stop" }
announce = { protocol_id = "mc-rcon", command = "say {msg}" }

[health]                               # R8 探針
kind = "tcp"                           # tcp | rcon | rest | docker
port_ref = "game"

[players_query]                        # R6
kind = "rcon"
command = "list"

[mods]                                 # R11 itzg 唯一擁有者
owner = "image-native"
plugin_dir = "/data/plugins"
modpack_env = ["MODRINTH", "AUTO_CURSEFORGE"]
manual_formats = ["mrpack", "curseforge-zip"]
manual_mount = "/modpacks"             # 手動檔掛載處,交由 itzg 對應 env 取用
```
Palworld 範本:`[[command_protocols]]` 一組 `kind="rest"`(`actions[]`:players GET /v1/api/players、announce POST /v1/api/announce、shutdown…,`auth="basic"`,`password_ref="ADMIN_PASSWORD"`),另一組 `kind="rcon"; legacy=true`(首版不啟用)。

**`RuntimeBackend` / `GameCommandAdapter`(R4/R7,`internal/agent`):**
```go
type RuntimeBackend interface {
  Create(ctx, spec InstanceSpec) (RuntimeID, error)
  Start(ctx, id RuntimeID) error
  Stop(ctx, id RuntimeID, opts StopOpts) error          // 優雅停機(hooks.stop + grace)
  Status(ctx, id RuntimeID) (RuntimeStatus, error)
  List(ctx) ([]RuntimeRef, error)                        // R13
  Inspect(ctx, id RuntimeID) (RuntimeInfo, error)
  Remove(ctx, id RuntimeID, opts RemoveOpts) error       // 預設留 data/backups,purge 才刪
  Logs(ctx, id RuntimeID, opts LogOpts) (LogStream, error)
  ExecProcess(ctx, id RuntimeID, cmd ExecCmd) (ExecResult, error)   // 容器內程序,非遊戲指令
  Stats(ctx, id RuntimeID) (ResourceStats, error)
  Archive(ctx, id RuntimeID) (BackupID, error)           // R9 一致快照(呼叫端先 planned-stop)
  Restore(ctx, id RuntimeID, b BackupID) (RuntimeID, error) // 回傳新 runtime ID
  Events(ctx, since Cursor) (EventStream, error)          // R4/R8 die/health;reconnect+cursor+漏事件對帳
}
// Restart = core 層 Stop+Start 編排(持 lock),非介面方法,agent 亦不開 /restart。
// 遊戲指令與 RuntimeBackend 分離:
type GameCommandAdapter interface {
  Send(ctx, target CommandTarget, cmd GameCommand) (CommandResult, error)
}
// 實作:RconAdapter、PalworldRestAdapter。
type SecretRef struct{ Key string } // R12:值存金鑰庫;String()/MarshalJSON 一律 redact,不可回顯
```
實作:`DockerBackend`(`github.com/docker/docker/client`)、`MockBackend`(含 Events)、未來 `NativeBackend`。

**節點代理 API 契約(R5,版本化 OpenAPI + bearer;`{id}`=instance UUID):**
所有 HTTP/WS 需 `Authorization: Bearer <每次啟動 token>`;WS 檢查 Origin。端點:
- `GET /instances`、`POST /instances`、`GET /instances/{id}`、`DELETE /instances/{id}`(**無 PUT/PATCH**,首版不支援 update;**無 /restart**,restart 由 core 編排)
- `POST /instances/{id}/start`、`POST /instances/{id}/stop`
- `GET /instances/{id}/status`、`WS /instances/{id}/stats`、`WS /instances/{id}/logs`
- `WS /events`(RuntimeBackend.Events 轉發,供 R8/R13)
- `POST /instances/{id}/command`(經 GameCommandAdapter)
- `GET /instances/{id}/backups`、`POST /instances/{id}/backup`、`POST /instances/{id}/restore`
- `GET /health`

寫入端點接受 `Idempotency-Key`(TTL 10 分鐘,重播回原結果);錯誤以統一碼表(`ERR_PORT_CONFLICT`、`ERR_NOT_FOUND`、`ERR_LOCKED`、`ERR_UNAUTHORized` 等)。

**狀態機(R8;desired ∈ {Running, Stopped}):**
狀態集:`Created`、`Starting`、`Running`、`Stopping`、`Stopped`、`BackingUp`、`Restoring`、`Crashed`、`Error`、`Offline`(節點層)。轉移(摘):

| 從 | 事件 | 到 |
|---|---|---|
| Created | Start | Starting |
| Starting | 就緒(容器 running 且就緒探針過;無探針則容器 running) / 就緒逾時 | Running / Error |
| Running | Stop(planned) | Stopping → Stopped |
| Running | die(無 planned token)或探針連續失敗 | Crashed |
| Crashed | 重試未達上限 / 達上限 | Starting / Error(RESTART_GIVEUP) |
| Running/Stopped | backup | BackingUp → 原狀態 |
| any(非忙) | restore | Restoring → 依原 desired(Stopped/Starting) |
| any | 節點不可達 | Offline → 對帳後收斂 |

每操作帶 operation generation;planned-stop token 有 TTL(如 2×寬限期)、被對應 die 事件 consume、逾時或啟動失敗即清除,避免 stale token 遮蔽下次真崩潰。

**事件封套與碼表(R14):**
envelope:`{code, ts_utc, severity, instance_uuid?, node?, template_id?, details_json}`。必備 codes:`TEMPLATE_LOAD_FAILED`、`INSTANCE_CREATED`、`INSTANCE_CREATE_FAILED`、`INSTANCE_STARTED`、`INSTANCE_STOPPED`、`INSTANCE_CRASHED`、`INSTANCE_RESTARTED`、`RESTART_GIVEUP`、`HEALTH_PROBE_FAILED`、`BACKUP_STARTED/COMPLETED/FAILED`、`RESTORE_STARTED/COMPLETED/FAILED`、`ALERT_SENT/FAILED`、`RECONCILE_ORPHAN`、`RECONCILE_MISMATCH`、`NODE_OFFLINE`、`DB_QUARANTINE`。當 SQLite 不可用時,事件寫入 **fallback 診斷檔**(NDJSON)。

**持久化(R12):** SQLite(`modernc.org/sqlite`)+ 遷移(schema 版本)+ `PRAGMA integrity_check`;毀損 quarantine(改名保留)不覆寫。表:`instances`(uuid, template_id, variant, params_json, node, runtime_id, desired_state, observed_state, op_generation)、`port_reservations`(bind_ip, protocol, host_port UNIQUE, instance_uuid)、`schedules`、`settings`、`backups`(backup_id, instance_uuid, ts_utc, checksum)、`events`(envelope 欄位)。SecretRef 於 DB 僅存 key 名,值在 `go-keyring`。

## 關鍵流程

1. **建立(R2)**:驗證必填/EULA/SecretRef 入庫→DB 預留埠(唯一約束)→寫建立 journal→拉映像→建容器(打 `gsm.*` 標籤)→寫 DB 完成→清 journal;任一步失敗回滾(釋放預留、移除容器、不留完成紀錄)。
2. **監控(R6)**:core 訂閱 agent stats/log/events WS→背壓轉推;資料磁碟走宿主路徑;線上走探針。
3. **崩潰復原(R8)**:agent `Events` die→core 比對 planned-stop token 與探針→真崩潰才 `RestartPolicy` 退避→超上限停+告警;操作序列化。
4. **指令(R7)**:GUI→core→agent `command`→`GameCommandAdapter`(rcon raw / rest 具名動作)→回顯。
5. **備份/還原(R9)**:排程→planned-stop→`Archive`(一致快照+checksum,agent 擁有根,回 `BackupID`)→`RetentionPolicy`;還原=planned-stop→staging 驗證→原子切換→`Restore` 回新 runtime ID→依原 desired 收斂;全程 journal 保證 crash-safe。
6. **啟動對帳(R13)**:single-instance lock→agent `List`/`Inspect`(區分 daemon 不可用 vs 空)→對帳(孤兒/不一致/journal 未完成的建立)→重建事件訂閱→離線期 summary。

## 取捨與替代方案

- **技術選型(已裁決)**:Go 全棧 + Wails v2 + 官方 Docker SDK。
- **核心↔代理走 loopback HTTP+token**:Pterodactyl 模式讓多節點接縫為真 + 修補「同機任何程序可控制」;首版只做版本化 loopback,不做跨機。
- **restart 單一所有者**:由 core 編排,agent 不開 /restart,避免繞過 lock/planned-stop(依二審 H3)。
- **事件納入 RuntimeBackend**:die/health 經介面而非直碰 SDK,使 Mock/未來 Native 可等價替換(依二審 H2)。(2026-07-16:Native 等價替換已由 specs/native-backend/design.md 落實)
- **備份首版=停機快照 + bind mount**:一致性最穩;named volume 原子交換列 spike;agent 擁有備份根、對外 opaque BackupID(依二審 H6)。
- **SecretRef 型別**:輸入即入金鑰庫、統一 redaction;runtime 明文為明確例外(依二審 H7)。
- **模組 itzg 唯一擁有者**:不與 itzg 爭 /data(依二審 H10/rev.2)。
- **首版不支援 update**:改參數=移除重建,明確排除(依二審 H3)。
- **宣告式範本檔**:擴充=加檔。

## 風險

- Palworld REST/RCON 現況需查證 → **Palworld spike(T9 前置,由 T6 執行)**,鎖版本/映像;REST 為主、RCON legacy。
- 備份原子性(Windows/Docker Desktop、named volume)→ **備份 spike(T4 前置)**;首版收斂為停機 + bind mount。
- itzg 支援矩陣/`AUTO_CURSEFORGE` env/CurseForge 條款需查證 → **模組 spike(T14 前置)**;金鑰使用者自填。
- Docker on Windows 需 WSL2;反作弊/Windows-only 容器不支援 → 已知邊界,未來原生後端。
- Wails 複雜 UI 成本 → 成熟前端框架(Svelte/React)。
- 版本相容(modernc 對 Go 版本要求)→ T1 一併鎖定驗證。

## 測試策略

- **單元**:R1 範本載入(有效/缺欄位/版本/ID/adapter 不存在)+ 「原樣解析 design 範本」golden test;R4 MockBackend(含 List/Inspect/Remove/Archive/Restore/Events)跑通;R7 協定/動作選擇;R8 狀態機轉移表 + planned-stop token TTL/consume;R10 fake clock 驗窗口/hysteresis/cooldown + 429/5xx;R12 遷移/integrity/quarantine/SecretRef redaction;R14 envelope/codes。
- **整合(MockBackend,免 Docker)**:R2 建立各階段 failure injection + 埠衝突(wildcard)+ 併發;R3 啟停重建冪等 + 同 runtime ID + 60/30s;R4 core→agent→mock 崩潰路徑;R5 loopback+token 拒絕未授權 + Origin + 冪等鍵重播 + 錯誤碼;R8 planned-stop 不誤判 + 探針卡死 + 超上限;R9 archive/checksum/retention/原子還原/中斷 rollback;R10 producer→AlertSink→dispatcher 跨層;R13 對帳(孤兒/不一致/daemon 不可用/單一實例)。
- **整合(真 Docker,build tag/`-short`)**:R2/R3 真 Minecraft;R6 CPU/RAM/資料磁碟/log/玩家數;R7 Minecraft RCON `list`;R11 Paper+外掛+itzg Modrinth+手動匯入。
- **Spike(前置 gate)**:Palworld(T9 前,由 T6 執行)、備份(T4 前)、模組(T14 前);三者結論落檔。
- **端到端/基準**:重啟後 R12/R13 對帳;GUI 全流程;T16 效能 benchmark 回填 NFR 門檻。
