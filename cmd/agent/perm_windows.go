//go:build windows

package main

// secureFilePerm 在 Windows 為 no-op:Windows 不套用 unix 檔案模式位元,收斂 ACL 需另循 icacls/
// SetNamedSecurityInfo,超出本代理範圍。Windows 部署請以 NTFS 權限或專用服務帳號保護資料目錄
// (token 與私鑰檔),詳見部署文件。
func secureFilePerm(_ string) {}
