//go:build tools

// 本檔以空白匯入釘住後續任務將使用、但目前尚未在程式碼中 import 的依賴,
// 使 `go mod tidy` 於 T1 即把版本鎖進 go.mod/go.sum:
//   - modernc.org/sqlite            → T2 持久化(純 Go SQLite,免 CGO)
//   - github.com/zalando/go-keyring → T2 SecretRef 金鑰庫存取
//   - github.com/docker/docker      → T4 DockerBackend(官方 SDK)
//
// 受 `tools` build tag 保護,永不編入應用程式;正常 `go build ./...` 會略過。
// 因 repo 根為 Wails 的 package main,本檔亦宣告 package main(僅在 tools tag 下納入)。
package main

import (
	_ "github.com/docker/docker/client"
	_ "github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"
)
