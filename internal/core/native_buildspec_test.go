package core

import (
	"context"
	"errors"
	"testing"

	"servermonitor/internal/protocol"
)

// nativeCreateTemplate 同時宣告 [docker]/[native],game 埠用動態(host_port=0,不預留)以便同一
// harness 內多次 Create 不撞埠;供 Create 全路徑的 GOOS 分派驗證。
const nativeCreateTemplate = `
schema_version = 1
id = "mcdual"
name = "MC Dual"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "itzg/minecraft-server:java21"
[[variants]]
id = "fabric"
loader = "fabric"
[variants.env]
TYPE = "FABRIC"
[[ports]]
name = "game"
container = 25565
host_port = 0
bind_ip = "0.0.0.0"
protocol = "tcp"
required = true
[[params]]
key = "VERSION"
type = "string"
default = "1.21.1"
[[params]]
key = "EULA"
type = "bool"
required = true
[native.provision]
kind = "java"
java_major = 21
[native.launch]
command = ["{java}", "-jar", "{server_jar}", "nogui"]
`

// TestCreate_RuntimeDispatchByGOOS 走 Create 全路徑,驗證 GOOS 注入決定預設 runtime,且非 Windows
// 明確請求 native 被擋(不建立)。
func TestCreate_RuntimeDispatchByGOOS(t *testing.T) {
	h := newSvcHarness(t)
	writeTemplateFile(t, h.dir, "mcdual.toml", nativeCreateTemplate)
	if _, err := h.eng.LoadDir(h.dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	base := CreateOptions{TemplateID: "mcdual", Variant: "fabric", Params: map[string]string{"EULA": "true"}}

	// Windows 預設 → native(範本支援)。
	h.svc.goos = "windows"
	if _, err := h.svc.Create(context.Background(), base); err != nil {
		t.Fatalf("Create(windows): %v", err)
	}
	if got := h.backend.spec().Runtime; got != runtimeNative {
		t.Errorf("windows 預設 Runtime = %q,期望 native", got)
	}
	if h.backend.spec().Native == nil {
		t.Errorf("native 路徑應填 Native payload")
	}

	// Linux 預設 → docker。
	h.svc.goos = "linux"
	if _, err := h.svc.Create(context.Background(), base); err != nil {
		t.Fatalf("Create(linux): %v", err)
	}
	if got := h.backend.spec().Runtime; got != runtimeDocker {
		t.Errorf("linux 預設 Runtime = %q,期望 docker", got)
	}
	if h.backend.spec().Native != nil {
		t.Errorf("docker 路徑不應填 Native payload")
	}

	// Linux + 明確 native → 擋下,不建立(埠不預留、backend 不再被呼叫)。
	before := h.backend.createCount
	linuxNative := base
	linuxNative.Runtime = runtimeNative
	_, err := h.svc.Create(context.Background(), linuxNative)
	if !errors.Is(err, ErrNativeRequiresWindows) {
		t.Fatalf("Create(linux, native) err = %v,期望 ErrNativeRequiresWindows", err)
	}
	if h.backend.createCount != before {
		t.Errorf("被擋的建立不應呼叫 backend.Create(count %d→%d)", before, h.backend.createCount)
	}
}

// nativeBuildTemplate 是含 [docker] 與 [native](java)雙能力的 Minecraft-like 範本,用以驗證
// T9 buildSpec 的 native 翻譯路徑與 itzg env gate。[[native.config]] 帶一組非空 map 以驗翻譯。
const nativeBuildTemplate = `
schema_version = 1
id = "mcnative"
name = "MC Native Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "itzg/minecraft-server:java21"
[[variants]]
id = "fabric"
loader = "fabric"
[variants.env]
TYPE = "FABRIC"
[[variants]]
id = "vanilla"
loader = "vanilla"
[variants.env]
TYPE = "VANILLA"
[[ports]]
name = "game"
container = 25565
host_port = 25565
bind_ip = "0.0.0.0"
protocol = "tcp"
required = true
[[params]]
key = "VERSION"
type = "string"
default = "1.21.1"
[[params]]
key = "MEMORY"
type = "string"
default = "2G"
[[params]]
key = "EULA"
type = "bool"
required = true
[mods]
owner = "image-native"
manual_mount = "/modpacks"
modpack_loaders = ["vanilla", "forge", "fabric"]
[native.provision]
kind = "java"
java_major = 21
[native.launch]
command = ["{java}", "-Xmx{memory_mb}M", "-jar", "{server_jar}", "nogui"]
[[native.config]]
file = "server.properties"
format = "properties"
[native.config.map]
MOTD = "motd"
[native.mods]
mods_dir = "mods"
`

func parseNativeBuildTemplate(t *testing.T) *protocol.GameTemplate {
	t.Helper()
	tmpl, err := protocol.ParseTemplate([]byte(nativeBuildTemplate))
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	return tmpl
}

// TestBuildSpec_NativePayload:runtime=native 時 buildSpec 翻譯 [native] 區段為 NativeSpecPayload,
// 逐欄位斷言(含 MCVersion 從 Env 取、EULA 布林、Loader 取變體、Config 翻譯、ModsDir),並填 Resources。
func TestBuildSpec_NativePayload(t *testing.T) {
	tmpl := parseNativeBuildTemplate(t)
	opts := CreateOptions{
		TemplateID: "mcnative",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"}, // VERSION 用 default 1.21.1
		Resources:  &protocol.ResourceLimits{MemoryMB: 4096, CPUPercent: 50},
	}
	variantEnv, verr := resolveVariant(tmpl, opts.Variant)
	if verr != nil {
		t.Fatalf("resolveVariant: %v", verr)
	}
	spec := (&InstanceService{}).buildSpec("u-native", "local", tmpl, opts, variantEnv, runtimeNative, nil)

	if spec.Runtime != runtimeNative {
		t.Fatalf("Runtime = %q,期望 native", spec.Runtime)
	}
	if spec.Native == nil {
		t.Fatal("Native payload 為 nil")
	}
	p := spec.Native.Provision
	if p.Kind != "java" || p.JavaMajor != 21 {
		t.Errorf("Provision kind/java_major = %q/%d,期望 java/21", p.Kind, p.JavaMajor)
	}
	if p.Loader != "fabric" {
		t.Errorf("Provision.Loader = %q,期望 fabric(取自變體)", p.Loader)
	}
	if p.MCVersion != "1.21.1" {
		t.Errorf("Provision.MCVersion = %q,期望 1.21.1(自 Env VERSION default)", p.MCVersion)
	}
	if !p.EULA {
		t.Errorf("Provision.EULA = false,期望 true(自 Env EULA)")
	}
	if p.SteamAppID != "" || p.UpdateOnStart {
		t.Errorf("java 供應不應帶 steam 欄位: %+v", p)
	}
	wantCmd := []string{"{java}", "-Xmx{memory_mb}M", "-jar", "{server_jar}", "nogui"}
	if len(spec.Native.Launch.Command) != len(wantCmd) {
		t.Fatalf("Launch.Command = %v,期望 %v", spec.Native.Launch.Command, wantCmd)
	}
	for i, a := range wantCmd {
		if spec.Native.Launch.Command[i] != a {
			t.Errorf("Launch.Command[%d] = %q,期望 %q", i, spec.Native.Launch.Command[i], a)
		}
	}
	if len(spec.Native.Config) != 1 {
		t.Fatalf("Config 長度 = %d,期望 1", len(spec.Native.Config))
	}
	cm := spec.Native.Config[0]
	if cm.File != "server.properties" || cm.Format != "properties" || cm.Map["MOTD"] != "motd" {
		t.Errorf("Config 翻譯錯誤: %+v", cm)
	}
	if spec.Native.ModsDir != "mods" {
		t.Errorf("ModsDir = %q,期望 mods", spec.Native.ModsDir)
	}
	if spec.Resources == nil || spec.Resources.MemoryMB != 4096 || spec.Resources.CPUPercent != 50 {
		t.Errorf("Resources = %+v,期望 {4096,50}", spec.Resources)
	}
}

// TestBuildSpec_NativeSteamProvision:steamcmd 供應(Palworld-like)翻譯 SteamAppID/UpdateOnStart,
// 且不帶 java 專屬欄位(Loader/MCVersion/EULA)。
func TestBuildSpec_NativeSteamProvision(t *testing.T) {
	const steamTmpl = `
schema_version = 1
id = "palnative"
name = "Pal Native"
runtime = "docker"
data_dirs = ["/pal"]
[docker]
image = "example/pal:1"
[[ports]]
name = "game"
container = 8211
host_port = 8211
bind_ip = "0.0.0.0"
protocol = "udp"
required = true
[native.provision]
kind = "steamcmd"
steam_app_id = "2394010"
update_on_start = true
[native.launch]
command = ["{instance_dir}/PalServer.sh", "-port={port:game}"]
`
	tmpl, err := protocol.ParseTemplate([]byte(steamTmpl))
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	spec := (&InstanceService{}).buildSpec("u-pal", "local", tmpl, CreateOptions{TemplateID: "palnative"}, nil, runtimeNative, nil)
	if spec.Native == nil {
		t.Fatal("Native payload 為 nil")
	}
	p := spec.Native.Provision
	if p.Kind != "steamcmd" || p.SteamAppID != "2394010" || !p.UpdateOnStart {
		t.Errorf("steamcmd 供應翻譯錯誤: %+v", p)
	}
	if p.Loader != "" || p.MCVersion != "" || p.EULA {
		t.Errorf("steamcmd 不應帶 java 欄位: %+v", p)
	}
}

// TestBuildSpec_ItzgEnvGate:模組包 itzg env 映射只在 docker 路徑生效;native 路徑改以 Modpack
// 透傳且不寫 TYPE=MODRINTH/MODRINTH_MODPACK。
func TestBuildSpec_ItzgEnvGate(t *testing.T) {
	tmpl := parseNativeBuildTemplate(t)
	const slug = "cobblemon-fabric"
	opts := CreateOptions{
		TemplateID: "mcnative",
		Variant:    "vanilla",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackModrinth, Ref: slug},
	}
	variantEnv, _ := resolveVariant(tmpl, opts.Variant)

	// docker 路徑:itzg env 生效,Modpack 不透傳(nil),Native nil。
	dockerSpec := (&InstanceService{}).buildSpec("u-d", "local", tmpl, opts, variantEnv, runtimeDocker, nil)
	if dockerSpec.Env["TYPE"] != "MODRINTH" || dockerSpec.Env["MODRINTH_MODPACK"] != slug {
		t.Errorf("docker 路徑 itzg env 未生效: %+v", dockerSpec.Env)
	}
	if dockerSpec.Modpack != nil {
		t.Errorf("docker 路徑不應填 Modpack: %+v", dockerSpec.Modpack)
	}
	if dockerSpec.Native != nil {
		t.Errorf("docker 路徑不應填 Native")
	}

	// native 路徑:不寫 itzg env,改以 Modpack 透傳。
	nativeSpec := (&InstanceService{}).buildSpec("u-n", "local", tmpl, opts, variantEnv, runtimeNative, nil)
	if _, ok := nativeSpec.Env["MODRINTH_MODPACK"]; ok {
		t.Errorf("native 路徑不應寫 itzg MODRINTH_MODPACK: %+v", nativeSpec.Env)
	}
	if nativeSpec.Env["TYPE"] == "MODRINTH" {
		t.Errorf("native 路徑不應把 TYPE 覆寫為 MODRINTH: %+v", nativeSpec.Env)
	}
	if nativeSpec.Modpack == nil || nativeSpec.Modpack.Type != string(ModpackModrinth) || nativeSpec.Modpack.Ref != slug {
		t.Errorf("native 路徑 Modpack 透傳錯誤: %+v", nativeSpec.Modpack)
	}
}

// TestBuildSpec_NativeManualModpackViaMounts:手動上傳模組包在 native 下仍走 Mounts(不進 Modpack)。
func TestBuildSpec_NativeManualModpackViaMounts(t *testing.T) {
	tmpl := parseNativeBuildTemplate(t)
	opts := CreateOptions{
		TemplateID: "mcnative",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackManualMrpack, Ref: "C:/tmp/pack.mrpack"},
	}
	variantEnv, _ := resolveVariant(tmpl, opts.Variant)
	spec := (&InstanceService{}).buildSpec("u-manual", "local", tmpl, opts, variantEnv, runtimeNative, nil)

	if spec.Modpack != nil {
		t.Errorf("手動模組包不應填 Modpack(應走 Mounts): %+v", spec.Modpack)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Name != modpackMountName {
		t.Errorf("手動模組包 Mounts 缺失: %+v", spec.Mounts)
	}
}
