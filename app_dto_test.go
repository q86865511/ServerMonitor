package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"servermonitor/internal/agent"
	"servermonitor/internal/app"
	"servermonitor/internal/core"
	"servermonitor/internal/protocol"
)

// tmplCaps 造一個僅具指定執行能力的最小範本(toTemplateDTO/defaultRuntime 只看
// SupportsDocker/SupportsNative 與 Runtime 欄位)。
func tmplCaps(docker, native bool, runtime string) *protocol.GameTemplate {
	t := &protocol.GameTemplate{ID: "g", Name: "Game", Runtime: runtime}
	if docker {
		t.Docker = &protocol.DockerImage{Image: "img:1"}
	}
	if native {
		t.Native = &protocol.NativeSpec{}
	}
	return t
}

// TestTemplateRuntimes 驗證能力清單由 [docker]/[native] 區段存在推導,順序固定 docker、native,
// 且 native 僅在 Windows 列出(#13:非 Windows 過濾掉無法建立的 native 選項)。以 goos 參數注入,
// 使測試不隨執行平台(Windows 開發 vs Linux CI)漂移。
func TestTemplateRuntimes(t *testing.T) {
	cases := []struct {
		name           string
		docker, native bool
		goos           string
		want           []string
	}{
		{"windows-docker-only", true, false, "windows", []string{"docker"}},
		{"windows-native-only", false, true, "windows", []string{"native"}},
		{"windows-both", true, true, "windows", []string{"docker", "native"}},
		{"linux-both-hides-native", true, true, "linux", []string{"docker"}},
		{"linux-native-only-empty", false, true, "linux", nil},
		{"linux-docker-only", true, false, "linux", []string{"docker"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := templateRuntimes(tmplCaps(c.docker, c.native, "docker"), c.goos)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("templateRuntimes = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDefaultRuntime 驗證預選 runtime 對齊 core.resolveRuntime 的預設分派。
func TestDefaultRuntime(t *testing.T) {
	cases := []struct {
		name           string
		docker, native bool
		goos           string
		want           string
	}{
		{"windows-both-prefers-native", true, true, "windows", "native"},
		{"windows-docker-only", true, false, "windows", "docker"},
		{"windows-native-only", false, true, "windows", "native"},
		{"linux-both-prefers-docker", true, true, "linux", "docker"},
		{"linux-native-only-edge", false, true, "linux", "native"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := defaultRuntime(tmplCaps(c.docker, c.native, "docker"), c.goos)
			if got != c.want {
				t.Fatalf("defaultRuntime = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRuntimeFromID 驗證實例 runtime 標記由 RuntimeID 前綴推導(native: → native;裸 ID → docker)。
func TestRuntimeFromID(t *testing.T) {
	cases := []struct {
		id   protocol.RuntimeID
		want string
	}{
		{"native:abc-123", "native"},
		{"ctr-deadbeef", "docker"},
		{"", "docker"},
	}
	for _, c := range cases {
		if got := runtimeFromID(c.id); got != c.want {
			t.Fatalf("runtimeFromID(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// memKeyring 是 Keyring 的記憶體替身(免污染真實 OS 金鑰庫),Get 查無回 keyring.ErrNotFound。
type memKeyring struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemKeyring() *memKeyring { return &memKeyring{m: make(map[string]string)} }

func (k *memKeyring) key(s, u string) string { return s + "\x00" + u }
func (k *memKeyring) Set(s, u, p string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[k.key(s, u)] = p
	return nil
}
func (k *memKeyring) Get(s, u string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if v, ok := k.m[k.key(s, u)]; ok {
		return v, nil
	}
	return "", keyring.ErrNotFound
}
func (k *memKeyring) Delete(s, u string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	kk := k.key(s, u)
	if _, ok := k.m[kk]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.m, kk)
	return nil
}

// TestCurseForgeKeyBindings 驗證 GUI 綁定 SetCurseForgeAPIKey/CurseForgeKeyConfigured/CurseForgeEnabled
// 的端到端行為(native-backend R14):經真 Runtime(注入記憶體金鑰庫 + dispatch(native 真 adapter))。
// 設定金鑰 → 持久化(Configured 反映)且熱生效(Enabled 翻真,免重啟);清除 → 兩者翻回。
func TestCurseForgeKeyBindings(t *testing.T) {
	dataRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataRoot, "templates"), 0o755); err != nil {
		t.Fatalf("建立範本目錄: %v", err)
	}
	// dispatch 頂層:native 用真 provisionAdapter(無內嵌 key → 初始停用),docker=nil。
	factory := func(opts agent.BackendOptions) (agent.RuntimeBackend, error) {
		native, err := agent.NewNativeBackend(agent.NativeOptions{
			DataRoot:   filepath.Join(dataRoot, "instances"),
			BackupRoot: filepath.Join(dataRoot, "backups"),
			CacheRoot:  filepath.Join(dataRoot, "cache"),
			Node:       opts.Node,
			Prov:       agent.NewProvisionAdapter(opts.CacheRoot),
		})
		if err != nil {
			return nil, err
		}
		return agent.NewDispatchBackend(native, nil)
	}
	rt, err := app.Bootstrap(app.Options{
		DataRoot:           dataRoot,
		BuiltinTemplateDir: t.TempDir(),
		BackendFactory:     factory,
		Secrets:            core.NewSecretStoreWithKeyring("test", newMemKeyring()),
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer rt.Shutdown()

	a := &App{rt: rt}

	// 初始:未設定覆蓋、未啟用。
	if a.CurseForgeKeyConfigured() {
		t.Fatal("初始不應有使用者覆蓋金鑰")
	}
	if a.CurseForgeEnabled() {
		t.Fatal("初始無內嵌/覆蓋金鑰應停用")
	}

	// 設定:持久化 + 熱生效。
	if err := a.SetCurseForgeAPIKey("user-fake-key"); err != nil {
		t.Fatalf("SetCurseForgeAPIKey: %v", err)
	}
	if !a.CurseForgeKeyConfigured() {
		t.Fatal("設定後 CurseForgeKeyConfigured 應為 true")
	}
	if !a.CurseForgeEnabled() {
		t.Fatal("設定後 CurseForgeEnabled 應熱生效為 true")
	}

	// 清除:兩者翻回。
	if err := a.SetCurseForgeAPIKey(""); err != nil {
		t.Fatalf("清除 SetCurseForgeAPIKey: %v", err)
	}
	if a.CurseForgeKeyConfigured() {
		t.Fatal("清除後 CurseForgeKeyConfigured 應為 false")
	}
	if a.CurseForgeEnabled() {
		t.Fatal("清除後 CurseForgeEnabled 應翻回 false")
	}
}

// TestToCreateOptions 驗證建立請求轉填 core.CreateOptions:runtime 透傳、資源上限僅在非零時填入。
func TestToCreateOptions(t *testing.T) {
	t.Run("runtime-and-resources", func(t *testing.T) {
		req := CreateInstanceRequest{
			TemplateID: "mc", Variant: "paper", Node: "local",
			Runtime: "native", MemoryMB: 4096, CPUPercent: 50,
		}
		opts := req.toCreateOptions()
		if opts.Runtime != "native" {
			t.Fatalf("Runtime = %q, want native", opts.Runtime)
		}
		if opts.Resources == nil || opts.Resources.MemoryMB != 4096 || opts.Resources.CPUPercent != 50 {
			t.Fatalf("Resources = %+v, want {4096,50}", opts.Resources)
		}
	})
	t.Run("zero-resources-nil", func(t *testing.T) {
		opts := CreateInstanceRequest{TemplateID: "mc", Runtime: "docker"}.toCreateOptions()
		if opts.Resources != nil {
			t.Fatalf("Resources = %+v, want nil(兩者皆 0)", opts.Resources)
		}
		if opts.Runtime != "docker" {
			t.Fatalf("Runtime = %q, want docker", opts.Runtime)
		}
	})
	t.Run("partial-resources-cpu-only", func(t *testing.T) {
		opts := CreateInstanceRequest{TemplateID: "mc", CPUPercent: 25}.toCreateOptions()
		if opts.Resources == nil || opts.Resources.CPUPercent != 25 || opts.Resources.MemoryMB != 0 {
			t.Fatalf("Resources = %+v, want {0,25}", opts.Resources)
		}
	})
	t.Run("modpack-transfer", func(t *testing.T) {
		req := CreateInstanceRequest{TemplateID: "mc", Modpack: &ModpackRequest{Type: "modrinth", Ref: "fabulously-optimized"}}
		opts := req.toCreateOptions()
		if opts.Modpack == nil || string(opts.Modpack.Type) != "modrinth" || opts.Modpack.Ref != "fabulously-optimized" {
			t.Fatalf("Modpack = %+v, want modrinth/fabulously-optimized", opts.Modpack)
		}
	})
}
