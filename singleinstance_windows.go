//go:build windows

package main

import (
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// singleInstanceEventName 是單一實例喚醒用的具名事件名(Local 命名空間=每登入工作階段唯一)。
// 此事件不是權威鎖——權威鎖為 AppLock(internal/core/applock.go,作用於資料根);具名事件僅作
// 「後啟實例喚醒既有實例視窗」的跨行程訊號通道。兩者若不一致(如事件殘存但鎖已釋放),以
// AppLock 為準:事件只負責「叫醒」,是否真有實例在跑由 AppLock 裁定。
const singleInstanceEventName = `Local\ServerMonitor.SingleInstance.Wake`

// singleInstance 持有具名事件句柄,協調「第二實例喚醒第一實例視窗」的訊號。
// stopped/done 供 close 與 watch 迴圈交接:先立旗標並 SetEvent 喚醒等待中的 watcher、
// 等其退出後才 CloseHandle——Win32 明載不可關閉仍被 WaitForSingleObject 等待的句柄。
type singleInstance struct {
	handle  windows.Handle
	stopped atomic.Bool
	done    chan struct{}
}

// newSingleInstance 建立/開啟具名事件以偵測是否已有實例執行,回傳 (實例, isSecond)。
// isSecond=true 表示已有實例執行,呼叫端應於進入 GUI 前靜默退出(喚醒訊號已於此送出)。
//
// 採 CreateEvent 的 ERROR_ALREADY_EXISTS 慣用法:同名事件已存在時 CreateEvent 仍回有效句柄但
// 附 ERROR_ALREADY_EXISTS,據此判定自己為後啟者並立即 SetEvent 喚醒既有實例。事件為 auto-reset
// (manualReset=0),一次 SetEvent 只釋放一個等待者。建立失敗(名稱轉換/權限等)一律保守視為
// 第一實例續行,喚醒能力退化但不阻斷啟動——單一實例仍由 AppLock 權威把關。
func newSingleInstance() (*singleInstance, bool) {
	namePtr, err := windows.UTF16PtrFromString(singleInstanceEventName)
	if err != nil {
		return &singleInstance{}, false
	}
	handle, err := windows.CreateEvent(nil, 0, 0, namePtr)
	if handle == 0 {
		return &singleInstance{}, false
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		_ = windows.SetEvent(handle)
		_ = windows.CloseHandle(handle)
		return &singleInstance{}, true
	}
	return &singleInstance{handle: handle}, false
}

// watch 於背景等待喚醒訊號,收到即以 onWake 顯示主視窗。事件為 auto-reset,迴圈重覆等待,
// 直到 close 立下 stopped 旗標並 SetEvent 喚醒本迴圈(據旗標判定為關機訊號,不呼叫 onWake)。
func (s *singleInstance) watch(onWake func()) {
	if s == nil || s.handle == 0 {
		return
	}
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		for {
			ev, err := windows.WaitForSingleObject(s.handle, windows.INFINITE)
			if err != nil || ev != windows.WAIT_OBJECT_0 {
				return
			}
			if s.stopped.Load() {
				return
			}
			onWake()
		}
	}()
}

// wake 主動對具名事件發訊,回傳是否送出成功。供 OnStartup 偵測到 AppLock 衝突時的防禦性後備:
// 喚醒真正的第一實例(與其共用同一具名事件物件)。回傳 false(句柄失效/SetEvent 失敗)時呼叫端
// 應退回原錯誤對話框路徑,不得靜默退出。正常時序下後啟者已於 main 的 newSingleInstance 偵測並
// 提早退出,不會走到此路徑。
func (s *singleInstance) wake() bool {
	if s == nil || s.handle == 0 {
		return false
	}
	return windows.SetEvent(s.handle) == nil
}

// close 收束 watch 迴圈並釋放事件句柄(OnShutdown 呼叫;冪等):立 stopped 旗標→SetEvent 喚醒
// 等待中的 watcher→等其退出(上限 2 秒防呆)→才 CloseHandle,避免關閉仍被等待的句柄(UB)與
// 對 handle 欄位的併發讀寫。
func (s *singleInstance) close() {
	if s == nil || s.handle == 0 {
		return
	}
	s.stopped.Store(true)
	_ = windows.SetEvent(s.handle)
	if s.done != nil {
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
	}
	_ = windows.CloseHandle(s.handle)
	s.handle = 0
}
