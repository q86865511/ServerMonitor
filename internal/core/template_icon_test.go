package core

import (
	"os"
	"path/filepath"
	"testing"

	"servermonitor/internal/protocol"
)

// newIconEngine 建一個 engine 並白箱注入一個範本(id="g")、其來源目錄 dir 與 icon 欄位,
// 直接針對 IconPath 的路徑安全判定(R14),免寫完整合法 toml。
func newIconEngine(dir, icon string) *TemplateEngine {
	e := NewTemplateEngine(nil, nil)
	e.templates["g"] = &protocol.GameTemplate{ID: "g", Icon: icon}
	e.sources["g"] = dir
	return e
}

// TestTemplateEngine_IconPath 覆蓋 R14 路徑拘束:合法 200、無 icon 404、`../` 逃逸 404、
// 同層 sibling 目錄(templates-secret)404、目標為目錄 404、symlink 指向外部 404、範本不存在 404。
func TestTemplateEngine_IconPath(t *testing.T) {
	base := t.TempDir()
	tplDir := filepath.Join(base, "templates")
	iconsDir := filepath.Join(tplDir, "icons")
	if err := os.MkdirAll(iconsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legal := filepath.Join(iconsDir, "g.png")
	if err := os.WriteFile(legal, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 同層 sibling 目錄(名稱以 "templates" 為前綴,字串前綴法會誤放行)。
	secretDir := filepath.Join(base, "templates-secret")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "leak.png"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 範本目錄外的檔(供 `../` 逃逸與 symlink 目標)。
	outsideFile := filepath.Join(base, "outside.png")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("合法 icon → 可服務", func(t *testing.T) {
		e := newIconEngine(tplDir, "icons/g.png")
		got, ok := e.IconPath("g")
		if !ok {
			t.Fatal("期望可服務,得 false")
		}
		wantResolved, _ := filepath.EvalSymlinks(legal)
		if got != wantResolved {
			t.Fatalf("path = %q,期望 %q", got, wantResolved)
		}
	})
	t.Run("未宣告 icon → 404", func(t *testing.T) {
		if _, ok := newIconEngine(tplDir, "").IconPath("g"); ok {
			t.Fatal("期望 false")
		}
	})
	t.Run("icon 檔不存在 → 404", func(t *testing.T) {
		if _, ok := newIconEngine(tplDir, "icons/missing.png").IconPath("g"); ok {
			t.Fatal("期望 false")
		}
	})
	t.Run("../ 逃逸 → 404", func(t *testing.T) {
		if _, ok := newIconEngine(tplDir, "../outside.png").IconPath("g"); ok {
			t.Fatal("期望 false")
		}
	})
	t.Run("sibling-prefix 目錄(templates-secret)→ 404", func(t *testing.T) {
		if _, ok := newIconEngine(tplDir, "../templates-secret/leak.png").IconPath("g"); ok {
			t.Fatal("期望 false(sibling 目錄不得放行)")
		}
	})
	t.Run("目標為目錄(非一般檔)→ 404", func(t *testing.T) {
		if _, ok := newIconEngine(tplDir, "icons").IconPath("g"); ok {
			t.Fatal("期望 false")
		}
	})
	t.Run("symlink 指向目錄外 → 404", func(t *testing.T) {
		link := filepath.Join(iconsDir, "link.png")
		if err := os.Symlink(outsideFile, link); err != nil {
			t.Skipf("無法建立 symlink(Windows 需開發者模式/管理員權限):%v", err)
		}
		if _, ok := newIconEngine(tplDir, "icons/link.png").IconPath("g"); ok {
			t.Fatal("期望 false(symlink 指向目錄外)")
		}
	})
	t.Run("範本不存在 → 404", func(t *testing.T) {
		if _, ok := NewTemplateEngine(nil, nil).IconPath("nope"); ok {
			t.Fatal("期望 false")
		}
	})
}
