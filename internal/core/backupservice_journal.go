package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"servermonitor/internal/protocol"
)

// OpKind 是一筆操作 journal 的種類(備份/還原)。
type OpKind string

const (
	OpKindBackup  OpKind = "backup"
	OpKindRestore OpKind = "restore"
)

// OpJournalEntry 是一筆「進行中備份/還原」的 crash-safe 紀錄(R9 中斷邊界)。
//
// 設計選擇:刻意**不**重用建立用的 Journal(journal.go)。建立 Journal 的殘留檔被
// Reconciler.cleanupJournal 解讀為「未完成建立」→ 會 purge 移除孤兒容器;若備份/還原
// 的進行中紀錄寫進同一機制,對帳會把「備份中的存活實例」誤判為未完成建立而刪掉容器。
// 故備份/還原採**平行的獨立目錄與型別**,只承載收斂所需的最小資訊(原 desired 是否為
// Running),供中斷後由 BackupService.RecoverInterrupted 依原 desired 收斂,與建立對帳互不干擾。
type OpJournalEntry struct {
	UUID     string            `json:"uuid"`
	Node     string            `json:"node"`
	Kind     OpKind            `json:"kind"`
	BackupID protocol.BackupID `json:"backup_id,omitempty"` // 還原用:進行中的來源備份
	// ResumeToRunning 記錄操作開始前實例是否為 Running:中斷復原時據此收斂 desired
	// (true→重新啟動;false→保持停止),對齊 R9「依原 desired 狀態恢復」。
	ResumeToRunning bool `json:"resume_to_running"`
	// NewRuntimeID 於還原 nodeRestore 成功後、DB Upsert 前寫回(crash-safe):中斷復原據此認養
	// 還原產生的新容器,避免崩潰於兩者之間時 DB RuntimeID 仍指向已被切換掉的舊容器。備份不用。
	NewRuntimeID protocol.RuntimeID `json:"new_runtime_id,omitempty"`
	StartedAt    time.Time          `json:"started_at"`
}

// OpJournal 是檔案式的操作 journal:每筆進行中備份/還原對應 <dir>/<uuid>.json。
// 檔案存在=操作未完成;成功(或已收斂)後刪除。以檔案系統作為 crash-safe 媒介,語意同
// 建立用 Journal,但目錄與型別獨立(見 OpJournalEntry 設計選擇)。併發安全。
type OpJournal struct {
	mu  sync.Mutex
	dir string
}

// NewOpJournal 建立(或沿用)位於 dir 的操作 journal 目錄。dir 應與建立用 Journal 目錄不同。
func NewOpJournal(dir string) (*OpJournal, error) {
	if dir == "" {
		return nil, fmt.Errorf("core: OpJournal 目錄不可為空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("建立操作 journal 目錄失敗: %w", err)
	}
	return &OpJournal{dir: dir}, nil
}

// Begin 寫入一筆進行中操作紀錄(先寫暫存檔再改名,確保紀錄檔本身原子落地)。
func (j *OpJournal) Begin(entry OpJournalEntry) error {
	if entry.UUID == "" {
		return fmt.Errorf("core: 操作 journal 紀錄缺少 UUID")
	}
	if entry.StartedAt.IsZero() {
		entry.StartedAt = time.Now().UTC()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.writeLocked(entry)
}

// SetNewRuntimeID 為某 UUID 既有的進行中還原紀錄補記還原產生的新 RuntimeID(讀-改-寫,原子落地)。
// 紀錄不存在 → 錯誤(呼叫端須已 Begin 過)。供 Restore 在 DB Upsert 前落 journal,實現 #5 認養復原。
func (j *OpJournal) SetNewRuntimeID(uuid string, newID protocol.RuntimeID) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	data, err := os.ReadFile(j.pathFor(uuid))
	if err != nil {
		return fmt.Errorf("讀取操作 journal 紀錄失敗: %w", err)
	}
	var e OpJournalEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return fmt.Errorf("解析操作 journal 紀錄失敗: %w", err)
	}
	e.NewRuntimeID = newID
	return j.writeLocked(e)
}

// writeLocked 以「暫存檔 + rename」原子寫入一筆紀錄。呼叫端須已持 j.mu。
func (j *OpJournal) writeLocked(entry OpJournalEntry) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化操作 journal 紀錄失敗: %w", err)
	}
	final := j.pathFor(entry.UUID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("寫入操作 journal 暫存檔失敗: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("提交操作 journal 紀錄失敗: %w", err)
	}
	return nil
}

// Complete 清除某 UUID 的進行中操作紀錄(冪等:不存在也不報錯)。
func (j *OpJournal) Complete(uuid string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	err := os.Remove(j.pathFor(uuid))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清除操作 journal 紀錄失敗: %w", err)
	}
	return nil
}

// List 回傳所有殘留的進行中操作紀錄(供中斷復原),依 UUID 排序。損毀的個別檔略過。
func (j *OpJournal) List() ([]OpJournalEntry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	names, err := filepath.Glob(filepath.Join(j.dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("列舉操作 journal 目錄失敗: %w", err)
	}
	var out []OpJournalEntry
	for _, name := range names {
		data, rerr := os.ReadFile(name)
		if rerr != nil {
			continue
		}
		var e OpJournalEntry
		if json.Unmarshal(data, &e) != nil || e.UUID == "" {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].UUID < out[k].UUID })
	return out, nil
}

// pathFor 回傳某 UUID 的紀錄檔路徑。UUID 內的路徑分隔字元被清掉,避免逸出 dir。
func (j *OpJournal) pathFor(uuid string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(uuid)
	return filepath.Join(j.dir, safe+".json")
}
