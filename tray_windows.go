//go:build windows

package main

import (
	"context"
	_ "embed"
	"runtime"

	"github.com/energye/systray"
)

// hideWindowOnClose 控制「關閉視窗」是否縮到系統匣而非退出程式。僅 Windows 為 true——
// 此平台有系統匣圖示可將視窗喚回;其他平台無匣,關閉視窗即維持預設(退出),避免無法回復。
const hideWindowOnClose = true

// trayIconICO 內嵌系統匣圖示(build/windows/icon.ico,與應用視窗/執行檔同一枚多尺寸 ico)。
//
//go:embed build/windows/icon.ico
var trayIconICO []byte

// trayController 管理系統匣圖示與其生命週期(建立圖示、選單、左鍵行為,並於關閉時移除)。
type trayController struct{}

// newTray 建立系統匣控制器。
func newTray() *trayController { return &trayController{} }

// start 於背景啟動系統匣圖示。onOpen 顯示主視窗(選單「開啟主視窗」與左鍵單擊),
// onQuit 結束整個應用(選單「結束」,經 Wails 既有 OnShutdown 優雅收束後端)。
// systray.Run 會阻塞至 systray.Quit,故置於 goroutine。
func (t *trayController) start(_ context.Context, onOpen, onQuit func()) {
	onReady := func() {
		systray.SetIcon(trayIconICO)
		systray.SetTitle("ServerMonitor")
		systray.SetTooltip("ServerMonitor")

		mOpen := systray.AddMenuItem("開啟主視窗", "顯示 ServerMonitor 主視窗")
		mQuit := systray.AddMenuItem("結束", "結束 ServerMonitor")
		mOpen.Click(onOpen)
		// Quit 會反向觸發 systray.Quit 收束 tray 本身,改非同步派發避免在 click 回調鏈內自我等待。
		mQuit.Click(func() { go onQuit() })

		// 左鍵單擊圖示顯示主視窗;右鍵未設回調→systray 預設展開上述選單。
		systray.SetOnClick(func(systray.IMenu) { onOpen() })
	}
	// onExit 無需額外收尾:視窗與後端由 Wails OnShutdown 收束。
	go func() {
		// systray 的 init() 只鎖住主 goroutine;訊息迴圈(hidden window + GetMessage)跑在本
		// goroutine,必須自行釘住 OS thread,否則被排程遷移後訊息泵失效、匣圖示停止回應。
		runtime.LockOSThread()
		systray.Run(onReady, func() {})
	}()
}

// stop 移除系統匣圖示(OnShutdown 呼叫;冪等)。
func (t *trayController) stop() { systray.Quit() }
