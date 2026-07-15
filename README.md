# ServerMonitor

可擴充的遊戲伺服器管理與監控桌面工具。首波支援 Minecraft、Palworld 及類似的 Steam dedicated server,並以「遊戲範本」設計讓未來新遊戲能快速部署。

## 狀態

實作進行中(/pipeline,15/16)。規格 rev.3 已核可(見 `specs/game-server-manager/`)。**後端(T1–T14)與桌面 GUI(T15)完成**:完整生命週期/監控/指令/排程/備份/告警後端 + Wails 桌面介面(實例卡片、建立精靈、主控台(RCON 與 Palworld REST 動作)、排程/備份/告警設定、事件檢視)。build / test / -race / 真 Docker 整合 / svelte-check 皆綠。剩端到端驗證+效能基準+文件(T16)。

## 快速開始(使用者)

1. 開啟 Docker Desktop(必要)。
2. 執行 `build\bin\servermonitor.exe`(或開發模式 `wails dev`)。
3. 「建立伺服器」→ 選 Minecraft → 變體 Paper → 勾 EULA → 設 RCON 密碼 → 建立(首次拉映像需數分鐘)。
4. 卡片「啟動」→ 開「主控台」看 log、輸入 `list` 測指令 → 設定頁玩備份/排程/告警。
5. 遊戲連線:`localhost:25565`。應用資料在 `%LOCALAPPDATA%\ServerMonitor\`;**關閉本工具不會停伺服器**(容器由 Docker 維持,重開自動接管)。

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
