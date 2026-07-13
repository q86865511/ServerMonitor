package core

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

// memoryKeyring 是 Keyring 的記憶體替身,使 SecretStore 測試不觸碰真實
// Windows Credential Manager。Get 查無時回傳 keyring.ErrNotFound,
// 以驗證 SecretStore 對「查無」的映射。
type memoryKeyring struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemoryKeyring() *memoryKeyring {
	return &memoryKeyring{m: make(map[string]string)}
}

func (k *memoryKeyring) key(service, user string) string {
	return service + "\x00" + user
}

func (k *memoryKeyring) Set(service, user, password string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[k.key(service, user)] = password
	return nil
}

func (k *memoryKeyring) Get(service, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[k.key(service, user)]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (k *memoryKeyring) Delete(service, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	kk := k.key(service, user)
	if _, ok := k.m[kk]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.m, kk)
	return nil
}

// newTempStore 在暫存目錄開一個 Store,回傳 store 與 DB 檔路徑,並註冊清理。
func newTempStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gsm.db")
	st, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}
