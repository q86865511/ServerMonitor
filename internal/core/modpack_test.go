package core

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"servermonitor/internal/protocol"
)

// modpackTemplate 是含 [mods](modpack_loaders)與 CF_API_KEY 機密的 Minecraft-like 範本,
// 用以驗證 R11 模組包路徑(env 透傳 / 前置相容 / 手動檔格式)。變體 paper=plugin(不可套模組包)、
// fabric/vanilla=可套。埠選用不與其他測試範本衝突的高位埠,避免同 harness 內 wildcard 誤撞。
const modpackTemplate = `
schema_version = 1
id = "mcmod"
name = "MC Mod Test"
runtime = "docker"
data_dirs = ["/data"]
[docker]
image = "itzg/minecraft-server:java21"
[[variants]]
id = "paper"
loader = "paper"
[variants.env]
TYPE = "PAPER"
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
host_port = 25600
bind_ip = "0.0.0.0"
protocol = "tcp"
required = true
[[params]]
key = "EULA"
type = "bool"
required = true
[[secrets]]
key = "CF_API_KEY"
[mods]
owner = "image-native"
manual_mount = "/modpacks"
manual_formats = ["mrpack", "curseforge-zip"]
modpack_loaders = ["vanilla", "forge", "fabric", "quilt", "neoforge"]
`

// loadModpackTemplate 把 modpackTemplate 載入既有 harness 的範本引擎。
func loadModpackTemplate(t *testing.T, h *svcHarness) {
	t.Helper()
	writeTemplateFile(t, h.dir, "mcmod.toml", modpackTemplate)
	if _, err := h.eng.LoadDir(h.dir); err != nil {
		t.Fatalf("載入 mcmod 範本: %v", err)
	}
}

// modpackDualTemplate 同時宣告 [docker]/[native] 與 [mods],用以驗證 CF_API_KEY 必填判定的 runtime
// 分歧(#4):docker 路徑要求逐實例 key、native 路徑不要求(agent 端內嵌/設定 key)。game 埠用動態
// (host_port=0,不預留)以便同一 harness 內多次 Create 不撞埠。
const modpackDualTemplate = `
schema_version = 1
id = "mcmoddual"
name = "MC Mod Dual"
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
key = "EULA"
type = "bool"
required = true
[[secrets]]
key = "CF_API_KEY"
[mods]
owner = "image-native"
manual_mount = "/modpacks"
manual_formats = ["mrpack", "curseforge-zip"]
modpack_loaders = ["vanilla", "forge", "fabric", "quilt", "neoforge"]
[native.provision]
kind = "java"
java_major = 21
[native.launch]
command = ["{java}", "-jar", "{server_jar}", "nogui"]
[native.mods]
mods_dir = "mods"
`

// loadModpackDualTemplate 把 modpackDualTemplate 載入 harness 的範本引擎。
func loadModpackDualTemplate(t *testing.T, h *svcHarness) {
	t.Helper()
	writeTemplateFile(t, h.dir, "mcmoddual.toml", modpackDualTemplate)
	if _, err := h.eng.LoadDir(h.dir); err != nil {
		t.Fatalf("載入 mcmoddual 範本: %v", err)
	}
}

// writeZip 在 t.TempDir 造一個含指定 entries 的 zip 檔,回傳路徑。
func writeZip(t *testing.T, name string, entries ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("建立 zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, e := range entries {
		w, werr := zw.Create(e)
		if werr != nil {
			t.Fatalf("zip.Create(%s): %v", e, werr)
		}
		_, _ = w.Write([]byte("{}"))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}
	return p
}

// writeZipWith 在 t.TempDir 造一個 zip,entries 為「條目名→內容」,回傳路徑。
func writeZipWith(t *testing.T, name string, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("建立 zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for e, content := range entries {
		w, werr := zw.Create(e)
		if werr != nil {
			t.Fatalf("zip.Create(%s): %v", e, werr)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}
	return p
}

// TestModpack_ModrinthEnv:modrinth 來源(fabric 變體)→ spec env 含 TYPE=MODRINTH、
// MODRINTH_MODPACK=ref;成功建立。
func TestModpack_ModrinthEnv(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackModrinth, Ref: "cobblemon-fabric"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	spec := h.backend.spec()
	if spec.Env["TYPE"] != "MODRINTH" {
		t.Errorf("TYPE = %q, 期望 MODRINTH(覆寫變體 FABRIC)", spec.Env["TYPE"])
	}
	if spec.Env["MODRINTH_MODPACK"] != "cobblemon-fabric" {
		t.Errorf("MODRINTH_MODPACK = %q", spec.Env["MODRINTH_MODPACK"])
	}
}

// TestModpack_CurseForgeEnvWithKey:curseforge 來源(slug)+ CF_API_KEY →
// spec env 含 TYPE=AUTO_CURSEFORGE、CF_SLUG、CF_API_KEY(值來自 secret),且 DB params_json 無明文。
func TestModpack_CurseForgeEnvWithKey(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	rec, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "vanilla",
		Params:     map[string]string{"EULA": "true"},
		Secrets:    map[string]string{"CF_API_KEY": "cf-secret-123"},
		Modpack:    &ModpackSource{Type: ModpackCurseForge, Ref: "all-the-mods-8"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	spec := h.backend.spec()
	if spec.Env["TYPE"] != "AUTO_CURSEFORGE" {
		t.Errorf("TYPE = %q, 期望 AUTO_CURSEFORGE", spec.Env["TYPE"])
	}
	if spec.Env["CF_SLUG"] != "all-the-mods-8" {
		t.Errorf("CF_SLUG = %q", spec.Env["CF_SLUG"])
	}
	if spec.Env["CF_API_KEY"] != "cf-secret-123" {
		t.Errorf("CF_API_KEY = %q, 期望來自 secret 明文注入", spec.Env["CF_API_KEY"])
	}
	// DB 不落金鑰明文;金鑰入 keyring(UUID 命名空間)。
	got, _ := h.store.GetInstance(rec.UUID)
	if indexOf(string(got.ParamsJSON), "cf-secret-123") >= 0 {
		t.Errorf("params_json 不應含 CF_API_KEY 明文: %s", got.ParamsJSON)
	}
	if val, gerr := h.keyring.Get("test", instanceSecretKey(rec.UUID, "CF_API_KEY")); gerr != nil || val != "cf-secret-123" {
		t.Errorf("keyring CF_API_KEY = %q, err=%v", val, gerr)
	}
}

// TestModpack_CurseForgePageURL:curseforge 來源以完整 URL 指定 → 用 CF_PAGE_URL 而非 CF_SLUG。
func TestModpack_CurseForgePageURL(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "vanilla",
		Params:     map[string]string{"EULA": "true"},
		Secrets:    map[string]string{"CF_API_KEY": "k"},
		Modpack:    &ModpackSource{Type: ModpackCurseForge, Ref: "https://www.curseforge.com/minecraft/modpacks/all-the-mods-8"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	spec := h.backend.spec()
	if spec.Env["CF_PAGE_URL"] == "" || spec.Env["CF_SLUG"] != "" {
		t.Errorf("URL 來源應設 CF_PAGE_URL 不設 CF_SLUG, env=%+v", spec.Env)
	}
}

// TestModpack_CurseForgeMissingKey:curseforge 來源未提供 CF_API_KEY → 建立前擋、無副作用。
func TestModpack_CurseForgeMissingKey(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "vanilla",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackCurseForge, Ref: "all-the-mods-8"},
	})
	if !errors.Is(err, ErrModpackAPIKeyRequired) {
		t.Fatalf("期望 ErrModpackAPIKeyRequired,得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_NativeCurseForgeNoSecret:native 路徑 + CurseForge 模組包未提供 CF_API_KEY →
// 不因缺 secret 被擋(#4:native 由 agent 端內嵌/設定 key 供應);成功建立且走 native runtime。
func TestModpack_NativeCurseForgeNoSecret(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackDualTemplate(t, h)
	h.svc.goos = "windows" // Windows + 範本支援 native → 預設 native runtime

	rec, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmoddual",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackCurseForge, Ref: "12345:67890"},
	})
	if err != nil {
		t.Fatalf("native + CF 模組包(無 secret)不應被擋: %v", err)
	}
	if got := h.backend.spec().Runtime; got != runtimeNative {
		t.Errorf("Runtime = %q, 期望 native", got)
	}
	if rec.UUID == "" {
		t.Errorf("成功建立應回 UUID")
	}
}

// TestModpack_DockerCurseForgeStillRequiresSecret:同一雙能力範本走 docker 路徑時,CurseForge 仍
// 要求 CF_API_KEY(#4:docker/itzg 逐實例 key 未變);缺 key 建立前擋、無副作用。
func TestModpack_DockerCurseForgeStillRequiresSecret(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackDualTemplate(t, h)
	h.svc.goos = "windows"

	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmoddual",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"},
		Runtime:    runtimeDocker, // 顯式 docker → 仍要求 key
		Modpack:    &ModpackSource{Type: ModpackCurseForge, Ref: "all-the-mods-8"},
	})
	if !errors.Is(err, ErrModpackAPIKeyRequired) {
		t.Fatalf("docker 路徑缺 CF_API_KEY 應擋(ErrModpackAPIKeyRequired),得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_LoaderIncompatible:paper 變體(plugin 平台)+ 模組包 → 建立前依矩陣擋、無副作用。
func TestModpack_LoaderIncompatible(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "paper",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackModrinth, Ref: "cobblemon-fabric"},
	})
	if !errors.Is(err, ErrLoaderIncompatible) {
		t.Fatalf("期望 ErrLoaderIncompatible(paper + 模組包),得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_ManualMrpackValidAndMount:合法 mrpack(zip 含 modrinth.index.json)→ 成功;
// spec env 指向容器內 manual_mount 路徑;手動檔改走 Mounts 具名掛載(#3:不入 DataDirs、
// 不汙染備份);檔案位元組經上傳端點送達 agent 掛載目錄(端到端 Mock 層)。
func TestModpack_ManualMrpackValidAndMount(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	mrpack := writeZip(t, "world.mrpack", "modrinth.index.json", "overrides/config.txt")
	rec, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackManualMrpack, Ref: mrpack},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	spec := h.backend.spec()
	if spec.Env["TYPE"] != "MODRINTH" || spec.Env["MODRINTH_MODPACK"] != "/modpacks/world.mrpack" {
		t.Errorf("手動 mrpack env 應指向容器內路徑, env=%+v", spec.Env)
	}
	// #3:manual_mount 走 Mounts 具名掛載,不再併入 DataDirs(解除備份汙染)。
	if containsString(spec.DataDirs, "/modpacks") {
		t.Errorf("manual_mount 不應再進 DataDirs(改走 Mounts), DataDirs=%+v", spec.DataDirs)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Name != "modpack" || spec.Mounts[0].ContainerPath != "/modpacks" {
		t.Errorf("應有 modpack Mount 指向 /modpacks, Mounts=%+v", spec.Mounts)
	}
	// 端到端:檔案位元組經上傳端點落入 agent(MockBackend)的 mount 檔。
	got, ok := h.backend.MountFile(rec.UUID, "modpack", "world.mrpack")
	if !ok {
		t.Fatalf("上傳的模組包檔未落入 agent mount 目錄")
	}
	want, _ := os.ReadFile(mrpack)
	if len(got) == 0 || string(got) != string(want) {
		t.Errorf("上傳內容與本機檔不符 (len got=%d want=%d)", len(got), len(want))
	}
}

// TestModpack_ManualCurseZipValid:合法 curseforge-zip(zip 含 manifest.json)+ CF_API_KEY →
// 成功;spec env 含 TYPE=AUTO_CURSEFORGE、CF_MODPACK_ZIP 指向容器內路徑。
func TestModpack_ManualCurseZipValid(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	zip := writeZip(t, "pack.zip", "manifest.json")
	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "vanilla",
		Params:     map[string]string{"EULA": "true"},
		Secrets:    map[string]string{"CF_API_KEY": "k"},
		Modpack:    &ModpackSource{Type: ModpackManualCurseZip, Ref: zip},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	spec := h.backend.spec()
	if spec.Env["TYPE"] != "AUTO_CURSEFORGE" || spec.Env["CF_MODPACK_ZIP"] != "/modpacks/pack.zip" {
		t.Errorf("手動 cfzip env 應指向容器內路徑, env=%+v", spec.Env)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].ContainerPath != "/modpacks" {
		t.Errorf("手動 cfzip 應有 modpack Mount, Mounts=%+v", spec.Mounts)
	}
	if containsString(spec.DataDirs, "/modpacks") {
		t.Errorf("manual_mount 不應進 DataDirs, DataDirs=%+v", spec.DataDirs)
	}
}

// TestModpack_ManualInvalidZip:zip 缺必要索引檔 → 建立前擋(ErrModpackFormat)、無副作用。
func TestModpack_ManualInvalidZip(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	bad := writeZip(t, "bad.mrpack", "some-other-file.txt") // 缺 modrinth.index.json
	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackManualMrpack, Ref: bad},
	})
	if !errors.Is(err, ErrModpackFormat) {
		t.Fatalf("期望 ErrModpackFormat(缺索引檔),得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_ManualNotZip:非 zip 檔 → 建立前擋(ErrModpackFormat)、無副作用。
func TestModpack_ManualNotZip(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	notzip := filepath.Join(t.TempDir(), "plain.mrpack")
	if err := os.WriteFile(notzip, []byte("not a zip"), 0o644); err != nil {
		t.Fatalf("寫檔: %v", err)
	}
	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "fabric",
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackManualMrpack, Ref: notzip},
	})
	if !errors.Is(err, ErrModpackFormat) {
		t.Fatalf("期望 ErrModpackFormat(非 zip),得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_UnsupportedTemplate:範本無 [mods] 但選了模組包 → ErrModpackUnsupported、無副作用。
func TestModpack_UnsupportedTemplate(t *testing.T) {
	h := newSvcHarness(t) // svc 範本無 [mods]
	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "svc",
		Variant:    "paper",
		Params:     map[string]string{"EULA": "true"},
		Secrets:    map[string]string{"RCON_PASSWORD": "x"},
		Modpack:    &ModpackSource{Type: ModpackModrinth, Ref: "x"},
	})
	if !errors.Is(err, ErrModpackUnsupported) {
		t.Fatalf("期望 ErrModpackUnsupported(範本無 [mods]),得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_ManualUploadFailureRollback:手動模組包檔上傳失敗(代理建容器後、寫 DB 前)→
// 完整回滾(移除孤兒容器、釋放埠、刪機密、清 journal、不留 DB 紀錄),記 INSTANCE_CREATE_FAILED。
func TestModpack_ManualUploadFailureRollback(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)
	h.svc.newUUID = func() string { return "upload-fail-uuid" }
	h.backend.uploadErr = errors.New("injected upload failure")

	mrpack := writeZip(t, "world.mrpack", "modrinth.index.json")
	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod", Variant: "fabric",
		Params:  map[string]string{"EULA": "true"},
		Secrets: map[string]string{"CF_API_KEY": "k"}, // 供 keyring 刪除路徑驗證(非必填但可寫)
		Modpack: &ModpackSource{Type: ModpackManualMrpack, Ref: mrpack},
	})
	if err == nil {
		t.Fatal("期望 Create 因上傳失敗而失敗,得 nil")
	}

	// 容器確曾建立,回滾後代理端無殘留孤兒。
	if h.backend.createCount != 1 {
		t.Errorf("代理 Create 應被呼叫 1 次, 得 %d", h.backend.createCount)
	}
	refs, lerr := h.backend.List(context.Background())
	if lerr != nil {
		t.Fatalf("backend.List: %v", lerr)
	}
	if len(refs) != 0 {
		t.Errorf("回滾後代理不應殘留容器, 得 %d 個", len(refs))
	}
	if insts, _ := h.store.ListInstances(); len(insts) != 0 {
		t.Errorf("回滾後不應有 DB 實例紀錄, 得 %+v", insts)
	}
	if ports, _ := h.store.ListPortReservations(); len(ports) != 0 {
		t.Errorf("回滾後不應有埠預留, 得 %+v", ports)
	}
	if entries, _ := h.svc.journal.List(); len(entries) != 0 {
		t.Errorf("回滾後 journal 應清空, 得 %+v", entries)
	}
	if _, gerr := h.keyring.Get("test", instanceSecretKey("upload-fail-uuid", "CF_API_KEY")); gerr == nil {
		t.Error("回滾後機密應已從金鑰庫刪除")
	}
	if evs := queryEvents(t, h.events, protocol.EventInstanceCreateFailed); len(evs) != 1 {
		t.Errorf("INSTANCE_CREATE_FAILED 事件數 = %d, 期望 1", len(evs))
	}
}

// TestModpack_VariantRequired:選了模組包但未選變體 → 專屬 ErrModpackVariantRequired
// (不誤報 loader 不相容),建立前擋、無副作用(#8)。
func TestModpack_VariantRequired(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "", // 未選變體
		Params:     map[string]string{"EULA": "true"},
		Modpack:    &ModpackSource{Type: ModpackModrinth, Ref: "cobblemon-fabric"},
	})
	if !errors.Is(err, ErrModpackVariantRequired) {
		t.Fatalf("期望 ErrModpackVariantRequired,得 %v", err)
	}
	if errors.Is(err, ErrLoaderIncompatible) {
		t.Errorf("不應誤報為 ErrLoaderIncompatible: %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_ZipPathTraversalRejected:zip 條目含 .. 路徑穿越 → 整包拒(ErrModpackFormat),無副作用(#9)。
func TestModpack_ZipPathTraversalRejected(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	bad := writeZipWith(t, "trav.mrpack", map[string]string{
		"modrinth.index.json": "{}",
		"../evil.txt":         "pwned",
	})
	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod", Variant: "fabric",
		Params:  map[string]string{"EULA": "true"},
		Modpack: &ModpackSource{Type: ModpackManualMrpack, Ref: bad},
	})
	if !errors.Is(err, ErrModpackFormat) {
		t.Fatalf("期望 ErrModpackFormat(路徑穿越),得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_ZipAbsolutePathRejected:zip 條目以 / 開頭(絕對路徑)→ 整包拒(#9)。
func TestModpack_ZipAbsolutePathRejected(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	bad := writeZipWith(t, "abs.mrpack", map[string]string{
		"modrinth.index.json": "{}",
		"/etc/passwd":         "x",
	})
	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod", Variant: "fabric",
		Params:  map[string]string{"EULA": "true"},
		Modpack: &ModpackSource{Type: ModpackManualMrpack, Ref: bad},
	})
	if !errors.Is(err, ErrModpackFormat) {
		t.Fatalf("期望 ErrModpackFormat(絕對路徑),得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_ZipIndexNotJSON:索引檔存在但內容非 JSON 物件 → 拒(ErrModpackFormat),無副作用(#9)。
func TestModpack_ZipIndexNotJSON(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	bad := writeZipWith(t, "badjson.mrpack", map[string]string{
		"modrinth.index.json": "this is definitely not json",
	})
	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod", Variant: "fabric",
		Params:  map[string]string{"EULA": "true"},
		Modpack: &ModpackSource{Type: ModpackManualMrpack, Ref: bad},
	})
	if !errors.Is(err, ErrModpackFormat) {
		t.Fatalf("期望 ErrModpackFormat(索引檔非 JSON),得 %v", err)
	}
	h.assertNoSideEffects(t)
}

// TestModpack_NoModpackUnaffected:未選模組包時,行為與既有一致(不注入任何 modpack env)。
func TestModpack_NoModpackUnaffected(t *testing.T) {
	h := newSvcHarness(t)
	loadModpackTemplate(t, h)

	_, err := h.svc.Create(context.Background(), CreateOptions{
		TemplateID: "mcmod",
		Variant:    "paper",
		Params:     map[string]string{"EULA": "true"},
	})
	if err != nil {
		t.Fatalf("Create(無模組包): %v", err)
	}
	spec := h.backend.spec()
	if spec.Env["TYPE"] != "PAPER" {
		t.Errorf("未選模組包應保留變體 TYPE=PAPER, 得 %q", spec.Env["TYPE"])
	}
	if _, ok := spec.Env["MODRINTH_MODPACK"]; ok {
		t.Errorf("未選模組包不應有 MODRINTH_MODPACK, env=%+v", spec.Env)
	}
	if containsString(spec.DataDirs, "/modpacks") {
		t.Errorf("未選手動模組包不應掛載 manual_mount, DataDirs=%+v", spec.DataDirs)
	}
	if len(spec.Mounts) != 0 {
		t.Errorf("未選手動模組包不應有 Mounts, Mounts=%+v", spec.Mounts)
	}
}
