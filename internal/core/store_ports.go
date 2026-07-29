package core

import "fmt"

// PortReservation 是 port_reservations 表的一列(R2)。
// 衝突鍵為 (BindIP, Protocol, HostPort);唯一約束擋下完全相同鍵的重複預留。
type PortReservation struct {
	BindIP       string
	Protocol     string
	HostPort     int
	InstanceUUID string
	Name         string // 埠角色名(R12,取自範本 PortSpec.Name);既有資料為空 → 顯示埠但不標角色
}

// ReservePort 於 DB 以唯一約束預留一個埠鍵。
// 若相同 (bind_ip, protocol, host_port) 已存在,回傳 ErrPortReserved。
//
// 注意:本函式只擋「完全相同鍵」的重複(R2 基礎)。wildcard(0.0.0.0)與具體 IP
// 的重疊判定屬呼叫端(T7)的高階語意,於 InstanceService.Create 分階段流程中補齊;
// SetMaxOpenConns(1) 使併發預留序列化,故兩個併發建立不會同時通過。
func (s *Store) ReservePort(r PortReservation) error {
	_, err := s.db.Exec(`
		INSERT INTO port_reservations (bind_ip, protocol, host_port, instance_uuid, name)
		VALUES (?, ?, ?, ?, ?)`,
		r.BindIP, r.Protocol, r.HostPort, r.InstanceUUID, r.Name)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s/%s:%d", ErrPortReserved, r.BindIP, r.Protocol, r.HostPort)
		}
		return fmt.Errorf("預留埠失敗: %w", err)
	}
	return nil
}

// ReleasePort 釋放一個埠鍵(冪等:不存在也不報錯)。
func (s *Store) ReleasePort(bindIP, protocol string, hostPort int) error {
	_, err := s.db.Exec(`
		DELETE FROM port_reservations WHERE bind_ip = ? AND protocol = ? AND host_port = ?`,
		bindIP, protocol, hostPort)
	if err != nil {
		return fmt.Errorf("釋放埠失敗: %w", err)
	}
	return nil
}

// ReleasePortsForInstance 釋放某實例的所有埠預留(回滾/移除實例時使用)。
func (s *Store) ReleasePortsForInstance(instanceUUID string) error {
	_, err := s.db.Exec(`DELETE FROM port_reservations WHERE instance_uuid = ?`, instanceUUID)
	if err != nil {
		return fmt.Errorf("釋放實例 %s 的埠預留失敗: %w", instanceUUID, err)
	}
	return nil
}

// ListPortReservations 回傳所有埠預留,依 (bind_ip, protocol, host_port) 排序。
func (s *Store) ListPortReservations() ([]PortReservation, error) {
	return s.queryPortReservations(`
		SELECT bind_ip, protocol, host_port, instance_uuid, name
		FROM port_reservations ORDER BY bind_ip, protocol, host_port`)
}

// ListPortReservationsForInstance 回傳某實例的埠預留(依 protocol, host_port 排序)。
// 供指令協定/健康探針以「該實例實際預留的埠」解析目標,而非讀範本宣告值——覆寫與核心
// 動態分配後,範本宣告值已不代表實際埠。
func (s *Store) ListPortReservationsForInstance(instanceUUID string) ([]PortReservation, error) {
	return s.queryPortReservations(`
		SELECT bind_ip, protocol, host_port, instance_uuid, name
		FROM port_reservations WHERE instance_uuid = ? ORDER BY protocol, host_port`, instanceUUID)
}

// queryPortReservations 掃出埠預留列(ListPortReservations/…ForInstance 共用)。
func (s *Store) queryPortReservations(query string, args ...any) ([]PortReservation, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查詢埠預留清單失敗: %w", err)
	}
	defer rows.Close()

	var out []PortReservation
	for rows.Next() {
		var r PortReservation
		if err := rows.Scan(&r.BindIP, &r.Protocol, &r.HostPort, &r.InstanceUUID, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
