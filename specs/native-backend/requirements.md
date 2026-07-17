# native-backend — 需求規格（requirements.md）

> 建立日期：2026-07-16｜狀態：已核可（2026-07-16）
> 需求主體使用 EARS 句式。

## 概述

新增免 Docker 的本機行程執行後端（native backend），使用者只需安裝主程式即可建立與管理遊戲伺服器：系統自動供應 JVM（Adoptium）、下載各 loader 的伺服器檔案、供應 SteamCMD，全程無外部安裝需求。既有 Docker 後端保留，逐實例選擇 runtime，Windows 上以 native 為預設。

## 範圍（含明確不做）

- 包含：NativeBackend（RuntimeBackend 第二實作）、逐實例 runtime 分派鏈、範本 `[native]` 區段、JRE 自動供應、Minecraft 安裝器（Vanilla/Paper/Fabric/Forge/NeoForge）、SteamCMD 供應（Palworld）、行程收養與日誌落檔、Windows Job Objects 資源上限、Modrinth 模組/模組包 native 安裝（owner 分派重構）、CurseForge 模組包（R14，排最後、可獨立延後）、GUI runtime 選擇與供應進度、既有 game-server-manager 規格的 Docker 措辭鬆綁修訂。
- 明確不做：Linux/macOS 的 native 後端（Linux 平台強制 Docker）；Spigot（BuildTools 本機編譯）；容器級檔案系統/網路隔離的等價替代；CurseForge 的 CDN 直連與社群代理路徑（不合規，見 Q1）。

## 需求

### R1 本機行程執行後端

系統應提供 NativeBackend，以本機子行程執行伺服器，完整實作 RuntimeBackend 介面（Create/Start/Stop/Status/List/Inspect/Remove/Logs/ExecProcess/Stats/Archive/Restore/Events）。

驗收條件:
- 通過與 MockBackend/DockerBackend 相同的介面契約測試（既有 backend 契約測試若無則新建）。
- Create 建立實例工作目錄（沿用 `<DataRoot>/<uuid>/` 與 data_dirs 慣例）並產生啟動配置；不啟動行程。
- Stop 依 `opts.Grace` 先等待優雅退出（呼叫端先送 hooks.stop，同 Docker 現況），逾時強制終止整棵行程樹。
- Remove 停止行程並清理 runtime 中繼資料；資料目錄保留（與 Docker 後端行為一致）。

### R2 逐實例 runtime 分派

當建立實例時，系統應把範本的 `runtime` 欄位寫入 InstanceSpec，agent 依該欄位將操作分派至對應後端；未知 runtime 應回明確錯誤。

驗收條件:
- InstanceSpec 含 runtime 欄位；buildSpec 從範本填入；agent 對 `docker`/`native` 正確分派，未知值回 4xx 級錯誤。
- Windows 上新實例預設 runtime 為 native；使用者可改選 docker（偵測到 Docker 可用時）。
- Linux 平台上 runtime=native 的建立請求被驗證擋下，錯誤訊息說明 Linux 僅支援 Docker。
- 既有 docker 實例不受影響（升級後對帳、操作行為不變）。

### R3 範本 native 區段

系統應支援範本宣告 `[native]` 區段，定義該遊戲在本機行程模式下的啟動與設定方式；`[docker]` 與 `[native]` 至少存在一個，範本僅含其一時該遊戲只能以該 runtime 建立。

驗收條件:
- `[native]` 可宣告：供應類型（java/steamcmd）、版本需求（如 Java major 版）、啟動命令模板、params→遊戲設定檔的映射（server.properties / ini）。
- 範本驗證：`runtime` 允許值擴為 docker/native；缺對應區段時報錯欄位路徑明確（同現有 `docker.image` 風格）。
- 既有 minecraft.toml、palworld.toml 增補 `[native]` 後，原 docker 路徑行為不變（回歸測試）。

### R4 JRE 自動供應

當範本要求 Java 執行環境且本機快取無對應 major 版本時，系統應自 Adoptium API 下載對應 JRE，校驗 checksum 後解壓至共用快取目錄。

驗收條件:
- 快取命中時不重複下載；同 major 版本多實例共用。
- 下載走 HTTPS，checksum 不符時中止並報錯，不留半成品。
- 離線且無快取時，建立/啟動失敗並回「需要網路下載 Java <版本>」的明確錯誤。

### R5 Minecraft 伺服器安裝器

當建立 runtime=native 的 Minecraft 實例時，系統應依 variant（vanilla/paper/fabric/forge/neoforge）自動下載並安裝伺服器檔案至實例目錄。

驗收條件:
- Vanilla：Mojang version manifest 下載 server.jar，sha1 校驗。
- Paper：PaperMC API 下載對應 build，sha256 校驗。
- Fabric：官方 installer/launcher jar 產生可啟動配置。
- Forge/NeoForge：執行官方 installer 產生 libraries 與啟動配置。
- 安裝進度以既有事件機制回報（GUI 可顯示）；任一步失敗時實例建立回滾（沿用 journal 機制）。
- eula 參數為 true 時寫入 eula.txt（native 下無 itzg 代勞）。

### R6 SteamCMD 供應與 Palworld 安裝

當建立 runtime=native 的 Palworld 實例時，系統應自動下載 SteamCMD（Valve 官方匿名載點）並以 `app_update 2394010` 安裝伺服器至實例目錄。

驗收條件:
- SteamCMD 為共用快取，一次下載多實例共用。
- 安裝/更新進度回報；失敗回滾。
- 實例啟動時可選「啟動前檢查更新」（範本 hooks 或參數控制）。

### R7 行程生命週期：日誌落檔與收養

於 native 實例執行期間，系統應將行程 stdout/stderr 重導至實例目錄下的日誌檔（滾動上限），並記錄 PID 中繼資料；當 agent 重啟時，系統應收養仍在執行的行程，恢復管理。

驗收條件:
- Logs（含 follow 串流）由日誌檔 tail 提供，行為與 Docker logs 對等（既有 WS fanout 不改）。
- agent 重啟後：行程仍在 → 狀態回報 running 且可 Stop/指令；行程已死 → 合成 die 事件走既有崩潰復原。
- 日誌檔滾動不中斷串流；上限可設定，預設值寫入規格。

### R8 監控統計與事件

於 native 實例執行期間，系統應以行程樹為單位回報 CPU/記憶體統計（填入既有 ResourceStats），並在行程退出時合成事件（等價 Docker die），驅動既有自動重啟與告警。

驗收條件:
- Stats 涵蓋整棵行程樹（java 子行程、SteamCMD 更新子行程）。
- CPUPercent 語意與 Docker 版一致（占可用核心百分比），GUI 無需分辨後端。
- 非預期退出觸發既有自動重啟狀態機（game-server-manager R8 的行為不變）。

### R9 資源上限（Windows Job Objects）

在 Windows 的 native 模式下，系統應以 Job Objects 對實例行程樹強制 CPU 與記憶體上限；上限取自與 Docker 後端相同的實例資源設定。

驗收條件:
- 記憶體超限時行程被系統終止並產生可辨識事件（區別於一般 crash，告警訊息標明「超出記憶體上限」）。
- CPU 上限以 Job CPU rate control 強制，實測占用不超過設定值（容忍誤差寫入測試）。
- 收養後的行程重新納入 Job 管理。
- Job 設定不得使 agent 行程退出時連帶殺掉伺服器行程（與 R7 收養共存）。

### R10 備份/還原沿用

系統應讓 native 實例沿用既有 Archive/Restore 停機一致 tar 快照流程，備份格式與 Docker 後端互通。

驗收條件:
- native 實例備份→還原成功，checksum 驗證通過。
- 同一實例資料在 docker/native 後端間可透過備份還原互轉（同範本、同 data_dirs 前提）。

### R11 模組/模組包 native 安裝（Modrinth）

當 runtime=native 且範本宣告模組支援時，系統應自行從 Modrinth 解析並安裝模組/模組包至實例資料目錄（取代 Docker 模式下委由 itzg 映像處理的路徑）。

驗收條件:
- 模組安裝的 owner 概念由「image-native 寫死」重構為依後端分派；modpack.go 內 itzg 專屬 env 映射（internal\core\modpack.go:164-178）只在 docker 路徑生效。
- Modrinth modpack（mrpack）解析、依賴下載、檔案落位正確；版本相容性不符時報明確錯誤。
- 手動上傳模組包路徑（既有功能）在 native 下仍可用。
- CurseForge 見 R14。

### R12 GUI：runtime 選擇與供應進度

當使用者建立實例時，GUI 應提供 runtime 選擇（Windows 預設 native），並在供應階段（JRE/伺服器檔案/SteamCMD 下載）顯示進度；native 與 docker 的能力差異應明示。

驗收條件:
- 未裝 Docker 的乾淨 Windows 機器上，GUI 全流程（建立→啟動→主控台→備份）可用，無任何外部安裝提示。
- 供應進度有百分比或階段描述；失敗時錯誤訊息可指出是哪個供應步驟。
- Docker 不可用時 docker 選項置灰並說明原因（沿用既有 Docker 偵測）。

### R13 啟動對帳納入 native

當管理核心啟動時，系統應對 native 實例執行對帳：PID 存活比對、狀態修正、孤兒行程處置，與既有 Docker 對帳（gsm.* 標籤掃描）並存。

驗收條件:
- DB 記 running 但行程已死 → 修正狀態並記事件（同 Docker 對帳語意）。
- 行程存活但 DB 記 stopped → 依既有規則處置（收養或停止，與 Docker 對帳同策略）。
- Docker daemon 不可用不影響 native 實例對帳（兩者獨立降級）。

### R14 CurseForge 模組包（可獨立延後）

若建置內含專案 CurseForge API key（編譯時嵌入，可由使用者設定覆蓋），則系統應支援 CurseForge 模組包：經官方 API 解析 manifest 並下載模組；當模組作者停用第三方散布（API 回 downloadUrl 為 null）時，系統應列出被擋模組、引導使用者以瀏覽器手動下載，並監看指定資料夾以檔名＋hash 驗證後自動匯入。

驗收條件:
- 無 key 的建置：CurseForge 選項不可見或明示未啟用，其餘功能（含 Modrinth）不受影響。
- 有 key 的建置：CurseForge modpack 安裝成功；含 opted-out 模組的 modpack 可經 fallback 流程完成安裝。
- 使用者可在設定中填自己的 key 覆蓋內嵌 key（比照 Prism Launcher）。
- 文件揭露：key 為本專案專屬、衍生作品須自行申請（CurseForge 第三方 API 條款要求）。
- 本需求依賴專案方取得 key；未取得前，對應任務可獨立延後，不阻塞其餘需求交付。

## 非功能需求

- 安全：所有自動下載一律 HTTPS＋官方來源＋checksum 校驗；機密注入行為與 Docker 後端等價（env 傳遞，金鑰庫存放不變）。
- 相容性：native 後端支援 Windows 10/11 x64；Linux 平台強制 Docker（native 選項不可見）。
- 磁碟：JRE 與 SteamCMD 為共用快取，不隨實例重複；快取位置在 DataRoot 下，解除安裝可整目錄清除。
- 效能：native 監控輪詢開銷不高於 Docker 後端現況；日誌 fanout 沿用既有背壓機制，不新增路徑。
- 隔離性降級明示：native 模式無容器隔離（檔案系統/網路），文件與 GUI 提示需載明。

## 開放問題

- Q1【已裁決 2026-07-16 → R14】CurseForge 免使用者 key：可行——比照 Prism Launcher，專案方向 console.curseforge.com 申請專屬 key 嵌入應用（Prism 的 key 公開在其 repo CMakeLists.txt，CF 實務默許，僅要求衍生作品換 key）。已升格為 R14（排最後、可獨立延後，依賴使用者取得 key）。CDN 直連與社群代理不合規/不可靠，列入明確不做。
- Q2 Forge/NeoForge installer 輸出結構版本間有異：設計採「以官方 installer 產出的啟動腳本/args 檔為啟動來源、不自解析」降風險；tasks T4 落地前以 spike 確認並補回滾細節。
