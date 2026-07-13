package core

import (
	"errors"
	"sync"
	"testing"
)

// TestReservePort_UniqueBlocksDuplicate 驗證唯一約束擋下完全相同鍵的重複預留(R2)。
func TestReservePort_UniqueBlocksDuplicate(t *testing.T) {
	st, _ := newTempStore(t)

	r := PortReservation{BindIP: "0.0.0.0", Protocol: "udp", HostPort: 25565, InstanceUUID: "u1"}
	if err := st.ReservePort(r); err != nil {
		t.Fatalf("首次預留應成功: %v", err)
	}

	// 完全相同鍵 → ErrPortReserved。
	if err := st.ReservePort(r); !errors.Is(err, ErrPortReserved) {
		t.Errorf("重複相同鍵應回 ErrPortReserved, got %v", err)
	}

	// 不同 host_port → 成功。
	if err := st.ReservePort(PortReservation{BindIP: "0.0.0.0", Protocol: "udp", HostPort: 25566, InstanceUUID: "u1"}); err != nil {
		t.Errorf("不同埠應成功: %v", err)
	}
	// 相同埠不同協定 → 不同鍵,成功。
	if err := st.ReservePort(PortReservation{BindIP: "0.0.0.0", Protocol: "tcp", HostPort: 25565, InstanceUUID: "u1"}); err != nil {
		t.Errorf("不同協定應成功: %v", err)
	}

	list, err := st.ListPortReservations()
	if err != nil {
		t.Fatalf("ListPortReservations: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("預留數=%d 期望 3", len(list))
	}
}

// TestReleasePort 驗證釋放後可重新預留,且釋放為冪等。
func TestReleasePort(t *testing.T) {
	st, _ := newTempStore(t)
	r := PortReservation{BindIP: "127.0.0.1", Protocol: "tcp", HostPort: 8080, InstanceUUID: "u1"}
	if err := st.ReservePort(r); err != nil {
		t.Fatalf("預留: %v", err)
	}
	if err := st.ReleasePort(r.BindIP, r.Protocol, r.HostPort); err != nil {
		t.Fatalf("釋放: %v", err)
	}
	// 釋放後可重新預留。
	if err := st.ReservePort(r); err != nil {
		t.Errorf("釋放後重新預留應成功: %v", err)
	}
	// 冪等:再釋放不存在的鍵不報錯。
	if err := st.ReleasePort("1.2.3.4", "udp", 9); err != nil {
		t.Errorf("釋放不存在鍵應冪等: %v", err)
	}
}

// TestReleasePortsForInstance 驗證依實例釋放所有埠。
func TestReleasePortsForInstance(t *testing.T) {
	st, _ := newTempStore(t)
	_ = st.ReservePort(PortReservation{BindIP: "0.0.0.0", Protocol: "tcp", HostPort: 1, InstanceUUID: "A"})
	_ = st.ReservePort(PortReservation{BindIP: "0.0.0.0", Protocol: "tcp", HostPort: 2, InstanceUUID: "A"})
	_ = st.ReservePort(PortReservation{BindIP: "0.0.0.0", Protocol: "tcp", HostPort: 3, InstanceUUID: "B"})

	if err := st.ReleasePortsForInstance("A"); err != nil {
		t.Fatalf("ReleasePortsForInstance: %v", err)
	}
	list, _ := st.ListPortReservations()
	if len(list) != 1 || list[0].InstanceUUID != "B" {
		t.Errorf("釋放實例 A 後應只剩 B: %+v", list)
	}
}

// TestReservePort_Concurrent 驗證併發預留同一鍵時,恰有一個成功、其餘被擋
// (SetMaxOpenConns(1) 使寫入序列化,唯一約束保證不同時通過,對齊 R2)。
func TestReservePort_Concurrent(t *testing.T) {
	st, _ := newTempStore(t)

	const n = 12
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		success  int
		reserved int
	)
	r := PortReservation{BindIP: "0.0.0.0", Protocol: "udp", HostPort: 30000, InstanceUUID: "shared"}

	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := st.ReservePort(r)
			mu.Lock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrPortReserved):
				reserved++
			default:
				t.Errorf("非預期錯誤: %v", err)
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if success != 1 {
		t.Errorf("恰一個併發預留應成功, got success=%d", success)
	}
	if reserved != n-1 {
		t.Errorf("其餘應全被擋, got reserved=%d 期望 %d", reserved, n-1)
	}
}
