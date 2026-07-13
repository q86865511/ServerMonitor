package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// FallbackRecorder 於 SQLite 不可用時,把事件封套以 NDJSON(每行一則)追加寫入診斷檔(R14)。
// 也供 Store.Open 在偵測 DB 毀損時寫入 DB_QUARANTINE(此時 DB 本身不可信)。
type FallbackRecorder struct {
	mu   sync.Mutex
	path string
}

// NewFallbackRecorder 建立寫入指定路徑的 fallback 診斷記錄器。
func NewFallbackRecorder(path string) *FallbackRecorder {
	return &FallbackRecorder{path: path}
}

// Record 以一行 NDJSON 追加寫入一則事件封套。
func (f *FallbackRecorder) Record(ev protocol.Event) error {
	if ev.TsUTC.IsZero() {
		ev.TsUTC = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("序列化 fallback 事件失敗: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("開啟 fallback 診斷檔失敗: %w", err)
	}
	defer file.Close()

	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("寫入 fallback 診斷檔失敗: %w", err)
	}
	return nil
}

// EventFilter 是事件查詢條件(R14:依實例/類型/時間)。零值欄位不參與過濾。
type EventFilter struct {
	InstanceUUID *string
	Code         *protocol.EventCode
	Since        *time.Time // 含界(ts_utc >= Since)
	Until        *time.Time // 含界(ts_utc <= Until)
	Limit        int        // >0 才限制筆數
}

// EventLogOptions 是 EventLog 選項。
type EventLogOptions struct {
	// RetentionMax 是保留的最新事件筆數上限(輪替避免無限增長);0 = 不限。
	RetentionMax int
	// Now 回傳當前時間,預設 time.Now;供測試注入。
	Now func() time.Time
}

// EventLog 將 protocol.Event 寫入 events 表並支援過濾查詢與 retention 輪替(R14)。
// 當 SQLite 不可用(store 為 nil 或寫入失敗)時,事件改落 fallback NDJSON 診斷檔。
type EventLog struct {
	mu           sync.Mutex
	store        *Store
	fallback     *FallbackRecorder
	retentionMax int
	now          func() time.Time
}

// NewEventLog 建立事件記錄器。store 可為 nil(視為 DB 不可用,一律走 fallback);
// fallback 可為 nil(則 DB 不可用時 Append 回錯)。
func NewEventLog(store *Store, fallback *FallbackRecorder, opts EventLogOptions) *EventLog {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &EventLog{
		store:        store,
		fallback:     fallback,
		retentionMax: opts.RetentionMax,
		now:          now,
	}
}

// Append 寫入一則事件。未填時間補當前時間、未填嚴重度補 info。
// 優先寫 DB;store 不可用或寫入失敗則落 fallback NDJSON(不因 DB 故障丟事件)。
func (l *EventLog) Append(ev protocol.Event) error {
	if ev.TsUTC.IsZero() {
		ev.TsUTC = l.now().UTC()
	}
	if ev.Severity == "" {
		ev.Severity = protocol.SeverityInfo
	}
	if ev.Code == "" {
		return fmt.Errorf("事件缺少 code")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.store == nil || l.store.db == nil {
		return l.recordFallback(ev)
	}
	if err := l.insert(ev); err != nil {
		// DB 寫入失敗視為不可用,退回 fallback 以保全事件。
		if fbErr := l.recordFallback(ev); fbErr != nil {
			return fmt.Errorf("DB 寫入失敗(%v)且 fallback 亦失敗: %w", err, fbErr)
		}
		return nil
	}
	return l.enforceRetention()
}

func (l *EventLog) recordFallback(ev protocol.Event) error {
	if l.fallback == nil {
		return fmt.Errorf("事件無法寫入:DB 不可用且未設 fallback")
	}
	return l.fallback.Record(ev)
}

func (l *EventLog) insert(ev protocol.Event) error {
	_, err := l.store.db.Exec(`
		INSERT INTO events (code, ts_utc, severity, instance_uuid, node, template_id, details_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(ev.Code), formatTS(ev.TsUTC), string(ev.Severity),
		ev.InstanceUUID, ev.Node, ev.TemplateID, rawOrNil(ev.DetailsJSON))
	return err
}

func (l *EventLog) enforceRetention() error {
	if l.retentionMax <= 0 {
		return nil
	}
	_, err := l.store.db.Exec(`
		DELETE FROM events
		WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`, l.retentionMax)
	if err != nil {
		return fmt.Errorf("事件 retention 輪替失敗: %w", err)
	}
	return nil
}

// Query 依 filter 查詢事件,結果依時間(其次 id)遞增排序。
func (l *EventLog) Query(f EventFilter) ([]protocol.Event, error) {
	if l.store == nil || l.store.db == nil {
		return nil, fmt.Errorf("DB 不可用,無法查詢事件")
	}

	var (
		conds []string
		args  []any
	)
	if f.InstanceUUID != nil {
		conds = append(conds, "instance_uuid = ?")
		args = append(args, *f.InstanceUUID)
	}
	if f.Code != nil {
		conds = append(conds, "code = ?")
		args = append(args, string(*f.Code))
	}
	if f.Since != nil {
		conds = append(conds, "ts_utc >= ?")
		args = append(args, formatTS(*f.Since))
	}
	if f.Until != nil {
		conds = append(conds, "ts_utc <= ?")
		args = append(args, formatTS(*f.Until))
	}

	query := `SELECT code, ts_utc, severity, instance_uuid, node, template_id, details_json FROM events`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY ts_utc ASC, id ASC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := l.store.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查詢事件失敗: %w", err)
	}
	defer rows.Close()

	var out []protocol.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func scanEvent(sc rowScanner) (protocol.Event, error) {
	var (
		ev       protocol.Event
		code     string
		tsStr    string
		severity string
		instance sql.NullString
		node     sql.NullString
		template sql.NullString
		details  sql.NullString
	)
	if err := sc.Scan(&code, &tsStr, &severity, &instance, &node, &template, &details); err != nil {
		return protocol.Event{}, err
	}
	ev.Code = protocol.EventCode(code)
	ev.Severity = protocol.Severity(severity)
	ts, err := parseTS(tsStr)
	if err != nil {
		return protocol.Event{}, fmt.Errorf("解析事件時間失敗: %w", err)
	}
	ev.TsUTC = ts
	ev.InstanceUUID = nullToPtr(instance)
	ev.Node = nullToPtr(node)
	ev.TemplateID = nullToPtr(template)
	if details.Valid && details.String != "" {
		ev.DetailsJSON = json.RawMessage(details.String)
	}
	return ev, nil
}

// rawOrNil 讓空 JSON 以 SQL NULL 存入(而非空字串),避免查詢時得到空 details。
func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func nullToPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}
