package core

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"servermonitor/internal/protocol"
)

// TestStore_MigrateAndReopen 驗證遷移後寫入的資料於重開後完整重現(R12)。
func TestStore_MigrateAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gsm.db")

	st, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	inst := InstanceRecord{
		UUID:          "uuid-1",
		TemplateID:    "minecraft",
		Variant:       "paper",
		ParamsJSON:    json.RawMessage(`{"MEMORY":"2G"}`),
		Node:          "local",
		RuntimeID:     protocol.RuntimeID("ctr-abc"),
		DesiredState:  protocol.InstanceStateRunning,
		ObservedState: protocol.InstanceStateCreated,
		OpGeneration:  3,
	}
	if err := st.UpsertInstance(inst); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}
	if err := st.SetSetting("theme", "dark"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := st.UpsertSchedule(ScheduleRecord{ID: "sch-1", InstanceUUID: "uuid-1", Kind: "backup", Enabled: true}); err != nil {
		t.Fatalf("UpsertSchedule: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重開同一路徑,資料應完整重現。
	st2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen Open: %v", err)
	}
	defer st2.Close()

	if q, _ := st2.Quarantined(); q {
		t.Errorf("健康 DB 重開不應 quarantine")
	}

	got, err := st2.GetInstance("uuid-1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.TemplateID != inst.TemplateID || got.Variant != inst.Variant ||
		got.RuntimeID != inst.RuntimeID || got.DesiredState != inst.DesiredState ||
		got.ObservedState != inst.ObservedState || got.OpGeneration != inst.OpGeneration {
		t.Errorf("實例重現不符: got=%+v want=%+v", got, inst)
	}
	if string(got.ParamsJSON) != `{"MEMORY":"2G"}` {
		t.Errorf("params_json 不符: %s", got.ParamsJSON)
	}

	if v, ok, _ := st2.GetSetting("theme"); !ok || v != "dark" {
		t.Errorf("setting 不符: got=%q ok=%v", v, ok)
	}

	schedules, err := st2.ListSchedules()
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(schedules) != 1 || schedules[0].ID != "sch-1" || !schedules[0].Enabled {
		t.Errorf("schedule 不符: %+v", schedules)
	}
}

// TestStore_MigrationV2AddsLastFiredUTCOnOldDB 驗證 T11 雙審 #6 新增的 v2 遷移
// (schedules.last_fired_utc)套用在「只跑過 v1」的舊庫上不會出錯,既有排程資料完整保留,
// 且新欄位對舊資料為 NULL(ListSchedules 讀回 LastFiredUTC == nil)。
func TestStore_MigrationV2AddsLastFiredUTCOnOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 手刻只套用 v1 的舊庫(不經 Store.Open/applyMigrations,模擬升級前建立的資料庫)。
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatalf("建版本表: %v", err)
	}
	for _, m := range migrations {
		if m.version != 1 {
			continue // 只套用 v1,刻意不跑後續遷移
		}
		for _, stmt := range m.stmts {
			if _, err := raw.Exec(stmt); err != nil {
				t.Fatalf("套用 v1 遷移失敗: %v", err)
			}
		}
		if _, err := raw.Exec(`INSERT INTO schema_version (version) VALUES (?)`, m.version); err != nil {
			t.Fatalf("寫入 schema_version: %v", err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO schedules (id, instance_uuid, kind, spec_json, enabled) VALUES (?, ?, ?, ?, ?)`,
		"legacy-sch", "uuid-legacy", "restart", `{"at":"04:30"}`, 1); err != nil {
		t.Fatalf("插入舊排程資料: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("關閉手刻連線: %v", err)
	}

	// 以現行版本開庫:應自動套用 v2(ALTER TABLE ADD COLUMN),舊庫可正常開啟。
	st, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open 升級後的舊庫失敗: %v", err)
	}
	defer st.Close()

	if q, _ := st.Quarantined(); q {
		t.Errorf("正常升級不應觸發 quarantine")
	}

	recs, err := st.ListSchedules()
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "legacy-sch" {
		t.Fatalf("舊排程資料應完整保留, 得 %+v", recs)
	}
	if recs[0].LastFiredUTC != nil {
		t.Errorf("舊資料的 last_fired_utc 應為 NULL, 得 %v", recs[0].LastFiredUTC)
	}

	current, err := currentSchemaVersion(st.db)
	if err != nil {
		t.Fatalf("currentSchemaVersion: %v", err)
	}
	if current != schemaVersion() {
		t.Errorf("schema 版本應已升至最新 %d, 得 %d", schemaVersion(), current)
	}
}

// TestStore_GetInstanceNotFound 驗證查無回 ErrNotFound。
func TestStore_GetInstanceNotFound(t *testing.T) {
	st, _ := newTempStore(t)
	if _, err := st.GetInstance("nope"); err != ErrNotFound {
		t.Errorf("期望 ErrNotFound, got %v", err)
	}
}

// TestStore_QuarantineOnCorruptDB 驗證毀損 DB 進 quarantine:改名保留原檔、
// 不以空庫覆寫、DB_QUARANTINE 事件落 fallback,並以新庫降級啟動(R12)。
func TestStore_QuarantineOnCorruptDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gsm.db")
	fallbackPath := filepath.Join(dir, "fallback.ndjson")

	// 注入壞檔:寫入非 SQLite 位元組。
	garbage := []byte("這不是資料庫,而是毀損的位元組 garbage-DB-content-0xDEADBEEF")
	if err := os.WriteFile(path, garbage, 0o644); err != nil {
		t.Fatalf("寫入壞檔: %v", err)
	}

	fb := NewFallbackRecorder(fallbackPath)
	fixedTime := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	st, err := Open(path, Options{Fallback: fb, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open(毀損) 應降級啟動而非報錯: %v", err)
	}
	defer st.Close()

	// 應標記 quarantined。
	q, qp := st.Quarantined()
	if !q {
		t.Fatalf("期望 quarantined=true")
	}

	// 隔離檔應保留「原始壞位元組」——不得截斷或覆寫。
	saved, err := os.ReadFile(qp)
	if err != nil {
		t.Fatalf("讀隔離檔: %v", err)
	}
	if !bytes.Equal(saved, garbage) {
		t.Errorf("隔離檔內容非原始壞檔: got %q", saved)
	}

	// 原路徑現為全新可用 DB(不含原壞位元組)。
	nowBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("讀新 DB: %v", err)
	}
	if bytes.Contains(nowBytes, garbage) {
		t.Errorf("新 DB 竟含原壞位元組(疑似覆寫原檔)")
	}
	// 新庫可正常寫讀。
	if err := st.UpsertInstance(InstanceRecord{UUID: "u1", TemplateID: "t"}); err != nil {
		t.Fatalf("新庫寫入: %v", err)
	}
	if _, err := st.GetInstance("u1"); err != nil {
		t.Fatalf("新庫讀取: %v", err)
	}

	// DB_QUARANTINE 事件落 fallback NDJSON。
	fbData, err := os.ReadFile(fallbackPath)
	if err != nil {
		t.Fatalf("讀 fallback: %v", err)
	}
	var ev protocol.Event
	if err := json.Unmarshal(bytes.TrimSpace(fbData), &ev); err != nil {
		t.Fatalf("解析 fallback 事件: %v (raw=%s)", err, fbData)
	}
	if ev.Code != protocol.EventDBQuarantine {
		t.Errorf("fallback 事件 code=%q 期望 %q", ev.Code, protocol.EventDBQuarantine)
	}
	if ev.Severity != protocol.SeverityError {
		t.Errorf("fallback 事件 severity=%q 期望 error", ev.Severity)
	}
}

// TestStore_FreshOpenNoQuarantine 驗證全新路徑開庫不觸發 quarantine。
func TestStore_FreshOpenNoQuarantine(t *testing.T) {
	st, _ := newTempStore(t)
	if q, _ := st.Quarantined(); q {
		t.Errorf("全新 DB 不應 quarantine")
	}
}
