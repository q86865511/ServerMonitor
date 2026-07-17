package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// JavaProvisioner 供應 Adoptium(Eclipse Temurin)JRE 至共用快取(R4)。
// 快取佈局:<CacheRoot>/jre/<major>/...(解壓後的 JRE 目錄樹,內含 bin/java.exe)。
type JavaProvisioner struct {
	cacheRoot string       // 共用快取根
	client    *http.Client // 下載/API 共用
	apiBase   string       // Adoptium API base URL(可注入)
	locks     *keyedMutex  // per-major 併發鎖:同 major 併發 Ensure 不重複下載
}

// newJavaProvisioner 由 Provisioner.New 建構。
func newJavaProvisioner(cacheRoot string, client *http.Client, apiBase string) *JavaProvisioner {
	return &JavaProvisioner{
		cacheRoot: cacheRoot,
		client:    client,
		apiBase:   apiBase,
		locks:     newKeyedMutex(),
	}
}

// adoptiumAsset 是 Adoptium API v3 GET /v3/assets/latest/{feature_version}/{jvm_impl} 回應
// 陣列元素(BinaryAssetView)的最小子集。欄位路徑經官方 OpenAPI 文件確認
// (https://api.adoptium.net/q/openapi)。
type adoptiumAsset struct {
	Binary struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		ImageType    string `json:"image_type"`
		Package      struct {
			Name     string `json:"name"`
			Link     string `json:"link"`
			Checksum string `json:"checksum"` // sha256(hex)
		} `json:"package"`
	} `json:"binary"`
	Version struct {
		Semver string `json:"semver"`
	} `json:"version"`
}

// Ensure 確保本機快取存在指定 major 版的 JRE 並回傳 java.exe 絕對路徑。
// 快取命中時直接回傳、不打網路;未命中則自 Adoptium 下載對應資產、sha256 校驗、解壓入快取。
// progress 可為 nil。離線/網路錯誤時回含「需要網路下載 Java <major>」語意的包裝錯誤。
func (j *JavaProvisioner) Ensure(ctx context.Context, major int, progress ProgressFunc) (string, error) {
	jreDir := j.jreDir(major)

	// 快取命中(免鎖快路徑)。
	if exe, ok := findJavaExe(jreDir); ok {
		return exe, nil
	}

	// 同 major 序列化,避免併發重複下載。
	unlock := j.locks.lock(strconv.Itoa(major))
	defer unlock()

	// 取得鎖後再查一次:可能已被前一個持鎖者供應完成。
	if exe, ok := findJavaExe(jreDir); ok {
		return exe, nil
	}

	return j.provision(ctx, major, jreDir, progress)
}

// jreDir 回傳指定 major 版的快取目錄。
func (j *JavaProvisioner) jreDir(major int) string {
	return filepath.Join(j.cacheRoot, "jre", strconv.Itoa(major))
}

// provision 執行實際供應:查詢資產 → 下載校驗 → 解壓至 staging → 原子升格為快取目錄。
// 校驗或任一步失敗時,快取目錄 jreDir 不會出現(半成品留在被清除的暫存/staging)。
func (j *JavaProvisioner) provision(ctx context.Context, major int, jreDir string, progress ProgressFunc) (string, error) {
	asset, err := j.fetchAsset(ctx, major, progress)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(jreDir), 0o755); err != nil {
		return "", fmt.Errorf("provision: 建立 JRE 快取父目錄失敗: %w", err)
	}

	// 下載壓縮檔至與快取目錄同層的暫存路徑;下載工具自身於失敗時清除半成品。
	tmpZip := jreDir + ".zip.part"
	defer os.Remove(tmpZip)
	dlReq := downloadRequest{
		URL:      asset.Binary.Package.Link,
		DestPath: tmpZip,
		Checksum: checksumSpec{Algo: "sha256", Value: asset.Binary.Package.Checksum},
		Stage:    fmt.Sprintf("下載 Java %d", major),
		Progress: progress,
	}
	if err := download(ctx, j.client, dlReq); err != nil {
		return "", fmt.Errorf("provision: 需要網路下載 Java %d(下載失敗): %w", major, err)
	}

	// 解壓至 staging;僅在完整成功後才原子 rename 為 jreDir,確保 jreDir 只在完成時出現。
	staging := jreDir + ".partial"
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("provision: 清理殘留 staging 失敗: %w", err)
	}
	defer os.RemoveAll(staging) // 成功已 rename 掉,no-op;失敗則清除半成品。

	if err := extractZip(tmpZip, staging, fmt.Sprintf("解壓 Java %d", major), progress); err != nil {
		return "", fmt.Errorf("provision: 解壓 Java %d 失敗: %w", major, err)
	}

	stagedExe, ok := findJavaExe(staging)
	if !ok {
		return "", fmt.Errorf("provision: Java %d 解壓後找不到 bin/java.exe", major)
	}

	// 原子升格:staging → jreDir。理論上 jreDir 不存在(命中會走快路徑)。
	if err := os.RemoveAll(jreDir); err != nil {
		return "", fmt.Errorf("provision: 清理舊 JRE 快取失敗: %w", err)
	}
	if err := os.Rename(staging, jreDir); err != nil {
		return "", fmt.Errorf("provision: 升格 JRE 快取目錄失敗: %w", err)
	}

	// 換算升格後的 java.exe 路徑。
	rel, err := filepath.Rel(staging, stagedExe)
	if err != nil {
		return "", fmt.Errorf("provision: 換算 java.exe 路徑失敗: %w", err)
	}
	return filepath.Join(jreDir, rel), nil
}

// fetchAsset 查詢 Adoptium API 取得指定 major 的 windows/x64/jre/hotspot 資產。
// 網路錯誤/非 200 回應皆包裝為含「需要網路下載 Java <major>」語意的錯誤(對應 R4 離線驗收)。
func (j *JavaProvisioner) fetchAsset(ctx context.Context, major int, progress ProgressFunc) (adoptiumAsset, error) {
	progress.report(ProvisionProgress{
		Stage:   fmt.Sprintf("查詢 Java %d", major),
		Percent: -1,
		Detail:  "Adoptium API",
	})

	url := fmt.Sprintf("%s/v3/assets/latest/%d/hotspot?architecture=x64&image_type=jre&os=windows",
		strings.TrimRight(j.apiBase, "/"), major)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return adoptiumAsset{}, fmt.Errorf("provision: 建立 Adoptium 查詢請求失敗: %w", err)
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return adoptiumAsset{}, fmt.Errorf("provision: 需要網路下載 Java %d(查詢 Adoptium 失敗): %w", major, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return adoptiumAsset{}, fmt.Errorf("provision: 需要網路下載 Java %d(Adoptium 回應狀態 %d)", major, resp.StatusCode)
	}

	var assets []adoptiumAsset
	if err := json.NewDecoder(resp.Body).Decode(&assets); err != nil {
		return adoptiumAsset{}, fmt.Errorf("provision: 解析 Adoptium 回應失敗: %w", err)
	}

	for _, a := range assets {
		if !strings.EqualFold(a.Binary.OS, "windows") ||
			!strings.EqualFold(a.Binary.Architecture, "x64") ||
			!strings.EqualFold(a.Binary.ImageType, "jre") {
			continue
		}
		if a.Binary.Package.Link == "" {
			continue
		}
		if a.Binary.Package.Checksum == "" {
			return adoptiumAsset{}, fmt.Errorf("provision: Adoptium Java %d 資產缺少 checksum,拒絕未校驗下載", major)
		}
		return a, nil
	}
	return adoptiumAsset{}, fmt.Errorf("provision: Adoptium 無 Java %d 的 windows/x64/jre 資產", major)
}

// findJavaExe 在 root 下尋找 bin/java.exe,回傳其絕對路徑。用於快取命中判定與解壓後定位。
// Adoptium zip 解壓後為 <jdk-XX-jre>/bin/java.exe 結構,故需遞迴尋找(頂層目錄名含版本號)。
func findJavaExe(root string) (string, bool) {
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 目錄不存在等錯誤視為未命中,繼續。
		}
		if found != "" {
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(d.Name(), "java.exe") &&
			strings.EqualFold(filepath.Base(filepath.Dir(p)), "bin") {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", false
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return found, true
	}
	return abs, true
}

// keyedMutex 提供 per-key 的序列化鎖。取捨:採行程內 per-key mutex(而非跨行程 lock file)——
// 單一 agent 行程獨佔此快取(四層架構約定:agent 是檔案系統擁有者),行程內鎖已足以防止
// 同 major 併發重複下載;跨行程協調不在本套件範圍。
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

// lock 取得 key 對應的鎖,回傳解鎖函式。
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()

	m.Lock()
	return m.Unlock
}
