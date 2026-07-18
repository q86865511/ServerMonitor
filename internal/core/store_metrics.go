package core

import (
	"database/sql"
	"fmt"
	"time"
)

// MetricPoint 是 metrics 表的一列(R13:15s 聚合的資源時序)。記憶體兩欄與玩家數以指標表示
// nullable:nil = 該 bucket 不可採集/不適用(前端畫缺口,不補 0)。TsUTC 為對齊 UTC 的 15s
// bucket 起點。QueryMetricsSummary 回傳的全體平均列 InstanceUUID 為空。
type MetricPoint struct {
	InstanceUUID string
	TsUTC        time.Time
	CPUPercent   float64
	MemoryBytes  *int64 // nil = 該 bucket 記憶體不可採集(native Job 降級等)
	MemoryLimit  *int64 // nil = 同上(與 MemoryBytes 同進退)
	PlayerCount  *int   // nil = 不適用(範本無查詢/相依未注入)
}

// InsertMetric 寫入一筆聚合指標。以 (instance_uuid, ts_utc) 為主鍵;同 bucket 重寫以最新覆蓋
// (ON CONFLICT DO UPDATE),使重啟後對同一 bucket 的補寫冪等。
func (s *Store) InsertMetric(p MetricPoint) error {
	_, err := s.db.Exec(`
		INSERT INTO metrics (instance_uuid, ts_utc, cpu_percent, memory_bytes, memory_limit, player_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(instance_uuid, ts_utc) DO UPDATE SET
			cpu_percent  = excluded.cpu_percent,
			memory_bytes = excluded.memory_bytes,
			memory_limit = excluded.memory_limit,
			player_count = excluded.player_count`,
		p.InstanceUUID, formatTS(p.TsUTC), p.CPUPercent,
		nullInt64(p.MemoryBytes), nullInt64(p.MemoryLimit), nullInt(p.PlayerCount))
	if err != nil {
		return fmt.Errorf("寫入指標 %s@%s 失敗: %w", p.InstanceUUID, formatTS(p.TsUTC), err)
	}
	return nil
}

// QueryMetrics 回傳某實例自 since(含)起的聚合指標,按時間升冪(NULL 欄位透傳)。
func (s *Store) QueryMetrics(uuid string, since time.Time) ([]MetricPoint, error) {
	rows, err := s.db.Query(`
		SELECT ts_utc, cpu_percent, memory_bytes, memory_limit, player_count
		FROM metrics WHERE instance_uuid = ? AND ts_utc >= ? ORDER BY ts_utc ASC`,
		uuid, formatTS(since))
	if err != nil {
		return nil, fmt.Errorf("查詢指標時序失敗: %w", err)
	}
	defer rows.Close()

	var out []MetricPoint
	for rows.Next() {
		p := MetricPoint{InstanceUUID: uuid}
		var (
			tsStr  string
			mBytes sql.NullInt64
			mLimit sql.NullInt64
			pCount sql.NullInt64
		)
		if err := rows.Scan(&tsStr, &p.CPUPercent, &mBytes, &mLimit, &pCount); err != nil {
			return nil, err
		}
		if p.TsUTC, err = parseTS(tsStr); err != nil {
			return nil, fmt.Errorf("解析指標時間失敗: %w", err)
		}
		p.MemoryBytes = int64PtrFromNull(mBytes)
		p.MemoryLimit = int64PtrFromNull(mLimit)
		p.PlayerCount = intPtrFromNull(pCount)
		out = append(out, p)
	}
	return out, rows.Err()
}

// QueryMetricsSummary 回傳自 since(含)起、各 15s bucket 的全體平均(供總覽全體趨勢圖)。
// 分母僅計「該 bucket 有樣本的實例」——每列即一個 (instance, bucket) 樣本,故 GROUP BY ts_utc
// 的 AVG 天然只計有列的實例,無樣本實例不被當 0 攤平。記憶體/玩家數的 AVG 亦忽略 NULL 列
// (SQL AVG 語意):某 bucket 全數不可採集 → 回 NULL(前端畫缺口)。回傳列 InstanceUUID 為空。
func (s *Store) QueryMetricsSummary(since time.Time) ([]MetricPoint, error) {
	rows, err := s.db.Query(`
		SELECT ts_utc,
			AVG(cpu_percent)  AS cpu,
			AVG(memory_bytes) AS mem,
			AVG(memory_limit) AS lim,
			AVG(player_count) AS players
		FROM metrics WHERE ts_utc >= ?
		GROUP BY ts_utc ORDER BY ts_utc ASC`, formatTS(since))
	if err != nil {
		return nil, fmt.Errorf("查詢指標全體摘要失敗: %w", err)
	}
	defer rows.Close()

	var out []MetricPoint
	for rows.Next() {
		var (
			tsStr   string
			cpu     float64
			mem     sql.NullFloat64
			lim     sql.NullFloat64
			players sql.NullFloat64
		)
		if err := rows.Scan(&tsStr, &cpu, &mem, &lim, &players); err != nil {
			return nil, err
		}
		p := MetricPoint{CPUPercent: cpu}
		if p.TsUTC, err = parseTS(tsStr); err != nil {
			return nil, fmt.Errorf("解析指標時間失敗: %w", err)
		}
		p.MemoryBytes = int64PtrFromFloat(mem)
		p.MemoryLimit = int64PtrFromFloat(lim)
		p.PlayerCount = intPtrFromFloat(players)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneMetrics 刪除 ts_utc 早於 before 的指標(R13:36h 保留;走 idx_metrics_ts 範圍掃描)。
func (s *Store) PruneMetrics(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM metrics WHERE ts_utc < ?`, formatTS(before))
	if err != nil {
		return fmt.Errorf("清理過期指標失敗: %w", err)
	}
	return nil
}

// ---- nullable 轉換輔助 ----

func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func int64PtrFromNull(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func intPtrFromNull(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// int64PtrFromFloat 把 AVG(...) 的可空浮點四捨五入為 *int64(NULL→nil);記憶體位元組取整無損精度需求。
func int64PtrFromFloat(n sql.NullFloat64) *int64 {
	if !n.Valid {
		return nil
	}
	v := int64(n.Float64 + 0.5)
	return &v
}

// intPtrFromFloat 把 AVG(player_count) 的可空浮點四捨五入為 *int(NULL→nil)。
func intPtrFromFloat(n sql.NullFloat64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Float64 + 0.5)
	return &v
}
