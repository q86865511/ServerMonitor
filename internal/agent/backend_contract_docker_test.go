//go:build docker

// DockerBackend 掛 backend 契約測試套件(native-backend R1:Mock/Docker/Native 跑同一組不變量)。
// 需 Docker daemon,以 -tags docker 執行;沿用 docker_test.go 的 newITestBackend/itestSpec 前置。
package agent

import (
	"context"
	"testing"

	"servermonitor/internal/protocol"
)

func TestBackendContract_Docker(t *testing.T) {
	runBackendContract(t, backendContract{
		name: "docker",
		newBackend: func(t *testing.T) RuntimeBackend {
			b := newITestBackend(t) // daemon 不可用時於此 Skip
			// 契約結束後清掉本後端建立的容器與資料(避免 daemon 殘留)。
			t.Cleanup(func() {
				refs, err := b.List(context.Background())
				if err == nil {
					for _, r := range refs {
						_ = b.Remove(context.Background(), r.ID, RemoveOpts{Purge: true})
					}
				}
				_ = b.Close()
			})
			return b
		},
		makeSpec:  itestSpec,
		unknownID: protocol.RuntimeID("deadbeefdeadbeefdeadbeefdeadbeef"),
	})
}
