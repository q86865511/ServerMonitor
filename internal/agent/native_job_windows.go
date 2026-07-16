//go:build windows

package agent

// Windows Job Objects:對 native 實例行程樹強制記憶體/CPU 上限、提供行程樹 CPU/記憶體統計,並於
// 記憶體配額命中時經 completion port 設旗標供 reap 合成 oom 事件(native-backend R8/R9)。
//
// 設計要點:
//   - 一實例一 Job。Start 起行程後 AssignProcessToJobObject 掛入;之後該行程樹派生的子行程自動
//     納入同一 Job(java 子行程、SteamCMD 更新子行程),使統計與強殺涵蓋整棵樹。
//   - **刻意不設** JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE:agent 行程退出(Job handle 關閉)時不連坐
//     殺掉伺服器行程,與 R7 收養共存(R9 驗收:agent 退出伺服器續活)。
//   - OOM 偵測用 completion port(JOB_OBJECT_MSG_JOB_MEMORY_LIMIT):比「事後猜測退出原因」精確。
//   - 行程記憶體查詢用 psapi.dll GetProcessMemoryInfo(x/sys/windows 未涵蓋,以 LazyDLL 補)。

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// x/sys/windows 未提供的常數(以 winnt.h 為準)。
const (
	// CPU rate control(JOBOBJECT_CPU_RATE_CONTROL_INFORMATION.ControlFlags)。
	jobObjectCPURateControlEnable  = 0x1 // 啟用 CPU rate control
	jobObjectCPURateControlHardCap = 0x4 // 硬上限:不得超過 CpuRate

	// completion port 訊息碼(GetQueuedCompletionStatus 的 qty 參數攜帶)。
	jobObjectMsgProcessMemoryLimit = 9  // 單一行程達 ProcessMemoryLimit
	jobObjectMsgJobMemoryLimit     = 10 // 整個 Job 達 JobMemoryLimit
)

// x/sys/windows 未提供的結構(以 winnt.h / psapi.h 為準;欄位順序即記憶體佈局,勿改)。

// jobBasicAccountingInformation 對應 JOBOBJECT_BASIC_ACCOUNTING_INFORMATION:行程樹累計 CPU 時間
// (100ns 單位)與行程計數。
type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// jobCPURateControlInformation 對應 JOBOBJECT_CPU_RATE_CONTROL_INFORMATION;hard cap 模式下 Value
// 為 CpuRate(占全機 CPU 的萬分比,1..10000)。
type jobCPURateControlInformation struct {
	ControlFlags uint32
	Value        uint32
}

// jobBasicProcessIDList 對應 JOBOBJECT_BASIC_PROCESS_ID_LIST 的表頭;實際 PID 陣列為變長,查詢時
// 配置足量位元組緩衝、以此型別重解頭部再切片。
type jobBasicProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIdList             [1]uintptr
}

// jobAssociateCompletionPort 對應 JOBOBJECT_ASSOCIATE_COMPLETION_PORT。
type jobAssociateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

// processMemoryCounters 對應 PROCESS_MEMORY_COUNTERS(psapi.h);僅取 WorkingSetSize。
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// psapi.dll:x/sys/windows 未綁定 GetProcessMemoryInfo,以 LazyDLL 補(唯一手動綁定的 API)。
var (
	modpsapi                 = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = modpsapi.NewProc("GetProcessMemoryInfo")
)

// jobObject 是一實例的 Windows Job Object 控制點。
type jobObject struct {
	handle windows.Handle
	port   windows.Handle // OOM 偵測用 completion port;0=未建(OOM 偵測降級)
	memHit uint32         // atomic:記憶體配額命中旗標(0/1)

	closeOnce sync.Once
}

// newJobObject 建立 Job Object 並依 limits 設定記憶體/CPU 上限。恆建立(即使不限額,供 accounting)。
// 記憶體上限存在時另建 completion port 以偵測 OOM。任一上限設定失敗即回錯(由 attachJob 降級)。
func newJobObject(limits jobLimits) (*jobObject, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	j := &jobObject{handle: h}

	if limits.MemoryBytes > 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(limits.MemoryBytes)
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			_ = windows.CloseHandle(h)
			return nil, err
		}
		// 記憶體受限才需 OOM 偵測:建 completion port 並關聯(best-effort,失敗僅降級 OOM 偵測)。
		j.setupCompletionPort()
	}

	if limits.CPUPercent > 0 {
		rate := uint32(limits.CPUPercent) * 100 // 百分比→萬分比(占全機 CPU)
		if rate < 1 {
			rate = 1
		}
		if rate > 10000 {
			rate = 10000
		}
		cpu := jobCPURateControlInformation{
			ControlFlags: jobObjectCPURateControlEnable | jobObjectCPURateControlHardCap,
			Value:        rate,
		}
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectCpuRateControlInformation,
			uintptr(unsafe.Pointer(&cpu)), uint32(unsafe.Sizeof(cpu))); err != nil {
			_ = j.close()
			return nil, err
		}
	}

	return j, nil
}

// setupCompletionPort 建立 completion port、關聯 Job 並啟動監看 goroutine;失敗則靜默降級
// (port 留 0、無 OOM 事件,不影響上限強制與統計)。
func (j *jobObject) setupCompletionPort() {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return
	}
	assoc := jobAssociateCompletionPort{
		CompletionKey:  uintptr(j.handle),
		CompletionPort: port,
	}
	if _, err := windows.SetInformationJobObject(j.handle, windows.JobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc))); err != nil {
		_ = windows.CloseHandle(port)
		return
	}
	j.port = port
	go j.watchCompletionPort()
}

// watchCompletionPort 迴圈取 Job 通知,記憶體配額命中時設旗標。port 關閉(close)時 GetQueued 回錯,
// goroutine 退出。
func (j *jobObject) watchCompletionPort() {
	for {
		var code uint32
		var key uintptr
		var ov *windows.Overlapped
		if err := windows.GetQueuedCompletionStatus(j.port, &code, &key, &ov, windows.INFINITE); err != nil {
			return // port 已關閉或出錯:結束監看
		}
		switch code {
		case jobObjectMsgJobMemoryLimit, jobObjectMsgProcessMemoryLimit:
			atomic.StoreUint32(&j.memHit, 1)
		}
	}
}

// assign 把行程 pid 掛入本 Job。行程已在他人 Job 內(收養重掛可能遇到)時回可辨識錯誤供降級。
func (j *jobObject) assign(pid int) error {
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ph)
	return windows.AssignProcessToJobObject(j.handle, ph)
}

// stats 取行程樹累計 CPU 時間與 WorkingSetSize 加總(R8)。
func (j *jobObject) stats() (jobStats, error) {
	var acct jobBasicAccountingInformation
	if err := windows.QueryInformationJobObject(j.handle, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil); err != nil {
		return jobStats{}, err
	}
	// 100ns 單位 → time.Duration(ns):×100。
	cpu := time.Duration(acct.TotalUserTime+acct.TotalKernelTime) * 100
	ws, err := j.workingSetSum()
	if err != nil {
		return jobStats{}, err
	}
	return jobStats{CPUTime: cpu, WorkingSetSum: ws}, nil
}

// workingSetSum 列舉 Job 內所有行程並加總 WorkingSetSize(bytes)。已退出的行程(OpenProcess 失敗)
// 略過,不視為錯誤。
func (j *jobObject) workingSetSum() (uint64, error) {
	n := 128
	for attempt := 0; attempt < 6; attempt++ {
		size := int(unsafe.Sizeof(uint32(0)))*2 + n*int(unsafe.Sizeof(uintptr(0)))
		buf := make([]byte, size)
		err := windows.QueryInformationJobObject(j.handle, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buf[0])), uint32(size), nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return 0, err
		}
		hdr := (*jobBasicProcessIDList)(unsafe.Pointer(&buf[0]))
		if int(hdr.NumberOfAssignedProcesses) > n {
			n = int(hdr.NumberOfAssignedProcesses) + 16 // 緩衝不足:擴充重查
			continue
		}
		var total uint64
		pids := unsafe.Slice(&hdr.ProcessIdList[0], hdr.NumberOfProcessIdsInList)
		for _, pid := range pids {
			total += processWorkingSet(uint32(pid))
		}
		return total, nil
	}
	return 0, errors.New("job: 行程清單反覆超過緩衝")
}

// processWorkingSet 回 pid 的 WorkingSetSize(bytes);開啟或查詢失敗(行程已退出/無權限)回 0。
func processWorkingSet(pid uint32) uint64 {
	if pid == 0 {
		return 0
	}
	ph, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(ph)
	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	r1, _, _ := procGetProcessMemoryInfo.Call(uintptr(ph), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB))
	if r1 == 0 {
		return 0
	}
	return uint64(pmc.WorkingSetSize)
}

// terminate 終止整個 Job(行程樹一次收束,取代 taskkill /T)。
func (j *jobObject) terminate() error {
	return windows.TerminateJobObject(j.handle, 1)
}

// memoryLimitHit 回報是否曾觀測到記憶體配額命中(供 reap 合成 oom)。
func (j *jobObject) memoryLimitHit() bool {
	return atomic.LoadUint32(&j.memHit) == 1
}

// awaitMemoryLimit 在 timeout 內輪詢記憶體配額命中旗標(completion port 通知本質非同步,行程可能
// 早於通知投遞而退出);已命中即刻返回 true,逾時未命中回 false。
func (j *jobObject) awaitMemoryLimit(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if atomic.LoadUint32(&j.memHit) == 1 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// close 釋放 Job 與 completion port 控制代碼(冪等)。因未設 KILL_ON_JOB_CLOSE,關閉不影響行程存活。
func (j *jobObject) close() error {
	j.closeOnce.Do(func() {
		if j.port != 0 {
			_ = windows.CloseHandle(j.port) // 關 port 喚醒並結束 watchCompletionPort
		}
		if j.handle != 0 {
			_ = windows.CloseHandle(j.handle)
		}
	})
	return nil
}
