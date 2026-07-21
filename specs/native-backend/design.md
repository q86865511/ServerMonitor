# native-backend — 技術設計（design.md）

> 建立日期：2026-07-16｜狀態：已核可（2026-07-16）;實作完成 2026-07(任務全數收官,見 PROGRESS.md)
> 對照 requirements.md 撰寫；每條 R# 都必須出現在需求對應表。

## 架構概述

新增 `NativeBackend`（`agent.RuntimeBackend` 第二實作，internal\agent\backend.go:18）以本機子行程執行伺服器，並在 agent 端引入 `dispatchBackend`（同介面的組合實作）把 docker 與 native 兩後端併入單一節點代理：`Create` 依 `InstanceSpec.Runtime` 路由，其餘依 RuntimeID 前綴（`native:` 為 native、無前綴為既有 docker）路由，`List`/`Events` 聚合兩者且可個別降級。供應子系統（JRE、Minecraft 各 loader 安裝器、SteamCMD、Modrinth/CurseForge 模組）全部落在 agent 側（檔案系統擁有者），於 `NativeBackend.Create` 內同步執行、以事件串流回報進度、失敗走既有 Create journal 回滾（instance_service.go:222-233）。範本新增 `[native]` 區段宣告供應/啟動/設定映射；`runtime` 欄位改作「預設 runtime」，`[docker]`/`[native]` 區段的存在與否決定支援能力。核心的 runtime 選擇鏈打通：範本能力 → 平台策略（Windows 預設 native、Linux 禁 native）→ `InstanceSpec.Runtime` → agent 分派。Windows 資源上限與行程樹統計由 Job Objects 承擔。

## 需求對應表

| 需求 | 設計元素 | 說明 |
|---|---|---|
| R1 本機行程後端 | `agent.NativeBackend`（新）實作 `RuntimeBackend` 13 方法；`procSupervisor`（PID/Job/log）；共用既有 `eventHub`（event_hub.go:20）與 tar 邏輯（docker_backup.go） | Create 建工作目錄+供應+寫啟動配置不啟動；Stop 依 `opts.Grace`（backend.go:54）優雅等待後強殺行程樹；Remove 停行程清中繼、保資料；通過與 Mock/Docker 相同的 backend 契約測試 |
| R2 逐實例 runtime 分派 | `InstanceSpec.Runtime`（新欄位，dto.go:59）；`dispatchBackend`（新）；`buildSpec` 填入（instance_service.go:557）；平台策略 `runtimePolicy` | buildSpec 由範本+平台決定 runtime 寫入 spec；dispatcher 對 docker/native 路由，未知值回 `ERR_BAD_REQUEST`；Linux 上 native 於 Create 驗證擋下；既有 docker 實例 RuntimeID 無前綴、零遷移 |
| R3 範本 native 區段 | `protocol.NativeSpec`（新，GameTemplate 加 `Native *NativeSpec`）；`AdapterRegistry.runtimes` 加 `native`（template_engine.go:32）；`validate` 擴充（template_engine.go:150-164） | `[native]` 宣告 provision（java/steamcmd）+ launch（命令模板）+ config（params→server.properties / Palworld ini）；runtime 允許值擴為 docker/native；至少一區段存在，缺對應區段報明確欄位路徑 |
| R4 JRE 自動供應 | `provision.JavaProvisioner`（新，internal\agent\provision）；快取 `<CacheRoot>/jre/<major>/` | Adoptium API 下載對應 major、sha256 校驗、解壓；快取命中不重下、多實例共用；校驗失敗中止不留半成品；離線無快取回「需要網路下載 Java <版本>」 |
| R5 Minecraft 安裝器 | `provision.ServerInstaller` 介面 + Vanilla/Paper/Fabric/Forge/NeoForge 五實作；依 `Variant.Loader` 選取 | Vanilla=Mojang manifest+sha1；Paper=PaperMC API+sha256；Fabric/Forge/NeoForge=官方 installer 產啟動配置；進度走 `eventHub`；失敗回滾；eula 參數 true 寫 eula.txt |
> R5 事實加註（實作校正）：Paper 安裝實作採 PaperMC **v3 Fill API**（`https://fill.papermc.io/v3/projects/paper/versions/{ver}/builds`），非本表撰寫時的 v2（`api.papermc.io/v2`）；v2 已 sunset（2026-07 實查),故實作直接落 v3。<br>R5 Forge/NeoForge 事實加註：LaunchSpec 為範本級（非變體級），forge/neoforge 變體與 jar 型變體共用同一啟動命令；NativeBackend 於啟動期偵測「啟動來源為 installer 產出的 run.bat（無單一 server jar）」時就地改寫為腳本啟動、記憶體上限注入 `user_jvm_args.txt`（見 `internal/agent/native.go rewriteForStartScript`）。
| R6 SteamCMD/Palworld | `provision.SteamCMDProvisioner`（新）；快取 `<CacheRoot>/steamcmd/`；`NativeSpec.Provision.SteamAppID` | Valve 匿名載點下載 SteamCMD（共用快取）；`app_update 2394010`；進度回報+失敗回滾；`update_on_start` 於 `Start` 前重跑 app_update |
| R7 日誌落檔與收養 | `procSupervisor`：`proc.json`（PID+start-time）、`server.log` 滾動、`Logs()` tail | Logs(follow) 由檔案 tail，語意對等 Docker（WS fanout 不改）；agent 重啟收養存活 PID→running、已死→合成 die 走崩潰復原；滾動不中斷串流、上限預設 64 MiB×5 |
| R8 監控統計與事件 | `procSupervisor` 經 Job Objects 取行程樹 CPU/記憶體填 `ResourceStats`（dto.go:95）；行程退出合成 `RuntimeEventDie`（dto.go:128） | Stats 涵蓋整棵行程樹（java/steamcmd 子行程）；CPUPercent 語意同 Docker（占可用核心%，docker_monitor.go:117）；非預期退出觸發既有自動重啟狀態機（lifecycle.go:404 handleDie） |
| R9 Job Objects 資源上限 | `jobObject`（新，Windows；`golang.org/x/sys/windows`）：記憶體+CPU rate control | 記憶體超限→Job 終止+合成 `RuntimeEventOOM`（dto.go:130，區別 crash）；CPU rate control 強制；收養後重 `AssignProcessToJobObject`；**不設** `KILL_ON_JOB_CLOSE`（agent 退出不連坐，與 R7 共存） |
| R10 備份/還原沿用 | 抽出共用 `tarInstanceData`/checksum（docker_backup.go:35-70）供 native 復用；備份格式（data.tar+meta.json，docker_backup.go:24）互通 | native Archive/Restore 走同一停機一致 tar 快照；checksum 驗證；同實例資料可跨 docker/native 經備份互轉（同範本、同 data_dirs） |
| R11 Modrinth native 安裝 | `ModInstaller` 介面二實作：`itzgEnvInstaller`（docker，現 modpack.go:158 邏輯，gate 於 `runtime==docker`）、`provision.ModrinthInstaller`（native，agent 側）；`InstanceSpec.Modpack *ModpackRef`（新） | owner 由「image-native 寫死」重構為依 runtime 分派；itzg env 映射（modpack.go:164-178）只在 docker 生效；native 解析 mrpack 落位 instance mods 目錄；手動上傳沿用 Mounts（dto.go:67） |
| R12 GUI runtime 選擇/進度 | Wails 綁定新增 `DockerAvailable()`；`TemplateDTO` 加 `runtimes` 能力清單、`CreateInstanceRequest` 加 `runtime`；Svelte 建立表單 runtime 選擇＋供應進度條（訂閱 events WS 的 provision 進度） | 乾淨 Windows 全流程可用無外部安裝提示；進度含階段/百分比、失敗指出供應步驟；Docker 不可用時 docker 選項置灰＋原因 |
| R13 對帳納入 native | `Reconciler`（reconciler.go）沿用不改——經聚合 `dispatchBackend.List` 取 native `gsm.*` 標籤 refs；`NativeBackend` 建構時的收養掃描（R7） | DB running 但 PID 死→List 反映 stopped→修正狀態記事件；PID 活 DB stopped→依既有規則；dispatcher.List 對 docker 子後端錯誤軟降級，native 對帳不受 Docker daemon 影響 |
| R14 CurseForge（可延後） | `provision.CurseForgeInstaller`（實作與 Modrinth 同一 `ModProvider` 擴充點）；key 以 build-time `-ldflags -X` 注入＋app 設定可覆蓋；GUI BlockedMods 對話框＋監看資料夾 hash 匹配自動匯入 | 無 key 建置：CF 選項隱藏/置灰；有 key：官方 API 解析 manifest 下載；opted-out（downloadUrl=null）→列被擋模組、引導瀏覽器手動下載、監看匯入後續裝；文件揭露 key 專屬條款 |

## 介面與資料模型

### InstanceSpec.Runtime 與 Modpack（dto.go:59，向後相容）

```go
type InstanceSpec struct {
    // ... 既有欄位不動 ...
    Runtime string      `json:"runtime,omitempty"` // "docker" | "native"；空=docker（相容舊呼叫端）
    Modpack *ModpackRef `json:"modpack,omitempty"`  // R11/R14 遠端模組包來源；手動檔仍走 Mounts
}
type ModpackRef struct {
    Type string `json:"type"` // "modrinth"｜"curseforge"（R14）
    Ref  string `json:"ref"`  // slug / project id / URL
}
```
- 相容性：新增 `omitempty` 可選欄位，`AgentAPIVersion` 維持 `v1`（dto.go:179）。既有 docker 實例 RuntimeID 為裸容器 ID（無前綴），dispatcher 以「無 `native:` 前綴 → docker」路由，不需 DB 遷移（核心以 stored RuntimeID join 事件，lifecycle.go:585 uuidForRuntime；dispatcher 為 ID 一致性的唯一邊界）。

### Wails bindings 相容性（app.go）

方法名與既有簽章全部不動（app.go:218-512 共 27 個綁定方法）；只做加法：
- `CreateInstanceRequest`（app.go:136）加可選 `Runtime string`；`ModpackRequest.Type` 允許值擴充。
- `TemplateDTO`（app.go:73）既有 `Runtime` 欄位語意改「預設 runtime」，另加 `Runtimes []string` 能力清單。
- `InstanceDTO`（app.go:115）加 `Runtime string` 供列表顯示。
- `NodeStatusDTO`（app.go:209）加 `DockerAvailable bool`（節點在線與 Docker 能力分離）。
- 新增方法 `DockerAvailable() bool`；`RetryDocker()`（app.go:512）簽章不變、內部改為只重建 docker 子後端。
前端 wailsjs models 重新生成，舊呼叫全部照常有效。

### 範本 [native] schema（template.go）

```go
type GameTemplate struct { /* ... */ Native *NativeSpec `toml:"native"` }

type NativeSpec struct {
    Provision ProvisionSpec   `toml:"provision"`
    Launch    LaunchSpec      `toml:"launch"`
    Config    []ConfigMapping `toml:"config"`
    Mods      *NativeModsSpec `toml:"mods"` // native 模組落位目錄等（可選）
}
type ProvisionSpec struct {
    Kind          string `toml:"kind"`            // "java" | "steamcmd"
    JavaMajor     int    `toml:"java_major"`      // kind=java（如 21）
    SteamAppID    string `toml:"steam_app_id"`    // kind=steamcmd（如 "2394010"）
    UpdateOnStart bool   `toml:"update_on_start"` // R6 啟動前檢查更新
}
type LaunchSpec struct {
    Command    []string `toml:"command"`     // argv 模板；token: {java} {server_jar} {memory_mb} {instance_dir} {port:<name>}
    WorkingDir string   `toml:"working_dir"` // 相對 instance 資料根
}
type ConfigMapping struct {
    File    string            `toml:"file"`    // "server.properties" | "PalWorldSettings.ini"
    Format  string            `toml:"format"`  // "properties" | "palworld-ini"
    Section string            `toml:"section"` // palworld-ini 用
    Map     map[string]string `toml:"map"`     // paramKey -> configKey
}
```
- `Format` 用具名編碼器：`properties`（k=v 逐行）與 `palworld-ini`（單行 `OptionSettings=(K=V,...)` 打包）。不假裝單一泛型映射能涵蓋兩者。
- `runtime` 欄位改作**預設 runtime**；能力由區段存在推導（`SupportsDocker = Docker != nil`、`SupportsNative = Native != nil`）。既有範本語意不變。

### dispatchBackend（package agent）

```go
type dispatchBackend struct {
    native   RuntimeBackend // Windows 恆有；Linux 為 nil
    docker   RuntimeBackend // 可能 nil（daemon 不可用）
    dockerMu sync.RWMutex   // RetryDocker 替換保護
    hub      *eventHub      // 聚合事件（復用 event_hub.go）
}
const nativePrefix = "native:"
```
- 路由：`Create` 依 `spec.Runtime`；其餘依 RuntimeID 前綴（native 剝/加前綴轉呼子後端；docker 直穿）。`List` 聚合並對 native refs 加前綴，docker 子後端錯誤軟降級（回 native refs＋記錄，不整體失敗）。
- `Events`：dispatcher 自持 `eventHub`，背景 pump 訂閱兩子後端、轉 emit（native 加前綴）→ 核心見單一 cursor 空間，reconnect/Resync 語意不變。
- 能力/降級：`DockerAvailable() bool`、`RetryDocker() error`（只重建 docker 子後端；native 恆在，節點不因 Docker down 離線）。

### agent.BackendOptions（泛化注入，bootstrap.go:245）

```go
type BackendOptions struct { InstancesRoot, BackupRoot, CacheRoot, Node string }
type BackendFactory func(BackendOptions) (RuntimeBackend, error) // bootstrap.go:28 簽章泛化
```
- `defaultBackendFactory`（bootstrap.go:31）改為：建 NativeBackend（Windows 恆成功）＋盡力建 DockerBackend（失敗則 docker=nil），回 dispatchBackend。Linux：native=nil 僅 docker（沿用既有離線流程 bootstrap.go:247-255）。`CacheRoot = <dataRoot>/cache`（新目錄骨架）。

### agent.NativeBackend + procSupervisor

- **Create（不啟動）**：建 `<InstancesRoot>/<uuid>/`（沿用 DataDirs 慣例）→ 依 `[native].Provision` 供應（JRE/MC 安裝器/SteamCMD，進度 emit hub）→ 依 `[native].Config` 由 `spec.Env` 寫設定檔 → 安裝模組包（`spec.Modpack` 或手動 Mounts）→ 寫 `spec.json` 快照（同 docker.go:198）→ 寫 launch 配置。失敗回錯 → 核心既有 defer 回滾呼 `Remove` 清目錄；共用快取（JRE/SteamCMD）不回滾（冪等、校驗通過才入快取）。回 `RuntimeID="native:"+uuid`。
- **Start**：（UpdateOnStart 先 app_update）展開 Launch token → Job Object 內起行程 → stdout/stderr 導滾動 log → 寫 `proc.json` → emit start。
- **Stop**：呼叫端先送 hooks.stop（GameCommandAdapter，沿用不靠 stdin）→ 等 Grace → 逾時強殺整棵行程樹（TerminateJobObject）。
- **Status/List/Inspect**：`proc.json`＋PID 存活（＋start-time 防 PID 重用）合成；List 回帶 `gsm.*` 標籤 refs（供 R13）。
- **Stats**：Job accounting 取行程樹 CPU 累計（delta/牆鐘/NumCPU 對齊 Docker 正規化語意）＋列舉 Job 行程 WorkingSetSize 加總；DataDiskBytes 復用 dataDiskUsage（docker_monitor.go:145）。
- **Archive/Restore**：復用抽出的 tar/checksum（R10）。
- **Events**：行程退出 emit `die`（帶 exitCode）；Job 記憶體終止 emit `oom`。

### provision 子套件（internal\agent\provision）

- `Provisioner` 聚合 `JavaProvisioner`、`ServerInstaller` 五實作、`SteamCMDProvisioner`、`ModProvider`（Modrinth 首發、CurseForge R14）。各方法收 `progress func(ProvisionProgress)` 回呼（NativeBackend 轉 hub.emit），與 agent 內部型別解耦、可 httptest 獨立單元測。
- 純下載/校驗/解壓/跑 installer；不碰 Job/PID（supervisor 職責）。

### 事件擴充（dto.go:123）

- 新增 `RuntimeEventProvision`（供應進度）：`RuntimeEvent` 加可選 `Progress *ProvisionProgress`（stage/percent/detail）。可選欄位，既有 die/health/oom/resync 不變。

## 關鍵流程

**建立 native 實例（R2/R3/R4/R5/R11）**
1. 核心 `Create`（instance_service.go:185）：既有前置檢查（變體/參數/機密/模組包相容）；`runtimePolicy` 決定 runtime（範本能力 ∩ 平台；Windows 預設 native）。
2. 機密入庫→埠預留→寫 create journal（不變）。
3. `buildSpec` 填 `Runtime` 與 `Modpack`；docker 路徑才 `applyModpackEnv`（gate `runtime==docker`）。
4. `NodeClient.Create` → dispatcher → `NativeBackend.Create`：供應逐步 emit 進度（GUI 訂閱顯示）。
5. 任一步失敗 → 核心 defer 回滾（移除 runtime/釋放埠/刪機密/清 journal）。成功寫 DB、清 journal。

**啟動與監督（R7/R8/R9）**
1. `Start`：建 Job Object（記憶體+CPU 上限，與 docker 同設定來源）→ 起行程 AssignProcessToJobObject → log 落檔 → proc.json → emit start。
2. supervisor 監看退出：emit `die`（走既有自動重啟 lifecycle.go:404）；Job 記憶體終止 emit `oom`（告警標「超出記憶體上限」）。
3. Stats 週期取樣（沿用 agent WS 推送）。

**agent 重啟收養（R7/R13）**
1. NativeBackend 建構掃 `<InstancesRoot>/*/proc.json`：PID 活且 start-time 符 → 收養（重掛 Job、接管 log tail、running）；PID 死 → 合成 die 走崩潰復原。
2. 核心 Reconciler（不改）經聚合 List 對帳。

**CurseForge 模組包（R14，有 key 建置才啟用）**
1. GUI 選 CF modpack → `ModpackRef{Type:"curseforge"}` 透傳 → `CurseForgeInstaller` 經官方 API 解析 manifest。
2. 逐檔下載；`downloadUrl=null`（opted-out）者收集為 BlockedMods 清單 → 供應暫停、事件回報 GUI。
3. GUI 列被擋模組＋官網連結，使用者以瀏覽器手動下載；系統監看下載/匯入資料夾，檔名＋hash 匹配後自動複製進實例 → 續裝完成。

## 取捨與替代方案

- **採用 dispatchBackend（組合實作 RuntimeBackend），agent.Server 不動**：路由/聚合集中一型，復用 eventHub、resolve（server.go:172）不變。捨棄改 agent.Server 持 map[runtime]Backend：侵入良測試的薄層，churn 大。
- **採用 native RuntimeID 前綴、docker 裸穿**：既有 docker 實例零遷移。捨棄全前綴（需 DB 遷移）與 in-memory 映射（有狀態，違背薄層精神）。
- **供應落 agent 側、於 Create 內同步執行**：agent 是檔案系統擁有者（四層架構約定），對齊 Docker Create 內 ensureImage 阻塞拉映像的先例（docker.go:194）；進度走事件、失敗走 journal 回滾。捨棄核心側供應（破壞多節點抽象）與 Start 時供應（R5 明定建立時＋journal 為 Create 機制）。
- **Job Objects 同時做強制與行程樹統計**：一套解決 R8+R9，語意對齊 Docker。捨棄 gopsutil：與 Job Objects 重疊、多一個新模組相依（違反版本鎖定精神）。
- **config 具名編碼器（properties / palworld-ini）**：誠實面對兩種格式。捨棄單一泛型 k=v（無法表達 Palworld 巢狀打包值）。
- **owner 依 runtime 分派（ModInstaller 二實作）**：itzg env（docker）與 app-managed（native）並存。捨棄 owner 字串硬編。
- **CurseForge key 以 build-time 注入＋設定覆蓋**：比照 Prism（key 專屬本專案、衍生作品須換 key、使用者可覆蓋）；捨棄 CDN 直連與社群代理（不合規/不可靠，見 requirements Q1）。

## 風險

- **長時供應 vs HTTP 逾時**：Palworld SteamCMD 6-8GB、Forge installer 分鐘級，同步 Create 可能撞 NodeClient 逾時。緩解：native 建立路徑用寬裕/無讀逾時的 client、供應事件當心跳。
- **Job Object 收養再納管**：依賴 Win8+ 巢狀 Job；行程已在他人 Job 內可能失敗。緩解：失敗降級為「僅監督不強制上限」＋記可辨識事件，不阻斷收養（收養優先）。
- **離線語意變動**：native 恆在使「節點離線」≠「Docker 不可用」。緩解：分離「節點在線」與「Docker 能力」兩訊號（NodeStatusDTO.DockerAvailable），RetryDocker 改重建 docker 子後端。
- **Forge/NeoForge installer 輸出結構不定**（Q2）：以官方 installer 產出的啟動腳本/args 檔為啟動來源，不自解析；tasks 落地前以 spike 確認。
- **CurseForge key 申請不確定性**（R14）：審核時程/結果不可控。緩解：R14 為獨立可延後任務，無 key 建置功能完整（CF 選項隱藏）。
- **隔離性降級**：native 無容器隔離。緩解：文件與 GUI 明示；埠衝突由 OS bind 失敗回報。

## 測試策略

| R# | 驗證方式 | 平台需求 |
|---|---|---|
| R1 | backend 契約測試（新建共用套件，Mock/Docker/Native 跑同一組）；Create/Stop/Remove 單元測（假供應器、假行程） | 跨平台單元；行程樹強殺需真 Windows |
| R2 | dispatcher 路由單元測（前綴、未知 runtime 4xx、docker 錯誤時 List 軟降級）；Linux 拒 native 驗證測 | 跨平台單元（GOOS 注入） |
| R3 | 範本驗證表格測＋既有範本增 [native] 後 docker 路徑回歸 | 跨平台單元 |
| R4 | JavaProvisioner 單元測（httptest 假 Adoptium、快取、checksum、離線錯訊） | 跨平台單元 |
| R5 | 各安裝器單元測（httptest＋sha 校驗＋eula）；Forge/NeoForge installer 執行整合測 | installer 需真 JRE（Windows 整合） |
| R6 | SteamCMD 單元測（假載點、快取共用）；真 app_update 為 E2E（新 build tag `native`，可 skip） | 需網路＋Windows |
| R7 | 日誌滾動/tail、PID 收養/合成 die 單元測；真收養整合測 | 收養整合需真 Windows |
| R8 | Stats 正規化語意、行程樹加總單元測；die→自動重啟走既有狀態機測 | 真行程樹統計需 Windows |
| R9 | 記憶體超限→oom、CPU rate 實測（容忍誤差）、agent 退出伺服器續活 | **必須真 Windows** |
| R10 | native Archive→Restore checksum；docker↔native 備份互轉整合測 | tar 單元跨平台；互轉整合 |
| R11 | mrpack 解析/落位單元測（httptest）；owner 分派測；手動上傳 native 可用 | 跨平台單元 |
| R12 | 綁定單元測；前端進度/置灰測；乾淨 Windows 全流程 E2E | E2E 需真 Windows 無 Docker |
| R13 | Reconciler 對 native refs 對帳測；docker down 不影響 native 對帳 | 跨平台單元（假 backend） |
| R14 | CF API 單元測（httptest：正常/downloadUrl=null/fallback 匯入）；無 key 建置回歸測；真 API E2E 待 key | 跨平台單元；E2E 待 key |

> 需真 Windows 的項目（Job Objects、收養、行程樹、installer 執行、全流程 E2E）以 build tag `native` 區隔，CLAUDE.md 常用指令節同步。

## 新增 Go 相依（逐一）

- **`golang.org/x/sys`（windows 子套件）**：目前為 indirect v0.45.0（go.mod），提為直接相依、版本不變。用途：Job Objects 全套（Create/Assign/SetInformation/QueryInformation/Terminate）＋行程開啟與記憶體查詢。為何非它不可：Job Objects 是 Windows 強制資源上限唯一原生機制，且順帶提供行程樹統計；已在模組圖內，零新下載。
- 其餘全用標準庫（archive/zip、archive/tar、crypto/sha256、os/exec、net/http）。**無其他新模組**。
