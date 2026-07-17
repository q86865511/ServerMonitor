package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title:  "servermonitor",
		Width:  1024,
		Height: 768,
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
