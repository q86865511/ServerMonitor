# Palworld Spike(T6 — T9 `PalworldRestAdapter` 前置 gate)

> 目的:查證 Palworld 專用伺服器官方 REST API 與 RCON 現況,鎖定可用 Docker 映像版本,供 T7 範本與 T9 指令 adapter 據以實作。
> 產出日期:2026-07-13。查證方式:官方文件 + 官方社群映像 repo/README + GitHub Releases API。
> **標記慣例**:【事實】= 有官方/一手來源;【推論】= 由來源合理推導、未逐字查證;【待查】= 需 T9 實跑確認。

---

## 結論摘要(≤5 行)

1. **REST 可用**:官方 REST API 存在且穩定,共 **12 個具名端點**(4 個 GET 讀取 + 8 個 POST 動作),涵蓋伺服器資訊/指標/玩家/設定/公告/存檔/踢/封/解封/優雅關機/強制停止。認證為 HTTP Basic Auth(`admin` / AdminPassword)。
2. **足以支撐首版**:REST 涵蓋 T7/T9 首版所需全部具名動作(stop、announce、players、save、kick/ban),**REST 為主完全可行**。
3. **RCON 應標 legacy**:官方將於 1.0 起停止支援 RCON;社群主流映像自 **2.1.0 起把 RCON 標為 deprecated 且預設關閉、預設改用 REST**。→「首版 RCON 標 legacy、不列驗收」的決策**合理、有依據**。
4. **鎖定映像**:`thijsvanloef/palworld-server-docker:2.5.1`(2026-07-10 發布);REST 8212/tcp、遊戲 8211/udp、資料卷 `/palworld/`;env `REST_API_ENABLED=true`、`RCON_ENABLED=false`。
5. **待再查證**:`game-data` 端點方法、shutdown/kick 的 body 欄位精確拼寫與 `userid` 格式、映像 digest 落定 — 見文末清單。

---

## 1. 官方 REST API

### 1.1 啟用方式【事實】

在 `PalWorldSettings.ini`(`[/Script/Pal.PalGameWorldSettings]` 區段)設定:

| 設定鍵 | 值 | 說明 |
|---|---|---|
| `RESTAPIEnabled` | `True` | 啟用 REST API(預設 False) |
| `RESTAPIPort` | `8212` | REST 監聽埠(預設 8212) |
| `AdminPassword` | `"<強密碼>"` | 同時作為 REST Basic Auth 的密碼;啟用 API 前務必設強密碼 |

設定後需重啟伺服器。官方明確警告:**REST API 不設計為直接暴露公網**(可能被未授權操控)→ 我方應綁 `127.0.0.1`,由 core/agent 內部呼叫。

- **Base URL**:`http://<host>:8212/v1/api`【事實】
- **認證**:HTTP Basic Auth,username 固定為 `admin`,password = `AdminPassword` 的值【事實】
- **回應碼**:200 成功 / 400 bad request / 401 unauthorized【事實】

### 1.2 具名動作表【事實(方法與 body 來自官方文件 + XGamingServer 參考)】

> 路徑均相對於 Base URL `/v1/api`。body 為 JSON;GET 無 body。

| name | method | path | 認證 | 用途 | 請求 body(JSON) |
|---|---|---|---|---|---|
| info | GET | `/v1/api/info` | Basic | 伺服器名稱/版本/描述 | — |
| metrics | GET | `/v1/api/metrics` | Basic | FPS、線上人數、uptime、frametime | — |
| players | GET | `/v1/api/players` | Basic | 線上玩家(name、PlayerUID、SteamID、level、location) | — |
| settings | GET | `/v1/api/settings` | Basic | 當前伺服器設定(唯讀) | — |
| announce | POST | `/v1/api/announce` | Basic | 全服公告 | `{"message": "text"}` |
| kick | POST | `/v1/api/kick` | Basic | 踢出玩家 | `{"userid": "id", "message": "reason"}` |
| ban | POST | `/v1/api/ban` | Basic | 封鎖玩家 | `{"userid": "id", "message": "reason"}` |
| unban | POST | `/v1/api/unban` | Basic | 解除封鎖 | `{"userid": "id"}` |
| save | POST | `/v1/api/save` | Basic | 存檔世界 | 無 |
| shutdown | POST | `/v1/api/shutdown` | Basic | 倒數關機(附公告) | `{"waittime": 60, "message": "text"}` |
| stop | POST | `/v1/api/stop` | Basic | 立即強制停止 | 無 |
| game-data | GET【推論】 | `/v1/api/game-data`【推論】 | Basic | world actor 快照 | — |

**要求對應(T7/T9 需覆蓋的動作)全部滿足**:伺服器資訊=`info`、線上玩家=`players`、公告=`announce`、踢人=`kick`、封鎖=`ban`、存檔=`save`、關機=`shutdown`(優雅)/`stop`(強制)。

**補充【推論/待查】**:
- `game-data` 端點見於官方文件導覽,但其 HTTP 方法與確切路徑我未逐頁查證(依 nav 命名規律推為 GET `/v1/api/game-data`)。首版不需要,列待查。
- `kick/ban` 的 `userid` 於 Palworld 常為 `steam_<SteamID64>` 或 PlayerUID 形式;精確接受格式待 T9 實跑確認。
- `shutdown` body 欄位名文件寫 `waittime`(秒);拼寫以實跑為準(見待查清單)。

---

## 2. RCON 現況

**結論:RCON 已被官方與社群映像雙雙標為過時,首版標 `legacy=true`、不列驗收,決策合理。**

- **官方層**【事實(來自維護者官方 1.0 launch thread)】:「Palworld Servers will stop supporting RCON」— 官方將於 1.0 起停止支援 RCON,並引導改用 REST API。
- **社群主流映像層**(`thijsvanloef/palworld-server-docker`)【事實】:
  - 「RCON has been deprecated in this image since version **2.1.0**」— 自 2.1.0 起在該映像標為 deprecated。
  - 建議設定:`REST_API_ENABLED=true`、`RCON_ENABLED=false`;映像預設即 `RCON_ENABLED=false` / `REST_API_ENABLED=true`。
  - 官方遷移指引:使用最新映像 + 最新 compose/.env 時,**無需手動遷移**即從 RCON 轉 REST。
- **一致性註記**:對該映像 `README.md`(main 分支)做全文擷取時,擷取模型回報「未見 deprecation 字樣」;但官方 issue #834(維護者親撰的 1.0 launch thread)與 Docker Hub 說明均明列自 2.1.0 deprecate。→ 以 issue #834 為權威,判定 **RCON 確已 deprecated**;README 擷取為工具未掃到,非反證。

→ **對我方決策的影響**:REST 為主、RCON 僅保留 `legacy=true` 佔位(不啟用、不列首版驗收)是正確方向;RCON 未來可能被遊戲端整個移除,不宜投資。

---

## 3. 鎖定映像【事實】

**映像**:`thijsvanloef/palworld-server-docker`(GitHub 星數高、維護活躍的社群主流映像)

**鎖定版本**:`thijsvanloef/palworld-server-docker:2.5.1`
- 來源:GitHub Releases API,`tag_name = "2.5.1"`,發布於 **2026-07-10 16:19 UTC**(查證當下最新穩定版)。
- 採 semver(x.y.z)+ `latest` 雙軌。**務必鎖 semver tag(`2.5.1`),勿用 `latest`**(避免不可重現建置)。
- **digest 落定**【待查/建議】:本 spike 無 Docker 環境,未取實際 digest。建議 T7/T9 在建置時執行
  `docker buildx imagetools inspect thijsvanloef/palworld-server-docker:2.5.1` 取 `sha256:...` digest 寫入範本以做真正的不可變鎖定。

**埠**(容器內)【事實】:

| 埠 | 協定 | 用途 | 備註 |
|---|---|---|---|
| 8211 | UDP | 遊戲埠 | 必要,對外 |
| 27015 | UDP | Query(Steam) | 對外可選 |
| 8212 | TCP | REST API | **不可 port-forward**;我方綁 127.0.0.1 |
| 25575 | TCP | RCON | legacy,首版不開 |

**關鍵環境變數**【事實】:

| env | 預設 | 用途 |
|---|---|---|
| `REST_API_ENABLED` | `true` | 啟用 REST API |
| `REST_API_PORT` | `8212` | REST 埠 |
| `RCON_ENABLED` | `false` | RCON(legacy,維持關閉) |
| `RCON_PORT` | `25575` | RCON 埠 |
| `ADMIN_PASSWORD` | (無預設,必填) | 管理密碼;同時是 REST Basic Auth 的 password |
| `SERVER_NAME` / `PLAYERS` / `PORT` 等 | — | 一般伺服器設定(非本 spike 重點) |

**資料/存檔目錄(容器內)**:`/palworld/`【事實】(bind mount / volume 掛此路徑;對應 R9 備份根)。

---

## 4. 建議的 Palworld 範本 `command_protocols` 片段【推論 — 設計提案】

> 事實部分(端點 method/path/body、埠、env、資料路徑)如上;以下 **TOML 形狀是對齊本專案 design schema 的設計提案**,供 T7/T9 直接採用或微調。
> 對齊依據:`design.md` L97–122 的 Minecraft 範本結構與 L122 對 Palworld 的描述(`kind="rest"` 含 `actions[]`、`auth="basic"`、`password_ref`;另一組 `kind="rcon"; legacy=true`)。

```toml
# ── 埠(對齊 design R2 衝突鍵) ──────────────────────────────
[[ports]]
name = "game"
container = 8211
host_port = 8211
bind_ip = "0.0.0.0"
protocol = "udp"
required = true

[[ports]]
name = "query"
container = 27015
host_port = 27015
bind_ip = "0.0.0.0"
protocol = "udp"

[[ports]]
name = "rest"
container = 8212
host_port = 8212
bind_ip = "127.0.0.1"          # 官方警告:REST 不可暴露公網
protocol = "tcp"

[[ports]]
name = "rcon"
container = 25575
host_port = 25575
bind_ip = "127.0.0.1"
protocol = "tcp"               # legacy,首版不啟用

# ── secret(對齊 R12 SecretRef,不進 params_json) ───────────
[[secrets]]
key = "ADMIN_PASSWORD"          # 同時是 REST Basic Auth 的 password
label = "Admin 密碼(含 REST API 認證)"

# ── 指令協定:REST(主) ─────────────────────────────────
[[command_protocols]]
protocol_id = "palworld-rest"
kind = "rest"
host_port_ref = "rest"
base_path = "/v1/api"
auth = "basic"
username = "admin"              # Palworld REST 固定帳號
password_ref = "ADMIN_PASSWORD" # 指向 [[secrets]]
# actions[]:name 為具名動作;body 內 {占位} 由 adapter 以參數填入
actions = [
  { name = "info",     method = "GET",  path = "/info" },
  { name = "metrics",  method = "GET",  path = "/metrics" },
  { name = "players",  method = "GET",  path = "/players" },
  { name = "settings", method = "GET",  path = "/settings" },
  { name = "announce", method = "POST", path = "/announce", body = '{"message": "{msg}"}' },
  { name = "save",     method = "POST", path = "/save" },
  { name = "shutdown", method = "POST", path = "/shutdown", body = '{"waittime": {seconds}, "message": "{msg}"}' },
  { name = "stop",     method = "POST", path = "/stop" },
  { name = "kick",     method = "POST", path = "/kick",  body = '{"userid": "{userid}", "message": "{msg}"}' },
  { name = "ban",      method = "POST", path = "/ban",   body = '{"userid": "{userid}", "message": "{msg}"}' },
  { name = "unban",    method = "POST", path = "/unban", body = '{"userid": "{userid}"}' },
]

# ── 指令協定:RCON(legacy,首版不啟用、不列驗收) ──────────
[[command_protocols]]
protocol_id = "palworld-rcon"
kind = "rcon"
legacy = true
host_port_ref = "rcon"
password_ref = "ADMIN_PASSWORD"

# ── 生命週期 hooks(對齊 design R3/R8/R9) ──────────────────
# 注意:rest hook 以 action 名 + args 引用(rcon 版是 command 字串),
#       此為對 Minecraft 範本 hook 形狀的延伸,建議 T7 於 schema 正式化。
[hooks]
stop     = { protocol_id = "palworld-rest", action = "shutdown", args = { seconds = 30, msg = "Server shutting down" } }
announce = { protocol_id = "palworld-rest", action = "announce", args = { msg = "{msg}" } }

# ── 健康探針(對齊 R8,kind=rest) ──────────────────────────
[health]
kind = "rest"
port_ref = "rest"
action = "info"                 # 200 = healthy

# ── 線上玩家查詢(對齊 R6) ────────────────────────────────
[players_query]
kind = "rest"
action = "players"
```

**給 T7/T9 的落地備註**:
- `hooks.stop` 用 `shutdown`(附 `waittime` 倒數 + 公告)而非 `stop`(立即強制),符合 design R3「優雅停機 + grace」語意;`stop` 動作保留給硬停/逾時 fallback。
- REST adapter 需在 body 模板做參數注入時對 `{msg}` 等做 JSON 字串跳脫(避免注入破壞 JSON)。
- Basic Auth header:`Authorization: Basic base64("admin:" + ADMIN_PASSWORD)`。

---

## 5. 待再查證清單

| # | 項目 | 現狀 | 建議查證方式(T9) |
|---|---|---|---|
| 1 | `game-data` 端點的 HTTP 方法與確切路徑 | 【推論】GET `/v1/api/game-data` | 逐頁看官方 docs 或實跑 curl;首版非必要 |
| 2 | `shutdown` body 欄位拼寫(`waittime` vs `waitTime`)與型別 | 文件寫 `waittime`(整數秒) | 對真伺服器 POST 實測回應 |
| 3 | `kick/ban` 的 `userid` 接受格式(`steam_<id>` / PlayerUID / SteamID64) | 【待查】 | 由 `players` 回傳欄位比對後實測 |
| 4 | 映像 `2.5.1` 的實際 digest(sha256) | 未取(無 Docker 環境) | `docker buildx imagetools inspect ...:2.5.1` 落定後寫入範本 |
| 5 | `metrics`/`info`/`players` 回應 JSON 精確 schema(欄位名) | 概述已知,精確欄位未逐一查證 | 實跑取樣後寫入 adapter 解析結構 |
| 6 | 官方 RCON 是否已在特定遊戲版本「完全移除」(非僅 deprecated) | 【事實】官方宣告將停止支援;移除時點未逐版查證 | 維持 legacy 佔位即可,無需追版 |

---

## 來源

- 官方 REST API 文件(端點清單、Basic Auth、啟用設定):https://docs.palworldgame.com/api/rest-api/palwold-rest-api/
- 官方設定文件(RESTAPIEnabled/RESTAPIPort/AdminPassword):https://docs.palworldgame.com/settings-and-operation/configuration/
- XGamingServer REST API 參考(方法 GET/POST + body 欄位):https://xgamingserver.com/docs/palworld/rest-api
- 社群主流映像 repo/README(埠、env、資料路徑):https://github.com/thijsvanloef/palworld-server-docker/blob/main/README.md
- RCON deprecation(維護者 1.0 launch thread,自 2.1.0 deprecate):https://github.com/thijsvanloef/palworld-server-docker/issues/834
- 最新版本鎖定(`tag_name=2.5.1`,2026-07-10):https://api.github.com/repos/thijsvanloef/palworld-server-docker/releases/latest
- Docker Hub(映像頁):https://hub.docker.com/r/thijsvanloef/palworld-server-docker
