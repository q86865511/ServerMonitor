# game-server-manager — 技術設計(design.md)

> 建立日期:2026-07-13｜狀態:已核可(2026-07-13)
> 對照 requirements.md;每條 R# 出現在需求對應表。

## 架構概述

四層,收斂為單一 **Wails 應用(Go 後端 + web 前端)**。Wails 的 Go 後端內含 **管理核心**(實例登錄、排程器、監控聚合、告警分派、範本引擎)與 **本機節點代理**(代理 API 開在 127.0.0.1);核心以 `NodeClient`(HTTP)呼叫本機代理,保留多節點接縫為真。代理以抽象 `RuntimeBackend`(首版 Docker/官方 Docker SDK)操作容器,就地執行指令協定(RCON/REST)與備份。web 前端經 Wails bindings 呼叫 Go 後端。**遊戲範本** 為宣告式檔案,新增遊戲=加一份範本。

**生命週期(依裁決:僅 GUI 開啟時運作):** 監控、自動復原、排程備份/重啟、告警僅在應用開啟時運作;關閉應用會暫停這些自動化,但**遊戲伺服器容器由 Docker 獨立維持、不會停止**——重開應用即恢復。無隨 Windows 自啟的背景服務。遠端節點為未來獨立 Go 代理二進位(`cmd/agent`),複用同一 agent 套件。

**建置形態(Go module + Wails,單一 repo):**
- `internal/protocol` — 共享型別:範本 schema、API DTO、代理 API 契約(struct + JSON tag)。
- `internal/agent` — 節點代理:`RuntimeBackend` 介面(Docker/Mock/未來 Native)、指令協定通道、備份執行、Docker 事件監看、代理 HTTP/WS server。
- `internal/core` — 管理核心:實例登錄、範本引擎、排程器、監控聚合、告警、`NodeClient`(HTTP)。
- `app`(Wails 專案根)— Go 後端組裝 core+本機 agent 並綁定前端;`frontend/` 為 web 前端(Svelte 或 React)。
- (未來)`cmd/agent` — 遠端節點獨立代理二進位,複用 `internal/agent`。

## 需求對應表

| 需求 | 設計元素 | 說明 |
|---|---|---|
| R1 | `internal/core` 範本引擎 + `protocol.GameTemplate` schema + `templates/*.toml` | 宣告式範本檔載入/驗證;內建 minecraft、palworld 兩份;缺欄位拒載並記 log |
| R2 | core `InstanceService.Create` + agent `RuntimeBackend.Create` | 驗證必填參數/埠衝突→拉映像→建容器(未啟動)→登錄持久化 |
| R3 | agent `RuntimeBackend.{Start,Stop,Restart}`(優雅停機) | 停機先送停止指令/給寬限期;重複啟動不重建 |
| R4 | `internal/agent` `RuntimeBackend` 介面 + `DockerBackend`/`MockBackend` | 所有操作經介面;Mock 可替換跑通生命週期(單元測試) |
| R5 | `internal/core` `NodeClient`(HTTP) + agent HTTP/WS server + `NodeRegistry` | 核心經 localhost HTTP 呼叫代理;代理無回應→標離線不整體當 |
| R6 | agent stats 輪詢 + Docker stats API + `MetricsHub`(WS 推送) | ≤5s 推 CPU/RAM;log WS 串流;玩家數走範本 `players_query` |
| R7 | agent `CommandChannel`:`RconChannel` + `PalworldRestChannel` | 依範本 `command_protocol` 選通道;Palworld 首版 REST+RCON 皆備 |
| R8 | agent Docker-events 監看 + core `RestartPolicy` + `Scheduler` | 崩潰→退避重試→窗內超上限停+告警;排程重啟前發遊戲內公告(若支援) |
| R9 | core `Scheduler` + agent `BackupRunner` + `RetentionPolicy` | 定時打包 `data_dirs`;保留最近 N;還原前先停機再還原 |
| R10 | core `AlertDispatcher` + `DiscordWebhookChannel` | 崩潰/重啟失敗/備份失敗/資源超標→Discord;無管道僅記錄;送失敗有限重試 |
| R11 | core `ModpackProvider`:`ModrinthProvider`+`CurseForgeProvider` + 範本 `variants`/`mods` | 變體選型(Paper/Forge/Fabric);工具內搜尋下載模組包;保留手動匯入 |
| R12 | core SQLite 登錄(實例/排程/設定) + `go-keyring` 存敏感值 | UTF-8 持久化;重啟後重現;毀損以預設值容錯;RCON/webhook 存 OS 金鑰庫 |

## 介面與資料模型

**遊戲範本 schema(`protocol.GameTemplate`,TOML,擴充性核心):**
```toml
id = "minecraft"
name = "Minecraft: Java Edition"
runtime = "docker"
[docker]
image = "itzg/minecraft-server"
[[variants]]              # R11
id = "paper"; env = { TYPE = "PAPER" }
[[ports]]                # R2 埠衝突偵測依此
name = "game"; container = 25565; protocol = "tcp"; required = true
[[params]]               # R2 建立時使用者填
key = "MEMORY"; label = "記憶體上限"; type = "string"; default = "2G"
[command_protocol]       # R7
type = "rcon"; host_port_ref = "rcon"; password_param = "RCON_PASSWORD"
[health]                 # R8 崩潰偵測
type = "docker"
[players_query]          # R6
type = "rcon"; command = "list"
[mods]                   # R11
plugin_dir = "/data/plugins"; mod_dir = "/data/mods"; providers = ["modrinth","curseforge"]
data_dirs = ["/data"]    # R9 備份範圍
```

**`RuntimeBackend` 介面(R4,`internal/agent`):**
```go
type RuntimeBackend interface {
  Create(ctx context.Context, spec InstanceSpec) (RuntimeID, error)
  Start(ctx context.Context, id RuntimeID) error
  Stop(ctx context.Context, id RuntimeID, opts StopOpts) error   // 優雅停機
  Status(ctx context.Context, id RuntimeID) (RuntimeStatus, error)
  Logs(ctx context.Context, id RuntimeID, opts LogOpts) (LogStream, error)
  Exec(ctx context.Context, id RuntimeID, cmd ExecCmd) (ExecResult, error)
  Stats(ctx context.Context, id RuntimeID) (ResourceStats, error)
}
```
實作:`DockerBackend`(官方 `github.com/docker/docker/client`)、`MockBackend`(測試)、未來 `NativeBackend`(SteamCMD)。

**節點代理 API 契約(R5,core↔agent,HTTP+WS):**
`POST /instances`、`/instances/{id}/{start,stop,restart}`、`GET /instances/{id}/status`、`GET /instances/{id}/stats`、`WS /instances/{id}/logs`、`POST /instances/{id}/command`(指令協定)、`POST /instances/{id}/backup`、`GET /health`。

**持久化(R12):** SQLite(`modernc.org/sqlite`,純 Go 免 CGO)——`instances`(id, template_id, variant, params_json, node_id, state)、`schedules`(instance_id, kind, cron)、`settings`、`events`;敏感值(RCON 密碼、webhook URL)以 `go-keyring` 存 Windows 認證管理員,DB 只存參照。

## 關鍵流程

1. **建立(R2)**:GUI→core `Create`→驗證必填/埠衝突→`NodeClient` POST→agent `DockerBackend.Create`(Docker SDK 拉映像+建容器,未啟動)→core 持久化→GUI 更新。
2. **監控+log(R6)**:GUI 開實例→core 訂閱 agent stats/log WS→轉推 GUI;agent 每 ≤5s 推 stats,依範本跑 `players_query`。
3. **崩潰復原(R8)**:agent 監看 Docker events→容器 die→通知 core→`RestartPolicy` 退避重試→窗內超上限→停止+`AlertDispatcher`。
4. **指令(R7)**:GUI console→core→agent `command`→`CommandChannel`(RCON/Palworld REST)→容器→回顯。
5. **備份/還原(R9)**:排程 tick→core→agent `BackupRunner`(打包 `data_dirs` 至備份區)→`RetentionPolicy` 修剪;還原=停機→還原→啟動。

## 取捨與替代方案

- **技術選型(已裁決)** — 採 **Go 全棧:Wails 桌面 + Go 核心/代理**,Docker 用官方 SDK。Go 是容器工具母語(Docker、k8s、Pterodactyl Wings 皆 Go),Docker SDK 原生、單二進位代理利於未來遠端節點。架構(四層/介面/schema)與語言無關,選型只換函式庫。
- **核心↔代理走 HTTP(即使單機,於 Wails 行程內開 127.0.0.1)** — 採 Pterodactyl 模式讓多節點接縫為真;捨棄「行程內直呼」以免日後重構(代價:一個 localhost 端點,overhead 可忽略)。
- **生命週期:僅 GUI 開啟時運作(已裁決)** — 核心+代理內嵌於 Wails 應用,不裝隨 Windows 自啟的背景服務。取捨:最簡單、免服務安裝;代價是關閉應用時自動復原/排程備份暫停(遊戲容器仍由 Docker 維持不停)。日後要 24/7 自動化可加選裝背景服務模式(未來 spec)。
- **宣告式範本檔** — 擴充=加檔;捨棄「每款遊戲寫程式碼」。

## 風險

- Palworld REST API 端點細節隨版本演進 → 實作時對照官方文件驗證;REST/RCON 雙備,一方不可用可退。
- CurseForge API 需申請金鑰且有條款 → 金鑰選填,無金鑰時退回 Modrinth(開放 API)+ 手動匯入。
- Docker on Windows 需 WSL2;含反作弊/Windows-only 的遊戲 Linux 容器不支援 → 為選 Docker 的已知邊界,未來原生後端緩解。
- Wails 複雜 UI 開發成本 → 前端用成熟框架(Svelte/React)。
- 大存檔備份耗時/鎖檔 → 優雅停機或磁碟快照。

## 測試策略

- **單元**:R1 範本載入(有效/缺欄位/新增範本現身)、R4 MockBackend 跑通生命週期、R10 mock Discord 端點驗 payload+無管道不報錯+送失敗重試、R12 毀損設定容錯。
- **整合(MockBackend,免 Docker)**:R2 建立流程(參數驗證/埠衝突)、R3 啟停重啟、R5 core↔agent localhost HTTP + 斷線標離線、R8 模擬 die→重啟+超上限停+告警、R9 備份產檔/保留修剪/還原一致。
- **整合(真 Docker,以 build tag 或 `-short` 切換,手動/CI 選跑)**:R2/R3 建真 Minecraft 實例、R6 stats≤5s+log 串流+玩家數、R7 Minecraft RCON `list` 與 Palworld REST+RCON、R11 Paper 變體+外掛生效+Modrinth 下載+手動匯入。
- **端到端**:重啟應用後 R12 狀態重現;GUI 走一輪建立→啟動→看監控→下指令→備份。
