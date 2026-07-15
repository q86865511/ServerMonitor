# ServerMonitor

可擴充的遊戲伺服器管理與監控桌面工具。首波支援 Minecraft、Palworld 及類似的 Steam dedicated server,並以「遊戲範本」設計讓未來新遊戲能快速部署。

## 狀態

實作完成(/pipeline,16/16)。規格 rev.3 已核可(見 `specs/game-server-manager/`)。**後端(T1–T14)、桌面 GUI(T15)、端到端+效能基準+文件(T16)完成**:完整生命週期/監控/指令/排程/備份/告警後端 + Wails 桌面介面(實例卡片、建立精靈、主控台(RCON 與 Palworld REST 動作)、排程/備份/告警設定、事件檢視)。build / test / -race / 真 Docker 整合 / svelte-check 皆綠。**T16 真 Docker 端到端**:Minecraft 全流程(建立→啟動→監控→RCON 指令→備份→還原→停止→移除)與重啟後 R12/R13 對帳實跑通過;效能基準已量測回填 `design.md`(閒置工作集 ~18MB、每實例監控開銷 +~5MB);**Palworld REST E2E 實跑通過**(建立→認證就緒探測→players 動作回顯→停止;映像未快取的環境自動 skip,`docker pull thijsvanloef/palworld-server-docker:v2.5.1` 後可跑)。

## 快速開始(使用者)

1. 開啟 Docker Desktop(必要)。
2. 執行 `build\bin\servermonitor.exe`(或開發模式 `wails dev`)。
3. 「建立伺服器」→ 選 Minecraft → 變體 Paper → 勾 EULA → 設 RCON 密碼 → 建立(首次拉映像需數分鐘)。
4. 卡片「啟動」→ 開「主控台」看 log、輸入 `list` 測指令 → 設定頁玩備份/排程/告警。
5. 遊戲連線:`localhost:25565`。應用資料在 `%LOCALAPPDATA%\ServerMonitor\`;**關閉本工具不會停伺服器**(容器由 Docker 維持,重開自動接管)。

## 特色(規劃中)

- 一鍵建立/部署遊戲伺服器(Docker 執行後端)
- 資源監控 + 即時 log 主控台
- RCON / REST 指令
- 自動重啟 / 崩潰復原
- 排程備份 + 還原
- Discord 告警
- 模組 / 模組包支援(Paper/Forge/Fabric、Modrinth/CurseForge)
- 擴充新遊戲 = 加一份 `templates/*.toml`

## 技術棧

Go + Wails(桌面)+ web 前端 + Docker。

## 需求環境

- Windows 11
- **Docker Desktop(WSL2 後端)** — 執行遊戲容器與真 Docker 整合測試的唯一執行後端。個人/小型企業用途免費(見 [Docker Desktop 授權](https://docs.docker.com/subscription/desktop-license/));安裝見 [Docker Desktop for Windows](https://docs.docker.com/desktop/install/windows-install/),安裝時選 **WSL 2 backend**。首次啟動遊戲需下載映像(Minecraft 約 1.2GB、Palworld 約 6-8GB)。
- Go 1.26+、Wails CLI v2(開發用)、Node 18+/npm(前端)

## 安裝與執行

需先安裝 Go 1.26+ 與 Wails CLI v2(`go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0`;鎖版本對齊專案 pin 的 Wails v2.13,避免升級回歸)。

```
go build ./...               # 編譯全部套件
go test ./...                # 單元/整合測試(免 Docker)
go test -tags docker ./...   # 真 Docker 整合測試 + 端到端 + 效能基準(需 Docker daemon)
wails dev                    # 開發模式(GUI)
wails build                  # 產出 Windows 可執行檔
```

端到端(T16,需 Docker daemon;會實際拉映像、建/啟容器,測後自動清理):

```
go test -tags docker -run TestE2E ./internal/app/                     # Minecraft 全流程 + 重啟對帳 + 效能基準
go test -tags docker -run TestE2E_MinecraftFullLifecycle ./internal/app/
go test -tags docker -run TestE2E_Palworld ./internal/app/            # 需先 docker pull palworld 映像
go test -tags docker -run TestPerf_LogThroughput ./internal/core/     # log 高流量 fanout 基準
```

## 範本撰寫指南

新增「沿用既有 adapter」的遊戲 = 加一份 `templates/<id>.toml`,**不改核心**。範本放內建目錄(執行檔旁 `templates/`)或使用者目錄(`%LOCALAPPDATA%\ServerMonitor\templates\`),啟動時載入;缺欄位/版本/ID 重複/adapter 不存在會拒載並記 `TEMPLATE_LOAD_FAILED` 事件。schema 由 `internal/protocol/template.go` 定義,最小可跑範例見 `templates/minecraft.toml`、`templates/palworld.toml`。

schema 欄位速查(對映 `GameTemplate`):

- **頂層**:`schema_version`(整數,目前 1)、`id`(唯一)、`name`(顯示名)、`runtime`(首版固定 `"docker"`)、`data_dirs`(容器內資料目錄陣列,R9 備份範圍)。
- **`[docker]`**:`image`(鎖 tag,如 `itzg/minecraft-server:java21`)、`image_digest`(可選,填 `sha256:...` 做不可變鎖定,R11)。
- **`[[variants]]`**(可多筆):`id`、`loader`(loader 家族 paper/vanilla/forge/fabric/…,供模組包相容前置檢查)、`[variants.env]`(建立時透傳的容器 env,如 `TYPE = "PAPER"`)。
- **`[[ports]]`**(可多筆):`name`(供 host_port_ref/port_ref 參照)、`container`、`host_port`(0=動態)、`bind_ip`(wildcard `0.0.0.0` 與具體 IP 視為重疊)、`protocol`(tcp/udp)、`required`。衝突鍵 `(bind_ip, protocol, host_port)`(R2)。
- **`[[params]]`**(可多筆):`key`(即容器 env 變數名,itzg 慣例)、`label`、`type`(string/bool/int)、`default`、`required`(如 Minecraft `EULA` 未接受不得建立)。
- **`[[secrets]]`**(可多筆):`key`、`label`。輸入即入 OS 金鑰庫、不落 DB/log(R12);runtime 明文注入容器 env 為明確例外。
- **`[[command_protocols]]`**(tagged union,R7):`protocol_id`、`kind`(`rcon`|`rest`)。rcon 用 `host_port_ref`+`password_ref`(raw console);rest 用 `host_port_ref`(目標埠參照,指向 [[ports]].name)、`password_ref`(Basic Auth 密碼的 secret 參照,指向 [[secrets]].key)、`auth`(如 `basic`)、`username`、`[[command_protocols.actions]]`(具名動作:`action_id`/`method`/`path`)。`legacy=true` 標記首版不啟用的協定(如 Palworld RCON)。
- **`[hooks]`**(生命週期映射,R3/R8/R9):`stop`/`announce`/`quiesce`/`resume`,各為 `{ protocol_id, command 或 action_id, args, message_key }`。rcon 用 `command`(可含 `{msg}`),rest 用 `action_id`+`args`,`message_key` 指定動態訊息寫入的 args 欄位。
- **`[health]`**(就緒/存活探針,R8):`kind`(tcp/rcon/rest/docker;未定義=容器 running 即就緒)、`port_ref`、`action_id`(可選,僅 kind=rest)。⚠️ tcp 探針撥的是 Docker 發布埠,其 userland proxy 會在容器程序尚未真正綁定前即接受連線→就緒「假陽性」提早;遊戲伺服器建議用 `rcon`/`rest`(協定就緒才算健康),tcp 僅適合無指令協定的簡單服務。kind=rest **建議搭配 `action_id`**(指向某具名 rest 動作,如 Palworld `info`)——探測經指令協定送出、帶該協定的 Basic Auth 認證;省略 `action_id` 則對 `port_ref` 根路徑發**未認證** GET 判 2xx,只適合無認證的簡單服務(對「全端點需認證」的伺服器會恆判不就緒)。
- **`[players_query]`**(線上玩家數,R6):`kind`(rcon/rest)、`command`(rcon)或 `action_id`(rest);省略則 GUI 顯示「不適用」。
- **`[mods]`**(itzg 原生模組/模組包,R11):`owner`(如 `image-native`)、`plugin_dir`、`modpack_env`(如 `["MODRINTH","AUTO_CURSEFORGE"]`)、`manual_formats`、`manual_mount`、`modpack_loaders`(允許套用模組包的變體 loader;外掛平台如 paper 不列)。工具不解壓 `/data`,交 itzg 自管。

## 版本鎖定(已 pin,勿升級)

依賴刻意鎖定以避免 Windows/Docker 相容性回歸(見 `go.mod`、專案 `CLAUDE.md`):

- **Go 1.26+**、**Wails v2.13**(非 v3 alpha)。
- **Docker client `github.com/docker/docker` v27.5.1** — ⚠️ **`github.com/docker/go-connections` 必須維持 v0.5.0**,v0.7.0 會壞 Windows 編譯。
- **`gorilla/websocket` v1.5.3**、**`modernc.org/sqlite` v1.34.5**(純 Go 免 CGO)、**`github.com/zalando/go-keyring` v0.2.8**(Windows Credential Manager)、**`github.com/pelletier/go-toml/v2`**。

## Spike 結論索引

三個前置查證(結論落檔於 `specs/game-server-manager/spikes/`):

- **`palworld-spike.md`** — Palworld 官方 REST API(12 具名端點、Basic Auth)可支撐首版;RCON 官方標 deprecated 故列 legacy;鎖定映像 `thijsvanloef/palworld-server-docker:v2.5.1`。
- **`backup-spike.md`** — Windows/Docker Desktop(WSL2)下以 host bind mount 作實例資料目錄、停機後 host 端 tar+checksum 打包/還原的一致性可行,定案首版備份=停機快照 + bind mount。
- **`modpack-spike.md`** — itzg 支援矩陣/`AUTO_CURSEFORGE` env/CurseForge 條款查證;itzg 為 `/data` 唯一安裝擁有者,`CF_API_KEY` 由使用者自填。

## 規格文件

`specs/game-server-manager/`:`requirements.md`(需求)、`design.md`(設計)、`tasks.md`(任務)。
