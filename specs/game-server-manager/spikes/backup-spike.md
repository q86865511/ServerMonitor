# 備份 Spike(T4 — `DockerBackend.{Archive,Restore}` 前置 gate)

> 目的:實測在 Windows / Docker Desktop(WSL2 後端)下,以 **host bind mount** 作為實例資料目錄,停機後從 host 端 tar 打包 + checksum、再解包還原的一致性與可行性,供 T4 `DockerBackend` 的 `Archive`/`Restore` 據以實作。
> 產出日期:2026-07-13。查證方式:**真 Docker 實跑**(daemon 29.4.1、busybox 映像)+ Go 1.26.5 rename 語意實測。
> **標記慣例**:【實測】= 本 spike 真跑驗證;【推論】= 由實測合理推導、未逐案窮舉;【待查】= 需後續真遊戲映像確認。

---

## 結論摘要(≤5 行)

1. **可行**:host bind mount 作實例資料目錄,容器寫入即時對 Windows host 可見;`docker stop` 後 host 端讀到的資料與容器內 **checksum 逐一相符**——停機快照具一致性。
2. **完整迴圈驗證通過**:host 端 tar 打包 → sha256 → 解包到 staging → 逐檔 checksum 相符 → 新容器掛還原資料讀取正確。「停機快照 + bind mount」首版方案成立,無須改為 named volume。
3. **原子切換用 rename-aside**:Windows `os.Rename` **不能**覆蓋既有目錄(Access denied);Restore 必須「先把現行資料 rename 到 `.old`,再把 staging rename 就位,最後刪 `.old`」。同卷 rename 為 metadata 操作,足夠原子;跨步驟崩潰由 core journal 復原。
4. **實作定案**:`Archive` 用 Go `archive/tar` 打包實例資料根 + `crypto/sha256` 邊寫邊算 checksum;`Restore` 先驗 checksum 再解包 staging、rename-aside 切換、建**新**容器回傳新 RuntimeID。備份根由 agent 擁有(`<backupRoot>/<uuid>/<backupID>/`),對外 opaque `BackupID`。
5. **待查**:真遊戲映像(itzg Minecraft、Palworld)資料含 symlink/大量小檔時的打包耗時與 symlink 保真 — 見文末清單。(截至 2026-07-21 仍為開放項:T16 E2E 的備份/還原僅覆核 vanilla Paper 資料,未含 Modrinth 模組安裝或 symlink 情境)

---

## 1. 實測環境

| 項目 | 值 |
|---|---|
| OS | Windows 11 Pro(26200) |
| Docker | Docker Desktop,daemon **29.4.1**,WSL2 後端 |
| 測試映像 | `busybox`(輕量,驗證資料面即可) |
| host 資料根 | `C:\Users\...\Temp\gsm-spike\inst1\`(NTFS,`C:` 於 Docker Desktop 預設共用) |
| Go | 1.26.5 windows/amd64(rename 語意實測) |

---

## 2. 實測步驟與結果【實測】

### 2.1 bind mount 寫入與跨界可見性

- `docker run -v <host>/inst1/data:/data busybox`,容器內寫 `save.txt`(12B)與 `world/region.bin`(1 MiB 隨機)。
- 容器端 `sha256sum`:
  - `save.txt` = `d79f2e37784e5cd8631963896ebc6c9c66934af94a1854504717eaec04bc3d09`
  - `world/region.bin` = `1e9495e103521f4a8d311bbb30f52936d47f0dcfedb16e653060eafba041e990`
- **停機前** Windows host 端即可列出 `save.txt`、`world/region.bin`(大小相符)。→ bind mount 雙向即時可見。

### 2.2 停機後一致性(核心問題)

- `docker stop gsm_spike1`(優雅停機)。
- **停機後** host 端 `sha256sum` 兩檔 = 與 2.1 容器端**完全相同**。
  → **結論:`docker stop` 後從 Windows host 讀取的資料是一致快照**,無 WSL2 檔案共用層的殘留/未刷寫問題。容器程序已結束、無檔案鎖,打包安全。

### 2.3 完整備份/還原迴圈

| 步驟 | 動作 | 結果 |
|---|---|---|
| tar | host 端 `tar -cf bkp1.tar -C inst1/data .` | 產出 1054720B,archive sha256 = `b1c7d93a…` |
| 解包 | 解到 `inst1_restore_staging/`(staging) | 成功 |
| 驗檔 | staging 內兩檔 `sha256sum` | 與原始**逐一相符** |
| 新容器 | 以 staging 為 `/data` 掛新 busybox 容器 | `docker logs` 顯示還原資料 checksum 正確 |

→ 停機快照 → 打包 → checksum → 解包 → 換新容器,資料**全程無失真**。

### 2.4 Windows 目錄 rename 語意(Restore 原子切換)【實測 Go 1.26.5】

- `os.Rename(staging, live)` 當 `live` **已存在** → **失敗**:`Access is denied`。
  (POSIX 可覆蓋、Windows 不行——這是 Restore 必須處理的平台差異。)
- **rename-aside** 模式成功:`Rename(live, live.old)` → `Rename(staging, live)` → `RemoveAll(live.old)`;最終 `live` 內容為還原值。
  → Restore 採此三步;`live.old` 殘留即代表切換中途崩潰,交 core journal 對帳清理(design 流程 5 crash-safe)。

---

## 3. 對 `DockerBackend` 實作的定案

- **host 版面**:每實例一個資料根 `<dataRoot>/<uuid>/`,其下依範本 `data_dirs` 各一子目錄(如 `/data` → `<dataRoot>/<uuid>/data`);控制檔 `instance.json`(spec 快照)置於資料根、**不在** bind mount 內,故不落入容器、也不進備份 tar。
- **Archive**(呼叫端已 planned-stop):Go `archive/tar` 打包資料根下各 data_dir 子目錄 → 邊寫邊 `sha256`;產物與 `meta.json`(`BackupMeta` + spec 快照)落 `<backupRoot>/<uuid>/<backupID>/`。`BackupID` opaque,uuid 關聯藏在路徑。
- **Restore**:讀 backup → 重算 tar sha256 與 `meta.json` 比對(staging 驗證)→ 解包到 `<dataRoot>/<uuid>/.gsm-restore-*` → 對每個 data_dir 子目錄 rename-aside 切換 → 依 backup 的 spec 建**新**容器(同 uuid、同 bind)→ 回新 RuntimeID。備份自包含,不依賴舊容器仍存在。
- **checksum**:`sha256` 十六進位存 `BackupMeta.Checksum`;Restore 前必先驗。
- **Purge**:`Remove` 帶 `Purge` 才刪 `<dataRoot>/<uuid>` 與 `<backupRoot>/<uuid>`;預設保留。

---

## 4. 已知邊界與待查

- 【推論】**bind mount 效能**:Windows→容器 bind mount 經 WSL2 檔案共用,I/O 明顯慢於 named volume / WSL2 原生路徑。備份是停機一次性打包,可接受;但**執行期**重 I/O 遊戲(大型 Minecraft 世界)的日常讀寫可能受影響——非 v1 阻斷項,未來可評估 named volume + 原子交換(design 已列為替代方案)或把 dataRoot 放 WSL2 原生路徑。
- 【推論】**權限模型**:Docker Desktop 的 Windows bind mount 對容器呈「寬鬆」權限(檔案不論容器 UID 皆可讀寫),故 itzg(uid 1000)可寫入 `/data`;與 Linux 原生 bind mount 的 uid 對應不同,但對本工具有利(免 chown)。
- 【待查】**symlink / 大量小檔**:真遊戲資料(itzg 外掛、模組、Palworld 存檔)若含 symlink 或數萬小檔,`archive/tar` 的保真與耗時需以 T4 真 Docker 整合測試(Minecraft/Palworld)與 T14 模組情境覆核;首版 tar 以「跟隨檔案內容、記錄相對路徑」為準,symlink 依 `archive/tar` 預設行為處理。(截至 2026-07-21 仍為開放項:`internal/app/e2e_docker_test.go` 的 T16 E2E 備份/還原僅用未安裝模組的 vanilla Paper 資料,未實測 symlink 或大量小檔情境)
- 【待查】**跨磁碟區 dataRoot/backupRoot**:若使用者把 backupRoot 設在不同磁碟區,Restore 的 staging 應與 dataRoot 同卷以保 rename 原子性;實作把 staging 建在 `<dataRoot>/<uuid>/` 底下即同卷,已規避。(已由 T4 實作覆核,2026-07:`internal/agent/docker_backup.go:115` 的 staging 路徑確為 `filepath.Join(root, ".gsm-restore-...")`,與 dataRoot 同卷)
