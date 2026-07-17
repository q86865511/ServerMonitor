package main

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// tplIconPrefix 是範本 icon 的 HTTP 路徑前綴(R14):GET /tpl-icons/{id}。
const tplIconPrefix = "/tpl-icons/"

// tplIconHandler 建立範本 icon 的 AssetServer fallback handler(R14)。resolve 以範本 id 解析出
// 可服務的 icon 檔案絕對路徑(見 Runtime.TemplateIconPath / TemplateEngine.IconPath 的路徑安全判定);
// 回 (‑, false) 時本 handler 回 404。resolve 內部負責 rt 未就緒(OnStartup 前為 nil)的判定。
//
// 此 handler 僅在嵌入的前端資產查無該路徑時才被 Wails AssetServer 呼叫(fallback);故對非
// /tpl-icons/ 前綴一律 404(交還前端 SPA 由 AssetServer 自身處理,不會走到這裡的成功分支)。
// 非 GET/HEAD 回 405。命中時讀檔回應並依副檔名設 Content-Type(無法判定時以內容嗅探)。
func tplIconHandler(resolve func(id string) (string, bool)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, tplIconPrefix) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, tplIconPrefix)
		if id == "" {
			http.NotFound(w, r)
			return
		}
		path, ok := resolve(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// resolve 與讀檔之間檔案消失等競態:一律回 404 而非 500(icon 為選配,前端有佔位兜底)。
			http.NotFound(w, r)
			return
		}
		ctype := mime.TypeByExtension(filepath.Ext(path))
		if ctype == "" {
			ctype = http.DetectContentType(data)
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(data)
	})
}
