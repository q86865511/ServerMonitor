package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"

	"servermonitor/internal/protocol"
)

// 具名匯入 modernc.org/sqlite(驅動名 "sqlite",純 Go 免 CGO):其 init 註冊
// database/sql 驅動,型別 *sqlite.Error 供判別約束違反(見 isUniqueViolation)。

// SQLite 擴充結果碼(見 modernc.org/sqlite/lib);於此以本地常數避免匯入龐大的 lib 套件。
const (
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
)

// 常見錯誤。
var (
	// ErrNotFound 表示查無指定記錄。
	ErrNotFound = errors.New("core: 記錄不存在")
	// ErrPortReserved 表示 (bind_ip, protocol, host_port) 已被預留(唯一約束擋下)。
	ErrPortReserved = errors.New("core: 埠已被預留")
)

// Options 是開庫選項。
type Options struct {
	// Fallback 於「DB 本身不可用」時記錄診斷事件(如 DB_QUARANTINE)。可為 nil。
	Fallback *FallbackRecorder
	// Now 回傳當前時間,預設 time.Now;供測試注入固定時鐘。
	Now func() time.Time
}

// Store 是 SQLite 持久化層(R12)。以 modernc.org/sqlite(純 Go)開庫,
// 啟動時執行遷移與 PRAGMA integrity_check;偵測毀損時進 quarantine(改名保留原檔、
// 以新庫降級啟動),絕不以空庫覆寫原檔。
type Store struct {
	db             *sql.DB
	path           string
	now            func() time.Time
	quarantined    bool
	quarantinePath string
}

// Open 開啟(或建立)位於 path 的 SQLite 資料庫。
//
// 若 path 已存在且非空,啟動時先跑 PRAGMA integrity_check;偵測毀損則:
//  1. 將原檔改名保留為 <path>.corrupt-<ts>(不刪除、不截斷);
//  2. 若提供 Fallback,寫入 DB_QUARANTINE 事件至 fallback 診斷媒介;
//  3. 在原 path 建立全新資料庫降級啟動。
//
// 隨後套用 schema 遷移。回傳的 Store 可經 Quarantined() 查詢是否曾隔離。
func Open(path string, opts Options) (*Store, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	var (
		quarantined    bool
		quarantinePath string
	)

	if fileNonEmpty(path) {
		ok, checkErr := runIntegrityCheck(path)
		if !ok {
			qp, err := quarantineFile(path, now())
			if err != nil {
				return nil, fmt.Errorf("隔離毀損資料庫失敗: %w", err)
			}
			quarantined = true
			quarantinePath = qp
			if opts.Fallback != nil {
				// DB 不可信,診斷事件走 fallback 媒介(R12/R14)。
				_ = opts.Fallback.Record(quarantineEvent(now(), path, qp, checkErr))
			}
		}
	}

	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if err := applyMigrations(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{
		db:             db,
		path:           path,
		now:            now,
		quarantined:    quarantined,
		quarantinePath: quarantinePath,
	}, nil
}

// Close 關閉底層資料庫。
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Quarantined 回報上次 Open 是否因毀損而隔離原檔,以及被保留的隔離檔路徑。
func (s *Store) Quarantined() (bool, string) {
	return s.quarantined, s.quarantinePath
}

// openDB 以固定 DSN 開庫並限制單一連線,使寫入序列化(桌面工具流量低,
// 以簡單性換取免 SQLITE_BUSY 與可預期的併發語意——如埠預留唯一約束)。
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("開啟資料庫失敗: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("連線資料庫失敗: %w", err)
	}
	return db, nil
}

// dsn 組出 modernc DSN。modernc 對非 file: 前綴的 DSN 會把 '?' 後的查詢自路徑剝離,
// 故 Windows 路徑(含反斜線與 C:)可原樣通過,pragma 仍套用於每條連線。
func dsn(path string) string {
	return path + "?_pragma=busy_timeout(5000)"
}

// runIntegrityCheck 以獨立、隨即關閉的連線跑 PRAGMA integrity_check。
// 回傳 (ok, 診斷錯誤)。無法開啟或查詢失敗(如「非資料庫檔」)均視為毀損。
//
// 關鍵:必須在回傳前完全關閉連線,否則 Windows 無法改名該檔(quarantine 會失敗)。
func runIntegrityCheck(path string) (ok bool, cause error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return false, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return false, err
	}
	if !strings.EqualFold(result, "ok") {
		return false, fmt.Errorf("integrity_check: %s", result)
	}
	return true, nil
}

// quarantineFile 將毀損檔改名保留為 <path>.corrupt-<ts>,不刪除、不截斷原內容。
// 一併移置可能存在的 -wal/-shm/-journal 側車檔,避免污染隨後在原路徑新建的資料庫。
func quarantineFile(path string, t time.Time) (string, error) {
	base := path + ".corrupt-" + t.UTC().Format("20060102T150405.000000000Z")
	qp := base
	for i := 1; fileExists(qp); i++ {
		qp = fmt.Sprintf("%s-%d", base, i)
	}
	if err := os.Rename(path, qp); err != nil {
		return "", err
	}
	for _, suf := range []string{"-wal", "-shm", "-journal"} {
		if fileExists(path + suf) {
			_ = os.Rename(path+suf, qp+suf)
		}
	}
	return qp, nil
}

// quarantineEvent 建構 DB_QUARANTINE 事件封套。
func quarantineEvent(t time.Time, original, quarantine string, cause error) protocol.Event {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	details, _ := json.Marshal(map[string]string{
		"original":   original,
		"quarantine": quarantine,
		"cause":      msg,
	})
	return protocol.Event{
		Code:        protocol.EventDBQuarantine,
		TsUTC:       t.UTC(),
		Severity:    protocol.SeverityError,
		DetailsJSON: details,
	}
}

// isUniqueViolation 判別錯誤是否為 UNIQUE / PRIMARY KEY 約束違反。
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code()
		return code == sqliteConstraintUnique || code == sqliteConstraintPrimaryKey
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileNonEmpty(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// --- 時間格式:固定寬度 RFC3339Nano(UTC),使文字排序即時序排序 ---

const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTS 以固定寬度格式(9 位小數 + Z)輸出 UTC 時間,確保 events.ts_utc 的
// 字典序等於時間序(供 ORDER BY 與 Since/Until 範圍過濾)。
func formatTS(t time.Time) string {
	return t.UTC().Format(tsLayout)
}

// parseTS 解析 formatTS 產出的時間字串。
func parseTS(s string) (time.Time, error) {
	return time.Parse(tsLayout, s)
}
