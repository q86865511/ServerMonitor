package core

import (
	"strings"
	"testing"

	"servermonitor/internal/protocol"
)

const validMinimalTemplate = `
schema_version = 1
id = "testgame"
name = "Test Game"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "example/testgame:1.0"
[[ports]]
name = "game"
container = 7777
host_port = 7777
bind_ip = "0.0.0.0"
protocol = "udp"
required = true
[[command_protocols]]
protocol_id = "tg-rcon"
kind = "rcon"
host_port_ref = "game"
[health]
kind = "tcp"
port_ref = "game"
`

// validNativeTemplate 是最小合法 native 範本(native-backend R3):runtime=native + [native] 區段。
const validNativeTemplate = `
schema_version = 1
id = "nativegame"
name = "Native Game"
runtime = "native"
data_dirs = ["/data"]
[native.provision]
kind = "java"
java_major = 21
[native.launch]
command = ["{java}", "-jar", "{server_jar}", "nogui"]
[[native.config]]
file = "server.properties"
format = "properties"
map = {}
`

// TestTemplateEngine_LoadValid 驗證合法範本載入成功並可查詢(R1)。
func TestTemplateEngine_LoadValid(t *testing.T) {
	log, _ := newTestEventLog(t)
	eng := NewTemplateEngine(nil, log)
	dir := t.TempDir()
	writeTemplateFile(t, dir, "testgame.toml", validMinimalTemplate)

	n, err := eng.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if n != 1 {
		t.Fatalf("載入份數 = %d, 期望 1", n)
	}
	tmpl, ok := eng.Get("testgame")
	if !ok || tmpl.Name != "Test Game" {
		t.Fatalf("Get(testgame) = %v, %v", tmpl, ok)
	}
	if len(eng.List()) != 1 {
		t.Fatalf("List 長度 = %d, 期望 1", len(eng.List()))
	}
	if len(queryEvents(t, log, protocol.EventTemplateLoadFailed)) != 0 {
		t.Fatalf("合法範本不應記 TEMPLATE_LOAD_FAILED")
	}
}

// TestTemplateEngine_RejectInvalid 驗證各類無效範本被拒載並記 TEMPLATE_LOAD_FAILED(含欄位),
// 且不影響同目錄的合法範本(R1)。
func TestTemplateEngine_RejectInvalid(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantField string
	}{
		{
			"missing_schema_version",
			strings.Replace(validMinimalTemplate, "schema_version = 1", "", 1),
			"schema_version",
		},
		{
			"unsupported_schema_version",
			strings.Replace(validMinimalTemplate, "schema_version = 1", "schema_version = 99", 1),
			"schema_version",
		},
		{
			"missing_id",
			strings.Replace(validMinimalTemplate, `id = "testgame"`, "", 1),
			"id",
		},
		{
			"unknown_runtime",
			strings.Replace(validMinimalTemplate, `runtime = "docker"`, `runtime = "podman"`, 1),
			"runtime",
		},
		{
			"unknown_command_protocol",
			strings.Replace(validMinimalTemplate, `kind = "rcon"`, `kind = "graphql"`, 1),
			"command_protocols[0].kind",
		},
		{
			"unknown_health_kind",
			strings.Replace(validMinimalTemplate, `kind = "tcp"`, `kind = "ping"`, 1),
			"health.kind",
		},
		{
			"missing_docker_image",
			strings.Replace(validMinimalTemplate, `image = "example/testgame:1.0"`, "", 1),
			"docker.image",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, _ := newTestEventLog(t)
			eng := NewTemplateEngine(nil, log)
			dir := t.TempDir()
			// 無效範本(id 換名避免 duplicate 誤判)+ 一份合法範本。
			writeTemplateFile(t, dir, "bad.toml", strings.Replace(tc.content, `id = "testgame"`, `id = "badgame"`, 1))
			writeTemplateFile(t, dir, "good.toml", validMinimalTemplate)

			n, err := eng.LoadDir(dir)
			if err != nil {
				t.Fatalf("LoadDir: %v", err)
			}
			if n != 1 {
				t.Fatalf("載入份數 = %d, 期望 1(僅合法範本)", n)
			}
			if _, ok := eng.Get("testgame"); !ok {
				t.Fatalf("合法範本 testgame 應不受無效範本影響而載入")
			}

			evs := queryEvents(t, log, protocol.EventTemplateLoadFailed)
			if len(evs) != 1 {
				t.Fatalf("TEMPLATE_LOAD_FAILED 事件數 = %d, 期望 1", len(evs))
			}
			if !strings.Contains(string(evs[0].DetailsJSON), `"field":"`+tc.wantField+`"`) {
				t.Fatalf("事件 field 不符,期望含 %q,得 %s", tc.wantField, evs[0].DetailsJSON)
			}
		})
	}
}

// TestTemplateEngine_DuplicateID 驗證 template ID 重複時,後者拒載並記事件,前者保留(R1)。
func TestTemplateEngine_DuplicateID(t *testing.T) {
	log, _ := newTestEventLog(t)
	eng := NewTemplateEngine(nil, log)
	dir := t.TempDir()
	// 兩份同 id、不同 name;依檔名排序 a.toml 先載、b.toml 判重複。
	writeTemplateFile(t, dir, "a.toml", validMinimalTemplate)
	writeTemplateFile(t, dir, "b.toml", strings.Replace(validMinimalTemplate, `name = "Test Game"`, `name = "Dup"`, 1))

	n, err := eng.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if n != 1 {
		t.Fatalf("載入份數 = %d, 期望 1", n)
	}
	tmpl, _ := eng.Get("testgame")
	if tmpl.Name != "Test Game" {
		t.Fatalf("重複 id 應保留先載者,得 name=%q", tmpl.Name)
	}
	evs := queryEvents(t, log, protocol.EventTemplateLoadFailed)
	if len(evs) != 1 || !strings.Contains(string(evs[0].DetailsJSON), `"field":"id"`) {
		t.Fatalf("重複 id 應記一筆 field=id 事件,得 %+v", evs)
	}
}

// TestTemplateEngine_BuiltinTemplates 驗證內建 minecraft + palworld 範本載入成功(R1)。
func TestTemplateEngine_BuiltinTemplates(t *testing.T) {
	log, _ := newTestEventLog(t)
	eng := NewTemplateEngine(nil, log)

	n, err := eng.LoadDir(builtinTemplatesDir)
	if err != nil {
		t.Fatalf("LoadDir(%s): %v", builtinTemplatesDir, err)
	}
	if failed := queryEvents(t, log, protocol.EventTemplateLoadFailed); len(failed) != 0 {
		t.Fatalf("內建範本不應有載入失敗: %+v", failed)
	}
	if n < 2 {
		t.Fatalf("內建範本載入份數 = %d, 期望 ≥2", n)
	}
	mc, ok := eng.Get("minecraft")
	if !ok {
		t.Fatal("minecraft 範本未載入")
	}
	// health 用 rcon(非 tcp):Docker 發布埠的 userland proxy 會讓 tcp 探針假陽性提早判就緒;
	// rcon 走 auth+空指令,伺服器完全起來才健康(T16 雙審 #1)。
	if mc.Health == nil || mc.Health.Kind != "rcon" {
		t.Errorf("minecraft health 應為 rcon,得 %+v", mc.Health)
	}
	pw, ok := eng.Get("palworld")
	if !ok {
		t.Fatal("palworld 範本未載入")
	}
	// palworld:REST 具名動作 + RCON legacy(spike)。
	if len(pw.CommandProtocols) != 2 {
		t.Fatalf("palworld command_protocols 數 = %d, 期望 2(rest+rcon)", len(pw.CommandProtocols))
	}
	var rest, rcon *protocol.CommandProtocol
	for i := range pw.CommandProtocols {
		switch pw.CommandProtocols[i].Kind {
		case "rest":
			rest = &pw.CommandProtocols[i]
		case "rcon":
			rcon = &pw.CommandProtocols[i]
		}
	}
	if rest == nil || len(rest.Actions) < 8 {
		t.Fatalf("palworld REST 具名動作不足: %+v", rest)
	}
	if rcon == nil || !rcon.Legacy {
		t.Fatalf("palworld RCON 應標 legacy: %+v", rcon)
	}
	if !strings.Contains(pw.Docker.Image, "2.5.1") {
		t.Errorf("palworld 映像應鎖 spike 指定 tag 2.5.1,得 %q", pw.Docker.Image)
	}
}

// TestTemplateEngine_NativeValidation 驗證 [native] 區段的載入與各必填缺漏的欄位路徑
// (native-backend R3)。合法 native 範本載入成功;各無效態被拒載並記精確 field。
func TestTemplateEngine_NativeValidation(t *testing.T) {
	const nativeMissingSection = `
schema_version = 1
id = "x"
name = "X"
runtime = "native"
data_dirs = ["/data"]
[docker]
image = "x:1.0"
`
	const noRuntimeSection = `
schema_version = 1
id = "x"
name = "X"
runtime = "docker"
data_dirs = ["/data"]
`
	const nativeMissingKind = `
schema_version = 1
id = "x"
name = "X"
runtime = "native"
data_dirs = ["/data"]
[native.provision]
java_major = 21
[native.launch]
command = ["a"]
`
	const nativeJavaNoMajor = `
schema_version = 1
id = "x"
name = "X"
runtime = "native"
data_dirs = ["/data"]
[native.provision]
kind = "java"
[native.launch]
command = ["a"]
`
	const nativeSteamNoAppID = `
schema_version = 1
id = "x"
name = "X"
runtime = "native"
data_dirs = ["/data"]
[native.provision]
kind = "steamcmd"
[native.launch]
command = ["a"]
`
	const nativeNoCommand = `
schema_version = 1
id = "x"
name = "X"
runtime = "native"
data_dirs = ["/data"]
[native.provision]
kind = "java"
java_major = 21
[native.launch]
working_dir = "."
`
	const nativeBadConfigFormat = `
schema_version = 1
id = "x"
name = "X"
runtime = "native"
data_dirs = ["/data"]
[native.provision]
kind = "java"
java_major = 21
[native.launch]
command = ["a"]
[[native.config]]
file = "f"
format = "yaml"
map = {}
`

	// 合法 native 範本:應載入成功、不記事件。
	t.Run("valid_native", func(t *testing.T) {
		log, _ := newTestEventLog(t)
		eng := NewTemplateEngine(nil, log)
		dir := t.TempDir()
		writeTemplateFile(t, dir, "native.toml", validNativeTemplate)
		n, err := eng.LoadDir(dir)
		if err != nil || n != 1 {
			t.Fatalf("LoadDir: n=%d err=%v", n, err)
		}
		tmpl, ok := eng.Get("nativegame")
		if !ok {
			t.Fatal("合法 native 範本應載入")
		}
		if !tmpl.SupportsNative() || tmpl.SupportsDocker() {
			t.Errorf("能力推導錯誤:SupportsNative=%v SupportsDocker=%v", tmpl.SupportsNative(), tmpl.SupportsDocker())
		}
		if len(queryEvents(t, log, protocol.EventTemplateLoadFailed)) != 0 {
			t.Fatal("合法 native 範本不應記 TEMPLATE_LOAD_FAILED")
		}
	})

	// 各無效態:斷言拒載且事件 field 精確。
	cases := []struct {
		name      string
		content   string
		wantField string
	}{
		{"runtime_native_missing_section", nativeMissingSection, "native"},
		{"no_runtime_section", noRuntimeSection, "docker/native"},
		{"native_missing_kind", nativeMissingKind, "native.provision.kind"},
		{"native_java_no_major", nativeJavaNoMajor, "native.provision.java_major"},
		{"native_steam_no_app_id", nativeSteamNoAppID, "native.provision.steam_app_id"},
		{"native_no_command", nativeNoCommand, "native.launch.command"},
		{"native_bad_config_format", nativeBadConfigFormat, "native.config[0].format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, _ := newTestEventLog(t)
			eng := NewTemplateEngine(nil, log)
			dir := t.TempDir()
			writeTemplateFile(t, dir, "bad.toml", tc.content)
			n, err := eng.LoadDir(dir)
			if err != nil {
				t.Fatalf("LoadDir: %v", err)
			}
			if n != 0 {
				t.Fatalf("無效 native 範本應被拒載,得載入份數 %d", n)
			}
			evs := queryEvents(t, log, protocol.EventTemplateLoadFailed)
			if len(evs) != 1 {
				t.Fatalf("TEMPLATE_LOAD_FAILED 事件數 = %d, 期望 1", len(evs))
			}
			if !strings.Contains(string(evs[0].DetailsJSON), `"field":"`+tc.wantField+`"`) {
				t.Fatalf("事件 field 不符,期望含 %q,得 %s", tc.wantField, evs[0].DetailsJSON)
			}
		})
	}
}

// TestTemplateEngine_BuiltinNativeCapability 驗證內建 minecraft/palworld 增補 [native] 後,
// 兩後端能力皆宣告(SupportsDocker && SupportsNative),且 docker 預設 runtime 不變(回歸)。
func TestTemplateEngine_BuiltinNativeCapability(t *testing.T) {
	log, _ := newTestEventLog(t)
	eng := NewTemplateEngine(nil, log)
	if _, err := eng.LoadDir(builtinTemplatesDir); err != nil {
		t.Fatalf("LoadDir(%s): %v", builtinTemplatesDir, err)
	}
	for _, id := range []string{"minecraft", "palworld"} {
		tmpl, ok := eng.Get(id)
		if !ok {
			t.Fatalf("%s 範本未載入", id)
		}
		if !tmpl.SupportsDocker() || !tmpl.SupportsNative() {
			t.Errorf("%s 應同時支援 docker/native,得 docker=%v native=%v", id, tmpl.SupportsDocker(), tmpl.SupportsNative())
		}
		if tmpl.Runtime != "docker" {
			t.Errorf("%s 預設 runtime 應維持 docker(回歸),得 %q", id, tmpl.Runtime)
		}
	}
	// provision 種類:minecraft=java、palworld=steamcmd。
	mc, _ := eng.Get("minecraft")
	if mc.Native.Provision.Kind != "java" || mc.Native.Provision.JavaMajor != 21 {
		t.Errorf("minecraft native provision = %+v, 期望 java/21", mc.Native.Provision)
	}
	pw, _ := eng.Get("palworld")
	if pw.Native.Provision.Kind != "steamcmd" || pw.Native.Provision.SteamAppID != "2394010" {
		t.Errorf("palworld native provision = %+v, 期望 steamcmd/2394010", pw.Native.Provision)
	}
}

// TestTemplateEngine_ThirdPartyTemplate 驗證 R1:放第三份測試範本(沿用既有 docker/rcon/tcp
// adapter)不改核心即被載入。
func TestTemplateEngine_ThirdPartyTemplate(t *testing.T) {
	log, _ := newTestEventLog(t)
	eng := NewTemplateEngine(nil, log)
	dir := t.TempDir()
	writeTemplateFile(t, dir, "custom.toml", validMinimalTemplate) // testgame:沿用 docker+rcon+tcp

	n, err := eng.LoadDir(dir)
	if err != nil || n != 1 {
		t.Fatalf("LoadDir: n=%d err=%v", n, err)
	}
	if _, ok := eng.Get("testgame"); !ok {
		t.Fatal("第三方範本應不改核心即被載入")
	}
}
