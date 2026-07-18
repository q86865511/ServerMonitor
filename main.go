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
	// 在 wails.Run 前設定套件層預設值,供 App.OnStartup 內的 app.Bootstrap(app.Options{})
	// 讀取(該呼叫式所在的 app.go 屬並行任務範圍,本次不改動其呼叫鏈)。
	appcore.DefaultBuiltinFS = builtinTemplates

	// Create an instance of the app structure
	app := NewApp()

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
		OnStartup:        app.OnStartup,
		OnShutdown:       app.OnShutdown,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
