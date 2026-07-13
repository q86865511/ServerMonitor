# game-server-manager — 技術設計(design.md)

> 建立日期:2026-07-13｜狀態:已核可(2026-07-13),經 Codex 二審修訂(2026-07-13, rev.2)
> 對照 requirements.md;每條 R# 出現在需求對應表。

## 架構概述

四層,收斂為單一 **Wails 應用(Go 後端 + web 前端)**。Wails 的 Go 後端內含 **管理核心**(實例登錄、狀態機、排程器、監控聚合、告警分派、範本引擎、對帳)與 **本機節點代理**(代理 API 綁 127.0.0.1,含每次啟動的 bearer token);核心以 `NodeClient`(HTTP)呼叫本機代理,保留多節點接縫為真。代理以抽象 `RuntimeBackend`(首版 Docker/官方 Docker SDK)操作容器與資料封存,另以 `GameCommandAdapter` 就地執行遊戲指令(RCON/REST)。web 前端經 Wails bindings 呼叫 Go 後端。**遊戲範本** 為版本化宣告式檔案。

**生命週期(依裁決:僅 GUI 開啟時運作):** 監控、自動復原、排程備份/重啟、告警僅在應用開啟時運作;關閉應用會暫停自動化,但**遊戲容器由 Docker 獨立維持**——重開應用時經 **啟動對帳(R13)** 恢復 observed 狀態與事件訂閱。無隨 Windows 自啟的背景服務。遠端節點為未來獨立 Go 代理二進位(`cmd/agent`),首版只完成版本化 loopback 契約,不實作跨機部署。

**建置形態(Go module + Wails,單一 repo):**
- `internal/protocol` — 共享型別:版本化範本 schema、API DTO、代理 API 契約(struct + JSON tag)、事件型別。
- `internal/agent` — 節點代理:`RuntimeBackend` 介面(Docker/Mock/未來 Native)、`GameCommandAdapter`、備份封存、Docker 事件監看、代理 HTTP/WS server(含 bearer 認證)。
- `internal/core` — 管理核心:實例登錄、狀態機、對帳、範本引擎、排程器、監控聚合、告警、事件記錄、`NodeClient`。
- `app`(Wails 專案根)— Go 後端組裝 core+本機 agent 並綁定前端;`frontend/` 為 web 前端(Svelte 或 React)。
- (未來)`cmd/agent` — 遠端節點獨立代理二進位,複用 `internal/agent`。

## 需求對應表

| 需求 | 設計元素 | 說明 |
|---|---|---|
| R1 | `internal/core` 範本引擎 + `protocol.GameTemplate`(含 `schema_version`)+ `templates/*.toml` | 版本化宣告式範本;缺欄位/版本不符/ID 重複拒載並記事件;標示所需 adapter 是否存在 |
| R2 | core `InstanceService.Create`(原子+回滾) + agent `RuntimeBackend.Create` + Docker 標籤 | 驗證必填/EULA/埠衝突鍵→拉映像→建容器(`Created`)→寫 DB;任一步失敗回滾不留孤兒 |
| R3 | agent `RuntimeBackend.{Start,Stop,Status}` + core Restart 編排 + per-instance lock | 優雅停機(停止指令+寬限期+逾時強停);啟動輪詢至 Running/逾時報錯;重啟同容器;冪等 |
| R4 | `internal/agent` `RuntimeBackend` 介面(+List/Inspect/Remove/Archive/Restore/ExecProcess)+ `GameCommandAdapter` | 生命週期與封存經介面;遊戲指令另走 adapter;Mock 跑通(含 core→agent→mock) |
| R5 | core `NodeClient`(HTTP+bearer) + agent HTTP/WS server(loopback+token+Origin) + `NodeRegistry` | 版本化契約(OpenAPI);未帶 token 拒絕;代理無回應→標離線 |
| R6 | agent stats 輪詢 + Docker stats + 資料磁碟採集 + 探針 + `MetricsHub`(WS 推送,背壓) | ≤5s 推 CPU/RAM;資料磁碟用量走宿主路徑/helper;線上狀態走探針;log 有界緩衝 |
| R7 | agent `GameCommandAdapter`:`RconAdapter` + `PalworldRestAdapter` + 範本 `command_protocols[]` | RCON=raw console;REST=具名動作;Palworld REST 為主、RCON legacy 不啟用 |
| R8 | core 狀態機(desired/observed)+ planned-stop token + `RestartPolicy` + 健康探針 + `Scheduler` | 區分計畫停止/崩潰;窗內超上限停+告警;卡死經探針;排程 UTC;操作序列化 |
| R9 | core `Scheduler` + agent `RuntimeBackend.{Archive,Restore}` + `RetentionPolicy` + checksum | 一致快照(save hook/停機);備份清單+checksum;還原=停機→驗證→原子切換→復原原狀態 |
| R10 | core `AlertDispatcher` + `DiscordWebhookChannel` + 門檻窗口/hysteresis/cooldown | 四類事件固定 payload;資源門檻去重;重試遵守 429/5xx;無管道僅記錄 |
| R11 | 範本 `variants`/`mods(owner=image-native)` + itzg `MODRINTH`/`AUTO_CURSEFORGE` + 手動格式 | itzg 為唯一安裝擁有者;工具不解壓 /data;支援矩陣;不相容前置檢查 |
| R12 | core SQLite(`modernc.org/sqlite`)+ migrations + integrity check + quarantine + `go-keyring` 參照 | 重啟重現;毀損隔離不覆寫;敏感值存金鑰庫;遊戲 runtime 設定為明確例外 |
| R13 | core `Reconciler`(啟動 List/Inspect 對帳)+ Docker label(UUID)+ single-instance lock | DB↔容器對帳;孤兒標記;重開恢復事件訂閱;拒絕第二實例接管 |
| R14 | core `EventLog`(結構化,SQLite events 表)+ 查詢 API + 輪替 | event code/時間/severity/關聯;其他 R# 的「已記錄」以 event code 判定 |

## 介面與資料模型

**遊戲範本 schema(`protocol.GameTemplate`,合法 TOML,擴充性核心):**
```toml
schema_version = 1
id = "minecraft"
name = "Minecraft: Java Edition"
runtime = "docker"
data_dirs = ["/data"]              # R9 備份範圍(頂層,非在 [mods] 之下)

[docker]
image = "itzg/minecraft-server"

[[variants]]                        # R11
id = "paper"
[variants.env]
TYPE = "PAPER"

[[ports]]                           # R2 衝突鍵 (bind_ip, protocol, container)
name = "game"
container = 25565
protocol = "tcp"
required = true

[[ports]]
name = "rcon"
container = 25575
protocol = "tcp"

[[params]]                          # R2 建立時使用者填
key = "MEMORY"
label = "記憶體上限"
type = "string"
default = "2G"

[[params]]
key = "EULA"
label = "接受 Minecraft EULA"
type = "bool"
required = true                     # R2 未接受不得建立,不暗中預設

[[command_protocols]]               # R7 可多種
kind = "rcon"                       # raw console
host_port_ref = "rcon"
password_param = "RCON_PASSWORD"

[health]                            # R8 健康探針
kind = "tcp"                        # tcp | rcon | rest | docker
port_ref = "game"

[players_query]                     # R6
kind = "rcon"
command = "list"

[mods]                              # R11 itzg 原生為唯一擁有者
owner = "image-native"              # 工具不另解壓 /data
plugin_dir = "/data/plugins"
modpack_env = ["MODRINTH", "AUTO_CURSEFORGE"]
manual_formats = ["mrpack", "curseforge-zip"]
```
Palworld 範本以 `[[command_protocols]]` 定義 `kind = "rest"`(具名動作:players/announce/kick/save/shutdown,Basic Auth)為主,另一組 `kind = "rcon"; legacy = true`(首版不啟用,待查證)。

**`RuntimeBackend` 與 `GameCommandAdapter` 介面(R4/R7,`internal/agent`):**
```go
type RuntimeBackend interface {
  Create(ctx context.Context, spec InstanceSpec) (RuntimeID, error)
  Start(ctx context.Context, id RuntimeID) error
  Stop(ctx context.Context, id RuntimeID, opts StopOpts) error     // 優雅停機
  Status(ctx context.Context, id RuntimeID) (RuntimeStatus, error)
  List(ctx context.Context) ([]RuntimeRef, error)                  // R13 對帳
  Inspect(ctx context.Context, id RuntimeID) (RuntimeInfo, error)
  Remove(ctx context.Context, id RuntimeID) error
  Logs(ctx context.Context, id RuntimeID, opts LogOpts) (LogStream, error)
  ExecProcess(ctx context.Context, id RuntimeID, cmd ExecCmd) (ExecResult, error) // 容器內程序,非遊戲指令
  Stats(ctx context.Context, id RuntimeID) (ResourceStats, error)
  Archive(ctx context.Context, id RuntimeID, dst string) (ArchiveInfo, error)     // R9 備份
  Restore(ctx context.Context, id RuntimeID, src string) error                    // R9 還原
}
// Restart = core 層 Stop+Start 編排,非介面方法。
// 遊戲指令與 RuntimeBackend 分離:
type GameCommandAdapter interface {
  Send(ctx context.Context, target CommandTarget, cmd GameCommand) (CommandResult, error)
}
// 實作:RconAdapter(Minecraft/Palworld-legacy)、PalworldRestAdapter(具名動作)。
```
實作:`DockerBackend`(官方 `github.com/docker/docker/client`)、`MockBackend`(測試)、未來 `NativeBackend`(SteamCMD)。

**節點代理 API 契約(R5,core↔agent,版本化 + bearer 認證):**
所有 HTTP/WS 需 `Authorization: Bearer <每次啟動 token>`;WS 檢查 Origin。端點:
- `GET /instances`、`POST /instances`、`GET /instances/{id}`、`DELETE /instances/{id}`
- `POST /instances/{id}/{start,stop,restart}`(restart=編排)
- `GET /instances/{id}/status`、`WS /instances/{id}/stats`、`WS /instances/{id}/logs`
- `WS /events`(容器/狀態事件串流,供 R8/R13)
- `POST /instances/{id}/command`(遊戲指令,經 `GameCommandAdapter`)
- `GET /instances/{id}/backups`、`POST /instances/{id}/backup`、`POST /instances/{id}/restore`
- `GET /health`

契約以 OpenAPI(或等價)版本化,定義 request/response、錯誤碼與冪等鍵。

**狀態機與對帳(R8/R13):** 每實例維護 `desired`(Running/Stopped)與 `observed` 狀態、planned-stop token、per-instance lock;`Reconciler` 於啟動時 `List`/`Inspect` 對帳 DB 與容器(以 UUID Docker label 關聯),處理孤兒與不一致;應用以 lockfile/named mutex 確保單一實例。

**持久化(R12/R14):** SQLite(`modernc.org/sqlite`,純 Go 免 CGO)+ 遷移(schema 版本)+ `PRAGMA integrity_check`;毀損進 quarantine 不覆寫。表:`instances`(uuid, template_id, variant, params_json, node_id, runtime_id, desired_state, observed_state)、`schedules`、`settings`、`backups`(uuid, instance_uuid, ts, checksum, path)、`events`(code, ts_utc, severity, instance_uuid, message)。敏感值於 DB 存金鑰庫參照,實體以 `go-keyring` 存 Windows 認證管理員。

## 關鍵流程

1. **建立(R2)**:GUI→core `Create`→驗證必填/EULA/埠衝突→`NodeClient` POST→agent `DockerBackend.Create`(Docker SDK 拉映像+建容器,`Created`,打 UUID label)→core 寫 DB;任一步失敗回滾(移除容器/不留 DB 紀錄)。
2. **監控+log(R6)**:GUI 開實例→core 訂閱 agent stats/log WS→背壓後轉推 GUI;agent 每 ≤5s 推 stats,資料磁碟走宿主路徑,線上狀態走探針,依範本跑 `players_query`。
3. **崩潰復原(R8)**:agent 監看 Docker events→容器 die→core 比對 planned-stop 意圖與健康探針→非計畫退出/卡死才 `RestartPolicy` 退避重試→窗內超上限→停止+`AlertDispatcher`;操作序列化。
4. **指令(R7)**:GUI console→core→agent `command`→`GameCommandAdapter`(Minecraft RCON / Palworld REST 具名動作)→回顯;Palworld RCON legacy 不啟用。
5. **備份/還原(R9)**:排程 tick→core→agent `Archive`(一致快照:save hook/停機→打包+checksum)→`RetentionPolicy` 修剪;還原=停機→staging 驗證→原子切換→恢復原 desired 狀態。
6. **啟動對帳(R13)**:應用啟動→single-instance lock→agent `List`/`Inspect`→與 DB 對帳(孤兒/不一致處理)→重建事件訂閱。

## 取捨與替代方案

- **技術選型(已裁決)** — Go 全棧:Wails 桌面 + Go 核心/代理,Docker 用官方 SDK。架構(四層/介面/schema)與語言無關,選型只換函式庫。
- **相依版本鎖定(依二審)** — 明訂:Go 版本、**Wails v2 stable**(非 v3 alpha)、Docker client 模組 `github.com/docker/docker/client` 及版本 + 最低 Engine API、`modernc.org/sqlite` 版本(留意其對 Go 版本要求)、`github.com/zalando/go-keyring` 版本。
- **核心↔代理走 HTTP(即使單機,loopback+token)** — Pterodactyl 模式讓多節點接縫為真;加 bearer token 與 Origin 檢查修補「同機任何程序可控制」的洞。首版只做版本化 loopback 契約,不做 `cmd/agent`/跨機 TLS/節點註冊。
- **生命週期:僅 GUI 開啟時運作(已裁決)** — 核心+代理內嵌於 Wails;代價是關閉時自動化暫停(容器仍由 Docker 維持),以 R13 對帳彌補重開落差。日後要 24/7 可加選裝背景服務(未來 spec)。
- **模組安裝擁有者=itzg 原生(依二審)** — 首版用 `MODRINTH`/`AUTO_CURSEFORGE` + 手動受限檔案;捨棄自建下載器以免與 itzg 爭奪 `/data` 互刪、且低估成本。自建串接延後。
- **Palworld 指令:REST 具名動作為主、RCON legacy(依二審)** — REST 是固定具名動作非自由指令台;RCON 現況需查證,首版不綁。
- **宣告式範本檔** — 擴充=加檔;捨棄「每款遊戲寫程式碼」。

## 風險

- Palworld REST 端點/認證與 RCON 現況隨版本演進且需查證 → 先做 time-boxed spike 並鎖 Palworld 伺服器版本/映像(tag 或 digest);REST 為主,RCON 標 legacy。
- CurseForge API 條款/金鑰限制 → 首版倚賴 itzg `AUTO_CURSEFORGE`(`CF_API_KEY` 可由使用者自填)+ 手動受限檔案;正式發布前查證條款與 attribution。
- Docker on Windows 需 WSL2;含反作弊/Windows-only 遊戲 Linux 容器不支援 → 選 Docker 的已知邊界,未來原生後端緩解。
- 備份 Docker named volume:Windows agent 無法直接打包 → 明定 bind mount 或以 helper container/volume mount 封存,取一致快照。
- GUI 關閉期的孤兒/重複容器 → R13 對帳 + UUID label + idempotency key + single-instance lock。
- Wails 複雜 UI 開發成本 → 前端用成熟框架(Svelte/React)。
- 監控磁碟語意誤用(Docker block I/O≠資料容量) → 以宿主路徑/helper 採集資料用量。

## 測試策略

- **單元**:R1 範本載入(有效/缺欄位/版本不符/ID 重複/adapter 標示)+ 「原樣解析 design 範例」golden test、R4 MockBackend(含 List/Inspect/Remove/Archive/Restore)跑通、R7 adapter 選擇、R10 mock Discord(payload+去重+429/5xx 重試+無管道)、R12 遷移/integrity/quarantine、R14 event code/欄位。
- **整合(MockBackend,免 Docker)**:R2 建立原子性/回滾/埠衝突/EULA、R3 啟停重啟冪等+同容器、R4 core→agent→mock、R5 loopback+token 拒絕未授權+斷線標離線、R8 planned-stop 不誤判+崩潰重啟+超上限停+探針卡死、R9 archive/checksum/retention/原子還原+復原原狀態、R13 對帳(孤兒/不一致/單一實例)。
- **整合(真 Docker,build tag/`-short` 切換)**:R2/R3 建真 Minecraft、R6 CPU/RAM/資料磁碟/log 串流/玩家數、R7 Minecraft RCON `list`、R11 Paper+外掛+itzg Modrinth 模組包+手動匯入。
- **Palworld spike(先做,鎖版本)**:查證並驗 REST 具名動作;RCON legacy 不列首版驗收。
- **端到端**:重啟應用後 R12/R13 狀態重現與對帳;GUI 走一輪建立→啟動→監控→下指令→備份→設告警。
