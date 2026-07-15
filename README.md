# ServerMonitor

可擴充的遊戲伺服器管理與監控桌面工具。首波支援 Minecraft、Palworld 及類似的 Steam dedicated server,並以「遊戲範本」設計讓未來新遊戲能快速部署。

## 狀態

實作進行中(/pipeline)。規格 rev.3 已核可(見 `specs/game-server-manager/`)。**後端全部完成(T1–T14,14/16)**:共享型別、持久化(SQLite)、Docker 執行後端、節點代理 HTTP/WS API、管理核心、狀態機/生命週期/啟動對帳、RCON 與 Palworld REST 指令、監控聚合(stats/logs 串流+背壓)、健康探針與自動崩潰復原、UTC 排程、備份/還原(停機一致快照+保留策略+crash-safe journal)、Discord 告警(門檻/去重/重試)、itzg 模組包、內建 Minecraft/Palworld 範本(build / test / -race / 真 Docker 整合測試皆綠)。剩桌面 GUI(T15)與端到端+文件(T16)。

## 特色(規劃中)

- 一鍵建立/部署遊戲伺服器(Docker 執行後端)
- 資源監控 + 即時 log 主控台
- RCON / REST 指令
- 自動重啟 / 崩潰復原
- 排程備份 + 還原
- Discord 告警
- 模組 / 模組包支援(Paper/Forge/Fabric、Modrinth/CurseForge)
- 擴充新遊戲 = 加一份 `templates/*.toml`

## 技術棧

Go + Wails(桌面)+ web 前端 + Docker。

## 需求環境

- Windows 11
- Docker Desktop(WSL2)— 執行遊戲容器與真 Docker 整合測試
- Go 1.26+、Wails CLI v2(開發用)、Node 18+/npm(前端)

## 安裝與執行

需先安裝 Go 1.26+ 與 Wails CLI v2(`go install github.com/wailsapp/wails/v2/cmd/wails@latest`)。

```
go build ./...               # 編譯全部套件
go test ./...                # 單元/整合測試(免 Docker)
go test -tags docker ./...   # 真 Docker 整合測試(需 Docker daemon)
wails dev                    # 開發模式(GUI)
wails build                  # 產出 Windows 可執行檔
```

## 規格文件

`specs/game-server-manager/`:`requirements.md`(需求)、`design.md`(設計)、`tasks.md`(任務)。
