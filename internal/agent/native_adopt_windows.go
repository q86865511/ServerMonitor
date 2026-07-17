//go:build windows

package agent

// agent 重啟收養的 Windows 平台實作(native-backend T11 / R7)：以 OS 行程控制代碼判 PID 存活、
// 取行程建立時間(防 PID 重用)、並阻塞等待收養行程結束取退出碼。非 Windows 由 native_adopt_stub.go
// 提供 no-op(平台策略：Linux 僅 docker，native 不執行收養)。

import (
	"time"

	"golang.org/x/sys/windows"
)

// adoptedProcessStatus 回報 pid 是否仍執行，並取其 OS 行程建立時間(供 start-time 防 PID 重用比對)。
// 行程已退出/無法開啟時回 (false, zero)；存活但取不到建立時間時回 (true, zero)，由呼叫端寬鬆處理。
//
// #7:存活判定改用 WaitForSingleObject(h, 0),不再以 GetExitCodeProcess==STILL_ACTIVE(259)判定——後者
// 會把「正常退出碼恰為 259」的行程誤判為仍存活。WaitForSingleObject 以行程控制代碼的 signaled 狀態為準:
// WAIT_TIMEOUT=未 signaled(仍執行)、WAIT_OBJECT_0=已 signaled(已結束),不受退出碼數值干擾。
func adoptedProcessStatus(pid int) (alive bool, createdAt time.Time) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false, time.Time{}
	}
	defer windows.CloseHandle(h)

	event, werr := windows.WaitForSingleObject(h, 0)
	if werr != nil {
		return false, time.Time{} // WAIT_FAILED:無法判定,保守視為不可收養。
	}
	if event != uint32(windows.WAIT_TIMEOUT) {
		return false, time.Time{} // WAIT_OBJECT_0(已 signaled=已結束)等:非存活。
	}

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return true, time.Time{} // 存活但無建立時間：交由 startTimeMatches 寬鬆採信。
	}
	return true, time.Unix(0, creation.Nanoseconds()).UTC()
}

// waitForAdoptedExit 阻塞等待收養行程 pid 結束並回傳其退出碼。無法開啟(可能已退出)時視為已結束
// (回 -1)。收養無 exec.Cmd，故以行程控制代碼 WaitForSingleObject 取代 cmd.Wait()。
func waitForAdoptedExit(pid int) int {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return -1
	}
	defer windows.CloseHandle(h)

	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		return -1
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return -1
	}
	return int(int32(code))
}
