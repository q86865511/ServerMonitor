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
- 真 Docker 整合測試:`go test -tags docker ./...`(需 Docker daemon;build tag 為 `docker`)
- 端到端(T16,需 Docker;會拉映像/建啟容器,測後自動清理):`go test -tags docker -run TestE2E ./internal/app/`
  - 單跑 Minecraft 全流程:`go test -tags docker -run TestE2E_MinecraftFullLifecycle ./internal/app/`
  - Palworld(需先 `docker pull thijsvanloef/palworld-server-docker:v2.5.1`,約 6-8GB;否則自動 skip):`-run TestE2E_Palworld`
  - log 高流量 fanout 基準:`go test -tags docker -run TestPerf_LogThroughput ./internal/core/`
- native 後端測試(免 Docker;build tag 為 `native`;僅 Windows,非 Windows 自動 skip):`go test -tags native ./...`
  - native Minecraft 全流程 E2E(真下載 Adoptium JRE + Mojang vanilla server.jar 約 100-200MB,建立→啟動→RCON 就緒→備份→還原→移除;heavy,`-short` 跳過):`go test -tags native -run TestE2E_NativeMinecraftFullLifecycle ./internal/app/ -timeout 25m`
  - native Palworld SteamCMD E2E(app_update 2394010 約 6-8GB,預設 skip;顯式啟用):`GSM_NATIVE_PALWORLD_E2E=1 go test -tags native -run TestE2E_NativePalworld ./internal/app/ -timeout 40m`
  - docker↔native 備份互轉整合測(需 Docker daemon,雙 tag;daemon 不可用自動 skip):`go test -tags "docker native" -run TestBackupInterop_DockerNative ./internal/agent/`
- 開發(GUI):`wails dev`｜建置:`wails build`

## 依賴版本(已鎖定,勿升級)

Go 1.26+｜Wails v2.13｜docker v27.5.1(**go-connections 必須 v0.5.0**,v0.7.0 會壞 Windows 編譯)｜gorilla/websocket v1.5.3｜modernc/sqlite v1.34.5｜go-keyring v0.2.8｜go-toml/v2。

## 架構約定

- 四層:桌面 GUI → 管理核心 → 節點代理(localhost HTTP)→ 遊戲範本。詳見 `design.md`。
- 所有伺服器操作經 `RuntimeBackend` 介面,不散落直呼 Docker SDK。
- 核心↔代理經 `NodeClient`(HTTP),即使單機亦然(預留多節點)。
- 敏感值(RCON 密碼、webhook URL)存 OS 金鑰庫(go-keyring),不明文落檔。
- 新增遊戲=加範本檔;新增執行後端=實作 `RuntimeBackend` 介面。

## 前端慣例(gui-redesign 之後)

- 目錄:`frontend/src/lib/` 下分 `ui/`(共用元件)、`shell/`(側欄/頂欄)、`pages/`(路由頁,`pages/server/` 詳細頁分頁、`pages/wizard/` 建立精靈)、`stores/`(狀態層);`router.ts` 自製 hash 路由。
- 元件一律 Svelte 5 runes(`$props`/`$state`/`$derived`,callback props);跨頁狀態用 svelte/store。
- Wails 事件訂閱**集中在 stores/**(引用計數+操作鏈序列化),元件不得自行 EventsOn(唯一例外:精靈的 `provision` 事件隨精靈生命週期)。`$effect` 的 cleanup 必以區域副本捕捉 uuid 類反應式參數。
- 色彩只用 `style.css` `:root` tokens,元件內不硬編碼色值;手寫碼禁 `any`(wailsjs 生成碼除外);UI 文案繁中。
- 後端資料為 nil/不可採集→顯「不適用」/「—」,不捏造數值;趨勢圖 NULL 畫缺口不補 0。
- Go 綁定簽章變動後必跑 `wails build` 重生成 `frontend/wailsjs/`(勿手改)。
