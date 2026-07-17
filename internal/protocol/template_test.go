package protocol

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseTemplate_MinecraftGolden 原樣解析 design.md 的 Minecraft 範本範例,
// 斷言關鍵欄位正確落位(對應 T1 驗收:「原樣解析 design 範本範例」golden TOML 測試)。
func TestParseTemplate_MinecraftGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "minecraft.toml"))
	if err != nil {
		t.Fatalf("讀取 golden testdata 失敗: %v", err)
	}

	tmpl, err := ParseTemplate(data)
	if err != nil {
		t.Fatalf("ParseTemplate 失敗: %v", err)
	}

	// 頂層
	if tmpl.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, 期望 1", tmpl.SchemaVersion)
	}
	if tmpl.ID != "minecraft" {
		t.Errorf("ID = %q, 期望 minecraft", tmpl.ID)
	}
	if tmpl.Runtime != "docker" {
		t.Errorf("Runtime = %q, 期望 docker", tmpl.Runtime)
	}
	// data_dirs 於頂層
	if len(tmpl.DataDirs) != 1 || tmpl.DataDirs[0] != "/data" {
		t.Errorf("DataDirs = %v, 期望 [/data]", tmpl.DataDirs)
	}

	// [docker]
	if tmpl.Docker == nil {
		t.Fatal("Docker 為 nil,期望有 [docker] 區段")
	}
	if tmpl.Docker.Image != "itzg/minecraft-server" {
		t.Errorf("Docker.Image = %q", tmpl.Docker.Image)
	}
	if tmpl.Docker.ImageDigest != "sha256:..." {
		t.Errorf("Docker.ImageDigest = %q", tmpl.Docker.ImageDigest)
	}

	// [[variants]]
	if len(tmpl.Variants) != 1 {
		t.Fatalf("len(Variants) = %d, 期望 1", len(tmpl.Variants))
	}
	if tmpl.Variants[0].ID != "paper" {
		t.Errorf("Variants[0].ID = %q, 期望 paper", tmpl.Variants[0].ID)
	}
	if tmpl.Variants[0].Env["TYPE"] != "PAPER" {
		t.Errorf("Variants[0].Env[TYPE] = %q, 期望 PAPER", tmpl.Variants[0].Env["TYPE"])
	}

	// [[ports]] — host_port / bind_ip 為驗收重點
	if len(tmpl.Ports) != 2 {
		t.Fatalf("len(Ports) = %d, 期望 2", len(tmpl.Ports))
	}
	game := tmpl.Ports[0]
	if game.Name != "game" || game.Container != 25565 || game.HostPort != 25565 {
		t.Errorf("Ports[0] = %+v, 期望 game/25565/25565", game)
	}
	if game.BindIP != "0.0.0.0" || game.Protocol != "tcp" || !game.Required {
		t.Errorf("Ports[0] binding = %+v, 期望 0.0.0.0/tcp/required", game)
	}
	rcon := tmpl.Ports[1]
	if rcon.Name != "rcon" || rcon.HostPort != 25575 || rcon.BindIP != "127.0.0.1" {
		t.Errorf("Ports[1] = %+v, 期望 rcon/25575/127.0.0.1", rcon)
	}

	// [[params]]
	if len(tmpl.Params) != 2 {
		t.Fatalf("len(Params) = %d, 期望 2", len(tmpl.Params))
	}
	if tmpl.Params[0].Key != "MEMORY" {
		t.Errorf("Params[0].Key = %q, 期望 MEMORY", tmpl.Params[0].Key)
	}
	if def, ok := tmpl.Params[0].Default.(string); !ok || def != "2G" {
		t.Errorf("Params[0].Default = %v (%T), 期望字串 2G", tmpl.Params[0].Default, tmpl.Params[0].Default)
	}
	if tmpl.Params[1].Key != "EULA" || tmpl.Params[1].Type != "bool" || !tmpl.Params[1].Required {
		t.Errorf("Params[1] = %+v, 期望 EULA/bool/required", tmpl.Params[1])
	}

	// [[secrets]]
	if len(tmpl.Secrets) != 1 || tmpl.Secrets[0].Key != "RCON_PASSWORD" {
		t.Errorf("Secrets = %+v, 期望 [RCON_PASSWORD]", tmpl.Secrets)
	}

	// [[command_protocols]] — tagged union(rcon)
	if len(tmpl.CommandProtocols) != 1 {
		t.Fatalf("len(CommandProtocols) = %d, 期望 1", len(tmpl.CommandProtocols))
	}
	cp := tmpl.CommandProtocols[0]
	if cp.ProtocolID != "mc-rcon" || cp.Kind != "rcon" {
		t.Errorf("CommandProtocols[0] = %+v, 期望 mc-rcon/rcon", cp)
	}
	if cp.HostPortRef != "rcon" || cp.PasswordRef != "RCON_PASSWORD" {
		t.Errorf("CommandProtocols[0] refs = %+v, 期望 host_port_ref=rcon password_ref=RCON_PASSWORD", cp)
	}

	// [hooks]
	if tmpl.Hooks.Stop == nil {
		t.Fatal("Hooks.Stop 為 nil")
	}
	if tmpl.Hooks.Stop.ProtocolID != "mc-rcon" || tmpl.Hooks.Stop.Command != "stop" {
		t.Errorf("Hooks.Stop = %+v, 期望 mc-rcon/stop", tmpl.Hooks.Stop)
	}
	if tmpl.Hooks.Announce == nil || tmpl.Hooks.Announce.Command != "say {msg}" {
		t.Errorf("Hooks.Announce = %+v, 期望 command=say {msg}", tmpl.Hooks.Announce)
	}

	// [health]
	if tmpl.Health == nil || tmpl.Health.Kind != "tcp" || tmpl.Health.PortRef != "game" {
		t.Errorf("Health = %+v, 期望 tcp/game", tmpl.Health)
	}

	// [players_query]
	if tmpl.PlayersQuery == nil || tmpl.PlayersQuery.Kind != "rcon" || tmpl.PlayersQuery.Command != "list" {
		t.Errorf("PlayersQuery = %+v, 期望 rcon/list", tmpl.PlayersQuery)
	}

	// [mods] — owner 為驗收重點
	if tmpl.Mods == nil {
		t.Fatal("Mods 為 nil")
	}
	if tmpl.Mods.Owner != "image-native" {
		t.Errorf("Mods.Owner = %q, 期望 image-native", tmpl.Mods.Owner)
	}
	if tmpl.Mods.PluginDir != "/data/plugins" || tmpl.Mods.ManualMount != "/modpacks" {
		t.Errorf("Mods dirs = %+v", tmpl.Mods)
	}
	if len(tmpl.Mods.ModpackEnv) != 2 || tmpl.Mods.ModpackEnv[0] != "MODRINTH" || tmpl.Mods.ModpackEnv[1] != "AUTO_CURSEFORGE" {
		t.Errorf("Mods.ModpackEnv = %v", tmpl.Mods.ModpackEnv)
	}
	if len(tmpl.Mods.ManualFormats) != 2 || tmpl.Mods.ManualFormats[0] != "mrpack" || tmpl.Mods.ManualFormats[1] != "curseforge-zip" {
		t.Errorf("Mods.ManualFormats = %v", tmpl.Mods.ManualFormats)
	}
}

// TestParseTemplate_HookArgsMessageKeyUsername 驗證雙審修正 #6 新增的 schema 欄位:
// Hook.Args(rest hook 靜態參數,如 Palworld shutdown 的 waittime)、Hook.MessageKey(動態訊息
// 要寫入 Args 的欄位名)、CommandProtocol.Username(rest basic auth 帳號,可選)。
func TestParseTemplate_HookArgsMessageKeyUsername(t *testing.T) {
	data := []byte(`
schema_version = 1
id = "x"
name = "X"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "x:1.0"
[[command_protocols]]
protocol_id = "rest-main"
kind = "rest"
auth = "basic"
username = "customadmin"
[hooks]
stop = { protocol_id = "rest-main", action_id = "shutdown", args = { waittime = "30", message = "bye" } }
announce = { protocol_id = "rest-main", action_id = "announce", message_key = "message" }
`)
	tmpl, err := ParseTemplate(data)
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	if len(tmpl.CommandProtocols) != 1 || tmpl.CommandProtocols[0].Username != "customadmin" {
		t.Fatalf("CommandProtocols[0].Username = %+v, 期望 customadmin", tmpl.CommandProtocols)
	}
	if tmpl.Hooks.Stop == nil || tmpl.Hooks.Stop.Args["waittime"] != "30" || tmpl.Hooks.Stop.Args["message"] != "bye" {
		t.Fatalf("Hooks.Stop.Args = %+v, 期望 waittime=30 message=bye", tmpl.Hooks.Stop)
	}
	if tmpl.Hooks.Announce == nil || tmpl.Hooks.Announce.MessageKey != "message" {
		t.Fatalf("Hooks.Announce.MessageKey = %+v, 期望 message", tmpl.Hooks.Announce)
	}
}

// TestParseTemplate_NativeSchema 驗證 native-backend R3 新增的 [native] schema 落位:
// provision(kind/java_major/steam_app_id/update_on_start)、launch(command/working_dir)、
// config(file/format/section/map),以及能力推導 helper。
func TestParseTemplate_NativeSchema(t *testing.T) {
	data := []byte(`
schema_version = 1
id = "x"
name = "X"
runtime = "native"
data_dirs = ["/data"]
[native.provision]
kind = "steamcmd"
steam_app_id = "2394010"
update_on_start = true
[native.launch]
command = ["{instance_dir}/PalServer.sh", "-port={port:game}"]
working_dir = "sub"
[native.mods]
mods_dir = "mods"
[[native.config]]
file = "PalWorldSettings.ini"
format = "palworld-ini"
section = "/Script/Pal.PalGameWorldSettings"
[native.config.map]
SERVER_NAME = "ServerName"
`)
	tmpl, err := ParseTemplate(data)
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	if tmpl.Native == nil {
		t.Fatal("Native 為 nil,期望有 [native] 區段")
	}
	if !tmpl.SupportsNative() || tmpl.SupportsDocker() {
		t.Errorf("能力推導錯誤:SupportsNative=%v SupportsDocker=%v", tmpl.SupportsNative(), tmpl.SupportsDocker())
	}
	p := tmpl.Native.Provision
	if p.Kind != "steamcmd" || p.SteamAppID != "2394010" || !p.UpdateOnStart {
		t.Errorf("Provision = %+v, 期望 steamcmd/2394010/update_on_start", p)
	}
	if len(tmpl.Native.Launch.Command) != 2 || tmpl.Native.Launch.WorkingDir != "sub" {
		t.Errorf("Launch = %+v", tmpl.Native.Launch)
	}
	if tmpl.Native.Mods == nil || tmpl.Native.Mods.ModsDir != "mods" {
		t.Errorf("Mods = %+v, 期望 mods_dir=mods", tmpl.Native.Mods)
	}
	if len(tmpl.Native.Config) != 1 {
		t.Fatalf("len(Config) = %d, 期望 1", len(tmpl.Native.Config))
	}
	cm := tmpl.Native.Config[0]
	if cm.File != "PalWorldSettings.ini" || cm.Format != "palworld-ini" || cm.Section != "/Script/Pal.PalGameWorldSettings" {
		t.Errorf("Config[0] = %+v", cm)
	}
	if cm.Map["SERVER_NAME"] != "ServerName" {
		t.Errorf("Config[0].Map = %v, 期望 SERVER_NAME=ServerName", cm.Map)
	}
}
