# 範本撰寫指南

> 返回 [README](../README.zh-TW.md)｜[English README](../README.md)

新增「沿用既有 adapter」的遊戲 = 加一份 `templates/<id>.toml`,**不改核心**。內建範本自 2026-07-18 起以 go:embed 打進執行檔(啟動時抽出到資料目錄 `templates-builtin\`,單一 exe 自足);自訂範本放使用者目錄(`%LOCALAPPDATA%\ServerMonitor\templates\`),啟動時載入;缺欄位/版本/ID 重複/adapter 不存在會拒載並記 `TEMPLATE_LOAD_FAILED` 事件。schema 由 `internal/protocol/template.go` 定義,最小可跑範例見 `templates/minecraft.toml`、`templates/palworld.toml`。

## schema 欄位速查(對映 `GameTemplate`)

- **頂層**:`schema_version`(整數,目前 1)、`id`(唯一)、`name`(顯示名)、`icon`(選配,字串,範本目錄相對路徑圖示,如 `"icons/minecraft.png"`;經 AssetServer `/tpl-icons/{id}` 服務,R14)、`runtime`(必填字串,`"docker"` 或 `"native"`;範本宣告的「驗證用」預設後端——需為已註冊 adapter,且範本已宣告對應區段,否則拒載,如 `runtime = "docker"` 時 `[docker]` 須鎖 image/image_digest,`runtime = "native"` 時 `[native]` 須存在)、`data_dirs`(容器內資料目錄陣列,R9 備份範圍;docker 與 native 共用——native 模式下 `NativeBackend` 於實例根下依此逐一建子目錄,對齊 docker bind 慣例,見 `internal/agent/native.go` Create)。
  - 範本可同時宣告 `[docker]` 與 `[native]` 兩區段,兩種執行能力可並存(見下)。實際每實例採用哪個後端**不是**由頂層 `runtime` 欄位決定,而是由「能力 ∩ 平台」動態解析(`internal/core/runtime_policy.go` `resolveRuntime`):使用者可於建立時顯式指定 `docker`/`native`(需範本有對應區段,且 native 僅 Windows 可用);未指定時,Windows 且範本宣告 `[native]` → 優先選 native,否則若範本宣告 `[docker]` → 選 docker;Linux 一律僅能用 `[docker]`(native 需 Windows,非 Windows 平台請求 native 會被拒)。
- **`[docker]`**:`image`(鎖 tag,如 `itzg/minecraft-server:java21`)、`image_digest`(可選,填 `sha256:...` 做不可變鎖定,R11)。
- **`[[variants]]`**(可多筆):`id`、`loader`(loader 家族 paper/vanilla/forge/fabric/…,供模組包相容前置檢查)、`[variants.env]`(建立時透傳的容器 env,如 `TYPE = "PAPER"`)。
- **`[[ports]]`**(可多筆):`name`(供 host_port_ref/port_ref 參照)、`container`、`host_port`(0=動態)、`bind_ip`(wildcard `0.0.0.0` 與具體 IP 視為重疊)、`protocol`(tcp/udp)、`required`。衝突鍵 `(bind_ip, protocol, host_port)`(R2)。
- **`[[params]]`**(可多筆):`key`(即容器 env 變數名,itzg 慣例)、`label`、`type`(string/bool/int)、`default`、`required`(如 Minecraft `EULA` 未接受不得建立)。
- **`[[secrets]]`**(可多筆):`key`、`label`、`required`(布林,選填,預設 `false`;核心流程依賴時標 `true`,如 Minecraft `RCON_PASSWORD`、Palworld `ADMIN_PASSWORD`——stop hook 與健康探針皆依賴,無它核心流程不可用)。輸入即入 OS 金鑰庫、不落 DB/log(R12);runtime 明文注入容器 env 為明確例外。
- **`[[command_protocols]]`**(tagged union,R7):`protocol_id`、`kind`(`rcon`|`rest`)。rcon 用 `host_port_ref`+`password_ref`(raw console);rest 用 `host_port_ref`(目標埠參照,指向 [[ports]].name)、`password_ref`(Basic Auth 密碼的 secret 參照,指向 [[secrets]].key)、`auth`(如 `basic`)、`username`、`[[command_protocols.actions]]`(具名動作:`action_id`/`method`/`path`)。`legacy=true` 標記首版不啟用的協定(如 Palworld RCON)。
- **`[hooks]`**(生命週期映射,R3/R8/R9):`stop`/`announce`/`quiesce`/`resume`,各為 `{ protocol_id, command 或 action_id, args, message_key }`。rcon 用 `command`(可含 `{msg}`),rest 用 `action_id`+`args`,`message_key` 指定動態訊息寫入的 args 欄位。
- **`[health]`**(就緒/存活探針,R8):`kind`(tcp/rcon/rest/docker;未定義=容器 running 即就緒)、`port_ref`、`action_id`(可選,僅 kind=rest)。⚠️ tcp 探針撥的是 Docker 發布埠,其 userland proxy 會在容器程序尚未真正綁定前即接受連線→就緒「假陽性」提早;遊戲伺服器建議用 `rcon`/`rest`(協定就緒才算健康),tcp 僅適合無指令協定的簡單服務。kind=rest **建議搭配 `action_id`**(指向某具名 rest 動作,如 Palworld `info`)——探測經指令協定送出、帶該協定的 Basic Auth 認證;省略 `action_id` 則對 `port_ref` 根路徑發**未認證** GET 判 2xx,只適合無認證的簡單服務(對「全端點需認證」的伺服器會恆判不就緒)。
- **`[players_query]`**(線上玩家數,R6):`kind`(rcon/rest)、`command`(rcon)或 `action_id`(rest);省略則 GUI 顯示「不適用」。
- **`[mods]`**(itzg 原生模組/模組包,R11):`owner`(如 `image-native`)、`plugin_dir`、`modpack_env`(如 `["MODRINTH","AUTO_CURSEFORGE"]`)、`manual_formats`、`manual_mount`、`modpack_loaders`(允許套用模組包的變體 loader;外掛平台如 paper 不列)。工具不解壓 `/data`,交 itzg 自管。
- **`[native]`**(native-backend R3,選配區段;`internal/protocol/template.go` `NativeSpec`):存在即宣告此範本亦可以本機行程模式(免 Docker)建立,由 `SupportsNative()` 推導。三個子區段皆由 `internal/agent` 的 `NativeBackend`/`writeConfigFile` 於建立與啟動期解讀,驗證規則見 `internal/core/template_engine.go` `validateNative`。
  - **`[native.provision]`**:`kind`(`"java"` 或 `"steamcmd"`,必填,無對應安裝器會拒載)。
    - `kind = "java"`:`java_major`(整數,必填且須 >0,所需 Java 主版本,如 `21`;供應器實際依 `[[variants]].loader` 選 vanilla/paper/fabric/forge/neoforge 五種安裝器之一,見 `internal/agent/provision/provision.go` `InstallServerByLoader`)。範例(`templates/minecraft.toml`):
      ```toml
      [native.provision]
      kind = "java"
      java_major = 21
      ```
    - `kind = "steamcmd"`:`steam_app_id`(字串,必填,Steam App ID,如 Palworld `"2394010"`)、`update_on_start`(布林,選填,啟動前是否重跑 `app_update` 檢查更新,R6)。範例(`templates/palworld.toml`):
      ```toml
      [native.provision]
      kind = "steamcmd"
      steam_app_id = "2394010"
      update_on_start = true
      ```
  - **`[native.launch]`**:啟動命令模板(R7),必填至少一元素。
    - `command`(字串陣列,argv 模板,必填):支援 token `{java}`、`{server_jar}`、`{start_script}`、`{memory_mb}`、`{instance_dir}`、`{port:<name>}`(`<name>` 對映 `[[ports]].name`,展開為實際主機埠);未知 `{port:name}` 會回錯。forge/neoforge 無單一 server jar(安裝器產出 `run.bat`+`user_jvm_args.txt`),`NativeBackend` 於啟動期偵測到此情形會就地把 jar 型命令改寫為腳本啟動,範本本身仍以 jar 型 token 撰寫即可(見 `internal/agent/native.go` `rewriteForStartScript`)。
    - `working_dir`(字串,選配,相對實例根;空字串=實例根本身)。
    - 範例(jar 型,`templates/minecraft.toml`):
      ```toml
      [native.launch]
      working_dir = "data"
      command = ["{java}", "-Xmx{memory_mb}M", "-jar", "{server_jar}", "nogui"]
      ```
    - 範例(SteamCMD 型,`templates/palworld.toml`):
      ```toml
      [native.launch]
      command = ["{instance_dir}/PalServer.exe", "-port={port:game}"]
      ```
  - **`[[native.config]]`**(可多筆,native 無 itzg 代勞,遊戲設定檔由此區段產出):`file`(字串,必填,設定檔路徑,相對 `working_dir`;經路徑拘束,不得逃逸實例根)、`format`(必填,`"properties"` 逐行 k=v,或 `"palworld-ini"` 單行 `OptionSettings=(K=V,...)` 打包;僅此兩值,其餘拒載)、`section`(字串,僅 `palworld-ini` 用,ini section 名)、`[native.config.map]`(`paramKey`→`configKey`,只有存在於實例 env 的 `paramKey` 才落檔,未提供者交伺服器預設)、`[native.config.set]`(`configKey`→字面值或 `{port:<name>}` token 的固定/衍生值,同鍵時覆蓋 `map`,用於「native 執行必需但非使用者參數」的設定,如 Minecraft 的 `enable-rcon`/`rcon.port`)、`quote`(字串陣列,選填,列出需雙引號包裹值的 `configKey`;僅 `palworld-ini` 編碼器生效,`properties` 格式忽略此欄位)。
    - 範例(properties 型,`templates/minecraft.toml`):
      ```toml
      [[native.config]]
      file = "server.properties"
      format = "properties"
      [native.config.map]
      RCON_PASSWORD = "rcon.password"
      MOTD = "motd"
      MAX_PLAYERS = "max-players"
      [native.config.set]
      "enable-rcon" = "true"
      "rcon.port" = "{port:rcon}"
      "server-port" = "{port:game}"
      "query.port" = "{port:game}"
      ```
    - 範例(palworld-ini 型,含 `quote`,`templates/palworld.toml`):
      ```toml
      [[native.config]]
      file = "Pal/Saved/Config/WindowsServer/PalWorldSettings.ini"
      format = "palworld-ini"
      section = "/Script/Pal.PalGameWorldSettings"
      quote = ["ServerName", "AdminPassword"]
      [native.config.map]
      SERVER_NAME = "ServerName"
      ADMIN_PASSWORD = "AdminPassword"
      [native.config.set]
      RESTAPIEnabled = "True"
      RCONEnabled = "False"
      RESTAPIPort = "{port:rest}"
      ```
  - **`[native.mods]`**(選配,R11):`mods_dir`(字串,相對實例根的模組落位目錄,如 `"data/mods"`;未宣告時 native 模組包安裝會回明確錯誤,不臆測預設值)。範例(`templates/minecraft.toml`):
      ```toml
      [native.mods]
      mods_dir = "data/mods"
      ```

> R# 為需求編號,對映 `specs/game-server-manager/requirements.md`。
