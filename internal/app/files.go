package app

import (
	"context"
	"io"

	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// 階段 5:伺服器檔案管理的 Runtime 轉發。瀏覽/下載/上傳/刪除經 registry.Call 路由到「該實例所屬
// 節點」(自 store 取 rec.Node,與生命週期/備份操作一致,支援遠端節點的實例;非固定本機 r.node)。
// 路徑拘束(遍歷/symlink 逃逸/中繼檔保護)全數由節點端 agent 施加(見 internal/agent/files.go),
// 本層不重複判定,也不得自行拼接路徑——避免兩處判準漂移而生出「這層放行、那層拒絕」的縫。
//
// 下載/上傳刻意以 io.Writer / io.Reader 進出而非 []byte:串流全程不落記憶體(伺服器世界檔可達
// GB 級),且 ReadCloser 不外洩出 registry.Call——節點狀態觀測在回呼返回時即完成,不會因呼叫端
// 忘記 Close 而留下未收束的連線。

// instanceNode 回傳某實例所屬節點識別(供實例層操作路由到正確節點,含遠端);實例不存在回錯。
func (r *Runtime) instanceNode(uuid string) (string, error) {
	rec, err := r.store.GetInstance(uuid)
	if err != nil {
		return "", err
	}
	return rec.Node, nil
}

// ListFiles 列出某實例資料目錄下 rel 的直接子項(rel 空字串=資料根)。
func (r *Runtime) ListFiles(ctx context.Context, uuid, rel string) ([]protocol.FileEntry, error) {
	node, err := r.instanceNode(uuid)
	if err != nil {
		return nil, err
	}
	var out []protocol.FileEntry
	cerr := r.registry.Call(node, func(c *core.NodeClient) error {
		resp, e := c.ListFiles(ctx, uuid, rel)
		out = resp.Entries
		return e
	})
	return out, cerr
}

// DownloadFile 串流下載某實例的一個檔案到 w,回傳已寫入位元組數。
func (r *Runtime) DownloadFile(ctx context.Context, uuid, rel string, w io.Writer) (int64, error) {
	node, err := r.instanceNode(uuid)
	if err != nil {
		return 0, err
	}
	var n int64
	cerr := r.registry.Call(node, func(c *core.NodeClient) error {
		var e error
		n, e = c.DownloadFile(ctx, uuid, rel, w)
		return e
	})
	return n, cerr
}

// UploadFile 串流上傳 rd 的內容,覆寫/建立某實例資料目錄下的 rel 檔案(父目錄須已存在)。
func (r *Runtime) UploadFile(ctx context.Context, uuid, rel string, rd io.Reader) error {
	node, err := r.instanceNode(uuid)
	if err != nil {
		return err
	}
	return r.registry.Call(node, func(c *core.NodeClient) error {
		return c.UploadFile(ctx, uuid, rel, rd)
	})
}

// DeleteFile 刪除某實例資料目錄下的 rel;rel 為目錄時須 recursive=true。
func (r *Runtime) DeleteFile(ctx context.Context, uuid, rel string, recursive bool) error {
	node, err := r.instanceNode(uuid)
	if err != nil {
		return err
	}
	return r.registry.Call(node, func(c *core.NodeClient) error {
		return c.DeleteFile(ctx, uuid, rel, recursive)
	})
}
