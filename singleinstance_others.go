//go:build !windows

package main

// singleInstance 在非 Windows 平台為 no-op 存根:此平台不實作具名事件喚醒通道。單一實例仍由
// AppLock 於資料根層把關(internal/core/applock.go),第二實例會在 Bootstrap 取鎖失敗時退出。
type singleInstance struct{}

func newSingleInstance() (*singleInstance, bool) { return &singleInstance{}, false }
func (s *singleInstance) watch(_ func())         {}
func (s *singleInstance) wake() bool             { return false }
func (s *singleInstance) close()                 {}
