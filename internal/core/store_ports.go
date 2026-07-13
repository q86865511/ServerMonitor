package core

import "fmt"

// PortReservation 是 port_reservations 表的一列(R2)。
// 衝突鍵為 (BindIP, Protocol, HostPort);唯一約束擋下完全相同鍵的重複預留。
type PortReservation struct {
	BindIP       string
	Protocol     string
	HostPort     int
	InstanceUUID string
}

// ReservePort 於 DB 以唯一約束預留一個埠鍵。
// 若相同 (bind_ip, protocol, host_port) 已存在,回傳 ErrPortReserved。
//
// 注意:本函式只擋「完全相同鍵」的重複(R2 基礎)。wildcard(0.0.0.0)與具體 IP
// 的重疊判定屬呼叫端(T7)的高階語意,於 InstanceService.Create 分階段流程中補齊;
// SetMaxOpenConns(1) 使併發預留序列化,故兩個併發建立不會同時通過。
func (s *Store) ReservePort(r PortReservation) error {
	_, err := s.db.Exec(`
		INSERT INTO port_reservations (bind_ip, protocol, host_port, instance_uuid)
		VALUES (?, ?, ?, ?)`,
		r.BindIP, r.Protocol, r.HostPort, r.InstanceUUID)
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
	rows, err := s.db.Query(`
		SELECT bind_ip, protocol, host_port, instance_uuid
		FROM port_reservations ORDER BY bind_ip, protocol, host_port`)
	if err != nil {
		return nil, fmt.Errorf("查詢埠預留清單失敗: %w", err)
	}
	defer rows.Close()

	var out []PortReservation
	for rows.Next() {
		var r PortReservation
		if err := rows.Scan(&r.BindIP, &r.Protocol, &r.HostPort, &r.InstanceUUID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
