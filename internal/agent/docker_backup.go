package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"

	"servermonitor/internal/protocol"
)

// 備份的打包/解包/checksum/原子切換與備份記錄讀寫等 package 內共用核心已抽至 backup_archive.go
// (native-backend R10:與 NativeBackend 共用、格式互通)。本檔僅保留 DockerBackend 專屬的
// Archive/Restore 編排(取 uuid、重建容器、跨實例還原防護)。

// Archive 對(呼叫端已 planned-stop 的)實例做一致快照:tar host bind mount 資料 + sha256
// checksum,連同 spec 快照落 agent 擁有的備份根,回傳對外 opaque 的 BackupID(R9)。
func (b *DockerBackend) Archive(ctx context.Context, id protocol.RuntimeID) (protocol.BackupID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	j, err := b.cli.ContainerInspect(ctx, string(id))
	if err != nil {
		return "", mapDockerErr(err)
	}
	if j.Config == nil {
		return "", fmt.Errorf("agent: 容器 %s 無 Config,無法封存", id)
	}
	uuid := j.Config.Labels[labelUUID]
	if uuid == "" {
		return "", fmt.Errorf("agent: 容器 %s 缺 gsm.uuid 標籤,無法封存", id)
	}
	// spec 以資料根的快照為準;缺檔時由 inspect 盡力重建(TemplateID 可能遺失)。
	spec, serr := b.readInstanceSpec(uuid)
	if serr != nil {
		spec = specFromInspect(j)
	}

	backupID := protocol.BackupID(strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + shortRand())
	bkpDir := filepath.Join(b.backupInstanceRoot(uuid), string(backupID))
	if err := os.MkdirAll(bkpDir, 0o755); err != nil {
		return "", fmt.Errorf("建立備份目錄失敗: %w", err)
	}

	checksum, err := b.tarInstanceData(uuid, filepath.Join(bkpDir, backupDataFile))
	if err != nil {
		_ = os.RemoveAll(bkpDir)
		return "", fmt.Errorf("封存資料失敗: %w", err)
	}

	rec := backupRecord{
		Meta: protocol.BackupMeta{
			BackupID:     backupID,
			InstanceUUID: uuid,
			Game:         spec.TemplateID,
			TsUTC:        time.Now().UTC(),
			Checksum:     checksum,
		},
		Spec: spec,
	}
	if err := writeBackupRecord(bkpDir, rec); err != nil {
		_ = os.RemoveAll(bkpDir)
		return "", err
	}
	return backupID, nil
}

// Restore 以備份還原:驗 checksum → 解包 staging → rename-aside 原子切換資料 →
// 依備份 spec 建「新」容器回傳新 RuntimeID(R9;舊容器留給 core 切換)。
func (b *DockerBackend) Restore(ctx context.Context, id protocol.RuntimeID, bid protocol.BackupID) (protocol.RuntimeID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	// 先由舊容器標籤取 uuid(盡力);備份自包含,uuid 最終以備份記錄為準。
	inspectedUUID := ""
	if j, ierr := b.cli.ContainerInspect(ctx, string(id)); ierr == nil && j.Config != nil {
		inspectedUUID = j.Config.Labels[labelUUID]
	}
	bkpDir, rec, err := b.findBackup(inspectedUUID, bid)
	if err != nil {
		return "", err
	}
	uuid := rec.Meta.InstanceUUID
	if uuid == "" {
		return "", fmt.Errorf("agent: 備份 %s 缺 InstanceUUID", bid)
	}
	// 防跨實例還原:目標容器 uuid 已知且與備份記錄不符時拒絕(避免以 A 的端點提交 B 的
	// BackupID 而改到 B)。inspectedUUID 為空(舊容器已不存在)才容許純以備份記錄重建。
	if inspectedUUID != "" && inspectedUUID != uuid {
		return "", fmt.Errorf("agent: 備份 %s 屬實例 %s,與目標實例 %s 不符,拒絕還原", bid, uuid, inspectedUUID)
	}

	// staging 驗證:重算 tar checksum 與備份記錄比對。
	tarPath := filepath.Join(bkpDir, backupDataFile)
	sum, err := fileSHA256(tarPath)
	if err != nil {
		return "", fmt.Errorf("讀取備份資料失敗: %w", err)
	}
	if sum != rec.Meta.Checksum {
		return "", fmt.Errorf("agent: 備份 %s checksum 不符,拒絕還原", bid)
	}

	root := b.instanceDataRoot(uuid)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("建立實例資料根失敗: %w", err)
	}
	staging := filepath.Join(root, ".gsm-restore-"+shortRand())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", fmt.Errorf("建立還原 staging 失敗: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := untar(tarPath, staging); err != nil {
		return "", fmt.Errorf("解包備份失敗: %w", err)
	}

	// 逐 data_dir 子目錄 rename-aside 原子切換;全有或全無:任一步失敗即反向回滾至還原前狀態。
	commit, rollback, err := applyRestoreSwap(root, staging, os.Rename)
	if err != nil {
		return "", err
	}

	// 依備份 spec 建新容器(同 uuid、同 bind),回傳新 RuntimeID;不自動啟動。
	// Create 失敗時回滾資料(換回舊資料),使實例資料與還原前完全一致。
	newID, err := b.Create(ctx, rec.Spec)
	if err != nil {
		rollback()
		return "", fmt.Errorf("還原後建立新容器失敗: %w", err)
	}
	// Create 成功後才永久刪除換出的舊資料(trash),確保刪除前新容器已就緒。
	commit()
	return newID, nil
}

// findBackup 定位備份目錄與記錄。uuid 已知時只在該 uuid 目錄下找,找不到即回 ErrNotFound
// (不跨 uuid 掃描——避免以某實例端點提交他實例 BackupID 時誤配到別的實例);僅 uuid 為空
// (舊容器已不存在)才容許掃描所有 uuid 目錄。
func (b *DockerBackend) findBackup(uuid string, bid protocol.BackupID) (string, backupRecord, error) {
	// 路徑遍歷防禦(還原路徑):bid 同樣充當目錄名 join,未消毒可逃逸備份根定位到任意 meta.json。
	if err := validateBackupID(bid); err != nil {
		return "", backupRecord{}, err
	}
	tryDir := func(dir string) (string, backupRecord, bool) {
		rec, err := readBackupRecord(dir)
		if err != nil {
			return "", backupRecord{}, false
		}
		return dir, rec, true
	}
	if uuid != "" {
		dir := filepath.Join(b.backupInstanceRoot(uuid), string(bid))
		if d, rec, ok := tryDir(dir); ok {
			return d, rec, nil
		}
		return "", backupRecord{}, ErrNotFound // 不 fallback 跨 uuid 掃描
	}
	// uuid 為空:掃描所有 uuid 目錄。
	uuids, err := os.ReadDir(b.backupRoot)
	if err == nil {
		for _, u := range uuids {
			if !u.IsDir() {
				continue
			}
			dir := filepath.Join(b.backupRoot, u.Name(), string(bid))
			if d, rec, ok := tryDir(dir); ok {
				return d, rec, nil
			}
		}
	}
	return "", backupRecord{}, ErrNotFound
}

// tarInstanceData 打包實例資料根下各 data_dir(排除中繼/mounts/暫存),回傳 sha256 checksum。
// 打包核心已抽至共用 tarDir(backup_archive.go);本方法保留為 DockerBackend 的薄包裝
// (以 uuid 定位資料根),既有測試與呼叫端不變。
func (b *DockerBackend) tarInstanceData(uuid, tarPath string) (string, error) {
	return tarDir(b.instanceDataRoot(uuid), tarPath)
}

// specFromInspect 由容器 inspect 盡力重建 InstanceSpec(缺 instance.json 時的 fallback;
// TemplateID/Variant 不可從 Docker 還原,留空)。
func specFromInspect(j types.ContainerJSON) protocol.InstanceSpec {
	spec := protocol.InstanceSpec{}
	if j.Config != nil {
		spec.Image = j.Config.Image
		spec.Labels = cloneStringMap(j.Config.Labels)
		spec.UUID = j.Config.Labels[labelUUID]
		spec.Node = j.Config.Labels[labelNode]
		spec.Env = map[string]string{}
		for _, kv := range j.Config.Env {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				spec.Env[kv[:i]] = kv[i+1:]
			}
		}
	}
	for _, m := range j.Mounts {
		if m.Type == "bind" && m.Destination != "" {
			spec.DataDirs = append(spec.DataDirs, m.Destination)
		}
	}
	spec.Ports = portsFromInspect(j)
	return spec
}
