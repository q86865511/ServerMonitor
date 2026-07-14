package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAppLock_SecondInstanceRejected(t *testing.T) {
	dir := t.TempDir()
	l1, err := AcquireAppLock(dir)
	if err != nil {
		t.Fatalf("AcquireAppLock(1): %v", err)
	}
	defer l1.Release()

	if _, err := AcquireAppLock(dir); !errors.Is(err, ErrAppLocked) {
		t.Fatalf("第二次取鎖應回 ErrAppLocked, 得 %v", err)
	}
}

func TestAppLock_ReleaseThenReacquire(t *testing.T) {
	dir := t.TempDir()
	l1, err := AcquireAppLock(dir)
	if err != nil {
		t.Fatalf("AcquireAppLock(1): %v", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := AcquireAppLock(dir)
	if err != nil {
		t.Fatalf("釋放後應可再取, 得 %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("Release(2): %v", err)
	}
}

func TestAppLock_StalePIDTakenOver(t *testing.T) {
	dir := t.TempDir()
	// 殘留鎖檔記錄一個幾乎不可能存活的 PID(模擬崩潰未清)。
	lockPath := filepath.Join(dir, appLockFileName)
	if err := os.WriteFile(lockPath, []byte("999999999\n"), 0o644); err != nil {
		t.Fatalf("寫殘留鎖檔: %v", err)
	}
	l, err := AcquireAppLock(dir)
	if err != nil {
		t.Fatalf("stale 鎖應可接管, 得 %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestAppLock_LiveLockNotStale(t *testing.T) {
	dir := t.TempDir()
	// 鎖檔記錄本程序 PID(存活)→ 不應被判為 stale、不可接管。
	lockPath := filepath.Join(dir, appLockFileName)
	if err := os.WriteFile(lockPath, []byte(itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatalf("寫鎖檔: %v", err)
	}
	if _, err := AcquireAppLock(dir); !errors.Is(err, ErrAppLocked) {
		t.Fatalf("存活 PID 的鎖不應被接管, 得 %v", err)
	}
}

// itoa 避免 import strconv 只為一處;保持測試精簡。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
