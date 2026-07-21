# native-backend — 任務清單（tasks.md）

> 建立日期：2026-07-16｜狀態：已核可（2026-07-16）;實作完成 2026-07(任務全數收官,見 PROGRESS.md)
> 本檔為 /pipeline 任務清單來源：pipeline 第 1 步直接採用本清單與 HARD/NORMAL 標記，不重新拆解。
> 勾選（`- [x]`）只由 pipeline 第 5 步收尾回寫，其他階段不動。

## 任務

- [x] T1 [HARD] protocol/範本層擴充：InstanceSpec 加 Runtime/Modpack 可選欄位、NativeSpec schema（provision/launch/config 具名編碼器）、template_engine 驗證擴充（runtime 允許值 docker/native、預設 runtime 語意、能力由區段推導、缺區段報明確欄位路徑）、minecraft/palworld 範本增補 `[native]` 區段（對應 R2/R3；驗收：範本驗證表格測通過、既有範本 docker 路徑回歸測通過、`go build ./...` 綠；依賴：無）
- [x] T2 [HARD] provision 子套件基礎＋JRE 供應：Provisioner 骨架與 progress 回呼契約、下載/sha 校驗/解壓共用工具（校驗通過才入快取、不留半成品）、JavaProvisioner（Adoptium API、`<CacheRoot>/jre/<major>/` 共用快取、離線明確錯誤）（對應 R4；驗收：httptest 假 Adoptium 單元測——快取命中不重下、checksum 不符中止、離線錯訊；依賴：無）
- [x] T3 [NORMAL] MC 安裝器：Vanilla（Mojang manifest＋sha1）＋Paper（PaperMC API＋sha256）＋eula.txt 寫檔（對應 R5；驗收：httptest 單元測含校驗失敗路徑；依賴：T2）
- [x] T4 [HARD] MC 安裝器：Fabric＋Forge＋NeoForge——先 spike 確認各 installer 輸出結構（Q2），以官方 installer 產出的啟動腳本/args 檔為啟動來源不自解析（對應 R5；驗收：spike 結論記錄於 PR、installer 執行整合測（Windows＋真 JRE）產出可啟動配置；依賴：T2、T3）
- [x] T5 [NORMAL] SteamCMDProvisioner：Valve 匿名載點下載、`<CacheRoot>/steamcmd/` 共用快取、app_update、update_on_start 支援（對應 R6；驗收：假載點單元測＋快取共用測；真 app_update 歸 T13 E2E；依賴：T2）
- [x] T6 [HARD] NativeBackend 本體＋procSupervisor：13 方法全實作、proc.json（PID＋start-time）、stdout/stderr 滾動日誌＋tail Logs(follow)、Stop 依 Grace 後強殺行程樹、Archive/Restore（把 docker_backup.go 的 tar/checksum 抽共用後復用）、backend 契約測試套件（Mock/Docker/Native 同組）（對應 R1/R7/R10；驗收：契約測試三後端通過、日誌滾動不中斷串流測、`go test ./...` 綠；依賴：T1、T2）
- [x] T7 [HARD] Job Objects：記憶體＋CPU rate 上限強制、行程樹統計填 ResourceStats（語意對齊 Docker 正規化）、oom 事件（區別 crash）、不設 KILL_ON_JOB_CLOSE、收養後重掛（失敗降級僅監督＋可辨識事件）、x/sys 提直接相依（對應 R8/R9；驗收：真 Windows 測——超限終止產 oom 事件、CPU 實測占用（容忍誤差）、agent 退出伺服器續活；依賴：T6）
- [x] T8 [HARD] dispatchBackend＋bootstrap 泛化：前綴路由、List/Events 聚合與 docker 軟降級、BackendOptions/BackendFactory 簽章泛化、CacheRoot 目錄骨架、RetryDocker 改重建 docker 子後端、節點在線與 Docker 能力訊號分離（對應 R2/R13；驗收：路由單元測（未知 runtime 4xx、docker down 時 native 照常）、既有 bootstrap 測試回歸；依賴：T6）
- [x] T9 [HARD] core runtime 選擇鏈＋模組 owner 分派重構：buildSpec 填 Runtime/Modpack、runtimePolicy（Windows 預設 native、Linux 擋 native、範本能力交集）、applyModpackEnv gate 於 runtime==docker（itzg env 只在 docker 生效）（對應 R2/R11；驗收：GOOS 注入單元測（Linux 拒 native）、docker 實例建立回歸測、itzg gate 測；依賴：T1、T8）
- [x] T10 [NORMAL] ModrinthInstaller：mrpack 解析、依賴下載、落位 instance mods 目錄、版本相容錯誤、手動上傳（Mounts）native 路徑驗證、ModProvider 擴充點介面定型（對應 R11；驗收：httptest＋假 mrpack 單元測、手動上傳整合測；依賴：T2、T9）
- [x] T11 [HARD] 收養與對帳整合：NativeBackend 建構掃描收養（PID 活→running 重掛 Job；死→合成 die 走崩潰復原）、Reconciler 對 native refs 對帳驗證（DB/實際不一致修正）、docker daemon down 不影響 native 對帳（對應 R7/R8/R13；驗收：真 Windows 收養整合測（重啟 agent 行程仍在）、對帳單元測（假 backend）；依賴：T6、T7、T8）
- [x] T12 [HARD] GUI：bindings 加法（TemplateDTO.Runtimes、CreateInstanceRequest.Runtime、InstanceDTO.Runtime、NodeStatusDTO.DockerAvailable、DockerAvailable()——方法名/既有簽章零變更）、Svelte 建立表單 runtime 選擇（Windows 預設 native、docker 不可用置灰＋原因）、供應進度條（訂閱 provision 事件）、native/docker 差異提示（對應 R12；驗收：綁定單元測、前端建置綠、手動走查截圖；依賴：T8、T9）
- [x] T13 [HARD] E2E＋文件＋既有規格修訂：乾淨 Windows 無 Docker 全流程 E2E（建立→啟動→主控台→備份→還原，build tag `native`）、Palworld 真 SteamCMD E2E（可 skip）、docker↔native 備份互轉整合測、CLAUDE.md 常用指令節/README 更新、specs/game-server-manager 三件套加註修訂驗證（不改原文語意、不動勾選）（對應 R10/R12＋修訂清單；驗收：E2E 實跑輸出、修訂 diff 列於 PR；依賴：T1–T12）
- [x] T14 [HARD] CurseForge（可獨立延後）：CurseForgeInstaller（官方 API、實作 ModProvider）、key build-time 注入＋設定覆蓋、無 key 建置隱藏 CF 選項、BlockedMods 對話框＋監看資料夾 hash 匹配自動匯入、條款揭露文件（對應 R14；驗收：httptest 單元測（正常/downloadUrl=null/fallback 匯入）、無 key 建置回歸測；真 API E2E 待 key 取得後補；依賴：T10、T12＋使用者取得 API key）

<!-- HARD＝架構設計、複雜邏輯、跨模組（pipeline 派 architect）；NORMAL＝規格明確照做（派 implementer）。
     雙向覆蓋檢查（2026-07-16）：
     R1→T6｜R2→T1,T8,T9｜R3→T1｜R4→T2｜R5→T3,T4｜R6→T5｜R7→T6,T11｜R8→T7,T11｜
     R9→T7｜R10→T6,T13｜R11→T9,T10｜R12→T12,T13｜R13→T8,T11｜R14→T14；
     每條任務均對應至少一個 R#，每條 R# 均被至少一條任務覆蓋。✓ -->

## 完成定義

- 全部任務勾選完成（T14 可依裁決延後，延後時於 PROGRESS.md 記錄），且各任務驗收條件有實跑證據。
- requirements.md 每條 R# 的驗收條件通過 pipeline 雙審對照。
- 相關文件（PROGRESS.md、README、專案 CLAUDE.md 指令節）依全域規則更新。
