# ServerMonitor — 專案規則(CLAUDE.md)

> 專案特有規則;全域規則見 `~/.claude/CLAUDE.md`,不在此重複。

## 簡介

可擴充的遊戲伺服器管理與監控桌面工具(Minecraft、Palworld 等 Steam dedicated server)。擴充性靠「遊戲範本」抽象——新增遊戲=加一份 `templates/*.toml`,不改核心。

## 規格位置

功能規格三件套:`specs/game-server-manager/{requirements,design,tasks}.md`(狀態:已核可 2026-07-13)。實作以 `/pipeline game-server-manager` 依 `tasks.md` 執行。

## 技術棧

Go(核心/代理)+ Wails(桌面)+ web 前端(Svelte/React)+ Docker(執行後端)。

## 常用指令(骨架建立後補正)

- 開發:`wails dev`
- 建置:`wails build`
- 測試:`go test ./...`
- 真 Docker 整合測試:`go test -tags docker ./...`(TODO:骨架建立後確認 build tag 名)

## 架構約定

- 四層:桌面 GUI → 管理核心 → 節點代理(localhost HTTP)→ 遊戲範本。詳見 `design.md`。
- 所有伺服器操作經 `RuntimeBackend` 介面,不散落直呼 Docker SDK。
- 核心↔代理經 `NodeClient`(HTTP),即使單機亦然(預留多節點)。
- 敏感值(RCON 密碼、webhook URL)存 OS 金鑰庫(go-keyring),不明文落檔。
- 新增遊戲=加範本檔;新增執行後端=實作 `RuntimeBackend` 介面。
