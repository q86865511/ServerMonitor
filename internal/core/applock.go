package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ErrAppLocked 表示同一資料根已被另一應用實例持有(R13 單一實例)。
// 後啟者據此拒絕接管並提示;作用域=傳入的資料根目錄。
var ErrAppLocked = errors.New("core: 另一應用實例正在執行中")

// appLockFileName 是資料根下的鎖檔名。
const appLockFileName = "gsm.lock"

// AppLock 是「同一資料根同時只有一個管理實例」的檔案鎖(R13)。
// 以 O_CREATE|O_EXCL 原子建立鎖檔持有;檔內寫入 PID 供診斷與 stale 判定。
// 程序正常結束時 Release 釋放(關閉並移除);程序崩潰殘留的鎖檔,若其 PID 已不存活,
// 下一次 Acquire 可接管(見 lockIsStale 的限制說明)。
type AppLock struct {
	path string
	file *os.File
}

// AcquireAppLock 於 dataRoot 取得單一實例鎖。已被存活的實例持有時回 ErrAppLocked;
// 若既有鎖檔的 PID 已不存活(stale)則接管之。dataRoot 必須存在。
func AcquireAppLock(dataRoot string) (*AppLock, error) {
	if dataRoot == "" {
		return nil, fmt.Errorf("core: AppLock 需要資料根目錄")
	}
	path := filepath.Join(dataRoot, appLockFileName)

	lock, err := createLockFile(path)
	if err == nil {
		return lock, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("core: 建立鎖檔失敗: %w", err)
	}

	// 鎖檔已存在:判定是否為 stale(持有者 PID 不存活)。
	if !lockIsStale(path) {
		return nil, ErrAppLocked
	}
	// 接管:移除殘留鎖檔後重試一次;若期間被他人搶得(仍 EEXIST)則回 ErrAppLocked。
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return nil, fmt.Errorf("core: 移除殘留鎖檔失敗: %w", rerr)
	}
	lock, err = createLockFile(path)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrAppLocked
		}
		return nil, fmt.Errorf("core: 建立鎖檔失敗: %w", err)
	}
	return lock, nil
}

// Release 釋放鎖:關閉並移除鎖檔(冪等)。
func (l *AppLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	closeErr := l.file.Close()
	l.file = nil
	rmErr := os.Remove(l.path)
	if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return fmt.Errorf("core: 移除鎖檔失敗: %w", rmErr)
	}
	if closeErr != nil {
		return fmt.Errorf("core: 關閉鎖檔失敗: %w", closeErr)
	}
	return nil
}

// Path 回傳鎖檔路徑(供診斷)。
func (l *AppLock) Path() string { return l.path }

// createLockFile 以 O_CREATE|O_EXCL 原子建立鎖檔並寫入本程序 PID。
// 已存在時回傳包裹 os.ErrExist 的錯誤(呼叫端以 errors.Is 判別)。
func createLockFile(path string) (*AppLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if _, werr := fmt.Fprintf(f, "%d\n", os.Getpid()); werr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, werr
	}
	_ = f.Sync()
	return &AppLock{path: path, file: f}, nil
}

// lockIsStale 讀取鎖檔記錄的 PID,回報其持有者是否已不存活(可安全接管)。
// 讀不到或解析不出 PID 時保守回 false(不接管),避免誤搶仍在執行的實例。
func lockIsStale(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	return !processAlive(pid)
}

// processAlive 回報 pid 對應的程序是否存活。
//
// 限制(刻意的簡化版,見任務說明):
//   - Windows:以 os.FindProcess(內部 OpenProcess)成功與否判定;句柄可能因 PID 重用而在
//     極少數情況誤報存活,但足以支撐「殘留鎖檔接管」的常見情境。
//   - Unix:os.FindProcess 恆成功,故以 signal 0 探測;EPERM(存在但無權)亦視為存活。
//
// 兩平台皆編譯(以 runtime.GOOS 分流),不需 build tag。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// FindProcess 成功即已開得程序句柄 → 視為存活;釋放句柄避免洩漏。
		_ = p.Release()
		return true
	}
	// Unix:signal 0 不影響目標,只探測存在性。
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
