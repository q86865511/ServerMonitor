package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/protocol"
)

// testSpec 造一個帶 gsm.uuid 標籤的 spec,使代理的 resolve(以標籤解析)可用。
func testSpec(uuid string) protocol.InstanceSpec {
	return protocol.InstanceSpec{
		UUID:   uuid,
		Image:  "example/image:1.0",
		Labels: map[string]string{labelUUID: uuid},
	}
}

func TestNodeClient_CreateListInspectStatus(t *testing.T) {
	hs, _ := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	ctx := context.Background()
	const uuid = "11111111-2222-3333-4444-555555555555"

	resp, err := c.Create(ctx, testSpec(uuid))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.UUID != uuid || resp.RuntimeID == "" {
		t.Fatalf("Create resp = %+v", resp)
	}

	list, err := c.List(ctx)
	if err != nil || len(list) != 1 || list[0].UUID != uuid {
		t.Fatalf("List = %+v, err=%v", list, err)
	}

	info, err := c.Inspect(ctx, uuid)
	if err != nil || info.ID != resp.RuntimeID {
		t.Fatalf("Inspect = %+v, err=%v", info, err)
	}

	st, err := c.Status(ctx, uuid)
	if err != nil || st.ID != resp.RuntimeID {
		t.Fatalf("Status = %+v, err=%v", st, err)
	}

	h, err := c.Health(ctx)
	if err != nil || h.Status != "ok" {
		t.Fatalf("Health = %+v, err=%v", h, err)
	}
}

func TestNodeClient_ErrorMapping_NotFound(t *testing.T) {
	hs, _ := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	_, err := c.Status(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("期望 ErrNodeNotFound,得 %v", err)
	}
}

func TestNodeClient_ErrorMapping_Unauthorized(t *testing.T) {
	hs, _ := newAgentServer(t)
	c := NewNodeClient(hs.URL, "wrong-token", nil)
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrNodeUnauthorized) {
		t.Fatalf("期望 ErrNodeUnauthorized,得 %v", err)
	}
}

func TestNodeClient_ErrorMapping_PortConflict(t *testing.T) {
	// 以包裝後端讓 Create 回 agent.ErrPortConflict,驗證映射為 ErrNodePortConflict。
	backend := &portConflictBackend{MockBackend: agent.NewMockBackend()}
	hs := newAgentServerWithBackend(t, backend)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	_, err := c.Create(context.Background(), testSpec("x"))
	if !errors.Is(err, ErrNodePortConflict) {
		t.Fatalf("期望 ErrNodePortConflict,得 %v", err)
	}
}

func TestNodeClient_Unreachable(t *testing.T) {
	// 起後立刻關閉,使連線被拒。
	dead := httptest.NewServer(nil)
	url := dead.URL
	dead.Close()

	c := NewNodeClient(url, agentTestToken, nil)
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("期望 ErrNodeUnreachable,得 %v", err)
	}
}

func TestNodeClient_EventsWS(t *testing.T) {
	hs, backend := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	const uuid = "evt-uuid"

	rid, err := backend.Create(context.Background(), testSpec(uuid))
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	backend.InjectDie(rid, 7) // cursor=1,以 since=0 訂閱補送

	conn, err := c.EventsWS(context.Background(), protocol.Cursor("0"))
	if err != nil {
		t.Fatalf("EventsWS: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ev protocol.RuntimeEvent
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if ev.Kind != protocol.RuntimeEventDie || ev.ExitCode == nil || *ev.ExitCode != 7 {
		t.Fatalf("event = %+v", ev)
	}
}

// countingCommandAdapter 記錄 Send 被呼叫次數(供驗證指令是否真的送達,而非被冪等重播)。
type countingCommandAdapter struct {
	mu    sync.Mutex
	calls int
}

func (a *countingCommandAdapter) Send(_ context.Context, _ protocol.CommandTarget, cmd protocol.GameCommand) (protocol.CommandResult, error) {
	a.mu.Lock()
	a.calls++
	n := a.calls
	a.mu.Unlock()
	return protocol.CommandResult{Success: true, Output: fmt.Sprintf("call-%d:%s", n, cmd.Raw)}, nil
}

func (a *countingCommandAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// TestNodeClient_Idempotency_DistinctCommandsBothReachBackend 是 A-client 修正的迴歸測試:
// 同一實例連續兩個「不同」command 呼叫,先前因把實例 UUID 當冪等鍵,第二個會在 TTL 內被重播
// 為第一個的結果(不送達轉接器)。修正後每次呼叫應各自產生新鍵,兩者都真正送達。
func TestNodeClient_Idempotency_DistinctCommandsBothReachBackend(t *testing.T) {
	backend := agent.NewMockBackend()
	adapter := &countingCommandAdapter{}
	s, err := agent.NewServer(agent.Config{Backend: backend, Commands: adapter, Token: agentTestToken})
	if err != nil {
		t.Fatalf("agent.NewServer: %v", err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() { hs.Close(); _ = s.Close() })

	c := NewNodeClient(hs.URL, agentTestToken, nil)
	ctx := context.Background()
	const uuid = "cmd-uuid"
	if _, err := backend.Create(ctx, testSpec(uuid)); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	res1, err := c.Command(ctx, uuid, protocol.CommandTarget{Kind: "rcon"}, protocol.GameCommand{ProtocolID: "p", Raw: "say hi"})
	if err != nil {
		t.Fatalf("Command 1: %v", err)
	}
	res2, err := c.Command(ctx, uuid, protocol.CommandTarget{Kind: "rcon"}, protocol.GameCommand{ProtocolID: "p", Raw: "save-all"})
	if err != nil {
		t.Fatalf("Command 2: %v", err)
	}

	if adapter.count() != 2 {
		t.Fatalf("轉接器應被呼叫 2 次(兩個不同指令都真正送達),得 %d 次", adapter.count())
	}
	if res1.Output == res2.Output {
		t.Fatalf("兩次不同指令的結果不應相同(第二次疑似被重播為第一次),得 res1=%q res2=%q", res1.Output, res2.Output)
	}
}

// TestNodeClient_Idempotency_StartStopStartAllExecute 是 A-client 修正的迴歸測試:
// start→stop→start 三次操作,先前第二個 start 與第一個 start 共用同一冪等鍵(同 method+path),
// 於 TTL 內被重播為第一次 start 的(陳舊)結果、容器實際上未真正再次啟動。
// 修正後三次呼叫都應真正執行,最終 runtime 狀態為 Running。
func TestNodeClient_Idempotency_StartStopStartAllExecute(t *testing.T) {
	hs, backend := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	ctx := context.Background()
	const uuid = "start-stop-start-uuid"

	rid, err := backend.Create(ctx, testSpec(uuid))
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}

	if err := c.Start(ctx, uuid); err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	if err := c.Stop(ctx, uuid, protocol.StopInstanceRequest{}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Start(ctx, uuid); err != nil {
		t.Fatalf("Start 2: %v", err)
	}

	st, err := backend.Status(ctx, rid)
	if err != nil {
		t.Fatalf("backend.Status: %v", err)
	}
	if !st.Running {
		t.Fatalf("第二次 start 後應真正 Running(不應被第一次 start 的冪等快取重播),得狀態 = %+v", st)
	}
}

// TestNodeClient_Idempotency_ConsecutiveBackupsBothPersist 是 A-client 修正的迴歸測試:
// 同一實例連續兩次 backup,先前共用 uuid 為冪等鍵,第二次會在 TTL 內被重播為第一次的
// (陳舊)備份中繼、未真正再次執行備份。修正後兩次都應真正執行,產生兩筆相異備份。
func TestNodeClient_Idempotency_ConsecutiveBackupsBothPersist(t *testing.T) {
	hs, backend := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	ctx := context.Background()
	const uuid = "backup-uuid"

	if _, err := backend.Create(ctx, testSpec(uuid)); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	b1, err := c.Backup(ctx, uuid)
	if err != nil {
		t.Fatalf("Backup 1: %v", err)
	}
	b2, err := c.Backup(ctx, uuid)
	if err != nil {
		t.Fatalf("Backup 2: %v", err)
	}
	if b1.BackupID == b2.BackupID {
		t.Fatalf("兩次備份不應得到同一 BackupID(第二次疑似被重播為第一次),得 %q", b1.BackupID)
	}

	backups, err := c.ListBackups(ctx, uuid)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("應有 2 份備份(兩次呼叫都真正執行),得 %d 份", len(backups))
	}
}

// TestNodeClient_UploadMount:上傳手動模組包檔到已宣告的 mount → 位元組落入 agent;
// 未知實例 → ErrNodeNotFound。
func TestNodeClient_UploadMount(t *testing.T) {
	hs, backend := newAgentServer(t)
	c := NewNodeClient(hs.URL, agentTestToken, nil)
	ctx := context.Background()
	const uuid = "mount-uuid"

	spec := testSpec(uuid)
	spec.Mounts = []protocol.MountSpec{{Name: "modpack", ContainerPath: "/modpacks"}}
	if _, err := backend.Create(ctx, spec); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	payload := []byte("PK\x03\x04 fake modpack")
	if err := c.UploadMount(ctx, uuid, "modpack", "world.mrpack", bytes.NewReader(payload)); err != nil {
		t.Fatalf("UploadMount: %v", err)
	}
	got, ok := backend.MountFile(uuid, "modpack", "world.mrpack")
	if !ok || string(got) != string(payload) {
		t.Fatalf("MountFile = %q ok=%v, 期望 %q", got, ok, payload)
	}

	// 未知實例 → 404 映射為 ErrNodeNotFound。
	if err := c.UploadMount(ctx, "nope", "modpack", "world.mrpack", bytes.NewReader(payload)); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("未知實例上傳期望 ErrNodeNotFound,得 %v", err)
	}
}

// TestNodeClient_PerOperationTimeout 驗證逾時分級機制(T16 雙審 #2):短操作(Status)以短逾時
// 保護、長操作(Stop/Backup/Restore)以較長逾時不被短逾時截斷、且呼叫端顯式較嚴 deadline 恆以其
// 為準。以縮小的預設值(ms 級)驗機制,不必等真實 30s 級逾時;測後還原全域預設。
func TestNodeClient_PerOperationTimeout(t *testing.T) {
	origShort, origStop := defaultNodeHTTPTimeout, defaultNodeStopTimeout
	defaultNodeHTTPTimeout, defaultNodeStopTimeout = 60*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { defaultNodeHTTPTimeout, defaultNodeStopTimeout = origShort, origStop })

	const delay = 180 * time.Millisecond // 介於短(60ms)與長(400ms)預設之間
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := NewNodeClient(srv.URL, agentTestToken, nil)
	const uuid = "timeout-uuid"

	// 短操作(Status):60ms 預設 < 180ms 延遲 → 被短逾時截斷(傳輸層失敗映射為 ErrNodeUnreachable)。
	if _, err := c.Status(context.Background(), uuid); !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("短操作應因 60ms 預設逾時而截斷(ErrNodeUnreachable),得 %v", err)
	}
	// 長操作(Stop):400ms 預設 > 180ms 延遲 → 不被短逾時截斷,正常完成。
	if err := c.Stop(context.Background(), uuid, protocol.StopInstanceRequest{}); err != nil {
		t.Fatalf("長操作(Stop)不應被短逾時截斷,得 %v", err)
	}
	// 呼叫端顯式帶較嚴 deadline(50ms < 180ms)時以其為準,即使是長操作亦被截斷。
	sctx, scancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer scancel()
	if err := c.Stop(sctx, uuid, protocol.StopInstanceRequest{}); !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("呼叫端較嚴 deadline 應以其為準截斷長操作,得 %v", err)
	}
}

// TestNodeClient_CreateLongTimeout 驗證 Create 走長逾時分級(雙審 #1):native 首次供應可遠超短操作
// 30s(實測 108s+),呼叫端 ctx 無 deadline 時不受短逾時截斷;呼叫端顯式較嚴 deadline 仍以其為準。
// 以縮小的預設值(ms 級)驗機制,不必等真實 30min;測後還原全域預設。
func TestNodeClient_CreateLongTimeout(t *testing.T) {
	origShort, origCreate := defaultNodeHTTPTimeout, defaultNodeCreateTimeout
	defaultNodeHTTPTimeout, defaultNodeCreateTimeout = 60*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { defaultNodeHTTPTimeout, defaultNodeCreateTimeout = origShort, origCreate })

	const delay = 180 * time.Millisecond // 介於短(60ms)與 create 長(400ms)預設之間
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := NewNodeClient(srv.URL, agentTestToken, nil)
	spec := protocol.InstanceSpec{UUID: "create-timeout-uuid"}

	// ctx 無 deadline:400ms create 預設 > 180ms 延遲 → 不被短逾時截斷,正常完成。
	if _, err := c.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create 不應被短逾時截斷(應走 create 長逾時),得 %v", err)
	}
	// 呼叫端顯式帶較嚴 deadline(50ms < 180ms)時以其為準,即使 Create 亦被截斷。
	cctx, ccancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer ccancel()
	if _, err := c.Create(cctx, spec); !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("呼叫端較嚴 deadline 應以其為準截斷 Create,得 %v", err)
	}
}

// portConflictBackend 讓 Create 一律回 agent.ErrPortConflict(其餘委派 MockBackend)。
type portConflictBackend struct {
	*agent.MockBackend
}

func (b *portConflictBackend) Create(ctx context.Context, spec protocol.InstanceSpec) (protocol.RuntimeID, error) {
	return "", agent.ErrPortConflict
}
