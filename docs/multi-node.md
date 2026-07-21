# 多節點(遠端/雲端節點)

> 返回 [README](../README.zh-TW.md)｜[English README](../README.md)

GUI 之外,伺服器可跑在遠端主機上:遠端跑獨立的**節點代理**(`cmd/agent`,純命令列單一執行檔,無需 GUI/WebView2/NSIS),本機 GUI 經 HTTPS+bearer token 管理。Linux 節點(如 Ubuntu 雲端主機)需安裝 Docker Engine(Linux 一律以 Docker 後端執行遊戲;native 為 Windows 專屬)。

## 部署流程

1. 交叉編譯並上傳:`GOOS=linux GOARCH=amd64 go build -o servermonitor-agent ./cmd/agent`,scp 到遠端主機。
2. 首次啟動 `./servermonitor-agent`(可零設定;選用 `--config agent.toml` 自訂 bind/資料目錄/TLS)。首啟自動生成**持久 token** 與**自簽 TLS 憑證**,console 印出監聽位址、憑證 SHA-256 指紋與 token。
3. GUI「節點」頁 →「新增節點」:填名稱、位址(`https://主機:9444`)與 token →「測試連線」→ 比對顯示的指紋與 agent console 印出者一致 →「確認新增」(TOFU 指紋釘選;之後憑證被換會直接拒連)。也支援自備正式憑證(agent.toml `[tls] mode="custom"`,GUI 留空指紋走系統 CA)。
4. 建立伺服器時精靈第一步選擇節點即可;防火牆需放行 agent 監聽埠。

## 安全注意

token 與 TLS 私鑰存於 agent 資料目錄(unix 權限 0600;Windows 請以 NTFS 權限保護);`[tls] mode="off"` 為明文 HTTP,僅限內網/VPN(如 WireGuard/Tailscale)。

## 目前限制

遠端節點的 Docker 能力於 GUI 顯示「未知」、遠端實例的背景監控/對帳尚未啟用(操作與日誌均可用),備份檔留在其所屬節點。
