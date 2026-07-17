package core

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func int64p(v int64) *int64 { return &v }
func intp(v int) *int       { return &v }

// TestStore_MigrationV3_ColumnsAndTable 驗證 v3 遷移後:instances.name / port_reservations.name
// 欄位存在且可讀寫(既有列為空預設),metrics 表存在可插入(R10/R12/R13)。
func TestStore_MigrationV3_ColumnsAndTable(t *testing.T) {
	st, _ := newTempStore(t)

	// instances.name 可寫可讀。
	if err := st.UpsertInstance(InstanceRecord{UUID: "u1", TemplateID: "minecraft", Name: "我的伺服器"}); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}
	got, err := st.GetInstance("u1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Name != "我的伺服器" {
		t.Errorf("instances.name 不符: got=%q", got.Name)
	}

	// 既有列(未帶 name)讀回為空字串(DEFAULT '')。
	if err := st.UpsertInstance(InstanceRecord{UUID: "u2", TemplateID: "palworld"}); err != nil {
		t.Fatalf("UpsertInstance u2: %v", err)
	}
	got2, _ := st.GetInstance("u2")
	if got2.Name != "" {
		t.Errorf("未帶 name 的實例應為空字串, got=%q", got2.Name)
	}

	// metrics 表存在:插入一筆後可查回。
	ts := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	if err := st.InsertMetric(MetricPoint{InstanceUUID: "u1", TsUTC: ts, CPUPercent: 12.5, MemoryBytes: int64p(100), MemoryLimit: int64p(1000), PlayerCount: intp(3)}); err != nil {
		t.Fatalf("InsertMetric: %v", err)
	}
	pts, err := st.QueryMetrics("u1", ts.Add(-time.Minute))
	if err != nil {
		t.Fatalf("QueryMetrics: %v", err)
	}
	if len(pts) != 1 || pts[0].CPUPercent != 12.5 {
		t.Fatalf("metrics 查回不符: %+v", pts)
	}
}

// TestStore_PortReservationName 驗證埠預留的 name 欄位可寫入且經 ListPortReservations 讀回(R12);
// 未帶 name 者讀回為空。
func TestStore_PortReservationName(t *testing.T) {
	st, _ := newTempStore(t)

	if err := st.ReservePort(PortReservation{BindIP: "0.0.0.0", Protocol: "udp", HostPort: 8211, InstanceUUID: "u1", Name: "game"}); err != nil {
		t.Fatalf("ReservePort game: %v", err)
	}
	if err := st.ReservePort(PortReservation{BindIP: "0.0.0.0", Protocol: "tcp", HostPort: 25575, InstanceUUID: "u1", Name: "rcon"}); err != nil {
		t.Fatalf("ReservePort rcon: %v", err)
	}
	// 未帶 name(模擬既有資料路徑)。
	if err := st.ReservePort(PortReservation{BindIP: "0.0.0.0", Protocol: "tcp", HostPort: 1, InstanceUUID: "u1"}); err != nil {
		t.Fatalf("ReservePort noname: %v", err)
	}

	list, err := st.ListPortReservations()
	if err != nil {
		t.Fatalf("ListPortReservations: %v", err)
	}
	names := map[int]string{}
	for _, p := range list {
		names[p.HostPort] = p.Name
	}
	if names[8211] != "game" || names[25575] != "rcon" {
		t.Errorf("埠角色名不符: %+v", names)
	}
	if names[1] != "" {
		t.Errorf("未帶 name 的埠應為空, got=%q", names[1])
	}
}

// TestStore_Metrics_NullMemoryAndSummary 驗證 nullable 記憶體透傳(NULL→nil)、QueryMetrics 升冪、
// QueryMetricsSummary 的分母僅計有樣本實例(GROUP BY bucket 的 AVG)與 NULL 忽略語意(R13)。
func TestStore_Metrics_NullMemoryAndSummary(t *testing.T) {
	st, _ := newTempStore(t)
	b0 := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	b1 := b0.Add(15 * time.Second)

	// bucket b0:實例 A(mem 有值)+ 實例 B(mem NULL,模擬 native 降級)。
	mustInsert(t, st, MetricPoint{InstanceUUID: "A", TsUTC: b0, CPUPercent: 10, MemoryBytes: int64p(200), MemoryLimit: int64p(1000), PlayerCount: intp(2)})
	mustInsert(t, st, MetricPoint{InstanceUUID: "B", TsUTC: b0, CPUPercent: 30, MemoryBytes: nil, MemoryLimit: nil, PlayerCount: nil})
	// bucket b1:僅實例 A 有樣本(B 已停止監控 → 不當 0 攤平)。
	mustInsert(t, st, MetricPoint{InstanceUUID: "A", TsUTC: b1, CPUPercent: 50, MemoryBytes: int64p(400), MemoryLimit: int64p(1000), PlayerCount: intp(5)})

	// QueryMetrics(A):升冪兩點,NULL 透傳(A 皆有值)。
	aPts, err := st.QueryMetrics("A", b0)
	if err != nil {
		t.Fatalf("QueryMetrics A: %v", err)
	}
	if len(aPts) != 2 || !aPts[0].TsUTC.Equal(b0) || !aPts[1].TsUTC.Equal(b1) {
		t.Fatalf("QueryMetrics A 升冪不符: %+v", aPts)
	}
	if aPts[0].MemoryBytes == nil || *aPts[0].MemoryBytes != 200 {
		t.Errorf("A@b0 memory 應為 200: %+v", aPts[0].MemoryBytes)
	}

	// QueryMetrics(B):mem 兩欄 NULL 透傳為 nil。
	bPts, _ := st.QueryMetrics("B", b0)
	if len(bPts) != 1 || bPts[0].MemoryBytes != nil || bPts[0].MemoryLimit != nil || bPts[0].PlayerCount != nil {
		t.Errorf("B@b0 的 NULL 欄位應透傳 nil: %+v", bPts)
	}

	// Summary:b0 分母=2(A,B 皆有 cpu 樣本)→ cpu avg=(10+30)/2=20;mem avg 忽略 B 的 NULL → 200。
	//         b1 分母=1(僅 A)→ cpu=50。
	sum, err := st.QueryMetricsSummary(b0)
	if err != nil {
		t.Fatalf("QueryMetricsSummary: %v", err)
	}
	if len(sum) != 2 {
		t.Fatalf("Summary 應有兩個 bucket: %+v", sum)
	}
	if sum[0].CPUPercent != 20 {
		t.Errorf("b0 cpu 全體平均應為 20(分母 2), got=%v", sum[0].CPUPercent)
	}
	if sum[0].MemoryBytes == nil || *sum[0].MemoryBytes != 200 {
		t.Errorf("b0 mem 平均應忽略 NULL 得 200, got=%v", sum[0].MemoryBytes)
	}
	if sum[1].CPUPercent != 50 {
		t.Errorf("b1 cpu 全體平均應為 50(分母 1,B 無樣本不攤平), got=%v", sum[1].CPUPercent)
	}
}

// TestStore_PruneMetrics 驗證 Prune 批刪 before 之前的列、保留其後(R13)。
func TestStore_PruneMetrics(t *testing.T) {
	st, _ := newTempStore(t)
	old := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC) // >36h 前
	recent := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	mustInsert(t, st, MetricPoint{InstanceUUID: "A", TsUTC: old, CPUPercent: 1})
	mustInsert(t, st, MetricPoint{InstanceUUID: "A", TsUTC: recent, CPUPercent: 2})

	if err := st.PruneMetrics(recent.Add(-time.Hour)); err != nil {
		t.Fatalf("PruneMetrics: %v", err)
	}
	pts, _ := st.QueryMetrics("A", old.Add(-time.Hour))
	if len(pts) != 1 || !pts[0].TsUTC.Equal(recent) {
		t.Errorf("Prune 後應只剩 recent 一點: %+v", pts)
	}
}

// TestStore_MigrationV3AppliesOnV2DB 驗證 v3 遷移套用在「只跑到 v2」的舊庫上不出錯、既有資料保留,
// 且新欄位對舊資料為空/新表可用(R10/R12/R13 相容性)。
func TestStore_MigrationV3AppliesOnV2DB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2legacy.db")

	// 手刻只套用到 v2 的舊庫(不經 Store.Open/applyMigrations,模擬升級前建立的資料庫)。
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatalf("建版本表: %v", err)
	}
	for _, m := range migrations {
		if m.version > 2 {
			continue // 只套用到 v2
		}
		for _, stmt := range m.stmts {
			if _, err := raw.Exec(stmt); err != nil {
				t.Fatalf("套用 v%d 遷移: %v", m.version, err)
			}
		}
		if _, err := raw.Exec(`INSERT INTO schema_version (version) VALUES (?)`, m.version); err != nil {
			t.Fatalf("寫入 schema_version: %v", err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO instances (uuid, template_id) VALUES (?, ?)`, "legacy", "minecraft"); err != nil {
		t.Fatalf("插入舊實例: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO port_reservations (bind_ip, protocol, host_port, instance_uuid) VALUES (?, ?, ?, ?)`,
		"0.0.0.0", "tcp", 25565, "legacy"); err != nil {
		t.Fatalf("插入舊埠: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("關閉手刻連線: %v", err)
	}

	// 以現行版本開庫:自動套用 v3。
	st, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open 升級後舊庫: %v", err)
	}
	defer st.Close()
	if q, _ := st.Quarantined(); q {
		t.Errorf("正常升級不應 quarantine")
	}

	// 舊實例保留、name 為空。
	inst, err := st.GetInstance("legacy")
	if err != nil {
		t.Fatalf("GetInstance legacy: %v", err)
	}
	if inst.Name != "" {
		t.Errorf("舊實例 name 應為空, got=%q", inst.Name)
	}
	// 舊埠保留、name 為空。
	list, _ := st.ListPortReservations()
	if len(list) != 1 || list[0].Name != "" {
		t.Errorf("舊埠應保留且 name 空: %+v", list)
	}
	// metrics 新表可用。
	if err := st.InsertMetric(MetricPoint{InstanceUUID: "legacy", TsUTC: time.Now().UTC(), CPUPercent: 1}); err != nil {
		t.Fatalf("升級後 metrics 表應可用: %v", err)
	}
	if current, _ := currentSchemaVersion(st.db); current != schemaVersion() {
		t.Errorf("schema 應升至最新 %d, 得 %d", schemaVersion(), current)
	}
}

func mustInsert(t *testing.T, st *Store, p MetricPoint) {
	t.Helper()
	if err := st.InsertMetric(p); err != nil {
		t.Fatalf("InsertMetric %+v: %v", p, err)
	}
}
