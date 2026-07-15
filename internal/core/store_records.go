package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"servermonitor/internal/protocol"
)

// InstanceRecord 是 instances 表的一列(持久化原語;非 T7 的 InstanceService)。
// 沿用 protocol 的識別碼與狀態列舉,供 core/GUI 一致引用。
type InstanceRecord struct {
	UUID          string
	TemplateID    string
	Variant       string
	ParamsJSON    json.RawMessage
	Node          string
	RuntimeID     protocol.RuntimeID
	DesiredState  protocol.InstanceState
	ObservedState protocol.InstanceState
	OpGeneration  int64
}

// UpsertInstance 以 uuid 為主鍵新增或更新一列實例。
func (s *Store) UpsertInstance(rec InstanceRecord) error {
	params := rec.ParamsJSON
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	_, err := s.db.Exec(`
		INSERT INTO instances
			(uuid, template_id, variant, params_json, node, runtime_id, desired_state, observed_state, op_generation)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
			template_id    = excluded.template_id,
			variant        = excluded.variant,
			params_json    = excluded.params_json,
			node           = excluded.node,
			runtime_id     = excluded.runtime_id,
			desired_state  = excluded.desired_state,
			observed_state = excluded.observed_state,
			op_generation  = excluded.op_generation`,
		rec.UUID, rec.TemplateID, rec.Variant, string(params), rec.Node,
		string(rec.RuntimeID), string(rec.DesiredState), string(rec.ObservedState), rec.OpGeneration)
	if err != nil {
		return fmt.Errorf("寫入實例 %s 失敗: %w", rec.UUID, err)
	}
	return nil
}

// GetInstance 依 uuid 取一列實例;查無回 ErrNotFound。
func (s *Store) GetInstance(uuid string) (InstanceRecord, error) {
	row := s.db.QueryRow(`
		SELECT uuid, template_id, variant, params_json, node, runtime_id, desired_state, observed_state, op_generation
		FROM instances WHERE uuid = ?`, uuid)
	rec, err := scanInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return InstanceRecord{}, ErrNotFound
	}
	return rec, err
}

// ListInstances 回傳所有實例,依 uuid 排序(穩定輸出)。
func (s *Store) ListInstances() ([]InstanceRecord, error) {
	rows, err := s.db.Query(`
		SELECT uuid, template_id, variant, params_json, node, runtime_id, desired_state, observed_state, op_generation
		FROM instances ORDER BY uuid`)
	if err != nil {
		return nil, fmt.Errorf("查詢實例清單失敗: %w", err)
	}
	defer rows.Close()

	var out []InstanceRecord
	for rows.Next() {
		rec, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteInstance 刪除一列實例(不連動刪除其埠預留,呼叫端另行 ReleasePortsForInstance)。
func (s *Store) DeleteInstance(uuid string) error {
	_, err := s.db.Exec(`DELETE FROM instances WHERE uuid = ?`, uuid)
	if err != nil {
		return fmt.Errorf("刪除實例 %s 失敗: %w", uuid, err)
	}
	return nil
}

// rowScanner 抽象 *sql.Row 與 *sql.Rows 的 Scan。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanInstance(sc rowScanner) (InstanceRecord, error) {
	var (
		rec      InstanceRecord
		params   string
		runtime  string
		desired  string
		observed string
	)
	if err := sc.Scan(&rec.UUID, &rec.TemplateID, &rec.Variant, &params, &rec.Node,
		&runtime, &desired, &observed, &rec.OpGeneration); err != nil {
		return InstanceRecord{}, err
	}
	rec.ParamsJSON = json.RawMessage(params)
	rec.RuntimeID = protocol.RuntimeID(runtime)
	rec.DesiredState = protocol.InstanceState(desired)
	rec.ObservedState = protocol.InstanceState(observed)
	return rec, nil
}

// --- settings(DB 內鍵值設定;有別於 R12「一般設定檔」,見 configfile.go)---

// SetSetting 寫入(或覆蓋)一個設定鍵值。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("寫入設定 %s 失敗: %w", key, err)
	}
	return nil
}

// GetSetting 讀取一個設定;第二回傳值表示鍵是否存在。
func (s *Store) GetSetting(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("讀取設定 %s 失敗: %w", key, err)
	}
	return value, true, nil
}

// --- schedules ---

// ScheduleRecord 是 schedules 表的一列。LastFiredUTC 為該排程最後一次成功觸發的到點分鐘
// (UTC,已 truncate 至分);nil 表示從未觸發或(舊資料)未記錄。供 Scheduler 跨程序(app 重啟)
// 去重(T11 雙審 #6):僅記憶體的去重狀態在同分鐘重啟後會遺忘,需以此欄位補強。
type ScheduleRecord struct {
	ID           string
	InstanceUUID string
	Kind         string
	SpecJSON     json.RawMessage
	Enabled      bool
	LastFiredUTC *time.Time
}

// UpsertSchedule 以 id 為主鍵新增或更新一列排程。last_fired_utc 刻意不隨一般欄位覆寫:
// CRUD(Create/UpdateSchedule)呼叫時不帶 LastFiredUTC(nil),若 ON CONFLICT 無條件覆寫會
// 抹掉 Scheduler.claim 已寫入的觸發紀錄;僅當呼叫端明確帶值時才更新,否則保留既有值
// (見 MarkScheduleFired,是寫入該欄位的唯一路徑)。
func (s *Store) UpsertSchedule(rec ScheduleRecord) error {
	spec := rec.SpecJSON
	if len(spec) == 0 {
		spec = json.RawMessage("{}")
	}
	var lastFired any
	if rec.LastFiredUTC != nil {
		lastFired = rec.LastFiredUTC.UTC().Format(time.RFC3339)
	}
	_, err := s.db.Exec(`
		INSERT INTO schedules (id, instance_uuid, kind, spec_json, enabled, last_fired_utc)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			instance_uuid  = excluded.instance_uuid,
			kind           = excluded.kind,
			spec_json      = excluded.spec_json,
			enabled        = excluded.enabled,
			last_fired_utc = COALESCE(excluded.last_fired_utc, schedules.last_fired_utc)`,
		rec.ID, rec.InstanceUUID, rec.Kind, string(spec), boolToInt(rec.Enabled), lastFired)
	if err != nil {
		return fmt.Errorf("寫入排程 %s 失敗: %w", rec.ID, err)
	}
	return nil
}

// MarkScheduleFired 記錄排程 id 已於 minute(UTC,呼叫端須先 truncate 至分)觸發,供其他
// Scheduler 實例(如 app 重啟後新建者)於 ListSchedules 讀回時據以去重(T11 雙審 #6)。
func (s *Store) MarkScheduleFired(id string, minute time.Time) error {
	_, err := s.db.Exec(`UPDATE schedules SET last_fired_utc = ? WHERE id = ?`,
		minute.UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("記錄排程 %s 觸發時刻失敗: %w", id, err)
	}
	return nil
}

// ListSchedules 回傳所有排程,依 id 排序。
func (s *Store) ListSchedules() ([]ScheduleRecord, error) {
	rows, err := s.db.Query(`SELECT id, instance_uuid, kind, spec_json, enabled, last_fired_utc FROM schedules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("查詢排程清單失敗: %w", err)
	}
	defer rows.Close()

	var out []ScheduleRecord
	for rows.Next() {
		var (
			rec       ScheduleRecord
			spec      string
			enabled   int
			lastFired sql.NullString
		)
		if err := rows.Scan(&rec.ID, &rec.InstanceUUID, &rec.Kind, &spec, &enabled, &lastFired); err != nil {
			return nil, err
		}
		rec.SpecJSON = json.RawMessage(spec)
		rec.Enabled = enabled != 0
		if lastFired.Valid && lastFired.String != "" {
			if t, perr := time.Parse(time.RFC3339, lastFired.String); perr == nil {
				tt := t.UTC()
				rec.LastFiredUTC = &tt
			}
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteSchedule 依 id 刪除一筆排程(不存在則 no-op)。
func (s *Store) DeleteSchedule(id string) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("刪除排程 %s 失敗: %w", id, err)
	}
	return nil
}

// --- backups(中繼;實際備份根由 agent 擁有,core 只存 opaque BackupID 與中繼)---

// PutBackup 寫入一筆備份中繼。
func (s *Store) PutBackup(m protocol.BackupMeta) error {
	_, err := s.db.Exec(`
		INSERT INTO backups (backup_id, instance_uuid, game, ts_utc, checksum)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(backup_id) DO UPDATE SET
			instance_uuid = excluded.instance_uuid,
			game          = excluded.game,
			ts_utc        = excluded.ts_utc,
			checksum      = excluded.checksum`,
		string(m.BackupID), m.InstanceUUID, m.Game, formatTS(m.TsUTC), m.Checksum)
	if err != nil {
		return fmt.Errorf("寫入備份中繼 %s 失敗: %w", m.BackupID, err)
	}
	return nil
}

// ListBackups 回傳某實例的備份中繼,依時間新到舊排序。
func (s *Store) ListBackups(instanceUUID string) ([]protocol.BackupMeta, error) {
	rows, err := s.db.Query(`
		SELECT backup_id, instance_uuid, game, ts_utc, checksum
		FROM backups WHERE instance_uuid = ? ORDER BY ts_utc DESC`, instanceUUID)
	if err != nil {
		return nil, fmt.Errorf("查詢備份清單失敗: %w", err)
	}
	defer rows.Close()

	var out []protocol.BackupMeta
	for rows.Next() {
		var (
			m     protocol.BackupMeta
			id    string
			tsStr string
		)
		if err := rows.Scan(&id, &m.InstanceUUID, &m.Game, &tsStr, &m.Checksum); err != nil {
			return nil, err
		}
		m.BackupID = protocol.BackupID(id)
		if m.TsUTC, err = parseTS(tsStr); err != nil {
			return nil, fmt.Errorf("解析備份時間失敗: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteBackup 刪除一筆備份中繼(保留策略刪最舊時同步清 store 列;不存在則 no-op)。
func (s *Store) DeleteBackup(backupID protocol.BackupID) error {
	_, err := s.db.Exec(`DELETE FROM backups WHERE backup_id = ?`, string(backupID))
	if err != nil {
		return fmt.Errorf("刪除備份中繼 %s 失敗: %w", backupID, err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
