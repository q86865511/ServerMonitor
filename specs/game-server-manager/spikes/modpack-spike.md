# 模組 / 模組包 spike(T14 前置 gate)

> 對應 R11。結論以 itzg 官方文件(readthedocs)與 CurseForge 官方條款為準;查證日期 2026-07-14。
> 原則(已裁決,design.md H10/rev.2):**itzg 為 `/data` 唯一安裝擁有者;工具絕不自行解壓到 `/data`。**
> 本工具只負責:選型別(變體)、透傳 itzg env、把「手動受限檔案」交給 itzg(掛載 + 對應 env),不自建下載器。

---

## 1. 動作表(工具該做什麼 → 設哪個 itzg env)

| 來源(我方 `ModpackSource.Type`) | itzg `TYPE` | 指定模組包的 env | 檔案是否需掛載進容器 | 備註 |
|---|---|---|---|---|
| `modrinth`(slug/URL) | `MODRINTH` | `MODRINTH_MODPACK`=slug／project id／page URL／version URL／自架 `.mrpack` URL | 否 | itzg 自動裝對應 loader(forge/fabric/quilt)|
| `manual-mrpack`(本機 `.mrpack`) | `MODRINTH` | `MODRINTH_MODPACK`=**容器內路徑**(如 `/modpacks/foo.mrpack`,須 `.mrpack` 副檔名)| **是**(bind mount)| 檔案須先掛載進容器 |
| `curseforge`(slug/URL) | `AUTO_CURSEFORGE` | `CF_PAGE_URL`=URL **或** `CF_SLUG`=slug | 否 | 需 `CF_API_KEY` |
| `manual-cfzip`(本機 zip) | `AUTO_CURSEFORGE` | `CF_MODPACK_ZIP`=**容器內路徑**(如 `/modpacks/pack.zip`)| **是**(bind mount)| 仍需 `CF_API_KEY`;可搭 `CF_MODPACK_MANIFEST` |

關鍵:選了模組包時,itzg 的 `TYPE` 由模組包機制接管(`MODRINTH` / `AUTO_CURSEFORGE`),**覆寫變體宣告的 `TYPE`**(如 `PAPER`/`FORGE`)。loader 由模組包內容決定,itzg 自行安裝。

### CF_API_KEY 交付
- 走既有 SecretRef 機制:範本宣告 `[[secrets]] key="CF_API_KEY"`(非必填——未被任何 command_protocol 引用,故 `requiredSecretKeys` 不會強制;僅在選 CurseForge 來源時由 T14 前置檢查要求)。
- runtime 以明文 env 注入(R12 例外,loopback 信任域);DB 不落明文;keyring 以 UUID 命名空間存。
- itzg 亦支援 `CF_API_KEY_FILE`(docker secret `/run/secrets/cf_api_key`);首版沿用既有 env 注入路徑,不另走 file secret。

## 2. loader / 遊戲版本支援矩陣(前置檢查用)

itzg Modrinth 模組包明文支援 loader:**forge、fabric、quilt**(`MODRINTH_LOADER` 可narrow;neoforge/paper 未列於 Modrinth 模組包頁)。
AUTO_CURSEFORGE 依模組包 manifest 決定 loader(forge/fabric/neoforge 常見)。

**相容性判準(我方矩陣,建立前靜態檢查):**

| 變體 loader | 可否套用「模組包來源」 | 理由 |
|---|---|---|
| `forge` / `fabric` / `quilt` / `neoforge` | 可 | 模組 loader,itzg 可在其上裝模組包 |
| `vanilla` | 可 | `TYPE=MODRINTH/AUTO_CURSEFORGE` 覆寫,itzg 自裝 loader |
| `paper` / `spigot` / `bukkit` | **否** | 外掛平台(Bukkit API),無法載入 Forge/Fabric 模組;選模組包 → 建立前報錯 |

範本以 `[mods].modpack_loaders` 宣告「允許套用模組包的變體 loader 集合」;`[[variants]].loader` 宣告各變體 loader。
前置檢查:選了模組包但變體 loader 不在 `modpack_loaders` → `ErrLoaderIncompatible`,建立前擋、無副作用。
遊戲版本相容(Minecraft 版本 × loader 版本)由 itzg 於容器內解析;我方僅擋「型別層級」不相容(plugin vs mod),版本層級交 itzg 回報(啟動 log),不在核心重造版本表。

## 3. 受限(第三方 opt-out)檔案處理

- CurseForge 部分模組作者關閉第三方 API 散佈(`CF_FORCE_INCLUDE_MODS` **無法**下載,官方明文)。
- itzg 機制:把手動下載的檔案放進容器 `/downloads`(`CF_DOWNLOADS_REPO`,預設 `/downloads`)下 `mods`/`modpacks`/`worlds` 子目錄,itzg 從該處取檔補齊。
- 我方立場:與「手動 modpack 檔」同一交付管道(bind mount 一個宿主目錄進容器),env 指向容器內路徑;**工具不解壓、不碰 `/data`**。首版把 `/downloads` 補檔列為與 `manual_mount` 相同的掛載能力,GUI(T15)提供放檔位置。

## 4. CurseForge API 條款(金鑰 / attribution)

- **金鑰使用者自申**:CurseForge 3rd-Party API 金鑰「non-transferable、不得分享給任何第三方」。故**我們不得內建/散佈自己的金鑰**;由終端使用者自行到 console.curseforge.com 申請並填入(SecretRef=`CF_API_KEY`)。此為我方設計(金鑰使用者自填)的條款依據。
- **attribution**:CurseForge 以「每個整合用自己的金鑰認證」來把下載歸因到來源平台;使用者用自己的金鑰即滿足歸因,無需我方額外註記義務。
- 結論:設計不變——`CF_API_KEY` 為 SecretRef、使用者自填、不內建金鑰。

## 5. 對實作的結論(供 T14 第二步)

1. `CreateOptions.Modpack *ModpackSource`(type + ref);type ∈ {modrinth, curseforge, manual-mrpack, manual-cfzip}。
2. 前置檢查(建立前、無副作用):
   - 範本須有 `[mods]`;變體 loader ∈ `modpack_loaders`,否則 `ErrLoaderIncompatible`。
   - curseforge / manual-cfzip 須提供 `CF_API_KEY` secret,否則 `ErrModpackAPIKeyRequired`。
   - manual-mrpack / manual-cfzip:以 `archive/zip` 驗格式(mrpack=zip 含 `modrinth.index.json`;curseforge-zip=zip 含 `manifest.json`),不合 → `ErrModpackFormat`。
3. `buildSpec`:依來源設 env(`TYPE` 覆寫變體 + `MODRINTH_MODPACK`/`CF_PAGE_URL`/`CF_SLUG`/`CF_MODPACK_ZIP`);`CF_API_KEY` 沿用既有 secret→env 注入。
4. 手動檔:env 指向容器內 `manual_mount/<檔名>`;掛載點加入 spec(bind mount)。

## 6. 待查證 / 已知邊界(不作為既定事實)

- **手動檔跨 core→agent 邊界的實體傳輸**:`MODRINTH_MODPACK`(本機檔)與 `CF_MODPACK_ZIP` 都要求檔案**已在容器內**,即需把宿主檔 bind mount 進容器。現行 `protocol.InstanceSpec`(T9 領域,dto.go)只有 `DataDirs []string`(容器路徑,由 agent 從自有 DataRoot 掛空目錄),**無承載檔案位元組或任意 mount 來源的欄位**,agent 亦無檔案上傳端點。故「手動檔位元組送達 agent 宿主掛載目錄」需 T9 邊界新增能力(InstanceSpec 專屬 mount/payload 欄位,或 agent 上傳端點)。T14 已完成 core 側:格式驗證、env/掛載點決策(spec 塑形);實體傳輸列為 T9 協調項(見回報)。
- **loader 版本細粒度相容**:交 itzg 於啟動解析,核心只擋型別層級(plugin vs mod)。
- **neoforge**:Modrinth 模組包頁未明列;AUTO_CURSEFORGE 依 manifest 支援。矩陣以 `modpack_loaders` 範本宣告,未來新 loader 加字串即可,不改核心。

## 來源

- itzg Modrinth 模組包:https://docker-minecraft-server.readthedocs.io/en/latest/types-and-platforms/mod-platforms/modrinth-modpacks/
- itzg AUTO_CURSEFORGE:https://docker-minecraft-server.readthedocs.io/en/latest/types-and-platforms/mod-platforms/auto-curseforge/
- itzg 手動 CurseForge server pack(`CF_SERVER_MOD`,已標 Deprecated,新部署用 AUTO_CURSEFORGE):https://docker-minecraft-server.readthedocs.io/en/latest/types-and-platforms/mod-platforms/curseforge/
- CurseForge 3rd-Party API 條款(金鑰 non-transferable / attribution):https://support.curseforge.com/support/solutions/articles/9000207405-curseforge-3rd-party-api-terms-and-conditions
- CurseForge API 金鑰申請:https://support.curseforge.com/support/solutions/articles/9000208346-about-the-curseforge-api-and-how-to-apply-for-a-key
