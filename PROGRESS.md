# ServerMonitor — 進度(PROGRESS.md)

## 目前狀態

規格(spec 三件套)已核可落檔、專案文件已初始化;尚未進入實作(本次依裁示只做到 /new-project + /spec-review)。

## 已完成

- [2026-07-13] 📄 R1 規格與專案初始化:完成 game-server-manager 規格三件套(requirements / design / tasks)並核可落檔於 `specs/game-server-manager/`;初始化 PROGRESS/CLAUDE/README 與 git。

## 進行中

(無)

## 待辦

> 完整任務見 `specs/game-server-manager/tasks.md`(15 項,標 HARD/NORMAL)。近期三項:

- [ ] T1 專案骨架 + 共享型別(Go module / Wails / 前端 / `internal` 目錄 / `protocol` 型別)
- [ ] T2 `RuntimeBackend` 介面 + `MockBackend`
- [ ] T3 `DockerBackend`(官方 Docker SDK)
- [ ] (後續)Codex 規格第二審(spec-review),依發現決定是否修規格

## 已知問題

(無)

## 重要決策紀錄

- [2026-07-13] 技術選型:Go 全棧 + Wails 桌面。理由:Go 為容器工具母語(Docker SDK 原生)、單二進位利於未來遠端節點、資源占用低。
- [2026-07-13] 部署拓撲:先單機,但核心/節點代理以 localhost HTTP 分離(Pterodactyl 模式),預留多節點(R5)。
- [2026-07-13] 執行後端:Docker 為主,抽象 `RuntimeBackend` 介面,預留原生 SteamCMD(R4)。
- [2026-07-13] 生命週期:僅 GUI 開啟時運作,不裝自啟背景服務——關閉應用暫停自動化,遊戲容器仍由 Docker 維持。
- [2026-07-13] 首版取捨:告警只做 Discord webhook;模組包含 Modrinth/CurseForge 串接;Palworld REST+RCON 皆做。
