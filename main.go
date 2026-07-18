package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	appcore "servermonitor/internal/app"
)

//go:embed all:frontend/dist
var assets embed.FS

// builtinTemplates 內嵌內建遊戲範本(templates/*.toml 與 templates/icons/),使單一執行檔
// 自足、不再依賴「執行檔旁 templates/ 目錄」——該法在 wails dev 下無效(執行檔位於暫存目錄,
// 旁邊沒有 templates/),曾導致 GUI 範本頁靜默顯示空清單。
//
//go:embed all:templates
var builtinTemplates embed.FS

func main() {
	// 單一實例:盡早(wails.Run 前)偵測是否已有實例執行。若是→喚醒既有實例視窗後靜默退出,
	// 不進入 GUI——避免第二實例閃現視窗或撞 AppLock 錯誤對話框(縮匣後重點 exe 的常見情境)。
	// 非 Windows 為 no-op 存根,恆回非後啟者(單一實例改由 AppLock 於資料根層把關)。
	singleInst, second := newSingleInstance()
	if second {
		return
	}

	// 在 wails.Run 前設定套件層預設值,供 App.OnStartup 內的 app.Bootstrap(app.Options{})
	// 讀取(該呼叫式所在的 app.go 屬並行任務範圍,本次不改動其呼叫鏈)。
	appcore.DefaultBuiltinFS = builtinTemplates

	// Create an instance of the app structure
	app := NewApp()
	app.singleInst = singleInst

	// Create application with options
	err := wails.Run(&options.App{
		Title:     "servermonitor",
		Width:     1440,
		Height:    900,
		MinWidth:  1280,
		MinHeight: 720,
		AssetServer: &assetserver.Options{
			Assets: assets,
			// Handler 為 fallback:嵌入前端資產查無該路徑時才進入,用於服務範本 icon(R14)。
			// rt 於 OnStartup 才就緒,故經 resolve 閉包於請求時 nil-check(未就緒回 404)。
			Handler: tplIconHandler(func(id string) (string, bool) {
				if app.rt == nil {
					return "", false
				}
				return app.rt.TemplateIconPath(id)
			}),
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		// HideWindowOnClose:關閉視窗時隱藏而非退出(縮到系統匣持續監控)。僅 Windows 為 true
		// (有系統匣可喚回);其他平台為 false,見 tray_*.go 的 hideWindowOnClose 常數。
		HideWindowOnClose: hideWindowOnClose,
		OnStartup:         app.OnStartup,
		OnShutdown:        app.OnShutdown,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
