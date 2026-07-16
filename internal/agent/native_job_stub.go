//go:build !windows

package agent

import "time"

// jobObject 是非 Windows 平台的 Job Object no-op 存根(native-backend T7)。native 執行後端的
// 資源上限與行程樹統計依賴 Windows Job Objects,平台策略亦禁 Linux 走 native(design:Linux 僅
// docker);此存根只為讓 internal/agent 於非 Windows 仍可編譯並跑跨平台單元測。
//
// 關鍵:newJobObject 在此回 (nil, nil),使 NativeBackend.attachJob 走「無 Job」路徑
// (procHandle.job=nil)——Stats 留零值、Stop 以 killProcessTree fallback。以下方法僅為滿足
// *jobObject 的方法集使 native.go 編譯;因 job 恆為 nil 且呼叫端皆以 h.job != nil 前置守衛,
// 實際不會被呼叫到。
type jobObject struct{}

// newJobObject 非 Windows 不建立 Job(回 nil),不視為錯誤。
func newJobObject(limits jobLimits) (*jobObject, error) { return nil, nil }

func (j *jobObject) assign(pid int) error                        { return nil }
func (j *jobObject) stats() (jobStats, error)                    { return jobStats{}, nil }
func (j *jobObject) terminate() error                            { return nil }
func (j *jobObject) close() error                                { return nil }
func (j *jobObject) memoryLimitHit() bool                        { return false }
func (j *jobObject) awaitMemoryLimit(timeout time.Duration) bool { return false }
