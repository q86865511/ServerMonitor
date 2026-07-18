package agent

// 免 Docker 的單元測試,鎖定 DockerBackend 建構時即時 ping daemon 的偵測與友善錯誤包裝。
// 以假 dockerAPI 注入 Ping 結果,不觸真 daemon。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// fakePingCli 只實作 dockerAPI 的 Ping;其餘方法由內嵌介面(nil)承接,本測試不觸及。
type fakePingCli struct {
	dockerAPI
	err error
}

func (f *fakePingCli) Ping(_ context.Context) (types.Ping, error) {
	return types.Ping{}, f.err
}

func TestPingDaemon_Failure(t *testing.T) {
	fake := &fakePingCli{err: errors.New("dial unix docker.sock: connect: no such file or directory")}
	err := pingDaemon(fake)
	if err == nil {
		t.Fatal("預期 ping 失敗回錯,實際為 nil")
	}
	if !strings.Contains(err.Error(), "無法連線 Docker daemon") || !strings.Contains(err.Error(), "Docker Desktop") {
		t.Fatalf("錯誤訊息缺關鍵字: %v", err)
	}
}

func TestPingDaemon_Success(t *testing.T) {
	fake := &fakePingCli{err: nil}
	if err := pingDaemon(fake); err != nil {
		t.Fatalf("預期 ping 成功,實際回錯: %v", err)
	}
}

// TestNewDockerBackend_PingFailurePropagates 以指向必然無回應的 tcp 埠強制連線失敗,
// 驗證 NewDockerBackend 端到端會在建構階段就回錯(而非 lazy 直到實際操作才爆錯)。
func TestNewDockerBackend_PingFailurePropagates(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	b, err := NewDockerBackend(DockerOptions{
		DataRoot:   filepath.Join(t.TempDir(), "data"),
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
	})
	if err == nil {
		b.Close()
		t.Fatal("預期無回應的 DOCKER_HOST 導致建構失敗,實際成功")
	}
	if !strings.Contains(err.Error(), "無法連線 Docker daemon") {
		t.Fatalf("錯誤訊息缺關鍵字: %v", err)
	}
}

func TestFriendlyDockerErr_ConnectionFailure(t *testing.T) {
	raw := client.ErrorConnectionFailed("")
	err := friendlyDockerErr(raw, "檢查映像 x 失敗")
	if !strings.Contains(err.Error(), "Docker 未啟動或連線中斷") || !strings.Contains(err.Error(), "Docker Desktop") {
		t.Fatalf("連線類錯誤未包裝為友善訊息: %v", err)
	}
	if !errors.Is(err, raw) {
		t.Fatalf("包裝後應可 errors.Is 回原始錯誤: %v", err)
	}
}

// TestMapDockerErr_ConnectionFailure 驗證執行期(啟動/停止/備份等共用路徑)daemon 斷線時,
// mapDockerErr 也回友善訊息,不讓 named-pipe 原始錯誤直達 GUI。
func TestMapDockerErr_ConnectionFailure(t *testing.T) {
	raw := client.ErrorConnectionFailed("")
	err := mapDockerErr(raw)
	if !strings.Contains(err.Error(), "Docker 未啟動或連線中斷") {
		t.Fatalf("連線類錯誤未包裝為友善訊息: %v", err)
	}
	if !errors.Is(err, raw) {
		t.Fatalf("包裝後應可 errors.Is 回原始錯誤: %v", err)
	}
	if other := errors.New("some other error"); mapDockerErr(other) != other {
		t.Fatal("非連線/NotFound 錯誤應原樣通過")
	}
}

func TestFriendlyDockerErr_FallbackForOtherErrors(t *testing.T) {
	raw := errors.New("some other docker api error")
	err := friendlyDockerErr(raw, "檢查映像 x 失敗")
	if strings.Contains(err.Error(), "Docker 未啟動或連線中斷") {
		t.Fatalf("非連線類錯誤不應被誤判為友善訊息: %v", err)
	}
	if !strings.Contains(err.Error(), "檢查映像 x 失敗") {
		t.Fatalf("非連線類錯誤應保留 fallback 前綴: %v", err)
	}
	if !errors.Is(err, raw) {
		t.Fatalf("包裝後應可 errors.Is 回原始錯誤: %v", err)
	}
}
