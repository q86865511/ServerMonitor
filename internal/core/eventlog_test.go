package core

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

func strptr(s string) *string { return &s }

// TestEventLog_AppendAndFilter 驗證依實例/類型/時間過濾(R14)。
func TestEventLog_AppendAndFilter(t *testing.T) {
	st, _ := newTempStore(t)
	log := NewEventLog(st, nil, EventLogOptions{})

	t0 := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	mk := func(code protocol.EventCode, inst string, at time.Time) protocol.Event {
		return protocol.Event{Code: code, InstanceUUID: strptr(inst), TsUTC: at, Severity: protocol.SeverityInfo}
	}
	events := []protocol.Event{
		mk(protocol.EventInstanceCreated, "A", t0),
		mk(protocol.EventInstanceStarted, "A", t0.Add(1*time.Hour)),
		mk(protocol.EventInstanceCrashed, "A", t0.Add(2*time.Hour)),
		mk(protocol.EventInstanceStarted, "B", t0.Add(3*time.Hour)),
	}
	for _, ev := range events {
		if err := log.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	// 依實例過濾。
	gotA, err := log.Query(EventFilter{InstanceUUID: strptr("A")})
	if err != nil {
		t.Fatalf("Query A: %v", err)
	}
	if len(gotA) != 3 {
		t.Errorf("實例 A 事件=%d 期望 3", len(gotA))
	}
	// 應依時間遞增。
	if len(gotA) == 3 && !(gotA[0].TsUTC.Before(gotA[1].TsUTC) && gotA[1].TsUTC.Before(gotA[2].TsUTC)) {
		t.Errorf("結果未依時間遞增: %+v", gotA)
	}

	// 依類型過濾。
	started := protocol.EventInstanceStarted
	gotStarted, err := log.Query(EventFilter{Code: &started})
	if err != nil {
		t.Fatalf("Query code: %v", err)
	}
	if len(gotStarted) != 2 {
		t.Errorf("STARTED 事件=%d 期望 2", len(gotStarted))
	}

	// 依時間範圍過濾(含界):[t0+1h, t0+2h] → 2 筆。
	since := t0.Add(1 * time.Hour)
	until := t0.Add(2 * time.Hour)
	gotRange, err := log.Query(EventFilter{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Query range: %v", err)
	}
	if len(gotRange) != 2 {
		t.Errorf("時間範圍事件=%d 期望 2", len(gotRange))
	}

	// 組合過濾 + Limit。
	gotLimited, err := log.Query(EventFilter{InstanceUUID: strptr("A"), Limit: 1})
	if err != nil {
		t.Fatalf("Query limit: %v", err)
	}
	if len(gotLimited) != 1 {
		t.Errorf("Limit=1 應回 1 筆, got %d", len(gotLimited))
	}
}

// TestEventLog_Retention 驗證保留上限與輪替(只留最新 N 筆)(R14)。
func TestEventLog_Retention(t *testing.T) {
	st, _ := newTempStore(t)
	fixed := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	// 固定時鐘使所有事件同秒,證明以插入序(id)為輪替依據。
	log := NewEventLog(st, nil, EventLogOptions{RetentionMax: 5, Now: func() time.Time { return fixed }})

	const total = 12
	for i := 0; i < total; i++ {
		ev := protocol.Event{
			Code:         protocol.EventInstanceStarted,
			Severity:     protocol.SeverityInfo,
			InstanceUUID: strptr(strconv.Itoa(i)),
		}
		if err := log.Append(ev); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	got, err := log.Query(EventFilter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("retention 後事件=%d 期望 5", len(got))
	}
	// 應為最新 5 筆:index 7..11。
	for i, ev := range got {
		want := strconv.Itoa(total - 5 + i)
		if ev.InstanceUUID == nil || *ev.InstanceUUID != want {
			t.Errorf("第 %d 筆 instance=%v 期望 %q", i, ev.InstanceUUID, want)
		}
	}
}

// TestEventLog_FallbackWhenDBUnavailable 驗證 DB 不可用時事件落 fallback NDJSON(R14)。
func TestEventLog_FallbackWhenDBUnavailable(t *testing.T) {
	dir := t.TempDir()
	fbPath := filepath.Join(dir, "events-fallback.ndjson")
	fb := NewFallbackRecorder(fbPath)

	// store 為 nil → 視為 DB 不可用。
	log := NewEventLog(nil, fb, EventLogOptions{})

	ev := protocol.Event{
		Code:         protocol.EventNodeOffline,
		Severity:     protocol.SeverityWarning,
		InstanceUUID: strptr("u1"),
		DetailsJSON:  json.RawMessage(`{"reason":"daemon down"}`),
	}
	if err := log.Append(ev); err != nil {
		t.Fatalf("Append(fallback): %v", err)
	}

	// 讀回 NDJSON 首行並解析。
	f, err := os.Open(fbPath)
	if err != nil {
		t.Fatalf("開 fallback: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatalf("fallback 檔無內容")
	}
	var back protocol.Event
	if err := json.Unmarshal(sc.Bytes(), &back); err != nil {
		t.Fatalf("解析 NDJSON: %v", err)
	}
	if back.Code != protocol.EventNodeOffline {
		t.Errorf("code=%q 期望 %q", back.Code, protocol.EventNodeOffline)
	}
	if back.TsUTC.IsZero() {
		t.Errorf("fallback 事件應自動補時間")
	}
	if back.InstanceUUID == nil || *back.InstanceUUID != "u1" {
		t.Errorf("instance_uuid 遺失: %v", back.InstanceUUID)
	}
}

// TestEventLog_FallbackWhenStoreClosed 驗證 DB 寫入失敗(store 已關)時退回 fallback。
func TestEventLog_FallbackWhenStoreClosed(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "gsm.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fbPath := filepath.Join(dir, "fb.ndjson")
	log := NewEventLog(st, NewFallbackRecorder(fbPath), EventLogOptions{})

	// 關閉 store 使後續 DB 寫入失敗。
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := log.Append(protocol.Event{Code: protocol.EventInstanceStopped, Severity: protocol.SeverityInfo}); err != nil {
		t.Fatalf("Append 應退回 fallback 而非報錯: %v", err)
	}
	data, err := os.ReadFile(fbPath)
	if err != nil || len(data) == 0 {
		t.Fatalf("關庫後事件應落 fallback: err=%v len=%d", err, len(data))
	}
}

// TestEventLog_RequiredCodesRoundTrip 驗證 design 明訂的必備 codes 皆可寫入並查回(R14)。
func TestEventLog_RequiredCodesRoundTrip(t *testing.T) {
	st, _ := newTempStore(t)
	log := NewEventLog(st, nil, EventLogOptions{})

	for _, code := range protocol.RequiredEventCodes {
		if err := log.Append(protocol.Event{Code: code, Severity: protocol.SeverityInfo}); err != nil {
			t.Fatalf("Append %s: %v", code, err)
		}
	}
	got, err := log.Query(EventFilter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	seen := make(map[protocol.EventCode]bool, len(got))
	for _, ev := range got {
		seen[ev.Code] = true
	}
	for _, code := range protocol.RequiredEventCodes {
		if !seen[code] {
			t.Errorf("必備 code %q 未查回", code)
		}
	}
}

// TestEventLog_NullableFieldsRoundTrip 驗證可空欄位(instance/node/template)正確以 NULL 存取。
func TestEventLog_NullableFieldsRoundTrip(t *testing.T) {
	st, _ := newTempStore(t)
	log := NewEventLog(st, nil, EventLogOptions{})

	// 全空可空欄位。
	if err := log.Append(protocol.Event{Code: protocol.EventTemplateLoadFailed, Severity: protocol.SeverityError}); err != nil {
		t.Fatalf("Append 空欄位: %v", err)
	}
	// 有值。
	if err := log.Append(protocol.Event{
		Code:         protocol.EventInstanceCreated,
		Severity:     protocol.SeverityInfo,
		InstanceUUID: strptr("u9"),
		Node:         strptr("local"),
		TemplateID:   strptr("minecraft"),
		DetailsJSON:  json.RawMessage(`{"k":1}`),
	}); err != nil {
		t.Fatalf("Append 有值: %v", err)
	}

	tmplFailed := protocol.EventTemplateLoadFailed
	empties, _ := log.Query(EventFilter{Code: &tmplFailed})
	if len(empties) != 1 {
		t.Fatalf("空欄位事件=%d 期望 1", len(empties))
	}
	if empties[0].InstanceUUID != nil || empties[0].Node != nil || empties[0].TemplateID != nil {
		t.Errorf("可空欄位應為 nil: %+v", empties[0])
	}
	if empties[0].DetailsJSON != nil {
		t.Errorf("空 details 應為 nil, got %s", empties[0].DetailsJSON)
	}

	created := protocol.EventInstanceCreated
	vals, _ := log.Query(EventFilter{Code: &created})
	if len(vals) != 1 {
		t.Fatalf("有值事件=%d 期望 1", len(vals))
	}
	v := vals[0]
	if v.InstanceUUID == nil || *v.InstanceUUID != "u9" || v.Node == nil || *v.Node != "local" ||
		v.TemplateID == nil || *v.TemplateID != "minecraft" {
		t.Errorf("有值欄位未正確重現: %+v", v)
	}
	if string(v.DetailsJSON) != `{"k":1}` {
		t.Errorf("details_json 未重現: %s", v.DetailsJSON)
	}
}
