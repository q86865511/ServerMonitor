# ServerMonitor — 專案規則(CLAUDE.md)

> 專案特有規則;全域規則見 `~/.claude/CLAUDE.md`,不在此重複。

## 簡介

可擴充的遊戲伺服器管理與監控桌面工具(Minecraft、Palworld 等 Steam dedicated server)。擴充性靠「遊戲範本」抽象——新增遊戲=加一份 `templates/*.toml`,不改核心。

## 規格位置

功能規格三件套:`specs/game-server-manager/{requirements,design,tasks}.md`(狀態:已核可 2026-07-13)。實作以 `/pipeline game-server-manager` 依 `tasks.md` 執行。

## 技術棧

Go(核心/代理)+ Wails v2(桌面)+ Svelte-TS 前端 + Docker(執行後端,官方 SDK)。

## 常用指令

> **PATH 注意**:本機 Go/Wails 經 winget/`go install` 安裝,可能不在既有 shell 的 PATH。跑 go/wails 前先 `export PATH="/c/Program Files/Go/bin:$HOME/go/bin:$PATH"`(Bash)。

- 編譯:`go build ./...`
- 單元/整合測試(免 Docker):`go test ./...`
- 前端測試(vitest,store 行為測試):`cd frontend && npm test`
- 真 Docker 整合測試:`go test -tags docker -p 1 ./...`(需 Docker daemon;build tag 為 `docker`。**`-p 1` 必加**:套件預設並行,多個 E2E 同時搶 daemon 會使 `TestE2E_MinecraftFullLifecycle` 假性逾時)
- 端到端(T16,需 Docker;會拉映像/建啟容器,測後自動清理):`go test -tags docker -run TestE2E ./internal/app/`
  - 單跑 Minecraft 全流程:`go test -tags docker -run TestE2E_MinecraftFullLifecycle ./internal/app/`
  - Palworld(需先 `docker pull thijsvanloef/palworld-server-docker:v2.5.1`,約 6-8GB;否則自動 skip):`-run TestE2E_Palworld`
  - log 高流量 fanout 基準:`go test -tags docker -run TestPerf_LogThroughput ./internal/core/`
- native 後端測試(免 Docker;build tag 為 `native`;僅 Windows,非 Windows 自動 skip):`go test -tags native ./...`
  - native Minecraft 全流程 E2E(真下載 Adoptium JRE + Mojang vanilla server.jar 約 100-200MB,建立→啟動→RCON 就緒→備份→還原→移除;heavy,`-short` 跳過):`go test -tags native -run TestE2E_NativeMinecraftFullLifecycle ./internal/app/ -timeout 25m`
  - native Palworld SteamCMD E2E(app_update 2394010 約 6-8GB,預設 skip;顯式啟用):`GSM_NATIVE_PALWORLD_E2E=1 go test -tags native -run TestE2E_NativePalworld ./internal/app/ -timeout 40m`
  - docker↔native 備份互轉整合測(需 Docker daemon,雙 tag;daemon 不可用自動 skip):`go test -tags "docker native" -run TestBackupInterop_DockerNative ./internal/agent/`
- 開發(GUI):`wails dev`｜建置:`wails build`｜安裝包:`wails build -nsis`(需 makensis,於 `C:\Program Files (x86)\NSIS`)
- 節點代理(遠端節點用,無 GUI):`go build ./cmd/agent`;Linux 交叉編譯 `GOOS=linux GOARCH=amd64 go build -o servermonitor-agent ./cmd/agent`

## 依賴版本(已鎖定,勿升級)

Go 1.26+｜Wails v2.13｜docker v27.5.1(**go-connections 必須 v0.5.0**,v0.7.0 會壞 Windows 編譯)｜gorilla/websocket v1.5.3｜modernc/sqlite v1.34.5｜go-keyring v0.2.8｜go-toml/v2｜energye/systray v1.0.3(系統匣;Windows 專屬路徑)。

## 架構約定

- 四層:桌面 GUI → 管理核心 → 節點代理(localhost HTTP)→ 遊戲範本。詳見 `design.md`。
- 所有伺服器操作經 `RuntimeBackend` 介面,不散落直呼 Docker SDK。
- 核心↔代理經 `NodeClient`(HTTP),即使單機亦然(預留多節點)。
- 敏感值(RCON 密碼、webhook URL)存 OS 金鑰庫(go-keyring),不明文落檔。
- 新增遊戲=加範本檔;新增執行後端=實作 `RuntimeBackend` 介面。
- `RuntimeBackend` 之外的可選能力(備份刪除、映像/容器管理、磁碟用量、檔案管理、資料清理)一律以**橫切介面**表達(如 `BackupDeleter`/`ImageManager`/`FileManager`/`instanceDataPurger`),於 agent 套件定義、docker/native 各自實作、`dispatchBackend` 型別斷言轉發、`server.go` 端點型別斷言啟用(缺失回 `ErrUnsupported`);**勿擴張核心 `RuntimeBackend` 介面**。
- 節點端**任何以 uuid/rel 拼接宿主路徑的操作,必經路徑拘束**:uuid 用 `validateInstanceUUID`、備份 id 用 `validateBackupID`、檔案相對路徑用 `resolveWithinRoot`(三層:cleanRelPath→withinRoot(filepath.Rel 擋 sibling-prefix)→assertNoLinkComponents 逐段 Lstat 拒穿越連結;Windows junction 為 ModeIrregular、EvalSymlinks 不解析,故以逐段 Lstat 為主力)。含明文機密的中繼檔(instance.json 等)不開放檔案管理存取。實例層操作(檔案/磁碟/備份)經 `rec.Node` 路由到該實例所屬節點,勿固定本機。
- 內建範本以 `//go:embed all:templates`(main.go)打進執行檔,啟動抽出到 `<dataRoot>\templates-builtin\`;使用者自訂範本放 `<dataRoot>\templates\`。範本**只在啟動時載入一次**(`loadTemplates`),無檔案監看,新增範本需重啟。
- **埠屬於實例,不屬於範本**:範本的 `host_port` 只是預設值。建立時經 `CreateOptions.PortOverrides` 覆寫,`host_port<=0` 由核心在 `reserveMu` 臨界區內挑可用埠(避開既有預留、本批已定案、實際 `net.Listen` 試綁),一律落成具體 `PortReservation`。**指令協定與健康探針必經 `resolveInstancePort`**(取該實例實際預留,查無才退回範本值),勿直接讀 `tmpl.Ports[].HostPort`——否則第二台同範本伺服器會探到錯的埠。
- 多節點:遠端節點跑 `cmd/agent`(TLS 自簽+SHA-256 指紋 pinning(TOFU)或自備憑證);節點 token 一律入 OS 金鑰庫(`NodeTokenRef`),config.json 只存非敏感連線設定且必經 `SaveAppConfig` 原子寫。

## 前端慣例(gui-redesign 之後)

- 目錄:`frontend/src/lib/` 下分 `ui/`(共用元件)、`shell/`(側欄/頂欄)、`pages/`(路由頁,`pages/server/` 詳細頁分頁、`pages/wizard/` 建立精靈)、`stores/`(狀態層);`router.ts` 自製 hash 路由。
- 元件一律 Svelte 5 runes(`$props`/`$state`/`$derived`,callback props);跨頁狀態用 svelte/store。
- Wails 事件訂閱**集中在 stores/**(引用計數+操作鏈序列化),元件不得自行 EventsOn(唯一例外:精靈的 `provision` 事件隨精靈生命週期)。`$effect` 的 cleanup 必以區域副本捕捉 uuid 類反應式參數。
- 色彩只用 `style.css` `:root` tokens,元件內不硬編碼色值;手寫碼禁 `any`(wailsjs 生成碼除外);UI 文案繁中。
- 後端資料為 nil/不可採集→顯「不適用」/「—」,不捏造數值;趨勢圖 NULL 畫缺口不補 0。
- Go 綁定簽章變動後必跑 `wails build` 重生成 `frontend/wailsjs/`(勿手改)。
