//go:build !windows

package main

import "os"

// secureFilePerm 於 unix 盡力把既有敏感檔(token/私鑰)權限收斂為 0600——os.WriteFile 不會修改
// 既存檔權限,故部署工具預建的 0644 檔需在此主動收斂。失敗僅忽略(非致命,盡力而為)。
func secureFilePerm(path string) {
	if path == "" {
		return
	}
	_ = os.Chmod(path, 0o600)
}
