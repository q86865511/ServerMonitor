package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// 告警分派的關鍵常數預設(R10「關鍵常數預設」)。
const (
	defaultAlertCooldown    = 5 * time.Minute  // dedup key 去重窗口
	defaultAlertRetryMax    = 3                // 送出重試次數上限
	defaultAlertRetryBase   = 2 * time.Second  // 重試退避起始
	defaultAlertRetryCap    = 60 * time.Second // 重試退避上限
	defaultAlertHTTPTimeout = 10 * time.Second // 單次 HTTP 請求逾時(未由呼叫端 ctx 限制時)

	// alertDispatchTimeout 是呼叫端(RestartPolicy.dispatchAlert / ThresholdMonitor.fire)
	// 包住 AlertSink.Alert() 的 ctx 逾時。獨立於 defaultProbeTimeout(探針逾時,5s)——告警
	// 送出需容納 DiscordWebhookChannel 最多 3 次嘗試 + 退避(2s+4s)+ 429 Retry-After 餘裕,
	// 5s 必然截斷重試(雙審 #7):90s 足夠覆蓋 3 次請求 + 兩次退避上限(60s cap)的極端情況。
	alertDispatchTimeout = 90 * time.Second

	// EventResourceThreshold 標記資源使用率(CPU/記憶體)持續超過使用者設定門檻(R10);
	// 由 threshold.go 的持續窗口/遲滯狀態機評估產生,經 AlertDispatcher 發送告警。仿
	// commandservice.go 的 EventHookFailed 慣例,於 core 層補碼(非 protocol.RequiredEventCodes
	// 必備目錄,因其為 R10 告警特有的衍生事件,不影響 R14 必備碼表驗收)。
	EventResourceThreshold protocol.EventCode = "RESOURCE_THRESHOLD"

	// DiscordWebhookSecretKey 是 Discord webhook URL 於 SecretStore 中的固定鍵名。
	DiscordWebhookSecretKey = "alert.discord.webhook_url"
)

// ErrChannelNotConfigured 表示告警管道尚未設定(如 Discord webhook URL 未存入金鑰庫)。
// AlertDispatcher 對此視為「僅記錄不報錯」:不重試、不計入 ALERT_FAILED。
var ErrChannelNotConfigured = errors.New("core: 告警管道未設定")

// AlertPayload 是四類事件(INSTANCE_CRASHED / RESTART_GIVEUP / BACKUP_FAILED /
// RESOURCE_THRESHOLD)共用的固定告警內容(R10「payload schema 明確」),供各 AlertChannel
// 實作組出各自的線上格式。
type AlertPayload struct {
	Code         protocol.EventCode
	Severity     protocol.Severity
	InstanceUUID string
	Node         string
	TemplateID   string
	TsUTC        time.Time
	Details      map[string]any
	Content      string // 人類可讀摘要(依事件類別組成的固定文案)
}

// AlertChannel 是實際送出一則告警 payload 的通道(Discord webhook 等)。實作若需自行判斷
// 「尚未設定」(如密碼/URL 未存入金鑰庫),回傳 ErrChannelNotConfigured 而非一般錯誤,
// AlertDispatcher 會將其視為 no-op、不重試、不報錯。
type AlertChannel interface {
	Send(ctx context.Context, payload AlertPayload) error
}

// configChecker 是 AlertChannel 的可選介面:實作者可藉此讓 AlertDispatcher 在送出前就
// 判斷「未設定」,避免白白佔用 dedup cooldown 名額(見 Alert 內的探測)。
type configChecker interface {
	Configured() bool
}

// dedupKey 是「短時間內去重」的鍵:(instance_uuid, code[, metric])(R10)。
type dedupKey struct {
	uuid   string
	code   protocol.EventCode
	metric string
}

// AlertDispatcherConfig 是 AlertDispatcher 的建構參數。
type AlertDispatcherConfig struct {
	Channel  AlertChannel // 實際送出通道;nil 視為「無管道」(僅記錄不報錯)
	Now      func() time.Time
	Cooldown time.Duration // dedup key 去重窗口;<=0 用 5 分鐘
}

// AlertDispatcher 是 AlertSink 的生產實作(R10):依 (instance_uuid, code[, metric]) 於
// cooldown 內去重,無管道設定時僅 no-op(呼叫端已先寫入來源事件,故「僅記錄」的語意已滿足),
// 有管道則組固定 payload 交給 AlertChannel 送出。
//
// 本身不記 ALERT_SENT/ALERT_FAILED——沿用既有慣例(RestartPolicy.dispatchAlert、
// Scheduler.fireBackup 皆依 Alert() 的回傳值自行記錄),使「呼叫 AlertSink」與「記錄結果」
// 的責任留在呼叫端,不重複記錄。
type AlertDispatcher struct {
	channel  AlertChannel
	now      func() time.Time
	cooldown time.Duration

	mu       sync.Mutex
	lastSent map[dedupKey]time.Time
}

// NewAlertDispatcher 建立 AlertDispatcher。
func NewAlertDispatcher(cfg AlertDispatcherConfig) *AlertDispatcher {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = defaultAlertCooldown
	}
	return &AlertDispatcher{
		channel:  cfg.Channel,
		now:      now,
		cooldown: cooldown,
		lastSent: make(map[dedupKey]time.Time),
	}
}

// Alert 實作 AlertSink:無管道 → no-op;dedup key 於 cooldown 內已送過 → no-op(去重);
// 否則組 payload 送出。
func (d *AlertDispatcher) Alert(ctx context.Context, ev protocol.Event) error {
	if d.channel == nil {
		return nil // 未設定任何管道:僅記錄(來源事件已由呼叫端寫入),不報錯
	}
	if cc, ok := d.channel.(configChecker); ok && !cc.Configured() {
		return nil // 通道存在但尚未設定實值(如 webhook URL 未存):同樣視為「無管道」
	}

	key := dedupKeyFor(ev)
	now := d.now()
	d.mu.Lock()
	d.evictExpiredLocked(now) // dedup map 無界成長防護(R10 衍生,雙審 #11):惰性清理過期項
	if last, ok := d.lastSent[key]; ok && now.Sub(last) < d.cooldown {
		d.mu.Unlock()
		return nil // cooldown 內已送過同一 dedup key:去重,不轟炸
	}
	d.mu.Unlock()

	payload := buildPayload(ev, now)
	if err := d.channel.Send(ctx, payload); err != nil {
		if errors.Is(err, ErrChannelNotConfigured) {
			return nil
		}
		return err // 送出失敗:不佔用 cooldown 名額(雙審 #8),同 key 下次觸發可立即再送
	}

	d.mu.Lock()
	d.lastSent[key] = now // 僅成功送出才寫入 dedup 記錄
	d.mu.Unlock()
	return nil
}

// evictExpiredLocked 清除 lastSent 中已超過 cooldown×2 的過期項(雙審 #11)。以 2 倍 cooldown
// 為門檻:確保清理時機晚於「該項已不可能再影響去重判斷」的時間點,同時不必為每個 key 另外
// 維護計時器。呼叫端須持有 d.mu;map 規模與受監控實例數同量級,全掃可接受。
func (d *AlertDispatcher) evictExpiredLocked(now time.Time) {
	expiry := d.cooldown * 2
	for k, ts := range d.lastSent {
		if now.Sub(ts) >= expiry {
			delete(d.lastSent, k)
		}
	}
}

// dedupKeyFor 由事件取出 (instance_uuid, code[, metric]);metric 取自 DetailsJSON 的
// "metric" 欄位(僅 RESOURCE_THRESHOLD 類事件會帶此欄位,其餘事件 metric 為空字串)。
func dedupKeyFor(ev protocol.Event) dedupKey {
	k := dedupKey{code: ev.Code}
	if ev.InstanceUUID != nil {
		k.uuid = *ev.InstanceUUID
	}
	if len(ev.DetailsJSON) > 0 {
		var details map[string]any
		if json.Unmarshal(ev.DetailsJSON, &details) == nil {
			if m, ok := details["metric"].(string); ok {
				k.metric = m
			}
		}
	}
	return k
}

// buildPayload 把事件封套轉為四類事件共用的固定 AlertPayload。
func buildPayload(ev protocol.Event, now time.Time) AlertPayload {
	ts := ev.TsUTC
	if ts.IsZero() {
		ts = now
	}
	p := AlertPayload{
		Code:     ev.Code,
		Severity: ev.Severity,
		TsUTC:    ts.UTC(),
	}
	if ev.InstanceUUID != nil {
		p.InstanceUUID = *ev.InstanceUUID
	}
	if ev.Node != nil {
		p.Node = *ev.Node
	}
	if ev.TemplateID != nil {
		p.TemplateID = *ev.TemplateID
	}
	if len(ev.DetailsJSON) > 0 {
		_ = json.Unmarshal(ev.DetailsJSON, &p.Details)
	}
	p.Content = formatAlertContent(p)
	return p
}

// formatAlertContent 依事件類別組出固定的人類可讀摘要。四類必備事件各有專屬文案;
// 未知碼(理論上不會發生,防禦性)落到通用格式。
func formatAlertContent(p AlertPayload) string {
	name := p.InstanceUUID
	if name == "" {
		name = "(未知實例)"
	}
	ts := p.TsUTC.Format(time.RFC3339)
	switch p.Code {
	case protocol.EventInstanceCrashed:
		return fmt.Sprintf("[ServerMonitor] 實例 %s 已崩潰(%s)", name, ts)
	case protocol.EventRestartGiveup:
		return fmt.Sprintf("[ServerMonitor] 實例 %s 自動重啟已放棄(%s)", name, ts)
	case protocol.EventBackupFailed:
		return fmt.Sprintf("[ServerMonitor] 實例 %s 備份失敗(%s)", name, ts)
	case EventResourceThreshold:
		return fmt.Sprintf("[ServerMonitor] 實例 %s 資源使用率持續超過門檻(%s)", name, ts)
	default:
		return fmt.Sprintf("[ServerMonitor] 實例 %s 事件 %s(%s)", name, p.Code, ts)
	}
}

// ---- DiscordWebhookChannel ----

// discordWebhookBody 是送往 Discord webhook 的請求主體,固定 schema 供斷言(R10「payload
// schema 明確」):content 為摘要文字,embeds[0] 帶結構化欄位(instance/time_utc/severity/
// node/template_id/details)。
type discordWebhookBody struct {
	Content string         `json:"content"`
	Embeds  []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title  string         `json:"title"` // event code
	Fields []discordField `json:"fields"`
}

type discordField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// DiscordWebhookChannel 是 AlertChannel 的 Discord webhook 實作(R10 首版唯一管道)。
// webhook URL 一律從 SecretStore 讀取(不明文落 DB/log),失敗依常數表重試。
type DiscordWebhookChannel struct {
	secrets    *SecretStore
	secretKey  string
	httpClient *http.Client
	sleep      func(ctx context.Context, d time.Duration) error
	maxRetries int
}

// DiscordWebhookChannelOption 供測試注入 httpClient/sleep/重試上限。
type DiscordWebhookChannelOption func(*DiscordWebhookChannel)

// WithDiscordHTTPClient 覆寫 HTTP client(測試指向 httptest server 時免另行修改)。
func WithDiscordHTTPClient(c *http.Client) DiscordWebhookChannelOption {
	return func(d *DiscordWebhookChannel) { d.httpClient = c }
}

// WithDiscordSleep 覆寫退避睡眠函式(測試以立即返回或記錄呼叫來取代真實等待)。
func WithDiscordSleep(fn func(ctx context.Context, d time.Duration) error) DiscordWebhookChannelOption {
	return func(d *DiscordWebhookChannel) { d.sleep = fn }
}

// NewDiscordWebhookChannel 建立以 secretKey(預設 DiscordWebhookSecretKey)讀取 URL 的通道。
func NewDiscordWebhookChannel(secrets *SecretStore, secretKey string, opts ...DiscordWebhookChannelOption) *DiscordWebhookChannel {
	if secretKey == "" {
		secretKey = DiscordWebhookSecretKey
	}
	c := &DiscordWebhookChannel{
		secrets:    secrets,
		secretKey:  secretKey,
		httpClient: &http.Client{Timeout: defaultAlertHTTPTimeout},
		sleep:      ctxSleep,
		maxRetries: defaultAlertRetryMax,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Configured 回報 webhook URL 是否已存入金鑰庫(供 AlertDispatcher 判斷「無管道」)。
func (c *DiscordWebhookChannel) Configured() bool {
	_, err := c.webhookURL()
	return err == nil
}

func (c *DiscordWebhookChannel) webhookURL() (string, error) {
	if c.secrets == nil {
		return "", ErrChannelNotConfigured
	}
	url, err := c.secrets.Get(protocol.SecretRef{Key: c.secretKey})
	if err != nil {
		if errors.Is(err, ErrSecretNotFound) {
			return "", ErrChannelNotConfigured
		}
		return "", fmt.Errorf("讀取 discord webhook 設定失敗: %w", err)
	}
	if url == "" {
		return "", ErrChannelNotConfigured
	}
	return url, nil
}

// Send 送出一則告警到 Discord webhook。重試策略(R10):最多 3 次,退避 base 2s cap 60s,
// 429 依 Retry-After 標頭等待、5xx 重試、4xx(非 429)不重試。所有錯誤訊息一律不含 webhook
// URL(避免 secret 外洩於事件/log)。
func (c *DiscordWebhookChannel) Send(ctx context.Context, payload AlertPayload) error {
	url, err := c.webhookURL()
	if err != nil {
		return err
	}

	body, err := json.Marshal(discordWebhookBody{
		Content: payload.Content,
		Embeds: []discordEmbed{{
			Title:  string(payload.Code),
			Fields: discordFieldsFor(payload),
		}},
	})
	if err != nil {
		return fmt.Errorf("序列化 discord payload 失敗: %w", err)
	}

	maxRetries := c.maxRetries
	if maxRetries <= 0 {
		maxRetries = defaultAlertRetryMax
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			// 不 %w 原始 error:net/http 的請求建立錯誤內含畸形 URL 本身(雙審 #10),
			// 外洩會讓 webhook URL 流入 ALERT_FAILED 事件明細。
			return errors.New("discord webhook URL 無效")
		}
		req.Header.Set("Content-Type", "application/json")

		resp, doErr := c.httpClient.Do(req)
		if doErr != nil {
			// 網路層錯誤(逾時、連線失敗等):視為可重試,錯誤訊息不夾帶原始 err(可能含 URL)。
			lastErr = fmt.Errorf("discord webhook 連線失敗(第 %d/%d 次嘗試)", attempt, maxRetries)
			if attempt == maxRetries {
				return lastErr
			}
			if serr := c.sleep(ctx, discordBackoff(attempt)); serr != nil {
				return serr
			}
			continue
		}

		status := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		switch {
		case status >= 200 && status < 300:
			return nil
		case status == http.StatusTooManyRequests:
			lastErr = fmt.Errorf("discord webhook 回應 429(第 %d/%d 次嘗試)", attempt, maxRetries)
			if attempt == maxRetries {
				return lastErr
			}
			wait := retryAfterOrBackoff(resp.Header.Get("Retry-After"), attempt)
			if serr := c.sleep(ctx, wait); serr != nil {
				return serr
			}
		case status >= 500:
			lastErr = fmt.Errorf("discord webhook 回應 %d(第 %d/%d 次嘗試)", status, attempt, maxRetries)
			if attempt == maxRetries {
				return lastErr
			}
			if serr := c.sleep(ctx, discordBackoff(attempt)); serr != nil {
				return serr
			}
		default:
			// 4xx(非 429):不重試,視為永久性失敗(如 webhook 已刪除/格式錯誤)。
			return fmt.Errorf("discord webhook 回應 %d,不重試", status)
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("discord webhook 重試已耗盡")
}

// discordFieldsFor 組出結構化欄位(instance/time_utc/severity/node/template_id/details)。
func discordFieldsFor(p AlertPayload) []discordField {
	fields := []discordField{
		{Name: "instance", Value: p.InstanceUUID},
		{Name: "time_utc", Value: p.TsUTC.Format(time.RFC3339)},
		{Name: "severity", Value: string(p.Severity)},
	}
	if p.Node != "" {
		fields = append(fields, discordField{Name: "node", Value: p.Node})
	}
	if p.TemplateID != "" {
		fields = append(fields, discordField{Name: "template_id", Value: p.TemplateID})
	}
	if len(p.Details) > 0 {
		if raw, err := json.Marshal(p.Details); err == nil {
			fields = append(fields, discordField{Name: "details", Value: string(raw)})
		}
	}
	return fields
}

// discordBackoff 是 5xx/網路錯誤的重試退避:2s、4s、...,上限 60s。attempt 為第幾次嘗試(≥1)。
func discordBackoff(attempt int) time.Duration {
	d := defaultAlertRetryBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= defaultAlertRetryCap {
			return defaultAlertRetryCap
		}
	}
	return d
}

// retryAfterOrBackoff 解析 Retry-After 標頭(秒數,含小數,或 HTTP-date);解析失敗則退回
// 一般退避(雙審 #9)。
func retryAfterOrBackoff(retryAfter string, attempt int) time.Duration {
	if retryAfter != "" {
		if d, ok := parseRetryAfter(retryAfter); ok {
			return d
		}
	}
	return discordBackoff(attempt)
}

// parseRetryAfter 解析 Retry-After 標頭值:優先當作秒數(含小數,如 "0.5")解析;失敗則
// 嘗試 HTTP-date 格式(如 "Wed, 21 Oct 2015 07:28:00 GMT"),換算為距今的等待時長。
// 已過期(負值)或超過退避上限一律夾在 [0, defaultAlertRetryCap] 內,避免異常標頭撐爆等待。
func parseRetryAfter(retryAfter string) (time.Duration, bool) {
	if secs, err := strconv.ParseFloat(retryAfter, 64); err == nil && secs >= 0 {
		return clampRetryWait(time.Duration(secs * float64(time.Second))), true
	}
	if t, err := http.ParseTime(retryAfter); err == nil {
		return clampRetryWait(time.Until(t)), true
	}
	return 0, false
}

// clampRetryWait 把等待時長夾在 [0, defaultAlertRetryCap] 內。
func clampRetryWait(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > defaultAlertRetryCap {
		return defaultAlertRetryCap
	}
	return d
}

// ctxSleep 是預設睡眠實作:依 ctx 睡眠 d,ctx 取消時提早回傳其錯誤。
func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var _ AlertSink = (*AlertDispatcher)(nil)
var _ AlertChannel = (*DiscordWebhookChannel)(nil)
