package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"servermonitor/internal/protocol"
)

// 階段 5:伺服器檔案管理。瀏覽/下載/上傳/刪除實例資料目錄的檔案,以「橫切能力」介面表達
// (比照 BackupDeleter/MountWriter/ImageManager),不擴張核心 RuntimeBackend 介面;節點代理
// Server 以型別斷言啟用對應端點,能力缺失回 ErrUnsupported。
//
// 執行位置:docker 與 native 的實例資料都落在宿主端同一位置 instanceDataRoot(uuid)(docker 為
// bind mount 來源、native 為工作目錄),故一律走宿主 filesystem,不 exec 進容器;兩後端共用同一
// 份以 instanceDataRoot 為根的實作(各一薄包裝,同 resources.go 的 instanceDiskUsage 作法)。
//
// 安全立場:rel 全數來自網路(GUI → core → agent),是本專案攻擊面最大的輸入。所有檔案操作
// 一律先經 resolveWithinRoot 取得受拘束的絕對路徑,語意對齊 core 既有且經證明的 icon 路徑防護
// (internal/core/template_engine.go IconPath):宣告層以 filepath.Rel 判邊界(非字串前綴,擋
// sibling-prefix),再以「不穿越任何連結類組件」擋 symlink/junction 逃逸(見 resolveWithinRoot
// 的三層說明——Windows 的 junction 是 IconPath 那套 EvalSymlinks 複驗擋不住的,此處補強)。

// FileManager 是節點層「實例資料目錄檔案管理」能力。以相對實例資料根的路徑 rel 定址
// (空字串=資料根本身;一律以 "/" 分隔,亦容忍 "\"),docker 與 native 皆實作。
type FileManager interface {
	// ListFiles 列出 rel 目錄的直接子項(不遞迴)。rel 非目錄 → ErrInvalidFilePath;不存在 → ErrNotFound。
	ListFiles(ctx context.Context, uuid, rel string) ([]protocol.FileEntry, error)
	// ReadFile 開啟 rel 一般檔案供串流下載;呼叫端負責 Close。非一般檔案 → ErrInvalidFilePath。
	ReadFile(ctx context.Context, uuid, rel string) (io.ReadCloser, protocol.FileEntry, error)
	// WriteFile 以 r 的內容覆寫/建立 rel 檔案(父目錄須已存在)。覆寫非一般檔案 → ErrInvalidFilePath。
	WriteFile(ctx context.Context, uuid, rel string, r io.Reader) error
	// DeleteFile 刪除 rel 檔案;rel 為目錄時須 recursive=true。symlink 本身一律拒刪(不跟隨)。
	DeleteFile(ctx context.Context, uuid, rel string, recursive bool) error
}

var (
	_ FileManager = (*DockerBackend)(nil)
	_ FileManager = (*NativeBackend)(nil)
)

// ErrInvalidFilePath 表示相對路徑不安全或目標型別不合(絕對路徑、含 NUL、含 ".." 片段、經
// symlink 逃逸資料根、對非目錄列舉、對非一般檔案讀寫、未帶 recursive 刪目錄、刪 symlink 本身)。
// 映射到 ERR_BAD_REQUEST。刻意不細分逃逸子類:對外只需「這條路徑不被接受」,細節留在節點側。
var ErrInvalidFilePath = errors.New("agent: invalid file path")

// ErrProtectedFile 表示目標是實例中繼檔(instance.json / native.json / proc.json / server.log*),
// 不開放讀寫刪。映射到 ERR_CONFLICT。理由有二:(1) instance.json 是含明文機密的 spec 快照,
// 可下載即等於洩漏 RCON/webhook 機密;(2) 誤刪 native.json/proc.json 會破壞實例的啟動與收養。
// 這批檔案同時被排除於備份打包之外(見 backup_archive.go isTopLevelMeta),定位一致:它們屬
// 「執行環境」而非「實例資料」。
var ErrProtectedFile = errors.New("agent: 實例中繼檔不可存取")

// ---- 路徑拘束 ----

// cleanRelPath 把使用者提供的相對路徑正規化為以 "/" 分隔的乾淨相對路徑(根為 "")。
// 拒絕:含 NUL、帶磁碟區/UNC 前綴或絕對路徑、任一片段為 ".."。
//
// 反斜線一律先轉為 "/" 再分段檢查:Windows 上 "a\..\..\secret" 若只按 "/" 分段會看不到 ".."
// 片段,而 filepath.Join 之後又確實會逃逸。磁碟區前綴另外判(Windows 的 filepath.IsAbs("/x")
// 為 false、IsAbs("C:x") 亦為 false,單靠 IsAbs 會漏)。
func cleanRelPath(rel string) (string, error) {
	if strings.ContainsRune(rel, '\x00') {
		return "", ErrInvalidFilePath
	}
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", ErrInvalidFilePath
	}
	s := strings.ReplaceAll(rel, `\`, "/")
	if strings.HasPrefix(s, "/") {
		return "", ErrInvalidFilePath // Windows 上 IsAbs("/x")=false,此處補判
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return "", ErrInvalidFilePath
		}
	}
	if s == "" {
		return "", nil
	}
	clean := path.Clean(s)
	if clean == "." {
		return "", nil
	}
	// 冗餘防禦:片段檢查已擋掉一切 "..",Clean 不應再產生逃逸結果。
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrInvalidFilePath
	}
	return clean, nil
}

// withinRoot 回報 target 是否落在 root 之內(含 root 本身)。以 filepath.Rel 的相對結果不以 ".."
// 起頭判定,刻意不用 strings.HasPrefix(target, root):後者有 sibling-prefix 漏洞——root=`X\ab`
// 而 target=`X\abc` 會被誤判為在內。跨磁碟或無法求相對路徑時(Rel 回錯)視為在外。
// 語意對齊 internal/core/template_engine.go 的 withinDir(該處已證明;此處於 agent 套件自持一份,
// 不跨套件相依)。
func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// isLinkLike 回報一個 lstat 結果是否為「連結類」項目:POSIX symlink,或 Windows 的 reparse point
// (目錄接合點 junction、app-exec 存根等)。
//
// 為何不只看 ModeSymlink:Windows 上 os.Lstat 對 junction 回 ModeIrregular 而非 ModeSymlink
// (實測 go1.26.5:mode=?rw-rw-rw-、IsDir()=false、Readlink 卻讀得出目標)。只判 ModeSymlink 會
// 讓 junction 整類逃逸漏網——而 junction 恰是本專案 Windows 部署最容易出現的向量:一般使用者
// 無需特權即可 `mklink /J` 在實例資料根裡接一個指向 C:\ 的目錄。
func isLinkLike(mode fs.FileMode) bool {
	return mode&(fs.ModeSymlink|fs.ModeIrregular) != 0
}

// assertNoLinkComponents 逐段檢查「root 之下(不含 root 本身)到 target」的每一個路徑組件,任一
// 為連結類即拒絕。root 自身的路徑不檢查:它由本程式設定(dataRoot/uuid),非攻擊者可控,且系統
// 目錄本就可能含 junction(如 C:\Documents and Settings),檢查它只會製造偽陽性。
//
// 這是連結層拘束的主力,取代原本單靠 filepath.EvalSymlinks 的作法——EvalSymlinks 在 Windows 上
// 不解析 junction(實測回傳原路徑且不回錯),據以複驗等於沒驗。逐段 Lstat 不依賴解析語意:凡是
// 連結一律不予穿越,故「連結指向根內或根外」都不需要判斷,一律拒絕。
func assertNoLinkComponents(root, target string) error {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return ErrInvalidFilePath
	}
	if rel == "." {
		return nil
	}
	cur := filepath.Clean(root)
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, lerr := os.Lstat(cur)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				return nil // 尾段尚不存在(上傳新檔):其上游組件已逐段驗過,無可穿越之物
			}
			return ErrInvalidFilePath
		}
		if isLinkLike(fi.Mode()) {
			return ErrInvalidFilePath
		}
	}
	return nil
}

// resolveWithinRoot 把相對路徑 rel 解析為 root 之下的絕對路徑,三層拘束皆通過才回傳。
// 每一個檔案操作都必須經它取得路徑,不得自行 filepath.Join。
//
//	1) 宣告層:cleanRelPath 拒絕絕對路徑/NUL/".." 片段後 Join,再以 withinRoot 複驗(擋 sibling-prefix)。
//	2) 連結層(主力):assertNoLinkComponents 逐段 Lstat,root 以下任一路徑組件為 symlink/junction
//	   即拒絕——不穿越連結,也就不可能經連結逃逸。
//	3) 連結層(補強):對 root 與目標各做 EvalSymlinks 後再 withinRoot 複驗。此層擋的是 POSIX
//	   symlink(EvalSymlinks 在此可靠),與第 2 層互為備援;目標尚不存在時改對其父目錄求值再接回
//	   檔名,父目錄亦不存在 → ErrNotFound(本層不自動建目錄)。
//
// 回傳「未解析」的 abs(非 EvalSymlinks 結果):刪除需以 os.Lstat 判定目標本身是否為連結,回傳
// 已解析路徑會使該判定失效。abs 的每個組件與完整解析結果都已在此證明落在 root 內。
//
// 已知限制(TOCTOU):本函式驗完到呼叫端實際 os.Open/Rename/Remove 之間存在競態窗口,同址的
// 惡意行程可在此空檔把某路徑組件抽換成連結以逃逸。Windows 無 openat/O_NOFOLLOW,難以可攜地做到
// 「驗證與開啟同一 handle」;本工具的主要部署為單一擁有者的 localhost 節點(攻擊者需已能在資料
// 根內落檔),blast radius 有限,故列為已知限制、本輪不修。
func resolveWithinRoot(root, rel string) (string, error) {
	if root == "" {
		return "", ErrInvalidFilePath
	}
	clean, err := cleanRelPath(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(root, filepath.FromSlash(clean))

	// 第一層拘束。
	if !withinRoot(root, abs) {
		return "", ErrInvalidFilePath
	}

	// 資料根須存在(不存在=實例未建立/已清除,而非路徑不合法)。
	if _, serr := os.Stat(root); serr != nil {
		if os.IsNotExist(serr) {
			return "", ErrNotFound
		}
		return "", ErrInvalidFilePath
	}

	// 第二層拘束:不穿越任何連結類組件(含 Windows junction)。
	if err := assertNoLinkComponents(root, abs); err != nil {
		return "", err
	}

	// 第三層拘束:EvalSymlinks 備援複驗。
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", ErrInvalidFilePath
	}
	realAbs, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", ErrInvalidFilePath
		}
		realParent, perr := filepath.EvalSymlinks(filepath.Dir(abs))
		if perr != nil {
			if os.IsNotExist(perr) {
				return "", ErrNotFound
			}
			return "", ErrInvalidFilePath
		}
		realAbs = filepath.Join(realParent, filepath.Base(abs))
	}
	if !withinRoot(realRoot, realAbs) {
		return "", ErrInvalidFilePath
	}
	return abs, nil
}

// metaFullyProtected 是「完全不開放存取」的頂層中繼檔(常數本身已是小寫):instance.json(含明文
// 機密的 spec 快照,可下載即等於洩漏 RCON/webhook 機密)、native.json / proc.json(誤刪破壞實例的
// 啟動與收養)。這些不列舉、不可讀寫刪。伺服器日誌(server.log*)另循 isReadOnlyMeta 開放讀取/下載
// (無機密、故障排除常用)但拒寫刪。
var metaFullyProtected = []string{instanceSpecFile, nativeMetaFile, procMetaFile}

// normalizeWinName 把檔名正規化為「Windows 檔案系統語意下的等價名」:轉小寫 + 剝除尾端的 '.' 與
// 空白。NTFS 大小寫不敏感,且開檔時忽略檔名尾端的點與空白(`Instance.json`、`INSTANCE.JSON`、
// `instance.json.`、`instance.json `+尾空白 都開到同一個 instance.json)。名比對一律先過此正規化,
// 否則大小寫敏感的精確比對會被上述別名繞過而讀出機密或刪改中繼檔(實測可繞過)。8.3 短名等
// 「檔名完全不同卻指向同一實體」的別名靠 protectionOf 的 os.SameFile 身分比對補擋。
func normalizeWinName(name string) string {
	return strings.TrimRight(strings.ToLower(name), ". ")
}

// topName 取 clean(cleanRelPath 輸出)的頂層檔名;clean 為空或含 '/'(子目錄下的項目,非中繼檔)
// 時回 ""。中繼檔保護僅適用於資料根頂層。
func topName(clean string) string {
	if clean == "" || strings.Contains(clean, "/") {
		return ""
	}
	return clean
}

// isProtectedMeta 回報 clean 是否為完全封鎖的頂層中繼檔(名比對層;身分比對別名見 protectionOf)。
func isProtectedMeta(clean string) bool {
	n := normalizeWinName(topName(clean))
	if n == "" {
		return false
	}
	for _, m := range metaFullyProtected {
		if n == m {
			return true
		}
	}
	return false
}

// isReadOnlyMeta 回報 clean 是否為頂層伺服器日誌(server.log 及其輪替檔 server.log.N)。
// 開放列舉與下載(無機密、故障排除常用),但不可寫入/刪除——覆寫或刪除執行中伺服器正在寫入的
// 日誌會干擾其記錄與輪替。名比對層;短名別名見 protectionOf。
func isReadOnlyMeta(clean string) bool {
	n := normalizeWinName(topName(clean))
	return n != "" && strings.HasPrefix(n, nativeLogFile)
}

// protectionOf 判定頂層目標 clean(abs 為其已拘束的絕對路徑)的中繼檔保護等級,回傳
// (protected 完全封鎖, readOnly 唯讀)。兩層並用:
//   - 名比對:normalizeWinName 後與中繼檔名比對——擋大小寫/尾點空白等 NTFS 檔名別名,且存在與否
//     無關(跨平台一致:Linux 上 `Instance.json` 這種別名雖是不同檔,仍一律保守封鎖)。
//   - 身分比對:abs 若為既存頂層檔,以 os.SameFile 與各中繼檔的實體比對——擋 8.3 短名(如
//     `INSTAN~1.JSO`)與硬連結:它們檔名不同卻指向同一 file-id,名比對看不出來。
//
// 只對頂層(clean 不含 '/')且既存的目標做身分比對:別名必與中繼檔同層,子目錄項目不可能是其別名;
// 尚不存在的目標(上傳新檔)無實體可比,名比對已足夠——以短名寫一個全新檔案不構成威脅。
func protectionOf(root, clean, abs string) (protected, readOnly bool) {
	if isProtectedMeta(clean) {
		return true, false
	}
	if isReadOnlyMeta(clean) {
		readOnly = true
	}
	if strings.Contains(clean, "/") {
		return protected, readOnly
	}
	ai, err := os.Stat(abs)
	if err != nil {
		return protected, readOnly // 尚不存在:名比對已足夠
	}
	for _, m := range metaFullyProtected {
		if mi, merr := os.Stat(filepath.Join(root, m)); merr == nil && os.SameFile(ai, mi) {
			return true, false
		}
	}
	if !readOnly {
		// 與根下任一 server.log*(含輪替檔)實體相同 → 唯讀(擋日誌的短名/硬連結別名寫刪)。
		if des, derr := os.ReadDir(root); derr == nil {
			for _, de := range des {
				if de.IsDir() || !strings.HasPrefix(normalizeWinName(de.Name()), nativeLogFile) {
					continue
				}
				if mi, merr := os.Stat(filepath.Join(root, de.Name())); merr == nil && os.SameFile(ai, mi) {
					readOnly = true
					break
				}
			}
		}
	}
	return protected, readOnly
}

// ---- 共用實作(docker / native 皆以 instanceDataRoot 為根)----

// listInstanceFiles 列出 root 下 rel 目錄的直接子項。屬性一律取 lstat 語意(os.ReadDir 的 DirEntry
// 不跟隨 symlink),故指向根外的 symlink 只會被「看見」而不會被跟隨讀取。中繼檔不列入(它們不開放
// 存取,列出只會讓前端顯示一個按了必然失敗的項目)。目錄在前、再依名稱排序,回傳順序穩定。
func listInstanceFiles(root, rel string) ([]protocol.FileEntry, error) {
	clean, err := cleanRelPath(rel)
	if err != nil {
		return nil, err
	}
	abs, err := resolveWithinRoot(root, rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: 目標不是目錄", ErrInvalidFilePath)
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.FileEntry, 0, len(des))
	for _, de := range des {
		childRel := path.Join(clean, de.Name())
		if isProtectedMeta(childRel) {
			continue
		}
		fi, ierr := de.Info()
		if ierr != nil {
			continue // 併發移除/權限:略過該項,不因單一子項失敗而整個列舉失敗
		}
		out = append(out, protocol.FileEntry{
			Name:        de.Name(),
			Path:        childRel,
			IsDir:       fi.IsDir(),
			SizeBytes:   fi.Size(),
			ModifiedUTC: fi.ModTime().UTC(),
			IsSymlink:   isLinkLike(fi.Mode()),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// readInstanceFile 開啟 root 下 rel 的一般檔案供串流下載。拒目錄/裝置/具名管道(對映 IconPath 的
// 「須為一般檔案」),拒中繼檔(明文機密外洩)。回傳的 FileEntry 供端點填 Content-Length。
func readInstanceFile(root, rel string) (io.ReadCloser, protocol.FileEntry, error) {
	clean, err := cleanRelPath(rel)
	if err != nil {
		return nil, protocol.FileEntry{}, err
	}
	if clean == "" {
		return nil, protocol.FileEntry{}, fmt.Errorf("%w: 目標不是一般檔案", ErrInvalidFilePath)
	}
	if isProtectedMeta(clean) { // 名比對快擋(存在與否無關)
		return nil, protocol.FileEntry{}, ErrProtectedFile
	}
	abs, err := resolveWithinRoot(root, rel)
	if err != nil {
		return nil, protocol.FileEntry{}, err
	}
	if protected, _ := protectionOf(root, clean, abs); protected { // 身分比對:擋 8.3 短名/硬連結別名
		return nil, protocol.FileEntry{}, ErrProtectedFile
	}
	// 唯讀日誌(server.log*)允許讀取/下載,不在此擋。
	// 先 Open 再對「已開啟的 handle」取 Stat(f.Stat):使回傳的 SizeBytes(端點填 Content-Length)與
	// 實際串流來源同一 handle 一致——避免先 Stat 再 Open 之間檔案被原子替換(遊戲存檔輪替)導致大小
	// 屬舊檔、handle 指新檔而 CopyN 靜默截斷成無法偵測的殘檔(codex P2)。os.Open 跟隨 symlink,其解析
	// 結果已由 resolveWithinRoot 證明落在根內。
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, protocol.FileEntry{}, ErrNotFound
		}
		return nil, protocol.FileEntry{}, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, protocol.FileEntry{}, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, protocol.FileEntry{}, fmt.Errorf("%w: 目標不是一般檔案", ErrInvalidFilePath)
	}
	entry := protocol.FileEntry{
		Name:        path.Base(clean),
		Path:        clean,
		SizeBytes:   info.Size(),
		ModifiedUTC: info.ModTime().UTC(),
	}
	if li, lerr := os.Lstat(abs); lerr == nil {
		entry.IsSymlink = isLinkLike(li.Mode())
	}
	return f, entry, nil
}

// writeInstanceFile 以 r 的內容建立/覆寫 root 下 rel 檔案。父目錄須已存在(本層不自動建目錄:
// 隱式建目錄會讓一個打錯的路徑在資料根下長出垃圾樹)。既有目標須為一般檔案——尤其拒絕覆寫
// symlink:對 symlink 開檔會寫穿到其目標,即使連結指向根內也是使用者未預期的寫入。
//
// 寫入為原子替換:先寫同目錄暫存檔,完整 Close 後 os.Rename 覆蓋目標;任一步失敗刪暫存檔、不動
// 既有目標。直接 O_TRUNC 就地覆寫會在傳輸中斷時先清空目標 → 損毀既有世界/設定檔(不可復原)。
func writeInstanceFile(root, rel string, r io.Reader) error {
	clean, err := cleanRelPath(rel)
	if err != nil {
		return err
	}
	if clean == "" {
		return fmt.Errorf("%w: 未指定目標檔名", ErrInvalidFilePath)
	}
	if isProtectedMeta(clean) || isReadOnlyMeta(clean) { // 名比對快擋:中繼檔不可寫、日誌唯讀
		return ErrProtectedFile
	}
	abs, err := resolveWithinRoot(root, rel)
	if err != nil {
		return err
	}
	if protected, readOnly := protectionOf(root, clean, abs); protected || readOnly { // 身分比對:擋短名/硬連結別名
		return ErrProtectedFile
	}
	if li, lerr := os.Lstat(abs); lerr == nil && !li.Mode().IsRegular() {
		return fmt.Errorf("%w: 目標已存在且不是一般檔案", ErrInvalidFilePath)
	}
	// 暫存檔落在目標同目錄(resolveWithinRoot 已證明該目錄在根內),以 os.CreateTemp 保證檔名唯一且
	// 不與中繼檔撞;同卷 rename 為原子操作。父目錄不存在的情形已由 resolveWithinRoot 提前回 ErrNotFound。
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(abs)+".gsm-upload-*")
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	tmpName := tmp.Name()
	if _, cerr := io.Copy(tmp, r); cerr != nil {
		tmp.Close()
		os.Remove(tmpName)
		return cerr
	}
	if cerr := tmp.Close(); cerr != nil {
		os.Remove(tmpName)
		return cerr
	}
	// CreateTemp 建檔為 0600;對齊舊有 O_CREATE 0644(遊戲伺服器與本程式同使用者,無需更嚴)。
	_ = os.Chmod(tmpName, 0o644)
	if rerr := os.Rename(tmpName, abs); rerr != nil {
		os.Remove(tmpName)
		return rerr
	}
	return nil
}

// deleteInstanceFile 刪除 root 下 rel。拒刪資料根本身與中繼檔;以 os.Lstat 判定型別(不跟隨),
// symlink 本身一律拒刪——跟隨刪除會刪到連結指向的根外目標,而刪連結本身又易與「刪掉那個資料夾」
// 的使用者意圖不符,故一律拒絕並由前端說明。目錄需 recursive=true 才遞迴刪。
func deleteInstanceFile(root, rel string, recursive bool) error {
	clean, err := cleanRelPath(rel)
	if err != nil {
		return err
	}
	if clean == "" {
		return fmt.Errorf("%w: 不可刪除實例資料根", ErrInvalidFilePath)
	}
	if isProtectedMeta(clean) || isReadOnlyMeta(clean) { // 名比對快擋:中繼檔不可刪、日誌唯讀
		return ErrProtectedFile
	}
	abs, err := resolveWithinRoot(root, rel)
	if err != nil {
		return err
	}
	if protected, readOnly := protectionOf(root, clean, abs); protected || readOnly { // 身分比對:擋短名/硬連結別名
		return ErrProtectedFile
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if isLinkLike(info.Mode()) {
		return fmt.Errorf("%w: 不可刪除符號連結", ErrInvalidFilePath)
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("%w: 刪除目錄需 recursive=true", ErrInvalidFilePath)
		}
		return os.RemoveAll(abs)
	}
	return os.Remove(abs)
}

// ---- DockerBackend / NativeBackend 薄包裝(同一份共用實作,根皆為 instanceDataRoot)----

func (b *DockerBackend) ListFiles(ctx context.Context, uuid, rel string) ([]protocol.FileEntry, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := validateInstanceUUID(uuid); err != nil {
		return nil, err
	}
	return listInstanceFiles(b.instanceDataRoot(uuid), rel)
}

func (b *DockerBackend) ReadFile(ctx context.Context, uuid, rel string) (io.ReadCloser, protocol.FileEntry, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, protocol.FileEntry{}, err
	}
	if err := validateInstanceUUID(uuid); err != nil {
		return nil, protocol.FileEntry{}, err
	}
	return readInstanceFile(b.instanceDataRoot(uuid), rel)
}

func (b *DockerBackend) WriteFile(ctx context.Context, uuid, rel string, r io.Reader) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := validateInstanceUUID(uuid); err != nil {
		return err
	}
	return writeInstanceFile(b.instanceDataRoot(uuid), rel, r)
}

func (b *DockerBackend) DeleteFile(ctx context.Context, uuid, rel string, recursive bool) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := validateInstanceUUID(uuid); err != nil {
		return err
	}
	return deleteInstanceFile(b.instanceDataRoot(uuid), rel, recursive)
}

func (b *NativeBackend) ListFiles(ctx context.Context, uuid, rel string) ([]protocol.FileEntry, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := validateInstanceUUID(uuid); err != nil {
		return nil, err
	}
	return listInstanceFiles(b.instanceDataRoot(uuid), rel)
}

func (b *NativeBackend) ReadFile(ctx context.Context, uuid, rel string) (io.ReadCloser, protocol.FileEntry, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, protocol.FileEntry{}, err
	}
	if err := validateInstanceUUID(uuid); err != nil {
		return nil, protocol.FileEntry{}, err
	}
	return readInstanceFile(b.instanceDataRoot(uuid), rel)
}

func (b *NativeBackend) WriteFile(ctx context.Context, uuid, rel string, r io.Reader) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := validateInstanceUUID(uuid); err != nil {
		return err
	}
	return writeInstanceFile(b.instanceDataRoot(uuid), rel, r)
}

func (b *NativeBackend) DeleteFile(ctx context.Context, uuid, rel string, recursive bool) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := validateInstanceUUID(uuid); err != nil {
		return err
	}
	return deleteInstanceFile(b.instanceDataRoot(uuid), rel, recursive)
}
