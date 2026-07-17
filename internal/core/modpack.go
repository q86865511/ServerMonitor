package core

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"servermonitor/internal/protocol"
)

// R11 模組/模組包(itzg 原生為唯一安裝擁有者)。本檔負責 core 側的模組包來源建模、
// 建立前的靜態相容/格式檢查,以及把來源轉為 itzg env(TYPE 覆寫 + 對應 env)。
//
// 手動檔傳輸(見 specs/.../spikes/modpack-spike.md §6):手動檔(mrpack/curseforge-zip)需檔案
// 已在容器內(MODRINTH_MODPACK/CF_MODPACK_ZIP=容器內路徑)。傳輸經 protocol.InstanceSpec.Mounts
// 具名掛載 seam:buildSpec 產出 Mounts=[{modpack, <manual_mount>}](不再併入 DataDirs,故不汙染
// 備份範圍),InstanceService.Create 於代理建容器後經 NodeClient.UploadMount 把本機檔位元組送達
// agent 宿主掛載目錄,env 指向 <manual_mount>/<檔名>。工具絕不自行解壓到 /data(itzg 為唯一擁有者)。

// ModpackType 是模組包來源種類。
type ModpackType string

const (
	// ModpackModrinth:以 Modrinth slug/ID/URL 指定(itzg TYPE=MODRINTH)。
	ModpackModrinth ModpackType = "modrinth"
	// ModpackCurseForge:以 CurseForge slug/URL 指定(itzg TYPE=AUTO_CURSEFORGE,需 CF_API_KEY)。
	ModpackCurseForge ModpackType = "curseforge"
	// ModpackManualMrpack:本機 .mrpack 檔(itzg TYPE=MODRINTH + 容器內路徑)。
	ModpackManualMrpack ModpackType = "manual-mrpack"
	// ModpackManualCurseZip:本機 CurseForge zip 檔(itzg TYPE=AUTO_CURSEFORGE + 容器內路徑,需 CF_API_KEY)。
	ModpackManualCurseZip ModpackType = "manual-cfzip"
)

// cfAPIKeySecret 是 CurseForge API 金鑰的 SecretRef 鍵名(範本 [[secrets]] 宣告、使用者自填)。
const cfAPIKeySecret = "CF_API_KEY"

// modpackMountName 是手動模組包檔的具名掛載鍵(agent 端對映 mounts/<name>/,見 protocol.MountSpec)。
const modpackMountName = "modpack"

// maxIndexJSONBytes 是手動模組包索引檔(modrinth.index.json / manifest.json)驗證時的讀取上限,
// 防禦 zip 內超大/高壓縮比條目(zip bomb);正常索引檔遠小於此。
const maxIndexJSONBytes = 32 << 20

// ModpackSource 是建立時選定的模組包來源(R11)。Ref 依 Type 而定:
//   - modrinth:slug / project id / page URL / 自架 .mrpack URL
//   - curseforge:slug 或 page URL
//   - manual-mrpack / manual-cfzip:本機檔案路徑(core 端可讀,供格式驗證)
type ModpackSource struct {
	Type ModpackType
	Ref  string
}

// 模組包相關錯誤哨符(R11)。以 errors.Is 判別;皆為「建立前阻擋、無副作用」。
var (
	// ErrModpackUnsupported 表示範本未宣告 [mods](不支援模組包)或來源種類未知。
	ErrModpackUnsupported = errors.New("core: 範本不支援模組包")
	// ErrLoaderIncompatible 表示變體 loader 不在範本 modpack_loaders(型別層級不相容,如 paper + 模組包)。
	ErrLoaderIncompatible = errors.New("core: 變體 loader 與模組包不相容")
	// ErrModpackAPIKeyRequired 表示 CurseForge 來源未提供必填的 CF_API_KEY。
	ErrModpackAPIKeyRequired = errors.New("core: CurseForge 模組包需提供 CF_API_KEY")
	// ErrModpackFormat 表示手動檔格式無效(非 zip、缺必要索引檔、索引檔非 JSON,或含非法條目名)。
	ErrModpackFormat = errors.New("core: 手動模組包檔格式無效")
	// ErrModpackVariantRequired 表示選了模組包但未選 loader 變體(無變體則無從判定相容性)。
	ErrModpackVariantRequired = errors.New("core: 模組包需選擇 loader 變體")
)

// ModpackError 攜帶不相容的細節(供 GUI 呈現可理解錯誤)。
type ModpackError struct {
	kind    error  // 上列哨符之一
	Variant string // 相關變體(loader 不相容時)
	Loader  string // 變體 loader
	Allowed []string
	Detail  string
}

func (e *ModpackError) Error() string {
	base := e.kind.Error()
	switch e.kind {
	case ErrLoaderIncompatible:
		return fmt.Sprintf("%s: 變體 %q(loader=%q)不在允許集合 %v", base, e.Variant, e.Loader, e.Allowed)
	default:
		if e.Detail != "" {
			return base + ": " + e.Detail
		}
		return base
	}
}

// Is 使 ModpackError 可被 errors.Is 對應到其攜帶的哨符。
func (e *ModpackError) Is(target error) bool { return target == e.kind }

// validateModpack 於建立前做模組包的靜態相容/格式檢查(R11)。純函式、無副作用
// (不觸碰金鑰庫/DB/journal/agent),呼叫時機在 Create 任一寫入動作之前。
//
// 檢查:範本須宣告 [mods];變體 loader 須在 modpack_loaders;CurseForge 來源(docker 路徑)須提供
// CF_API_KEY;手動檔須通過格式驗證(archive/zip 含必要索引檔)。版本層級相容交 itzg
// 於容器內解析(見 spike §2),此處只擋型別層級(plugin vs mod)。rt 為已解析的執行後端
// (docker/native),供 CF_API_KEY 必填判定(#4:native 不逐實例要求 key)。
func validateModpack(tmpl *protocol.GameTemplate, variant, rt string, opts CreateOptions) error {
	src := opts.Modpack
	if src == nil {
		return nil
	}
	if tmpl.Mods == nil {
		return &ModpackError{kind: ErrModpackUnsupported, Detail: "範本未宣告 [mods]"}
	}
	if !knownModpackType(src.Type) {
		return &ModpackError{kind: ErrModpackUnsupported, Detail: fmt.Sprintf("未知來源種類 %q", src.Type)}
	}
	if strings.TrimSpace(src.Ref) == "" {
		return &ModpackError{kind: ErrModpackUnsupported, Detail: "缺少模組包來源 ref"}
	}

	// 未選變體時無 loader 可判定相容性:回專屬明確錯誤(不誤報 loader 不相容)。
	if strings.TrimSpace(variant) == "" {
		return &ModpackError{kind: ErrModpackVariantRequired}
	}

	// loader 型別層級相容:變體 loader 須在範本 modpack_loaders 集合內。
	loader := variantLoader(tmpl, variant)
	if !loaderAllowed(tmpl.Mods.ModpackLoaders, loader) {
		return &ModpackError{
			kind:    ErrLoaderIncompatible,
			Variant: variant,
			Loader:  loader,
			Allowed: tmpl.Mods.ModpackLoaders,
		}
	}

	// CurseForge 來源需 CF_API_KEY——但僅 docker 路徑(itzg AUTO_CURSEFORGE 需逐實例 key)。
	// native 路徑由 agent 端內嵌/設定覆蓋 key 供應(未啟用時 agent 回明確錯誤),不在此強制(#4)。
	if rt == runtimeDocker && modpackNeedsAPIKey(src.Type) {
		if strings.TrimSpace(opts.Secrets[cfAPIKeySecret]) == "" {
			return &ModpackError{kind: ErrModpackAPIKeyRequired}
		}
	}

	// 手動檔格式驗證(archive/zip 含必要索引檔)。
	switch src.Type {
	case ModpackManualMrpack:
		if err := validateMrpackFile(src.Ref); err != nil {
			return &ModpackError{kind: ErrModpackFormat, Detail: err.Error()}
		}
	case ModpackManualCurseZip:
		if err := validateCurseZipFile(src.Ref); err != nil {
			return &ModpackError{kind: ErrModpackFormat, Detail: err.Error()}
		}
	}
	return nil
}

// applyModpackEnv 把模組包來源轉為 itzg env,疊加/覆寫到既有 env 上(R11)。
// TYPE 由模組包機制接管(覆寫變體宣告的 TYPE);手動檔的 env 指向容器內 manual_mount 路徑。
// CF_API_KEY 不在此注入——沿用既有 [[secrets]]→env 機制(buildSpec 的機密迴圈)。
func applyModpackEnv(tmpl *protocol.GameTemplate, src *ModpackSource, env map[string]string) {
	if src == nil || tmpl.Mods == nil {
		return
	}
	switch src.Type {
	case ModpackModrinth:
		env["TYPE"] = "MODRINTH"
		env["MODRINTH_MODPACK"] = src.Ref
	case ModpackManualMrpack:
		env["TYPE"] = "MODRINTH"
		env["MODRINTH_MODPACK"] = manualContainerPath(tmpl.Mods.ManualMount, src.Ref)
	case ModpackCurseForge:
		env["TYPE"] = "AUTO_CURSEFORGE"
		if isURL(src.Ref) {
			env["CF_PAGE_URL"] = src.Ref
		} else {
			env["CF_SLUG"] = src.Ref
		}
	case ModpackManualCurseZip:
		env["TYPE"] = "AUTO_CURSEFORGE"
		env["CF_MODPACK_ZIP"] = manualContainerPath(tmpl.Mods.ManualMount, src.Ref)
	}
}

// modpackMounts 回傳手動來源需要的具名掛載(mount name=modpack,容器路徑=範本 manual_mount);
// 非手動來源回 nil。掛載內容排除於備份範圍(見 protocol.MountSpec);檔案位元組由 Create 經
// NodeClient.UploadMount 送達,不併入 DataDirs(#3:解除備份汙染)。
func modpackMounts(tmpl *protocol.GameTemplate, src *ModpackSource) []protocol.MountSpec {
	if src == nil || tmpl.Mods == nil {
		return nil
	}
	switch src.Type {
	case ModpackManualMrpack, ModpackManualCurseZip:
		return []protocol.MountSpec{{Name: modpackMountName, ContainerPath: tmpl.Mods.ManualMount}}
	}
	return nil
}

// isManualModpack 回報來源是否為需上傳本機檔的手動模組包。
func isManualModpack(src *ModpackSource) bool {
	if src == nil {
		return false
	}
	return src.Type == ModpackManualMrpack || src.Type == ModpackManualCurseZip
}

// modpackMountFilename 取手動模組包本機檔的基礎檔名(上傳目的檔名,與 applyModpackEnv 的容器路徑對齊)。
func modpackMountFilename(ref string) string {
	return path.Base(filepath.ToSlash(ref))
}

// ---- 純函式輔助 ----

func knownModpackType(t ModpackType) bool {
	switch t {
	case ModpackModrinth, ModpackCurseForge, ModpackManualMrpack, ModpackManualCurseZip:
		return true
	}
	return false
}

func modpackNeedsAPIKey(t ModpackType) bool {
	return t == ModpackCurseForge || t == ModpackManualCurseZip
}

// variantLoader 取變體宣告的 loader;未選變體或變體未宣告 loader 回空字串(以「」表示未指定)。
func variantLoader(tmpl *protocol.GameTemplate, variant string) string {
	if variant == "" {
		return ""
	}
	for _, v := range tmpl.Variants {
		if v.ID == variant {
			return v.Loader
		}
	}
	return ""
}

// loaderAllowed 回報 loader 是否在允許集合內(大小寫不敏感)。
func loaderAllowed(allowed []string, loader string) bool {
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimSpace(a), loader) {
			return true
		}
	}
	return false
}

// manualContainerPath 由 manual_mount 容器目錄與本機檔路徑組出容器內檔案路徑(取檔名)。
func manualContainerPath(mount, localPath string) string {
	base := path.Base(filepath.ToSlash(localPath))
	return path.Join(mount, base)
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// containsString 回報 slice 是否含 s(精確比對)。
func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// validateMrpackFile 驗證 .mrpack:須為 zip 且含 modrinth.index.json(R11 手動格式)。
func validateMrpackFile(pathStr string) error {
	return validateZipHasEntry(pathStr, "modrinth.index.json")
}

// validateCurseZipFile 驗證 curseforge-zip:須為 zip 且含 manifest.json(R11 手動格式)。
func validateCurseZipFile(pathStr string) error {
	return validateZipHasEntry(pathStr, "manifest.json")
}

// validateZipHasEntry 以標準庫 archive/zip 開檔並確認含指定(頂層)索引檔。不解壓、不寫任何檔。
// 強化(#9):(1) 任一條目名含 .. 或以 / 開頭(絕對路徑/路徑穿越)→ 整包拒;(2) 找到索引檔後
// 開啟並確認可解析為 JSON 物件(不驗 schema),擋掉「副檔名對但內容非模組包索引」的偽檔。
func validateZipHasEntry(pathStr, entry string) error {
	rc, err := zip.OpenReader(pathStr)
	if err != nil {
		return fmt.Errorf("無法以 zip 開啟 %q: %w", pathStr, err)
	}
	defer rc.Close()

	var found *zip.File
	for _, f := range rc.File {
		if strings.HasPrefix(f.Name, "/") || strings.HasPrefix(f.Name, "\\") {
			return fmt.Errorf("zip 含非法條目(絕對路徑): %q", f.Name)
		}
		if zipEntryHasDotDot(f.Name) {
			return fmt.Errorf("zip 含非法條目(路徑穿越): %q", f.Name)
		}
		if path.Clean(f.Name) == entry {
			found = f
		}
	}
	if found == nil {
		return fmt.Errorf("zip 內缺少必要索引檔 %q", entry)
	}
	if err := validateZipEntryJSON(found); err != nil {
		return fmt.Errorf("索引檔 %q 非有效 JSON: %w", entry, err)
	}
	return nil
}

// zipEntryHasDotDot 回報 zip 條目名是否含 .. 路徑元素(以 / 或 \ 分隔皆檢查)。
func zipEntryHasDotDot(name string) bool {
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// validateZipEntryJSON 開啟 zip 條目並確認內容可解析為 JSON 物件(map[string]any)。
// 以 LimitReader 限制讀取量,防禦高壓縮比條目。
func validateZipEntryJSON(f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxIndexJSONBytes))
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	return nil
}
