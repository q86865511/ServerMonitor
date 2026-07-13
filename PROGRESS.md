# ServerMonitor — 進度(PROGRESS.md)

## 目前狀態

規格三件套經 Codex 三輪審查(兩輪深修 + 一輪聚焦複審)定稿為 rev.3,複審判定可進 pipeline;專案文件已初始化。依裁示止於 spec-review,尚未進入實作。

## 已完成

- [2026-07-13] 📄 R1 規格深修(rev.3):依 Codex 第二輪二審(3 解決/10 部分/9 高),把高+中嚴重補進規格——埠 host binding+預留、RuntimeBackend 補 Events、restart 單一所有者、SecretRef 型別、完整狀態轉移表、備份 BackupID/停機快照、事件封套+碼表、精確參數與版本下限、任務重排 + Palworld/備份/模組 spike 前置 gate。Codex 第三輪聚焦複審:9 高中 7 關閉、2 部分,修掉 2 阻擋項(啟動就緒條件四處對齊、備份 spike 掛入 T4 並修正 spike 任務編號)後判定可進 pipeline。
- [2026-07-13] 📄 R1 規格二審與修訂(rev.2):Codex 對三件套二審(15 高/12 中/2 低)。已修訂:修正非法 TOML schema、補全代理 API 契約與 `RuntimeBackend`(List/Inspect/Remove/Archive/Restore、ExecProcess 與遊戲指令分離)、驗收可測化、新增 R13 啟動對帳/單一實例與 R14 結構化事件記錄、代理 bearer 認證;並依裁決改 Palworld REST 為主/RCON legacy、CurseForge 改 itzg 原生。
- [2026-07-13] 📄 R1 規格與專案初始化:完成 game-server-manager 規格三件套(requirements / design / tasks)並核可落檔於 `specs/game-server-manager/`;初始化 PROGRESS/CLAUDE/README 與 git。

## 進行中

(無)

## 待辦

> 完整任務見 `specs/game-server-manager/tasks.md`(16 項,標 HARD/NORMAL)。近期三項:

- [ ] T1 骨架 + 共享型別 + 版本鎖定 + SecretRef
- [ ] T2 持久化 + 事件基礎(SQLite/遷移/quarantine/port_reservations/EventLog/keyring)
- [ ] T3 `RuntimeBackend` 介面(+Events)+ `MockBackend`
- [ ] (前置 spike)Palworld REST/RCON 查證、備份原子性、itzg/CurseForge 矩陣

## 已知問題

(無)

## 重要決策紀錄

- [2026-07-13] 技術選型:Go 全棧 + Wails 桌面。理由:Go 為容器工具母語(Docker SDK 原生)、單二進位利於未來遠端節點、資源占用低。
- [2026-07-13] 部署拓撲:先單機,但核心/節點代理以 localhost HTTP 分離(Pterodactyl 模式),預留多節點(R5)。
- [2026-07-13] 執行後端:Docker 為主,抽象 `RuntimeBackend` 介面,預留原生 SteamCMD(R4)。
- [2026-07-13] 生命週期:僅 GUI 開啟時運作,不裝自啟背景服務——關閉應用暫停自動化,遊戲容器仍由 Docker 維持。
- [2026-07-13] 首版取捨:告警只做 Discord webhook;模組包含 Modrinth/CurseForge 串接;Palworld REST+RCON 皆做。
- [2026-07-13] rev.2(依 Codex 二審修訂):Palworld 改「REST 具名動作為主、RCON 標 legacy(需查證)」;CurseForge 改「itzg 原生 AUTO_CURSEFORGE + 手動」,自建下載器延後;新增 R13 啟動對帳/單一實例、R14 結構化事件記錄;代理 API 加 bearer token 認證;備份改經 RuntimeBackend Archive/Restore;鎖定依賴版本(Wails v2 stable 等)。
- [2026-07-13] rev.3(依 Codex 兩輪二審深修):restart 由 core 單一編排(agent 不開 /restart);Docker 事件納入 `RuntimeBackend.Events`;敏感值改 `SecretRef` 型別(輸入即入金鑰庫+redaction);備份首版=停機快照+bind mount、agent 擁有備份根+opaque `BackupID`;首版不支援線上 update(改參數=重建);Palworld/備份/模組三個 spike 設為對應功能前置 gate;依賴版本鎖定給具體下限。
