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
)

// JournalEntry 是一筆「進行中建立」的紀錄(R2 原子建立 + R13 對帳)。
//
// 建立流程在「建容器前」寫入本紀錄、「寫 DB 完成後」清除。若程序於「建容器後、
// 寫 DB 前」崩潰,紀錄殘留於磁碟,供下次啟動的對帳(T8 Reconciler)依 UUID 找出
// 孤兒容器(比對 gsm.uuid 標籤)與已預留但未落地的埠(port_reservations.instance_uuid)
// 並清理。故本紀錄只需最小定位資訊,埠與容器細節由 DB/標籤反查。
type JournalEntry struct {
	UUID       string    `json:"uuid"`
	Node       string    `json:"node"`
	TemplateID string    `json:"template_id"`
	StartedAt  time.Time `json:"started_at"`
}

// Journal 是檔案式的建立 journal:每筆進行中建立對應 <dir>/<uuid>.json。
// 檔案存在=建立未完成;成功後刪除。以檔案系統作為 crash-safe 媒介(程序崩潰時
// 殘留檔即為待對帳清單),避免與 SQLite 交易生命週期耦合。併發安全。
type Journal struct {
	mu  sync.Mutex
	dir string
}

// NewJournal 建立(或沿用)位於 dir 的 journal 目錄。
func NewJournal(dir string) (*Journal, error) {
	if dir == "" {
		return nil, fmt.Errorf("core: Journal 目錄不可為空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("建立 journal 目錄失敗: %w", err)
	}
	return &Journal{dir: dir}, nil
}

// Begin 寫入一筆進行中建立紀錄。以「先寫暫存檔再改名」確保紀錄檔本身原子落地。
func (j *Journal) Begin(entry JournalEntry) error {
	if entry.UUID == "" {
		return fmt.Errorf("core: journal 紀錄缺少 UUID")
	}
	if entry.StartedAt.IsZero() {
		entry.StartedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 journal 紀錄失敗: %w", err)
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	final := j.pathFor(entry.UUID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("寫入 journal 暫存檔失敗: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("提交 journal 紀錄失敗: %w", err)
	}
	return nil
}

// Complete 清除某 UUID 的進行中建立紀錄(冪等:不存在也不報錯)。
func (j *Journal) Complete(uuid string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	err := os.Remove(j.pathFor(uuid))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清除 journal 紀錄失敗: %w", err)
	}
	return nil
}

// List 回傳所有殘留的進行中建立紀錄(供 T8 對帳),依 UUID 排序。損毀的個別檔略過。
func (j *Journal) List() ([]JournalEntry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	names, err := filepath.Glob(filepath.Join(j.dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("列舉 journal 目錄失敗: %w", err)
	}
	var out []JournalEntry
	for _, name := range names {
		data, rerr := os.ReadFile(name)
		if rerr != nil {
			continue
		}
		var e JournalEntry
		if json.Unmarshal(data, &e) != nil || e.UUID == "" {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].UUID < out[k].UUID })
	return out, nil
}

// pathFor 回傳某 UUID 的紀錄檔路徑。UUID 內的路徑分隔字元被清掉,避免逸出 dir。
func (j *Journal) pathFor(uuid string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(uuid)
	return filepath.Join(j.dir, safe+".json")
}
