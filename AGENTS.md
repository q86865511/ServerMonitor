# ServerMonitor — 專案規則(AGENTS.md)

> 專案特有規則;全域規則見 `~/.Codex/AGENTS.md`,不在此重複。

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
- 開發(GUI):`wails dev`｜建置:`wails build`

## 依賴版本(已鎖定,勿升級)

Go 1.26+｜Wails v2.13｜docker v27.5.1(**go-connections 必須 v0.5.0**,v0.7.0 會壞 Windows 編譯)｜gorilla/websocket v1.5.3｜modernc/sqlite v1.34.5｜go-keyring v0.2.8｜go-toml/v2。

## 架構約定

- 四層:桌面 GUI → 管理核心 → 節點代理(localhost HTTP)→ 遊戲範本。詳見 `design.md`。
- 所有伺服器操作經 `RuntimeBackend` 介面,不散落直呼 Docker SDK。
- 核心↔代理經 `NodeClient`(HTTP),即使單機亦然(預留多節點)。
- 敏感值(RCON 密碼、webhook URL)存 OS 金鑰庫(go-keyring),不明文落檔。
- 新增遊戲=加範本檔;新增執行後端=實作 `RuntimeBackend` 介面。
