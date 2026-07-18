package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestTplIconHandler 覆蓋 R14 handler 行為:有圖 200(含 body 與 Content-Type)、resolve 回 false
// (無圖/範本不存在/rt 未就緒)→ 404、空 id → 404、非 GET/HEAD → 405、HEAD → 200 無 body。
func TestTplIconHandler(t *testing.T) {
	dir := t.TempDir()
	iconFile := filepath.Join(dir, "g.png")
	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x01\x02\x03")
	if err := os.WriteFile(iconFile, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	resolve := func(id string) (string, bool) {
		if id == "g" {
			return iconFile, true
		}
		return "", false
	}
	h := tplIconHandler(resolve)

	t.Run("有圖 → 200 + bytes + Content-Type", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tpl-icons/g", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d,期望 200", rec.Code)
		}
		if rec.Body.Len() != len(pngBytes) {
			t.Fatalf("body len = %d,期望 %d", rec.Body.Len(), len(pngBytes))
		}
		if ct := rec.Header().Get("Content-Type"); ct == "" {
			t.Fatal("缺 Content-Type")
		}
	})
	t.Run("無圖(resolve false)→ 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tpl-icons/none", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d,期望 404", rec.Code)
		}
	})
	t.Run("空 id → 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tpl-icons/", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d,期望 404", rec.Code)
		}
	})
	t.Run("非 GET/HEAD → 405", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tpl-icons/g", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("code = %d,期望 405", rec.Code)
		}
	})
	t.Run("HEAD → 200 無 body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/tpl-icons/g", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d,期望 200", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatal("HEAD 不應有 body")
		}
	})
	t.Run("rt 未就緒(resolve 恆 false)→ 404", func(t *testing.T) {
		nilH := tplIconHandler(func(string) (string, bool) { return "", false })
		rec := httptest.NewRecorder()
		nilH.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tpl-icons/g", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d,期望 404", rec.Code)
		}
	})
}
