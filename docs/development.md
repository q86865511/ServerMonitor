# 開發者指南(建置/測試/版本鎖定)

> 返回 [README](../README.zh-TW.md)｜[English README](../README.md)

## 開發環境

- Windows 11、Go 1.26+、Node 18+/npm。
- Wails CLI v2:`go install github.com/wailsapp/wails/v2/cmd/wails@v2.13.0`(鎖版本對齊專案 pin 的 Wails v2.13,避免升級回歸)。
- NSIS 安裝包另需:`winget install NSIS.NSIS`。

## 建置指令

```bash
go build ./...               # 編譯全部套件
wails dev                    # 開發模式(GUI)
wails build                  # 產出 Windows 可執行檔
wails build -nsis            # 另產出 NSIS 安裝包
go build ./cmd/agent         # 遠端節點代理(本平台)
GOOS=linux GOARCH=amd64 go build -o servermonitor-agent ./cmd/agent   # 交叉編譯 Linux 節點代理
```

## 測試矩陣

```bash
go test ./...                                                        # 單元/整合測試(免 Docker)
go test -tags docker ./...                                           # 真 Docker 整合測試 + 端到端 + 效能基準(需 Docker daemon)
go test -tags docker -run TestE2E ./internal/app/                    # 端到端(需 Docker;拉映像建容器,測後自動清理)
go test -tags docker -run TestE2E_MinecraftFullLifecycle ./internal/app/
go test -tags docker -run TestE2E_Palworld ./internal/app/           # 需先 docker pull thijsvanloef/palworld-server-docker:v2.5.1
go test -tags docker -run TestPerf_LogThroughput ./internal/core/    # log 高流量 fanout 基準
go test -tags native ./...                                           # native 後端(僅 Windows,非 Windows 自動 skip)
go test -tags native -run TestE2E_NativeMinecraftFullLifecycle ./internal/app/ -timeout 25m
GSM_NATIVE_PALWORLD_E2E=1 go test -tags native -run TestE2E_NativePalworld ./internal/app/ -timeout 40m
go test -tags "docker native" -run TestBackupInterop_DockerNative ./internal/agent/   # docker↔native 備份互轉
```

完整指令說明另見專案 `CLAUDE.md`「常用指令」。

## 版本鎖定(已 pin,勿升級)

依賴刻意鎖定以避免 Windows/Docker 相容性回歸(見 `go.mod`、專案 `CLAUDE.md`):

- **Go 1.26+**、**Wails v2.13**(非 v3 alpha)。
- **Docker client `github.com/docker/docker` v27.5.1** — ⚠️ **`github.com/docker/go-connections` 必須維持 v0.5.0**,v0.7.0 會壞 Windows 編譯。
- **`gorilla/websocket` v1.5.3**、**`modernc.org/sqlite` v1.34.5**(純 Go 免 CGO)、**`github.com/zalando/go-keyring` v0.2.8**(Windows Credential Manager)、**`github.com/pelletier/go-toml/v2`**。

## Spike 結論索引

三個前置查證(結論落檔於 `specs/game-server-manager/spikes/`):

- **`palworld-spike.md`** — Palworld 官方 REST API(12 具名端點、Basic Auth)可支撐首版;RCON 官方標 deprecated 故列 legacy;鎖定映像 `thijsvanloef/palworld-server-docker:v2.5.1`。
- **`backup-spike.md`** — Windows/Docker Desktop(WSL2)下以 host bind mount 作實例資料目錄、停機後 host 端 tar+checksum 打包/還原的一致性可行,定案首版備份=停機快照 + bind mount。
- **`modpack-spike.md`** — itzg 支援矩陣/`AUTO_CURSEFORGE` env/CurseForge 條款查證;itzg 為 `/data` 唯一安裝擁有者,`CF_API_KEY` 由使用者自填。
