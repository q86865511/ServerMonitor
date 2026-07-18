//go:build !windows

package main

import "context"

// hideWindowOnClose:非 Windows 平台無系統匣,關閉視窗維持 Wails 預設(退出),避免視窗被
// 隱藏後無從回復。對齊 tray_windows.go 的同名常數,使 main.go 可跨平台引用。
const hideWindowOnClose = false

// trayController 在非 Windows 平台為 no-op 存根(系統匣為 Windows 專屬功能)。
type trayController struct{}

func newTray() *trayController                                             { return &trayController{} }
func (t *trayController) start(_ context.Context, _ func(), _ func()) {}
func (t *trayController) stop()                                            {}
