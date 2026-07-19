package agent

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"

	"servermonitor/internal/protocol"
)

// 階段 4:Docker 資源管理。映像/容器管理與每實例磁碟用量皆為 RuntimeBackend 之外的可選橫切能力
// (比照 BackupDeleter/MountWriter/instanceDataPurger 的模式):以獨立介面表達,不擴張核心
// RuntimeBackend 介面;節點代理 Server 以型別斷言啟用對應端點,能力缺失回 ErrUnsupported。
//
// 能力歸屬:映像/容器是 Docker 概念,僅 DockerBackend 實作(native 節點無);磁碟用量以宿主
// 目錄計算,docker 與 native 皆實作(且因共用資料/備份根,任一計得同值)。

// ImageManager 是節點層 Docker 映像管理能力(列出/刪除/prune)。僅 DockerBackend 實作。
type ImageManager interface {
	ListImages(ctx context.Context) ([]protocol.ImageSummary, error)
	RemoveImage(ctx context.Context, id string, force bool) error
	PruneImages(ctx context.Context) (protocol.PruneImagesResult, error)
}

// ContainerManager 是節點層 Docker 容器管理能力(列出全部/刪除)。列出不套 gsm 標籤過濾,故含
// 孤兒與非本工具建立的容器(供使用者清理);僅 DockerBackend 實作。
type ContainerManager interface {
	ListContainers(ctx context.Context) ([]protocol.ContainerSummary, error)
	RemoveContainer(ctx context.Context, id string, force bool) error
}

// instanceDiskUsager 以宿主端目錄大小回報一個實例的資料/備份磁碟用量(不依賴容器,以 uuid 定位)。
// docker 與 native 皆實作。
type instanceDiskUsager interface {
	InstanceDiskUsage(ctx context.Context, uuid string) (protocol.InstanceDiskUsage, error)
}

// ErrUnsupported 表示後端缺乏請求的橫切能力(如 native-only 節點無 Docker 映像/容器管理)。
// server 映為 ERR_UNSUPPORTED / HTTP 501。
var ErrUnsupported = errors.New("agent: 後端不支援此操作")

var (
	_ ImageManager       = (*DockerBackend)(nil)
	_ ContainerManager   = (*DockerBackend)(nil)
	_ instanceDiskUsager = (*DockerBackend)(nil)
	_ instanceDiskUsager = (*NativeBackend)(nil)
)

// ---- DockerBackend:映像管理 ----

// ListImages 列出本機 Docker 映像(只列頂層映像,等同 `docker images`)。ContainerCount:true 使
// docker 計算每個映像的使用容器數(否則 Summary.Containers 為 -1)。
func (b *DockerBackend) ListImages(ctx context.Context) ([]protocol.ImageSummary, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	imgs, err := b.cli.ImageList(ctx, image.ListOptions{ContainerCount: true})
	if err != nil {
		return nil, mapDockerErr(err)
	}
	out := make([]protocol.ImageSummary, 0, len(imgs))
	for _, im := range imgs {
		out = append(out, protocol.ImageSummary{
			ID:         im.ID,
			Tags:       append([]string(nil), im.RepoTags...),
			SizeBytes:  im.Size,
			CreatedUTC: time.Unix(im.Created, 0).UTC(),
			Containers: int(im.Containers),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RemoveImage 以精確 id 刪除映像(PruneChildren:true 同 `docker rmi` 連帶清無標籤父層)。映像被
// 使用中(有容器參照)時 docker 回錯,經 mapDockerErr 原樣透傳(前端據以提示強制刪除或先移除容器)。
func (b *DockerBackend) RemoveImage(ctx context.Context, id string, force bool) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	_, err := b.cli.ImageRemove(ctx, id, image.RemoveOptions{Force: force, PruneChildren: true})
	return mapDockerErr(err)
}

// PruneImages 清除懸掛(dangling)映像(空 filter,等同 `docker image prune`;不含 -a 的清全部未用),
// 回收位元組與被刪/取消標記的映像 ID。
func (b *DockerBackend) PruneImages(ctx context.Context) (protocol.PruneImagesResult, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.PruneImagesResult{}, err
	}
	rep, err := b.cli.ImagesPrune(ctx, filters.NewArgs())
	if err != nil {
		return protocol.PruneImagesResult{}, mapDockerErr(err)
	}
	deleted := make([]string, 0, len(rep.ImagesDeleted))
	for _, d := range rep.ImagesDeleted {
		if d.Deleted != "" {
			deleted = append(deleted, d.Deleted)
		} else if d.Untagged != "" {
			deleted = append(deleted, d.Untagged)
		}
	}
	return protocol.PruneImagesResult{ReclaimedBytes: int64(rep.SpaceReclaimed), Deleted: deleted}, nil
}

// ---- DockerBackend:容器管理 ----

// ListContainers 列出本機所有 Docker 容器(All:true 含已停止;不套 gsm 標籤過濾以含孤兒與非本工具
// 建立的容器)。Labels 原樣保留供上層辨識 gsm.* 標記。
func (b *DockerBackend) ListContainers(ctx context.Context) ([]protocol.ContainerSummary, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	cs, err := b.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, mapDockerErr(err)
	}
	out := make([]protocol.ContainerSummary, 0, len(cs))
	for _, c := range cs {
		out = append(out, protocol.ContainerSummary{
			ID:     c.ID,
			Names:  append([]string(nil), c.Names...),
			Image:  c.Image,
			State:  c.State,
			Labels: cloneStringMap(c.Labels),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RemoveContainer 以精確 id 刪除容器(force 亦刪執行中,對映 `docker rm -f`)。此為節點層直接刪除,
// 不經實例登錄/uuid 解析——供清理孤兒或非本工具建立的容器(不清宿主資料;那屬 Remove(Purge) 職責)。
func (b *DockerBackend) RemoveContainer(ctx context.Context, id string, force bool) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return mapDockerErr(b.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force}))
}

// ---- 每實例磁碟用量(docker 與 native 共用實作)----

// InstanceDiskUsage 回報一個實例的資料根與備份根磁碟用量(DockerBackend)。
func (b *DockerBackend) InstanceDiskUsage(ctx context.Context, uuid string) (protocol.InstanceDiskUsage, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.InstanceDiskUsage{}, err
	}
	return instanceDiskUsage(ctx, b.instanceDataRoot(uuid), b.backupInstanceRoot(uuid))
}

// InstanceDiskUsage 回報一個實例的資料根與備份根磁碟用量(NativeBackend)。
func (b *NativeBackend) InstanceDiskUsage(ctx context.Context, uuid string) (protocol.InstanceDiskUsage, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.InstanceDiskUsage{}, err
	}
	return instanceDiskUsage(ctx, b.instanceDataRoot(uuid), b.backupInstanceRoot(uuid))
}

// instanceDiskUsage 分別遞迴加總資料根與備份根的檔案位元組(目錄不存在計 0)。
func instanceDiskUsage(ctx context.Context, dataRoot, backupRoot string) (protocol.InstanceDiskUsage, error) {
	data, err := dirSizeBytes(ctx, dataRoot)
	if err != nil {
		return protocol.InstanceDiskUsage{}, err
	}
	backup, err := dirSizeBytes(ctx, backupRoot)
	if err != nil {
		return protocol.InstanceDiskUsage{}, err
	}
	return protocol.InstanceDiskUsage{DataBytes: data, BackupBytes: backup}, nil
}

// dirSizeBytes 盡力遞迴加總 root 下所有一般檔案的位元組。磁碟用量為盡力回報:目錄不存在計 0;個別
// 子項的任何錯誤(不存在、權限、Windows 上執行中伺服器的鎖定、並發移除)一律略過該項續計,不因單
// 一子項失敗而整體回錯。僅 ctx 取消回錯——避免超大目錄樹的長時遍歷無法中斷(認證後反覆請求耗 IO)。
// 不跟隨符號連結(WalkDir 對 symlink 目錄不遞迴)。
func dirSizeBytes(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return nil // 盡力:任何遍歷錯誤(不存在/權限/鎖定)略過該子樹續計
		}
		if d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return total, err
	}
	return total, nil // 其餘一律盡力回報部分總和
}
