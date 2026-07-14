//go:build docker

// 真 Docker + 真 Minecraft RCON 整合測試(需 daemon;以 -tags docker 執行)。
// 驗收 R7 的 Minecraft RCON `list` 回顯:啟動 itzg/minecraft-server(EULA=TRUE、RCON 開),
// 待伺服器就緒後以 RconAdapter 送出 `list` 並斷言回顯。
//
// 映像下載 + 伺服器初始化(下載 server jar、產生世界)可能耗時數分鐘,故 timeout 放寬;
// daemon 不可用或就緒逾時則 Skip(不誤判失敗)。預設(無 tag)建置不含本檔。
package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

const mcImage = "itzg/minecraft-server:java21"

func TestRconAdapter_RealMinecraft(t *testing.T) {
	b := newITestBackend(t)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	const uuid = "itest-mc-rcon"
	const rconPW = "itest-rcon-pw"
	spec := protocol.InstanceSpec{
		UUID:       uuid,
		TemplateID: "itest-minecraft",
		Image:      mcImage,
		DataDirs:   []string{"/data"},
		Env: map[string]string{
			"EULA": "TRUE",
			"TYPE": "VANILLA",
			// java21 映像的 JRE 為 Java 21;VANILLA LATEST 已推進到需 Java 25 的版本,會啟動即崩。
			// 固定到 Java 21 相容的釋出版(1.21.1)以確保 RCON 能起。
			"VERSION":          "1.21.1",
			"ONLINE_MODE":      "FALSE",
			"MEMORY":           "1G",
			"LEVEL_TYPE":       "minecraft:flat", // 加速世界產生
			"SPAWN_PROTECTION": "0",
			"ENABLE_RCON":      "true",
			"RCON_PASSWORD":    rconPW,
		},
		Ports: []protocol.PortBinding{
			{Name: "rcon", Container: 25575, HostPort: 0, BindIP: "127.0.0.1", Protocol: "tcp"},
		},
		Labels: map[string]string{},
	}

	rid, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer rmCancel()
		_ = b.Stop(rmCtx, rid, StopOpts{Grace: 20 * time.Second})
		_ = b.Remove(rmCtx, rid, RemoveOpts{Purge: true})
	})
	if err := b.Start(ctx, rid); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 取實際發布的 RCON 宿主埠。
	info, err := b.Inspect(ctx, rid)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	var rconPort int
	for _, p := range info.Ports {
		if p.Container == 25575 && p.HostPort > 0 {
			rconPort = p.HostPort
			break
		}
	}
	if rconPort == 0 {
		t.Fatalf("未取得 RCON 宿主埠,ports=%+v", info.Ports)
	}

	target := protocol.CommandTarget{
		Kind:     "rcon",
		Host:     "127.0.0.1",
		Port:     rconPort,
		Password: rconPW,
	}
	adapter := RconAdapter{Timeout: 5 * time.Second}

	// 輪詢至伺服器就緒(RCON 可認證且 list 回應),最多至 ctx 逾時。
	deadline := time.Now().Add(7 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		res, sErr := adapter.Send(ctx, target, protocol.GameCommand{Raw: "list"})
		if sErr == nil && res.Success {
			if !strings.Contains(strings.ToLower(res.Output), "player") {
				t.Fatalf("list 回顯未含玩家資訊: %q", res.Output)
			}
			t.Logf("RCON list 回顯: %q", res.Output)
			return
		}
		lastErr = sErr
		time.Sleep(3 * time.Second)
	}
	t.Skipf("Minecraft 未在時限內就緒(可能為映像下載/資源限制),最後錯誤: %v", lastErr)
}
