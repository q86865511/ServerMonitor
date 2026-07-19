package app

import (
	"context"

	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// 階段 4:Docker 資源管理的 Runtime 轉發。映像/容器管理與磁碟用量經 registry.Call 到本機節點
// (r.node);native-only 節點回 core.ErrNodeUnsupported(綁定層轉為可讀錯誤/前端置灰)。刪除備份
// 的 GUI 路徑委派 BackupService.DeleteBackup(節點刪 + 清 store + 記事件)。

// ListImages 列出本機節點的 Docker 映像(R:階段 4)。
func (r *Runtime) ListImages(ctx context.Context) ([]protocol.ImageSummary, error) {
	var out []protocol.ImageSummary
	err := r.registry.Call(r.node, func(c *core.NodeClient) error {
		var e error
		out, e = c.ListImages(ctx)
		return e
	})
	return out, err
}

// RemoveImage 刪除本機節點的一份映像(精確 id;force 亦刪被使用中映像的標記)。
func (r *Runtime) RemoveImage(ctx context.Context, id string, force bool) error {
	return r.registry.Call(r.node, func(c *core.NodeClient) error {
		return c.RemoveImage(ctx, id, force)
	})
}

// PruneImages 清除本機節點的懸掛映像,回傳回收量與被刪 ID。
func (r *Runtime) PruneImages(ctx context.Context) (protocol.PruneImagesResult, error) {
	var out protocol.PruneImagesResult
	err := r.registry.Call(r.node, func(c *core.NodeClient) error {
		var e error
		out, e = c.PruneImages(ctx)
		return e
	})
	return out, err
}

// ListContainers 列出本機節點所有 Docker 容器(含孤兒/非本工具建立)。
func (r *Runtime) ListContainers(ctx context.Context) ([]protocol.ContainerSummary, error) {
	var out []protocol.ContainerSummary
	err := r.registry.Call(r.node, func(c *core.NodeClient) error {
		var e error
		out, e = c.ListContainers(ctx)
		return e
	})
	return out, err
}

// RemoveContainer 刪除本機節點的一個容器(精確 id;force 亦刪執行中)。
func (r *Runtime) RemoveContainer(ctx context.Context, id string, force bool) error {
	return r.registry.Call(r.node, func(c *core.NodeClient) error {
		return c.RemoveContainer(ctx, id, force)
	})
}

// InstanceDiskUsage 查詢某實例的宿主磁碟用量(資料/備份根)。
func (r *Runtime) InstanceDiskUsage(ctx context.Context, uuid string) (protocol.InstanceDiskUsage, error) {
	var out protocol.InstanceDiskUsage
	err := r.registry.Call(r.node, func(c *core.NodeClient) error {
		var e error
		out, e = c.InstanceDiskUsage(ctx, uuid)
		return e
	})
	return out, err
}

// DeleteBackup 手動刪除某實例的一份備份(GUI 路徑;R9)。委派 BackupService(節點刪 agent 備份 +
// 清 store 中繼 + 記 BACKUP_DELETED)。ctx 衍生自 rootCtx 並計入 in-flight(關閉時取消/等待)。
func (r *Runtime) DeleteBackup(_ context.Context, uuid string, backupID protocol.BackupID) error {
	ctx, done := r.trackOp()
	defer done()
	return r.backups.DeleteBackup(ctx, uuid, backupID)
}
