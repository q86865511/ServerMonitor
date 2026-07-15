package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// ---- 測試替身:可腳本化回應序列的 fake Discord webhook 端點 ----

// stubResp 是 discordStub 依收到請求次序回應的一筆腳本回應。
type stubResp struct {
	status     int
	retryAfter string
}

// recordedRequest 是 discordStub 記錄的一次收到請求(已解析主體 + 原始位元組,供斷言
// schema 與「不含 URL」)。
type recordedRequest struct {
	body discordWebhookBody
	raw  string
}

// discordStub 依 script 依序回應(索引超出 script 長度時回 200),供重試策略測試。
type discordStub struct {
	mu       sync.Mutex
	requests []recordedRequest
	script   []stubResp
}

func (s *discordStub) handler(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body discordWebhookBody
	_ = json.Unmarshal(data, &body)

	s.mu.Lock()
	idx := len(s.requests)
	s.requests = append(s.requests, recordedRequest{body: body, raw: string(data)})
	resp := stubResp{status: http.StatusOK}
	if idx < len(s.script) {
		resp = s.script[idx]
	}
	s.mu.Unlock()

	if resp.retryAfter != "" {
		w.Header().Set("Retry-After", resp.retryAfter)
	}
	w.WriteHeader(resp.status)
}

func (s *discordStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *discordStub) requestAt(i int) recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[i]
}

func fieldEquals(fields []discordField, name, want string) bool {
	for _, f := range fields {
		if f.Name == name {
			return f.Value == want
		}
	}
	return false
}

func hasField(fields []discordField, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// noSleep 是重試測試共用的「立即返回」睡眠替身,避免真的等待退避秒數。
func noSleep(context.Context, time.Duration) error { return nil }

// ---- 四類事件固定 payload ----

func TestAlertDispatcher_FourEventPayloads(t *testing.T) {
	stub := &discordStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: DiscordWebhookSecretKey}, srv.URL); err != nil {
		t.Fatalf("Set webhook: %v", err)
	}
	channel := NewDiscordWebhookChannel(secrets, "", WithDiscordHTTPClient(srv.Client()))
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	d := NewAlertDispatcher(AlertDispatcherConfig{Channel: channel, Now: clk.Now})

	cases := []struct {
		name string
		ev   protocol.Event
	}{
		{"crashed", protocol.Event{
			Code: protocol.EventInstanceCrashed, Severity: protocol.SeverityError,
			InstanceUUID: strPtr("inst-1"), TsUTC: clk.Now(),
			DetailsJSON: mustJSON(map[string]any{"exit_code": 1}),
		}},
		{"giveup", protocol.Event{
			Code: protocol.EventRestartGiveup, Severity: protocol.SeverityError,
			InstanceUUID: strPtr("inst-2"), TsUTC: clk.Now(),
			DetailsJSON: mustJSON(map[string]any{"crashes_in_window": 3}),
		}},
		{"backup_failed", protocol.Event{
			Code: protocol.EventBackupFailed, Severity: protocol.SeverityError,
			InstanceUUID: strPtr("inst-3"), TsUTC: clk.Now(),
			DetailsJSON: mustJSON(map[string]any{"error": "disk full"}),
		}},
		{"resource_threshold", protocol.Event{
			Code: EventResourceThreshold, Severity: protocol.SeverityWarning,
			InstanceUUID: strPtr("inst-4"), TsUTC: clk.Now(),
			DetailsJSON: mustJSON(map[string]any{"metric": "cpu", "value_percent": 95.0, "threshold_percent": 80.0}),
		}},
	}

	for _, tc := range cases {
		if err := d.Alert(context.Background(), tc.ev); err != nil {
			t.Fatalf("%s: Alert: %v", tc.name, err)
		}
	}

	if n := stub.count(); n != len(cases) {
		t.Fatalf("收到請求數 = %d, 期望 %d", n, len(cases))
	}
	for i, tc := range cases {
		req := stub.requestAt(i)
		if len(req.body.Embeds) != 1 {
			t.Fatalf("%s: embeds 數 = %d, 期望 1", tc.name, len(req.body.Embeds))
		}
		if req.body.Embeds[0].Title != string(tc.ev.Code) {
			t.Errorf("%s: title = %q, 期望 %q", tc.name, req.body.Embeds[0].Title, tc.ev.Code)
		}
		if !fieldEquals(req.body.Embeds[0].Fields, "instance", *tc.ev.InstanceUUID) {
			t.Errorf("%s: instance 欄位缺或不符: %+v", tc.name, req.body.Embeds[0].Fields)
		}
		if !hasField(req.body.Embeds[0].Fields, "time_utc") {
			t.Errorf("%s: 缺 time_utc 欄位", tc.name)
		}
		if !hasField(req.body.Embeds[0].Fields, "severity") {
			t.Errorf("%s: 缺 severity 欄位", tc.name)
		}
		if req.body.Content == "" {
			t.Errorf("%s: content 不應為空", tc.name)
		}
	}
}

// ---- dedup key + cooldown 去重 ----

func TestAlertDispatcher_DedupCooldown(t *testing.T) {
	stub := &discordStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: DiscordWebhookSecretKey}, srv.URL); err != nil {
		t.Fatalf("Set webhook: %v", err)
	}
	channel := NewDiscordWebhookChannel(secrets, "", WithDiscordHTTPClient(srv.Client()))
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	d := NewAlertDispatcher(AlertDispatcherConfig{Channel: channel, Now: clk.Now, Cooldown: 5 * time.Minute})

	ev := protocol.Event{Code: protocol.EventRestartGiveup, InstanceUUID: strPtr("inst-1"), TsUTC: clk.Now()}
	if err := d.Alert(context.Background(), ev); err != nil {
		t.Fatalf("首次 Alert: %v", err)
	}
	if n := stub.count(); n != 1 {
		t.Fatalf("首次應送出, 得 %d", n)
	}

	clk.Advance(1 * time.Minute)
	if err := d.Alert(context.Background(), ev); err != nil {
		t.Fatalf("cooldown 內 Alert: %v", err)
	}
	if n := stub.count(); n != 1 {
		t.Errorf("cooldown 內應去重、只送 1 次, 得 %d", n)
	}

	clk.Advance(5 * time.Minute) // 累計 6 分鐘,超過 5 分鐘 cooldown
	if err := d.Alert(context.Background(), ev); err != nil {
		t.Fatalf("過 cooldown 後 Alert: %v", err)
	}
	if n := stub.count(); n != 2 {
		t.Errorf("過 cooldown 應再送一次, 得 %d", n)
	}

	// 不同 instance 不共用 dedup key,應各自送出。
	ev2 := protocol.Event{Code: protocol.EventRestartGiveup, InstanceUUID: strPtr("inst-2"), TsUTC: clk.Now()}
	if err := d.Alert(context.Background(), ev2); err != nil {
		t.Fatalf("不同實例 Alert: %v", err)
	}
	if n := stub.count(); n != 3 {
		t.Errorf("不同實例應各自送出, 得 %d", n)
	}
}

// ---- 無管道:僅記錄不報錯 ----

func TestAlertDispatcher_NoChannel(t *testing.T) {
	d := NewAlertDispatcher(AlertDispatcherConfig{})
	ev := protocol.Event{Code: protocol.EventBackupFailed, InstanceUUID: strPtr("x")}
	if err := d.Alert(context.Background(), ev); err != nil {
		t.Fatalf("無管道應 no-op 不報錯, 得 %v", err)
	}
}

func TestAlertDispatcher_ChannelNotConfigured(t *testing.T) {
	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring()) // 未 Set webhook
	channel := NewDiscordWebhookChannel(secrets, "")
	d := NewAlertDispatcher(AlertDispatcherConfig{Channel: channel})
	ev := protocol.Event{Code: protocol.EventBackupFailed, InstanceUUID: strPtr("x")}
	if err := d.Alert(context.Background(), ev); err != nil {
		t.Fatalf("webhook 未設定應 no-op 不報錯, 得 %v", err)
	}
}

// ---- DiscordWebhookChannel 重試策略 ----

func TestDiscordWebhookChannel_RetryOn429UsesRetryAfter(t *testing.T) {
	stub := &discordStub{script: []stubResp{{status: http.StatusTooManyRequests, retryAfter: "0"}, {status: http.StatusOK}}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: "k"}, srv.URL); err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	channel := NewDiscordWebhookChannel(secrets, "k",
		WithDiscordHTTPClient(srv.Client()),
		WithDiscordSleep(func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}))

	err := channel.Send(context.Background(), AlertPayload{Code: protocol.EventBackupFailed, InstanceUUID: "x", TsUTC: time.Now()})
	if err != nil {
		t.Fatalf("429 重試後應成功: %v", err)
	}
	if n := stub.count(); n != 2 {
		t.Errorf("應送出 2 次請求, 得 %d", n)
	}
	if len(slept) != 1 || slept[0] != 0 {
		t.Errorf("應依 Retry-After=0 等待 0, 得 %v", slept)
	}
}

func TestDiscordWebhookChannel_RetryOn5xxBackoff(t *testing.T) {
	stub := &discordStub{script: []stubResp{{status: 500}, {status: 502}, {status: http.StatusOK}}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: "k"}, srv.URL); err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	channel := NewDiscordWebhookChannel(secrets, "k",
		WithDiscordHTTPClient(srv.Client()),
		WithDiscordSleep(func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}))

	err := channel.Send(context.Background(), AlertPayload{Code: protocol.EventBackupFailed, InstanceUUID: "x", TsUTC: time.Now()})
	if err != nil {
		t.Fatalf("5xx 重試後應成功: %v", err)
	}
	if n := stub.count(); n != 3 {
		t.Errorf("應送出 3 次請求, 得 %d", n)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second}
	if len(slept) != 2 || slept[0] != want[0] || slept[1] != want[1] {
		t.Errorf("退避序列 = %v, 期望 %v(base 2s, 倍增)", slept, want)
	}
}

func TestDiscordWebhookChannel_4xxNoRetry(t *testing.T) {
	stub := &discordStub{script: []stubResp{{status: http.StatusBadRequest}}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: "k"}, srv.URL); err != nil {
		t.Fatal(err)
	}
	channel := NewDiscordWebhookChannel(secrets, "k", WithDiscordHTTPClient(srv.Client()), WithDiscordSleep(noSleep))

	err := channel.Send(context.Background(), AlertPayload{Code: protocol.EventBackupFailed, InstanceUUID: "x", TsUTC: time.Now()})
	if err == nil {
		t.Fatal("4xx 應回錯誤")
	}
	if n := stub.count(); n != 1 {
		t.Errorf("4xx 不應重試, 請求數 = %d, 期望 1", n)
	}
}

func TestDiscordWebhookChannel_RetriesExhausted(t *testing.T) {
	stub := &discordStub{script: []stubResp{{status: 500}, {status: 500}, {status: 500}}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: "k"}, srv.URL); err != nil {
		t.Fatal(err)
	}
	channel := NewDiscordWebhookChannel(secrets, "k", WithDiscordHTTPClient(srv.Client()), WithDiscordSleep(noSleep))

	err := channel.Send(context.Background(), AlertPayload{Code: protocol.EventBackupFailed, InstanceUUID: "x", TsUTC: time.Now()})
	if err == nil {
		t.Fatal("應在耗盡重試後回錯誤")
	}
	if n := stub.count(); n != defaultAlertRetryMax {
		t.Errorf("應嘗試 %d 次, 得 %d", defaultAlertRetryMax, n)
	}
}

// ---- webhook URL 不外洩於錯誤訊息 ----

func TestDiscordWebhookChannel_ErrorsDoNotLeakURL(t *testing.T) {
	stub := &discordStub{script: []stubResp{{status: 500}, {status: 500}, {status: 500}}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secretURL := srv.URL + "/webhooks/123/super-secret-token"
	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: "k"}, secretURL); err != nil {
		t.Fatal(err)
	}
	channel := NewDiscordWebhookChannel(secrets, "k", WithDiscordHTTPClient(srv.Client()), WithDiscordSleep(noSleep))

	err := channel.Send(context.Background(), AlertPayload{Code: protocol.EventBackupFailed, InstanceUUID: "x", TsUTC: time.Now()})
	if err == nil {
		t.Fatal("應失敗")
	}
	if strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("錯誤訊息不應包含 webhook URL/token: %v", err)
	}
}

// ---- 跨層接線:RestartPolicy(giveup)→ AlertDispatcher → fake Discord 端點 ----

func TestAlertDispatcher_CrossLayer_RestartGiveup(t *testing.T) {
	stub := &discordStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: DiscordWebhookSecretKey}, srv.URL); err != nil {
		t.Fatal(err)
	}
	channel := NewDiscordWebhookChannel(secrets, "", WithDiscordHTTPClient(srv.Client()))
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	dispatcher := NewAlertDispatcher(AlertDispatcherConfig{Channel: channel, Now: clk.Now})

	store, _ := newTempStore(t)
	events := NewEventLog(store, nil, EventLogOptions{})
	rp := NewRestartPolicy(RestartPolicyConfig{
		Store: store, Events: events, Alerts: dispatcher, Now: clk.Now,
		Window: 5 * time.Minute, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return 0 },
		after:   syncAfter,
	})
	rp.SetRestart(func(context.Context, string) error { return nil }, func(string) {})

	ec := 1
	for i := 0; i < 3; i++ {
		rp.OnCrash("giveup-1", &ec)
		clk.Advance(time.Second)
	}

	if n := stub.count(); n != 1 {
		t.Fatalf("跨層告警應送達 Discord 1 次, 得 %d", n)
	}
	req := stub.requestAt(0)
	if req.body.Embeds[0].Title != string(protocol.EventRestartGiveup) {
		t.Errorf("payload code = %q, 期望 %q", req.body.Embeds[0].Title, protocol.EventRestartGiveup)
	}
	if !fieldEquals(req.body.Embeds[0].Fields, "instance", "giveup-1") {
		t.Errorf("payload 缺 instance 欄位或不符: %+v", req.body.Embeds[0].Fields)
	}
	if n := len(queryEvents(t, events, protocol.EventAlertSent)); n != 1 {
		t.Errorf("ALERT_SENT = %d, 期望 1", n)
	}
	if n := len(queryEvents(t, events, protocol.EventRestartGiveup)); n != 1 {
		t.Errorf("RESTART_GIVEUP = %d, 期望 1", n)
	}
}

// ---- 雙審 #7:告警逾時獨立於探針逾時,足夠容納 3 次重試+退避 ----

func TestDiscordWebhookChannel_AlertDispatchTimeoutSurvivesRealBackoff(t *testing.T) {
	if defaultProbeTimeout >= alertDispatchTimeout {
		t.Fatalf("alertDispatchTimeout(%v) 應獨立於且遠大於 defaultProbeTimeout(%v)", alertDispatchTimeout, defaultProbeTimeout)
	}

	stub := &discordStub{script: []stubResp{{status: 500}, {status: 502}, {status: http.StatusOK}}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: "k"}, srv.URL); err != nil {
		t.Fatal(err)
	}
	// 不覆寫 sleep:用真實退避(2s + 4s = 6s 累計),驗證 alertDispatchTimeout(90s)不截斷
	// 這段重試;若沿用舊的 defaultProbeTimeout(5s)必然在第 3 次請求前就被 ctx 取消。
	channel := NewDiscordWebhookChannel(secrets, "k", WithDiscordHTTPClient(srv.Client()))

	ctx, cancel := context.WithTimeout(context.Background(), alertDispatchTimeout)
	defer cancel()
	err := channel.Send(ctx, AlertPayload{Code: protocol.EventBackupFailed, InstanceUUID: "x", TsUTC: time.Now()})
	if err != nil {
		t.Fatalf("應在 alertDispatchTimeout 內完成 3 次嘗試並成功: %v", err)
	}
	if n := stub.count(); n != 3 {
		t.Errorf("應送出 3 次請求, 得 %d", n)
	}
}

// ---- 雙審 #8:dedup 失敗不佔 cooldown,成功才佔 ----

func TestAlertDispatcher_DedupNotHeldOnFailure(t *testing.T) {
	// script 只 3 筆(全 500);第 4 筆起 discordStub 預設回 200,模擬「第一次全部失敗、
	// 第二次緊接著成功」的情境。
	stub := &discordStub{script: []stubResp{{status: 500}, {status: 500}, {status: 500}}}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: DiscordWebhookSecretKey}, srv.URL); err != nil {
		t.Fatal(err)
	}
	channel := NewDiscordWebhookChannel(secrets, "", WithDiscordHTTPClient(srv.Client()), WithDiscordSleep(noSleep))
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	d := NewAlertDispatcher(AlertDispatcherConfig{Channel: channel, Now: clk.Now, Cooldown: 5 * time.Minute})

	ev := protocol.Event{Code: protocol.EventBackupFailed, InstanceUUID: strPtr("inst-1"), TsUTC: clk.Now()}
	if err := d.Alert(context.Background(), ev); err == nil {
		t.Fatal("首次應因 Discord 端持續 5xx、重試耗盡而失敗")
	}
	if n := stub.count(); n != defaultAlertRetryMax {
		t.Fatalf("首次應耗盡 %d 次重試, 得請求數 %d", defaultAlertRetryMax, n)
	}

	// 失敗不應佔用 cooldown:立即再次 Alert(同 key)應真的再嘗試送出,而非被去重擋掉。
	clk.Advance(time.Second)
	if err := d.Alert(context.Background(), ev); err != nil {
		t.Fatalf("第二次應成功送達(未被首次失敗佔用 cooldown): %v", err)
	}
	if n := stub.count(); n != defaultAlertRetryMax+1 {
		t.Errorf("失敗不應佔用 cooldown,第二次應再送 1 次請求, 得總請求數 %d, 期望 %d", n, defaultAlertRetryMax+1)
	}

	// 成功後才佔用 cooldown:同 key 在 cooldown 內第三次應被去重、不再送。
	clk.Advance(time.Second)
	if err := d.Alert(context.Background(), ev); err != nil {
		t.Fatalf("第三次(cooldown 內)Alert: %v", err)
	}
	if n := stub.count(); n != defaultAlertRetryMax+1 {
		t.Errorf("成功後應佔用 cooldown、第三次應去重不送, 得總請求數 %d, 期望維持 %d", n, defaultAlertRetryMax+1)
	}
}

// ---- 雙審 #9:Retry-After 支援小數秒與 HTTP-date ----

func TestParseRetryAfter_DecimalSeconds(t *testing.T) {
	d, ok := parseRetryAfter("0.5")
	if !ok {
		t.Fatal("應可解析小數秒 Retry-After")
	}
	if d != 500*time.Millisecond {
		t.Errorf("got %v, want 500ms", d)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	target := time.Now().Add(3 * time.Second)
	d, ok := parseRetryAfter(target.UTC().Format(http.TimeFormat))
	if !ok {
		t.Fatal("應可解析 HTTP-date 格式 Retry-After")
	}
	// http.TimeFormat 只到秒精度,容許誤差。
	if d < 1*time.Second || d > 4*time.Second {
		t.Errorf("got %v, want 約 3s", d)
	}
}

func TestParseRetryAfter_PastHTTPDateClampsToZero(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour)
	d, ok := parseRetryAfter(past.UTC().Format(http.TimeFormat))
	if !ok {
		t.Fatal("應可解析 HTTP-date 格式 Retry-After")
	}
	if d != 0 {
		t.Errorf("已過期的 Retry-After 應夾到 0, 得 %v", d)
	}
}

// ---- 雙審 #10:畸形 URL 建立請求失敗時,錯誤不外洩原始 URL ----

func TestAlertDispatcher_MalformedURLErrorDoesNotLeakIntoEvent(t *testing.T) {
	secretToken := "super-secret-token"
	// DEL(0x7f)是 net/url 視為無效的控制字元,保證 http.NewRequestWithContext 失敗。
	malformedURL := "http://example.com/webhooks/" + secretToken + "\x7f"

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: DiscordWebhookSecretKey}, malformedURL); err != nil {
		t.Fatal(err)
	}
	channel := NewDiscordWebhookChannel(secrets, "", WithDiscordSleep(noSleep))
	d := NewAlertDispatcher(AlertDispatcherConfig{Channel: channel})

	ev := protocol.Event{Code: protocol.EventBackupFailed, InstanceUUID: strPtr("x")}
	err := d.Alert(context.Background(), ev)
	if err == nil {
		t.Fatal("畸形 URL 應回錯誤")
	}
	// 這個 error 字串正是 RestartPolicy/ThresholdMonitor 的 recordAlertResult 會寫入
	// ALERT_FAILED 事件明細("error" 欄位)的內容,故此處直接斷言即等同斷言事件不外洩。
	if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), malformedURL) {
		t.Errorf("錯誤訊息(將寫入 ALERT_FAILED 明細)不應含原始 URL: %v", err)
	}
}

// ---- 雙審 #11:dedup map 惰性清理,避免無界成長 ----

func TestAlertDispatcher_EvictsExpiredDedupEntries(t *testing.T) {
	stub := &discordStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	secrets := NewSecretStoreWithKeyring("test", newMemoryKeyring())
	if err := secrets.Set(protocol.SecretRef{Key: DiscordWebhookSecretKey}, srv.URL); err != nil {
		t.Fatal(err)
	}
	channel := NewDiscordWebhookChannel(secrets, "", WithDiscordHTTPClient(srv.Client()))
	clk := newFakeClock(time.Unix(1700000000, 0).UTC())
	d := NewAlertDispatcher(AlertDispatcherConfig{Channel: channel, Now: clk.Now, Cooldown: 1 * time.Minute})

	// 寫入多個「早已過期(> cooldown*2)」的 dedup 項。
	for i := 0; i < 5; i++ {
		key := dedupKey{uuid: fmt.Sprintf("stale-%d", i), code: protocol.EventBackupFailed}
		d.lastSent[key] = clk.Now()
	}
	clk.Advance(3 * time.Minute) // > cooldown(1m) * 2

	ev := protocol.Event{Code: protocol.EventBackupFailed, InstanceUUID: strPtr("fresh"), TsUTC: clk.Now()}
	if err := d.Alert(context.Background(), ev); err != nil {
		t.Fatalf("Alert: %v", err)
	}

	d.mu.Lock()
	n := len(d.lastSent)
	d.mu.Unlock()
	if n != 1 { // 過期項應已被清除,只剩剛成功送出的 "fresh" 這一筆
		t.Errorf("過期 dedup 項應被清除, 剩餘 lastSent 大小 = %d, 期望 1", n)
	}
}
