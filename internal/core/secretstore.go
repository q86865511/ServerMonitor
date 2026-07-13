package core

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"

	"servermonitor/internal/protocol"
)

// DefaultKeyringService 是寫入 OS 金鑰庫時使用的服務名(Windows Credential Manager
// 的 target 前綴)。實值以 (service, SecretRef.Key) 定位。
const DefaultKeyringService = "ServerMonitor"

// ErrSecretNotFound 表示金鑰庫中查無該 SecretRef。
var ErrSecretNotFound = errors.New("core: 金鑰庫查無此祕密")

// Keyring 抽象 OS 金鑰庫存取,使 SecretStore 可注入測試替身,
// 生產環境則委派 github.com/zalando/go-keyring(Windows Credential Manager backend)。
type Keyring interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

// osKeyring 委派 zalando/go-keyring 套件層級函式。
type osKeyring struct{}

func (osKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (osKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyring) Delete(service, user string) error        { return keyring.Delete(service, user) }

// SecretStore 以 SecretRef 為介面存取敏感值(R12):實值一律寫 OS 金鑰庫,
// DB 只保存 SecretRef.Key 參照,確保 DB 內不含明文;DTO/event/log 經 SecretRef 統一 redaction。
type SecretStore struct {
	service string
	kr      Keyring
}

// NewSecretStore 建立以 OS 金鑰庫為後端的 SecretStore。
func NewSecretStore(service string) *SecretStore {
	if service == "" {
		service = DefaultKeyringService
	}
	return &SecretStore{service: service, kr: osKeyring{}}
}

// NewSecretStoreWithKeyring 以注入的 Keyring 建立 SecretStore(供測試或替代後端)。
func NewSecretStoreWithKeyring(service string, kr Keyring) *SecretStore {
	if service == "" {
		service = DefaultKeyringService
	}
	return &SecretStore{service: service, kr: kr}
}

// Set 將實值寫入金鑰庫,鍵名取自 ref.Key。實值不落 DB、不回顯。
func (s *SecretStore) Set(ref protocol.SecretRef, value string) error {
	if ref.Key == "" {
		return errors.New("core: SecretRef.Key 不可為空")
	}
	if err := s.kr.Set(s.service, ref.Key, value); err != nil {
		return fmt.Errorf("寫入金鑰庫失敗: %w", err)
	}
	return nil
}

// Get 依 ref.Key 自金鑰庫取回實值;查無回 ErrSecretNotFound。
func (s *SecretStore) Get(ref protocol.SecretRef) (string, error) {
	if ref.Key == "" {
		return "", errors.New("core: SecretRef.Key 不可為空")
	}
	v, err := s.kr.Get(s.service, ref.Key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("讀取金鑰庫失敗: %w", err)
	}
	return v, nil
}

// Delete 自金鑰庫移除該祕密(冪等:查無視為成功,支援 secret 刪除/輪替)。
func (s *SecretStore) Delete(ref protocol.SecretRef) error {
	if ref.Key == "" {
		return errors.New("core: SecretRef.Key 不可為空")
	}
	err := s.kr.Delete(s.service, ref.Key)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("刪除金鑰庫祕密失敗: %w", err)
}
