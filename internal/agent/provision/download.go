package provision

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ErrChecksumMismatch 表示下載完成但校驗摘要與期望不符(半成品已被清除)。以 errors.Is 判別。
var ErrChecksumMismatch = errors.New("provision: 下載檔案校驗不符")

// checksumSpec 描述下載後的校驗方式。Value 為期望的 hex 摘要(大小寫不敏感);Value 空字串
// 表示跳過校驗(供無官方 checksum 的來源使用,呼叫端需自負風險)。
type checksumSpec struct {
	Algo  string // "sha256"(預設,空字串同義)| "sha1"
	Value string // 期望的 hex 摘要
}

func (c checksumSpec) skip() bool { return strings.TrimSpace(c.Value) == "" }

// newHasher 依演算法名回傳對應的 hash.Hash。MC 安裝器需 sha1(Vanilla)與 sha256(Paper),
// 故此處讓演算法可由呼叫端指定,不寫死 sha256。
func newHasher(algo string) (hash.Hash, error) {
	switch strings.ToLower(strings.TrimSpace(algo)) {
	case "", "sha256":
		return sha256.New(), nil
	case "sha1":
		return sha1.New(), nil
	default:
		return nil, fmt.Errorf("provision: 不支援的校驗演算法 %q", algo)
	}
}

// downloadRequest 是一次「下載 → 校驗 → 原子落地」的請求。
type downloadRequest struct {
	URL      string       // 來源 URL(生產環境為 HTTPS 官方載點)
	DestPath string       // 校驗通過後的目的檔絕對路徑
	Checksum checksumSpec // 期望摘要;Value 空則不校驗
	Stage    string       // 進度回報用的階段名
	Progress ProgressFunc // 可為 nil
}

// download 執行 HTTPS GET,先寫入與目的檔同目錄的暫存檔並邊寫邊算摘要,校驗通過才以 rename
// 原子移入 DestPath;過程任一步失敗都會清除暫存檔,不留半成品。進度依 Content-Length 換算
// 百分比(未知則回報 -1)。client 由呼叫端注入(測試以 httptest 的 client 注入)。
func download(ctx context.Context, client *http.Client, req downloadRequest) (err error) {
	hasher, herr := newHasher(req.Checksum.Algo)
	if herr != nil {
		return herr
	}

	destDir := filepath.Dir(req.DestPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("provision: 建立下載目的目錄失敗: %w", err)
	}

	// 暫存檔與目的檔同目錄,確保 rename 為同磁碟區的原子操作。
	tmp, err := os.CreateTemp(destDir, ".download-*.part")
	if err != nil {
		return fmt.Errorf("provision: 建立暫存檔失敗: %w", err)
	}
	tmpPath := tmp.Name()
	// 失敗時清暫存;成功 rename 後 tmpPath 已不存在,Remove 為 no-op。
	defer func() {
		tmp.Close()
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return fmt.Errorf("provision: 建立下載請求失敗: %w", err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("provision: 下載 %s 失敗: %w", req.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provision: 下載 %s 回應非預期狀態 %d", req.URL, resp.StatusCode)
	}

	pw := &progressWriter{
		total:    resp.ContentLength, // 未知時為 -1
		stage:    req.Stage,
		progress: req.Progress,
		lastPct:  -2, // 初值異於 0/-1,確保第一次寫入必回報一次進度
	}
	writers := []io.Writer{tmp, pw}
	if !req.Checksum.skip() {
		writers = append(writers, hasher)
	}
	if _, err = io.Copy(io.MultiWriter(writers...), resp.Body); err != nil {
		return fmt.Errorf("provision: 下載寫入失敗: %w", err)
	}

	if !req.Checksum.skip() {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, strings.TrimSpace(req.Checksum.Value)) {
			return fmt.Errorf("%w: 期望 %s,實得 %s", ErrChecksumMismatch, req.Checksum.Value, got)
		}
	}

	// 關閉暫存檔後才 rename(Windows 上開啟中的檔案無法 rename)。
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("provision: 關閉暫存檔失敗: %w", err)
	}
	if err = os.Rename(tmpPath, req.DestPath); err != nil {
		return fmt.Errorf("provision: 移入目的檔失敗: %w", err)
	}
	return nil
}

// progressWriter 是計數用的 io.Writer:累計已寫位元組並在百分比變動時回報進度。
// 不實際儲存資料(與檔案/hasher 併入 io.MultiWriter)。
type progressWriter struct {
	total    int64 // Content-Length;-1 表未知
	written  int64
	stage    string
	progress ProgressFunc
	lastPct  int
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.written += int64(n)
	if w.progress != nil {
		pct := -1
		if w.total > 0 {
			pct = int(w.written * 100 / w.total)
		}
		if pct != w.lastPct { // 節流:僅在百分比變動時回報(未知總量時只回報一次)
			w.lastPct = pct
			w.progress(ProvisionProgress{
				Stage:   w.stage,
				Percent: pct,
				Detail:  fmt.Sprintf("%d bytes", w.written),
			})
		}
	}
	return n, nil
}
