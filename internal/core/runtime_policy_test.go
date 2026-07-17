package core

import (
	"errors"
	"testing"

	"servermonitor/internal/protocol"
)

// tmplCaps 造一個僅具指定執行能力的最小範本(resolveRuntime 只看 SupportsDocker/SupportsNative)。
func tmplCaps(docker, native bool) *protocol.GameTemplate {
	t := &protocol.GameTemplate{}
	if docker {
		t.Docker = &protocol.DockerImage{Image: "example/img:1"}
	}
	if native {
		t.Native = &protocol.NativeSpec{}
	}
	return t
}

func TestResolveRuntime(t *testing.T) {
	const (
		both       = "both"
		dockerOnly = "docker-only"
		nativeOnly = "native-only"
	)
	caps := map[string]*protocol.GameTemplate{
		both:       tmplCaps(true, true),
		dockerOnly: tmplCaps(true, false),
		nativeOnly: tmplCaps(false, true),
	}

	cases := []struct {
		name      string
		tmpl      string
		requested string
		goos      string
		want      string
		wantErr   error // nil=期望成功
	}{
		// 預設(未指定):Windows 且範本支援 native → native。
		{"windows_default_both", both, "", "windows", runtimeNative, nil},
		{"windows_default_dockerOnly", dockerOnly, "", "windows", runtimeDocker, nil},
		{"windows_default_nativeOnly", nativeOnly, "", "windows", runtimeNative, nil},
		// 預設:非 Windows 一律退回 docker(範本支援 docker 時)。
		{"linux_default_both", both, "", "linux", runtimeDocker, nil},
		{"linux_default_dockerOnly", dockerOnly, "", "linux", runtimeDocker, nil},
		// 預設:非 Windows 的 native-only 範本 → 無 docker fallback,平台錯誤。
		{"linux_default_nativeOnly", nativeOnly, "", "linux", "", ErrNativeRequiresWindows},

		// 明確請求 docker:範本支援即通過(平台不限)。
		{"windows_req_docker_both", both, "docker", "windows", runtimeDocker, nil},
		{"linux_req_docker_both", both, "docker", "linux", runtimeDocker, nil},
		{"req_docker_nativeOnly", nativeOnly, "docker", "windows", "", ErrRuntimeUnsupported},

		// 明確請求 native:需範本支援 + Windows。
		{"windows_req_native_both", both, "native", "windows", runtimeNative, nil},
		{"linux_req_native_both", both, "native", "linux", "", ErrNativeRequiresWindows},
		{"windows_req_native_dockerOnly", dockerOnly, "native", "windows", "", ErrRuntimeUnsupported},

		// 未知 runtime 值。
		{"unknown_runtime", both, "podman", "windows", "", ErrUnknownRuntime},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveRuntime(caps[c.tmpl], c.requested, c.goos)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v,期望 errors.Is(%v)", err, c.wantErr)
				}
				if got != "" {
					t.Errorf("錯誤路徑仍回 runtime = %q,期望空", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("非預期錯誤: %v", err)
			}
			if got != c.want {
				t.Errorf("runtime = %q,期望 %q", got, c.want)
			}
		})
	}
}
