# specs/ — 規格文件索引

本目錄為 ServerMonitor 專案「規格驅動開發」的三件套規格(`requirements.md`/`design.md`/`tasks.md`),各功能經核可後以 `/pipeline` 依 `tasks.md` 執行實作。

## 功能總覽

| 功能 | 一句話 | 任務完成度 | 核可日 | 三件套 |
|---|---|---|---|---|
| game-server-manager | 核心功能:遊戲伺服器生命週期/監控/指令/備份/告警/模組/多節點管理(Docker 執行後端) | 16/16 | 2026-07-13 | [requirements](game-server-manager/requirements.md)｜[design](game-server-manager/design.md)｜[tasks](game-server-manager/tasks.md) |
| native-backend | Windows 免 Docker 執行後端:JRE/SteamCMD 自動供應、Job Objects 資源上限、備份與 Docker 後端互通 | 14/14 | 2026-07-16 | [requirements](native-backend/requirements.md)｜[design](native-backend/design.md)｜[tasks](native-backend/tasks.md) |
| gui-redesign | 桌面 GUI 全面重構:Application Shell、總覽儀表板、伺服器詳細頁五分頁、建立精靈、指標時序圖 | 15/15 | 2026-07-17 | [requirements](gui-redesign/requirements.md)｜[design](gui-redesign/design.md)｜[tasks](gui-redesign/tasks.md) |

三份規格的 `tasks.md` 均已全數勾選收官,狀態行已加註「實作完成 2026-07」;詳細進度見專案根目錄 `PROGRESS.md`。

## Spike 索引(game-server-manager)

實作前以 spike 查證高風險假設,產出落於 `game-server-manager/spikes/`:

| Spike | 結論摘要 | 連結 |
|---|---|---|
| palworld-spike | Palworld 官方 REST API(12 具名端點)足以支撐首版指令 adapter;RCON 官方與社群映像均已標為 deprecated,首版僅佔位不啟用;鎖定映像 `thijsvanloef/palworld-server-docker:v2.5.1`。部分細節(`game-data` 端點、`kick`/`ban` 的 `userid` 格式、映像 digest)截至 2026-07-21 仍為開放項,未被後續任務實跑覆核。 | [palworld-spike.md](game-server-manager/spikes/palworld-spike.md) |
| backup-spike | Windows/Docker Desktop 下以 host bind mount 作實例資料目錄,停機後打包/還原具一致性(checksum 逐一相符);Windows `os.Rename` 不可覆蓋既有目錄,Restore 採 rename-aside 三步驟原子切換。真遊戲資料含 symlink/大量小檔情境截至 2026-07-21 仍為開放項,未經真機驗證。 | [backup-spike.md](game-server-manager/spikes/backup-spike.md) |
| modpack-spike | 查證 itzg 各 loader/遊戲版本支援矩陣、`AUTO_CURSEFORGE` 實際 env 行為與 CurseForge API 條款/金鑰/attribution,供 T14 模組/模組包實作依據。 | [modpack-spike.md](game-server-manager/spikes/modpack-spike.md) |

## 交叉指引

Docker 與 native 為同一 `RuntimeBackend` 介面下的兩個執行後端;native-backend 規格建立於 game-server-manager 之上,備份格式互通。
