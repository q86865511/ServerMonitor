package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"

	"servermonitor/internal/core"
)

// 本檔集中 R5 多節點的管理面:遠端節點的撥測(TOFU 指紋)、新增、移除與列舉。綁定層(main.App)
// 薄轉發到這些 Runtime 方法並轉 JSON DTO;繁中錯誤訊息在此組出(綁定層直接透傳)。

// nodeProbeTimeout 是撥測/新增時單次探測(取指紋 + health)的整體上限。
const nodeProbeTimeout = 30 * time.Second

// validNodeName 限定節點名為 GUI/金鑰庫安全的字元(英數與 - _ .),長度 1..64。避免命名空間鍵
// (NodeTokenRef)含路徑分隔或空白造成金鑰庫異常,也讓 gsm.node 標籤值穩定。
var validNodeName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ProbeResult 是撥測一個(尚未新增的)節點的結果。Fingerprint 於 https 撥測成功時帶回,供 GUI
// 顯示讓使用者確認(TOFU);Err 非空表示失敗(繁中,可直接顯示),此時 OK=false。
type ProbeResult struct {
	OK          bool
	Fingerprint string
	Version     string
	Err         string
}

// NodeInfo 是已註冊節點的摘要(供 GUI 節點清單)。
type NodeInfo struct {
	Name            string
	BaseURL         string
	Online          bool
	DockerAvailable bool
	Fingerprint     string
	InsecureHTTP    bool
	LastErr         string
	Removable       bool // "local" 不可移除
	IsLocal         bool // 本機 in-process 節點(前端據此判定,不硬編 "local")
}

// ProbeNode 撥測一個節點(尚未持久化):https 先以「跳過驗證只取憑證」拿葉憑證指紋,再以 pin 該指紋
// 的連線帶 bearer 打 /health 驗 token;http 需顯式 insecureHTTP。回結果供 GUI 顯示指紋讓使用者確認
// (TOFU)。任何失敗以繁中 Err 回報,不拋 error(綁定層直接透傳結果)。
func (r *Runtime) ProbeNode(baseURL, token string, insecureHTTP bool) ProbeResult {
	baseURL = strings.TrimSpace(baseURL)
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ProbeResult{Err: "節點位址格式錯誤(需 http(s)://主機:埠)"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), nodeProbeTimeout)
	defer cancel()

	var fingerprint string
	if u.Scheme == "https" {
		fp, ferr := core.FetchLeafFingerprint(ctx, baseURL, 0)
		if ferr != nil {
			return ProbeResult{Err: nodeErrMsg(ferr)}
		}
		fingerprint = fp
	} else if !insecureHTTP {
		return ProbeResult{Err: "明文 HTTP 連線不安全:請改用 https,或確認為內網/VPN 後勾選允許明文連線"}
	}

	client, err := core.NewRemoteNodeClient(baseURL, token, core.NodeClientOptions{
		TLSFingerprint: fingerprint, InsecureHTTP: insecureHTTP,
	})
	if err != nil {
		return ProbeResult{Fingerprint: fingerprint, Err: nodeErrMsg(err)}
	}
	h, herr := client.Health(ctx)
	if herr != nil {
		return ProbeResult{Fingerprint: fingerprint, Err: nodeErrMsg(herr)}
	}
	return ProbeResult{OK: true, Fingerprint: fingerprint, Version: h.Version}
}

// AddNode 新增並持久化一個遠端節點:驗名稱唯一/格式 → token 入金鑰庫 → 寫 config → 即時註冊 +
// 上線探測。fingerprint 空且 https 表示走系統 CA 驗證。失敗回繁中 error(且不留半套狀態)。
func (r *Runtime) AddNode(name, baseURL, token, fingerprint string, insecureHTTP bool) error {
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	if !validNodeName.MatchString(name) {
		return errors.New("節點名稱格式錯誤(限英數與 . _ -,長度 1..64)")
	}
	if name == r.node || name == "local" {
		return errors.New("節點名稱與本機節點衝突,請改用其他名稱")
	}

	r.nodesMu.Lock()
	defer r.nodesMu.Unlock()

	cfg, _, err := core.LoadAppConfig(r.configPath)
	if err != nil {
		return fmt.Errorf("讀取設定檔失敗: %w", err)
	}
	for _, n := range cfg.Nodes {
		if n.Name == name {
			return fmt.Errorf("節點名稱 %q 已存在", name)
		}
	}

	// 先建 client(順帶驗證 URL/scheme/明文允許),再持久化,避免寫入無法連線的設定。
	client, err := core.NewRemoteNodeClient(baseURL, token, core.NodeClientOptions{
		TLSFingerprint: fingerprint, InsecureHTTP: insecureHTTP,
	})
	if err != nil {
		return nodeErr(err)
	}

	// token 先入金鑰庫(config 只存非敏感設定)。config 寫入失敗則回滾 token,不留孤兒。
	if err := r.secrets.Set(core.NodeTokenRef(name), token); err != nil {
		return fmt.Errorf("寫入節點 token 至金鑰庫失敗: %w", err)
	}
	cfg.Nodes = append(cfg.Nodes, core.NodeConfig{
		Name: name, BaseURL: strings.TrimRight(baseURL, "/"),
		TLSFingerprint: fingerprint, InsecureHTTP: insecureHTTP,
	})
	if err := core.SaveAppConfig(r.configPath, cfg); err != nil {
		_ = r.secrets.Delete(core.NodeTokenRef(name))
		return fmt.Errorf("寫入設定檔失敗: %w", err)
	}

	// 註冊 + 即時上線探測(離線不視為新增失敗;狀態由 registry.Call 更新,GUI 稍後顯示離線)。
	r.registry.Register(name, client)
	ctx, cancel := context.WithTimeout(context.Background(), nodeProbeTimeout)
	defer cancel()
	_ = r.registry.Call(name, func(c *core.NodeClient) error {
		_, herr := c.Health(ctx)
		return herr
	})
	return nil
}

// RemoveNode 移除一個遠端節點:反註冊、清 config 與金鑰庫 token。該節點仍掛有實例時拒絕(回明確
// 錯誤,避免留下無法管理的孤兒實例)。"local" 不可移除。
func (r *Runtime) RemoveNode(name string) error {
	name = strings.TrimSpace(name)
	if name == r.node || name == "local" {
		return errors.New("本機節點不可移除")
	}

	r.nodesMu.Lock()
	defer r.nodesMu.Unlock()

	// 掛有實例則拒絕(檢查 DB 實例記錄的 node 欄位)。
	if n, cerr := r.countInstancesOn(name); cerr != nil {
		return cerr
	} else if n > 0 {
		return fmt.Errorf("節點 %q 上仍有 %d 個實例,請先移除這些實例再移除節點", name, n)
	}

	// 先 Unregister:讓尚未開始的 CreateInstance(其 registry.Call 每次查 registry 取 client)立即
	// 以 ErrUnknownNode 失敗,收斂與並行建立的競態窗口。先保存 client 以便複查發現競態時回滾。
	client, clientErr := r.registry.Client(name)
	r.registry.Unregister(name)

	// 複查:Unregister 前已進入 registry.Call、正在遠端建立的並行 Create 仍可能寫入 DB。發現該節點
	// 出現實例則回滾註冊並拒絕移除,避免留下「DB 有記錄但 registry 無此節點」的孤兒。殘餘窗口極窄
	// (Create 已遠端建成、DB 寫入與本複查之間),根治需 Create/Remove 共用同一序列化鎖(後續)。
	if n, cerr := r.countInstancesOn(name); cerr != nil {
		if clientErr == nil {
			r.registry.Register(name, client)
		}
		return cerr
	} else if n > 0 {
		if clientErr == nil {
			r.registry.Register(name, client)
		}
		return fmt.Errorf("移除期間偵測到節點 %q 有新建實例,已取消移除,請先移除該實例", name)
	}

	cfg, _, err := core.LoadAppConfig(r.configPath)
	if err != nil {
		if clientErr == nil {
			r.registry.Register(name, client)
		}
		return fmt.Errorf("讀取設定檔失敗: %w", err)
	}
	idx := -1
	for i, n := range cfg.Nodes {
		if n.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("節點 %q 不存在", name)
	}
	cfg.Nodes = append(cfg.Nodes[:idx], cfg.Nodes[idx+1:]...)
	if err := core.SaveAppConfig(r.configPath, cfg); err != nil {
		if clientErr == nil {
			r.registry.Register(name, client)
		}
		return fmt.Errorf("寫入設定檔失敗: %w", err)
	}

	// token 清除為 best-effort(冪等 Delete;失敗僅記 log,不擋移除——config 已無此節點)。
	if derr := r.secrets.Delete(core.NodeTokenRef(name)); derr != nil {
		log.Printf("nodes: 清除節點 %q token 失敗(已從設定移除): %v", name, derr)
	}
	return nil
}

// countInstancesOn 回傳掛在指定節點的實例數(RemoveNode 移除前後複查用)。
func (r *Runtime) countInstancesOn(node string) (int, error) {
	insts, err := r.store.ListInstances()
	if err != nil {
		return 0, fmt.Errorf("查詢實例清單失敗: %w", err)
	}
	count := 0
	for _, rec := range insts {
		if rec.Node == node {
			count++
		}
	}
	return count, nil
}

// ListNodes 回傳所有已註冊節點的摘要(供 GUI 節點清單)。合併 registry 的線上狀態與 config 的
// 連線/信任設定;"local" 節點填本機 loopback 位址與 DockerAvailable、標為不可移除。
//
// 遠端節點的 DockerAvailable 目前一律回 false:節點在線與否經 registry 得知,但 Docker 能力需
// 額外查詢代理(現行 /health 未回報),留待後續擴充;GUI 對遠端不應據此置灰 docker 選項。
func (r *Runtime) ListNodes() []NodeInfo {
	cfg, _, _ := core.LoadAppConfig(r.configPath)
	byName := make(map[string]core.NodeConfig, len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		byName[n.Name] = n
	}

	statuses := r.registry.List()
	seen := make(map[string]bool, len(statuses))
	out := make([]NodeInfo, 0, len(statuses)+len(cfg.Nodes))
	for _, st := range statuses {
		seen[st.Node] = true
		info := NodeInfo{
			Name:    st.Node,
			Online:  st.Online,
			LastErr: st.LastErr,
		}
		if st.Node == r.node || st.Node == "local" {
			info.IsLocal = true
			info.DockerAvailable = r.DockerAvailable()
			info.Removable = false
			if c, err := r.registry.Client(st.Node); err == nil {
				info.BaseURL = c.BaseURL()
			}
		} else if nc, ok := byName[st.Node]; ok {
			info.BaseURL = nc.BaseURL
			info.Fingerprint = nc.TLSFingerprint
			info.InsecureHTTP = nc.InsecureHTTP
			info.Removable = true
		} else {
			// registry 有、config 無(理論上不會發生):仍列出,標可移除。
			info.Removable = true
		}
		out = append(out, info)
	}
	// 併入 config 有、registry 無的節點(建 client 失敗的損壞設定):標離線,仍可經 GUI 移除,
	// 不讓損壞節點成為 GUI 看不到也無法清除的隱形孤兒。
	for _, nc := range cfg.Nodes {
		if seen[nc.Name] {
			continue
		}
		out = append(out, NodeInfo{
			Name:         nc.Name,
			BaseURL:      nc.BaseURL,
			Online:       false,
			Fingerprint:  nc.TLSFingerprint,
			InsecureHTTP: nc.InsecureHTTP,
			LastErr:      "節點設定無法建立連線(請移除後重新加入)",
			Removable:    true,
		})
	}
	return out
}

// registerConfiguredNodes 於 Bootstrap 把 config 的遠端節點逐一以金鑰庫 token 建 NodeClient 註冊進
// registry(R5 多節點)。離線/設定錯不阻擋啟動:個別失敗記 log 跳過,節點的狀態於首次呼叫時由
// registry.Call 更新。查無 token 仍註冊(呼叫將以 401 失敗),使用者可經 GUI 重新加入修正。
func (r *Runtime) registerConfiguredNodes(nodes []core.NodeConfig) {
	for _, n := range nodes {
		if n.Name == "" || n.Name == r.node || n.Name == "local" {
			log.Printf("nodes: 略過非法或與本機衝突的節點設定 %q", n.Name)
			continue
		}
		token, terr := r.secrets.Get(core.NodeTokenRef(n.Name))
		if terr != nil {
			log.Printf("nodes: 節點 %q 查無金鑰庫 token(仍註冊,呼叫將需重新設定): %v", n.Name, terr)
			token = ""
		}
		client, err := core.NewRemoteNodeClient(n.BaseURL, token, core.NodeClientOptions{
			TLSFingerprint: n.TLSFingerprint, InsecureHTTP: n.InsecureHTTP,
		})
		if err != nil {
			// 建 client 失敗(設定損壞)不註冊,但 ListNodes 會從 config 併出此孤兒項供 GUI 移除。
			log.Printf("nodes: 建立節點 %q 客戶端失敗(略過註冊,GUI 仍可見並移除): %v", n.Name, err)
			continue
		}
		r.registry.Register(n.Name, client)
		// 註冊預設 online;開機時遠端可能已關機,先標離線再非同步探測——避免「未探測即顯示在線」
		// 誤導使用者選到實已離線的節點(本波尚無遠端背景編排會定期更新狀態)。
		r.registry.MarkOffline(n.Name, errNodeNotProbed)
		go func(name string) {
			ctx, cancel := context.WithTimeout(context.Background(), nodeProbeTimeout)
			defer cancel()
			_ = r.registry.Call(name, func(c *core.NodeClient) error {
				_, herr := c.Health(ctx)
				return herr
			})
		}(n.Name)
	}
}

// errNodeNotProbed 是節點註冊後、首次健康探測完成前的暫時離線原因。
var errNodeNotProbed = errors.New("尚未完成連線探測")

// nodeErrMsg 把節點層錯誤映射為可直接顯示的繁中訊息(撥測結果 Err 用)。
func nodeErrMsg(err error) string {
	switch {
	case errors.Is(err, core.ErrNodeUnauthorized):
		return "token 錯誤:節點拒絕授權(請確認已複製 agent 啟動時印出的 token)"
	case errors.Is(err, core.ErrNodeFingerprintMismatch):
		return "TLS 憑證指紋不符:節點憑證可能已更換,或連到了非預期的主機"
	case errors.Is(err, core.ErrInsecureHTTPNotAllowed):
		return "明文 HTTP 連線不安全:請改用 https,或確認為內網/VPN 後勾選允許明文連線"
	case errors.Is(err, core.ErrNodeUnreachable):
		return "無法連線到節點:請確認位址、埠、防火牆,以及 agent 是否運行中"
	default:
		return err.Error()
	}
}

// nodeErr 把節點層錯誤包成繁中 error(AddNode 等回 error 的路徑用)。
func nodeErr(err error) error {
	return errors.New(nodeErrMsg(err))
}
