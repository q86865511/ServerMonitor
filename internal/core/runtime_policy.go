package core

import (
	"errors"
	"fmt"

	"servermonitor/internal/protocol"
)

// runtime 分派鏈的 core 側落地(native-backend R2)。resolveRuntime 由「範本能力 ∩ 平台」
// 決定每實例的執行後端,寫入 InstanceSpec.Runtime 供 agent dispatcher 路由(見
// internal/agent/dispatch.go)。Windows 上範本支援 native 時預設 native;其餘平台強制 docker。

// runtime 後端識別值(對齊 agent dispatcher 接受的 spec.Runtime 值)。
const (
	runtimeDocker = "docker"
	runtimeNative = "native"
)

// runtime 分派相關錯誤哨符(R2)。以 errors.Is 判別;皆為「建立前阻擋、無副作用」。
var (
	// ErrUnknownRuntime 表示請求的 runtime 非已知後端值(docker/native 以外)。
	ErrUnknownRuntime = errors.New("core: 未知 runtime")
	// ErrRuntimeUnsupported 表示範本未宣告所選 runtime 的對應區段([docker]/[native])。
	ErrRuntimeUnsupported = errors.New("core: 範本不支援所選 runtime")
	// ErrNativeRequiresWindows 表示在非 Windows 平台請求 native(Linux 僅支援 Docker)。
	ErrNativeRequiresWindows = errors.New("core: native runtime 僅 Windows 支援,Linux 僅支援 Docker")
)

// knownRuntime 回報 r 是否為已知後端值(docker/native)。
func knownRuntime(r string) bool {
	return r == runtimeDocker || r == runtimeNative
}

// resolveRuntime 決定一個實例的執行後端(native-backend R2)。goos 以參數注入(單元測可注入
// 任意平台);生產路徑由 InstanceService 傳入 runtime.GOOS。
//
//   - requested 非空:驗證為已知值、範本宣告對應區段、平台允許(native 僅 Windows);任一不符回錯。
//   - requested 空(預設):Windows 且範本支援 native → native;否則範本支援 docker → docker;
//     皆不成立時(如非 Windows 的 native-only 範本)回明確錯誤(範本驗證保證至少宣告其一)。
func resolveRuntime(tmpl *protocol.GameTemplate, requested, goos string) (string, error) {
	if requested != "" {
		if !knownRuntime(requested) {
			return "", fmt.Errorf("%w: %q", ErrUnknownRuntime, requested)
		}
		switch requested {
		case runtimeNative:
			if !tmpl.SupportsNative() {
				return "", fmt.Errorf("%w: 範本未宣告 [native] 區段", ErrRuntimeUnsupported)
			}
			if goos != "windows" {
				return "", fmt.Errorf("%w(平台 %s)", ErrNativeRequiresWindows, goos)
			}
		case runtimeDocker:
			if !tmpl.SupportsDocker() {
				return "", fmt.Errorf("%w: 範本未宣告 [docker] 區段", ErrRuntimeUnsupported)
			}
		}
		return requested, nil
	}

	// 預設(未指定):Windows 優先 native,否則退回 docker。
	if goos == "windows" && tmpl.SupportsNative() {
		return runtimeNative, nil
	}
	if tmpl.SupportsDocker() {
		return runtimeDocker, nil
	}
	// 只剩「非 Windows 的 native-only 範本」:native 不可用且無 docker fallback。
	if tmpl.SupportsNative() {
		return "", fmt.Errorf("%w(平台 %s;範本僅宣告 [native])", ErrNativeRequiresWindows, goos)
	}
	// 範本驗證保證至少宣告 [docker]/[native] 其一,此分支理論上不可達。
	return "", fmt.Errorf("%w: 範本未宣告任何執行區段", ErrRuntimeUnsupported)
}
