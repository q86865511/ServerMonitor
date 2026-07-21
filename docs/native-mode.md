# Native 執行模式(免 Docker,Windows)

> 返回 [README](../README.zh-TW.md)｜[English README](../README.md)

除了 Docker 執行後端,Windows 上另提供 **native 執行後端**:伺服器以本機子行程直接執行,**只需安裝本主程式即可**——JVM(Adoptium JRE)、Minecraft 各 loader 伺服器檔案、Palworld 的 SteamCMD 全部由工具自動下載供應,無任何外部安裝需求(乾淨 Windows 機器亦可全流程建立→啟動→主控台→備份)。

- **runtime 選擇(逐實例)**:建立實例時可選執行後端。**Windows 預設 native**;偵測到 Docker 可用時亦可改選 docker(Docker 不可用時 docker 選項置灰並說明原因)。Linux 平台強制 Docker(native 選項不可見)。範本以 `[docker]`/`[native]` 區段宣告支援哪些後端(兩者至少其一;僅含其一時該遊戲只能以該 runtime 建立)。
- **支援矩陣**:
  - Minecraft 五種 loader:**Vanilla**(Mojang manifest + sha1)、**Paper**(PaperMC v3 Fill API + sha256)、**Fabric**(官方 installer)、**Forge**/**NeoForge**(官方 `--installServer` 產出 `run.bat` + `user_jvm_args.txt`,由後端於啟動期橋接為腳本啟動、記憶體上限注入 args 檔)。
  - **Palworld**:SteamCMD 匿名載點下載 + `app_update 2394010`;`update_on_start` 可於每次啟動前檢查更新。
- **資源上限(Windows Job Objects)**:記憶體/CPU 上限與 Docker 後端同一實例設定來源,以 Job Objects 強制;記憶體超限產生可辨識的「超出記憶體上限」告警(區別於一般 crash)。agent 退出不連坐殺伺服器——重啟後自動收養仍在執行的行程、重新納入 Job 管理。
- **與 Docker 模式的差異**:native **無容器級隔離**(檔案系統/網路);埠衝突由 OS bind 失敗直接回報(而非 Docker 埠映射)。監控/日誌/指令/備份/自動重啟/告警等上層行為與 Docker 後端一致,GUI 無須分辨後端。
- **備份互通**:native 與 docker 後端的備份格式相同(停機一致 tar 快照 + checksum),**同一實例資料可經備份在兩後端間互轉**(同範本、同 data_dirs 前提)。
- **共用快取**:JRE 與 SteamCMD 為跨實例共用快取(位於 `%LOCALAPPDATA%\ServerMonitor\cache\`),不隨實例重複下載;解除安裝可整目錄清除。

## CurseForge 模組包(native 模式,R14)

native 執行後端(免 Docker)可直接安裝 CurseForge 模組包,無須使用者自行處理外部工具。CurseForge 官方 API 需金鑰,行為比照 [Prism Launcher](https://prismlauncher.org/)。

- **金鑰為本專案專屬**:CurseForge 第三方 API 金鑰以編譯期 `-ldflags "-X servermonitor/internal/agent/provision.curseforgeAPIKey=<key>"` 注入,**不進 repo**。未內嵌金鑰的建置:GUI 的 native CurseForge 選項自動隱藏/置灰,其餘功能(含 Modrinth)不受影響。
- **衍生作品須自行申請金鑰**:依 CurseForge 第三方 API 條款,fork/衍生作品**不得沿用**本專案金鑰,須至 [console.curseforge.com](https://console.curseforge.com/) 申請自己的金鑰並替換注入值。
- **使用者可覆蓋金鑰**:**建議於 GUI「設定」頁的「CurseForge 模組包金鑰」直接填入/清除**——金鑰即存入 OS 金鑰庫、不明文落檔,且**即時生效免重啟**(建立精靈的 CurseForge 選項置灰狀態隨即更新)。或於設定檔(`%LOCALAPPDATA%\ServerMonitor\config.json`)填 `"curseforge_api_key": "<你的金鑰>"` 覆蓋內嵌金鑰(比照 Prism)。有覆蓋金鑰時即使建置未內嵌金鑰,native CurseForge 亦啟用。**經設定檔填入的明文金鑰,下次啟動時會自動遷入 OS 金鑰庫並自 `config.json` 移除**(金鑰不明文常駐設定檔);之後覆蓋值改由金鑰庫讀取。
- **來源填法(native)**:native CurseForge 遠端來源的 ref 需填 `projectID:fileID`(模組包檔的兩個數字 ID)或 cfzip 的**直接下載連結**(https);不支援 slug 或專案頁 URL。
- **作者停用第三方散布的模組(opted-out)**:部分模組作者停用第三方 API 下載(CurseForge API 回 `downloadUrl` 為 `null`)。系統會先安裝其餘可下載的模組與 overrides,再於建立精靈彈出「需手動下載的模組」對話框,列出各模組並附「開啟下載頁」按鈕。請以瀏覽器手動下載對應檔案,放入匯入資料夾 `<資料根>\cache\cf-imports\` 後**重新建立**——系統以檔名(及檔案長度)比對自動匯入續裝(匯入資料夾為共用快取,不隨個別實例刪除)。此為誠實的降級流程:因建立為同步阻塞,不採「暫停等待」以免與建立逾時衝突。
- **手動 CurseForge zip**:亦可選「手動 CurseForge zip 檔」來源,提供本機 cfzip 完整路徑直接安裝(同樣需啟用金鑰以查詢各模組下載連結)。

> 註:Docker 執行後端的 CurseForge 走 itzg 映像的 `AUTO_CURSEFORGE`,由**使用者自填** `CF_API_KEY`(存 OS 金鑰庫),與 native 的專案內嵌金鑰是兩條獨立路徑。
