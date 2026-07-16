//go:build !windows

package agent

import "time"

// agent 重啟收養的非 Windows no-op 存根(native-backend T11)。平台策略：Linux 僅走 docker，native
// 不在非 Windows 執行(native_job_stub.go 同理)。adoptedProcessStatus 一律回「不存活」，使
// adoptExisting 視任何殘留 proc.json 為已死(合成 die，不誤收養);waitForAdoptedExit 不會被呼叫到
// (收養路徑僅在 alive 時起 reapAdopted),回 -1 僅為滿足編譯。
func adoptedProcessStatus(pid int) (alive bool, createdAt time.Time) { return false, time.Time{} }

func waitForAdoptedExit(pid int) int { return -1 }
