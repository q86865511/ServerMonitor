# 範本撰寫指南

> 返回 [README](../README.zh-TW.md)｜[English README](../README.md)

新增「沿用既有 adapter」的遊戲 = 加一份 `templates/<id>.toml`,**不改核心**。內建範本自 2026-07-18 起以 go:embed 打進執行檔(啟動時抽出到資料目錄 `templates-builtin\`,單一 exe 自足);自訂範本放使用者目錄(`%LOCALAPPDATA%\ServerMonitor\templates\`),啟動時載入;缺欄位/版本/ID 重複/adapter 不存在會拒載並記 `TEMPLATE_LOAD_FAILED` 事件。schema 由 `internal/protocol/template.go` 定義,最小可跑範例見 `templates/minecraft.toml`、`templates/palworld.toml`。

## schema 欄位速查(對映 `GameTemplate`)

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

> R# 為需求編號,對映 `specs/game-server-manager/requirements.md`。
