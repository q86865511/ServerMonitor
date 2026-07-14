package core

import (
	"database/sql"
	"fmt"
)

// migration 是一個 schema 版本步進;stmts 於單一交易內依序執行。
type migration struct {
	version int
	stmts   []string
}

// migrations 是有序的 schema 遷移清單。新增結構變更=追加一個更高 version 的項目,
// 不要修改既有項目(已上線的資料庫只會套用尚未套用的版本)。
//
// 表結構對齊 design「持久化(R12)」:
//   - instances(uuid, template_id, variant, params_json, node, runtime_id,
//     desired_state, observed_state, op_generation)
//   - port_reservations(bind_ip, protocol, host_port UNIQUE, instance_uuid)
//   - schedules / settings / backups(backup_id, instance_uuid, ts_utc, checksum)
//   - events(envelope 欄位)
var migrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE instances (
				uuid           TEXT PRIMARY KEY,
				template_id    TEXT NOT NULL,
				variant        TEXT NOT NULL DEFAULT '',
				params_json    TEXT NOT NULL DEFAULT '{}',
				node           TEXT NOT NULL DEFAULT '',
				runtime_id     TEXT NOT NULL DEFAULT '',
				desired_state  TEXT NOT NULL DEFAULT '',
				observed_state TEXT NOT NULL DEFAULT '',
				op_generation  INTEGER NOT NULL DEFAULT 0
			)`,
			// 埠預留:唯一約束擋「完全相同鍵」的重複(R2 基礎)。
			// wildcard(0.0.0.0)與具體 IP 的重疊判定屬呼叫端(T7)語意,不在此強制。
			// instance_uuid 為軟參照(不設 FK),使儲存原語可獨立於實例建立順序使用。
			`CREATE TABLE port_reservations (
				bind_ip       TEXT NOT NULL,
				protocol      TEXT NOT NULL,
				host_port     INTEGER NOT NULL,
				instance_uuid TEXT NOT NULL DEFAULT '',
				UNIQUE (bind_ip, protocol, host_port)
			)`,
			`CREATE INDEX idx_port_reservations_instance ON port_reservations(instance_uuid)`,
			`CREATE TABLE schedules (
				id            TEXT PRIMARY KEY,
				instance_uuid TEXT NOT NULL DEFAULT '',
				kind          TEXT NOT NULL DEFAULT '',
				spec_json     TEXT NOT NULL DEFAULT '{}',
				enabled       INTEGER NOT NULL DEFAULT 1
			)`,
			`CREATE INDEX idx_schedules_instance ON schedules(instance_uuid)`,
			`CREATE TABLE settings (
				key   TEXT PRIMARY KEY,
				value TEXT NOT NULL
			)`,
			`CREATE TABLE backups (
				backup_id     TEXT PRIMARY KEY,
				instance_uuid TEXT NOT NULL DEFAULT '',
				game          TEXT NOT NULL DEFAULT '',
				ts_utc        TEXT NOT NULL,
				checksum      TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX idx_backups_instance ON backups(instance_uuid)`,
			// events:統一封套(R14)。ts_utc 為固定寬度 RFC3339Nano(UTC),
			// 使字典序即時序,支援 ORDER BY 與時間範圍過濾。
			`CREATE TABLE events (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				code          TEXT NOT NULL,
				ts_utc        TEXT NOT NULL,
				severity      TEXT NOT NULL DEFAULT 'info',
				instance_uuid TEXT,
				node          TEXT,
				template_id   TEXT,
				details_json  TEXT
			)`,
			`CREATE INDEX idx_events_instance ON events(instance_uuid)`,
			`CREATE INDEX idx_events_code ON events(code)`,
			`CREATE INDEX idx_events_ts ON events(ts_utc)`,
		},
	},
	{
		// v2(T11 雙審 #6):schedules 加 last_fired_utc,供跨程序(app 重啟)去重——
		// 僅記憶體的 lastFired 在同分鐘重啟後會遺忘,導致重觸發。NULL=從未觸發,
		// 既有資料相容(不需回填)。
		version: 2,
		stmts: []string{
			`ALTER TABLE schedules ADD COLUMN last_fired_utc TEXT`,
		},
	},
}

// schemaVersion 是本二進位期望的最新 schema 版本。
func schemaVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

// applyMigrations 建立版本表(若無)、讀取當前版本,套用所有更高版本的遷移。
// 每個遷移於單一交易內執行,確保部分套用不落地(crash-safe 邊界)。
func applyMigrations(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("建立版本表失敗: %w", err)
	}

	current, err := currentSchemaVersion(db)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyOneMigration(db, m); err != nil {
			return fmt.Errorf("套用遷移 v%d 失敗: %w", m.version, err)
		}
	}
	return nil
}

func currentSchemaVersion(db *sql.DB) (int, error) {
	var version sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("讀取 schema 版本失敗: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

func applyOneMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // Commit 成功後 Rollback 為 no-op

	for _, stmt := range m.stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, m.version); err != nil {
		return err
	}
	return tx.Commit()
}
