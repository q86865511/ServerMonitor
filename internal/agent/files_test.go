package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"servermonitor/internal/protocol"
)

// 階段 5:伺服器檔案管理的安全測試。本檔的核心命題只有一句——「任何 rel 都不得使檔案操作觸及
// 實例資料根之外」。攻擊向量分四類逐一驗證:
//  1. 語法層:絕對路徑、磁碟區前綴、".." 片段、NUL、sibling-prefix(root=`X\ab` vs `X\abc`)。
//  2. 連結層:目錄/檔案 symlink 指向根外。Windows 上 os.Symlink 需特權,目錄連結退回
//     `mklink /J`(junction,一般使用者即可建)——這條 fallback 實際抓出了一個真 bug:
//     filepath.EvalSymlinks 不解析 junction、os.Lstat 對其回 ModeIrregular 而非 ModeSymlink,
//     原本單靠 EvalSymlinks 的第二層拘束在 Windows 上形同虛設(見 files.go isLinkLike)。
//  3. 型別層:刪除 symlink 本身(不跟隨)、未帶 recursive 刪目錄、對非一般檔案讀寫。
//  4. 資產層:實例中繼檔(instance.json/native.json/proc.json/server.log)不可讀/寫/刪。
//
// 每個案例除了驗回錯,還驗「根外的受害目標仍原封不動」——只驗錯誤碼無法證明沒有副作用。

// fileTestRoot 建一個實例資料根 <tmp>/data/<uuid>,並在 <tmp>/secret 放一個「根外受害者」。
type fileTestRoot struct {
	tmp      string
	dataRoot string // <tmp>/data
	root     string // <tmp>/data/<uuid>
	secret   string // <tmp>/secret(根外目錄)
	victim   string // <tmp>/secret/victim.txt
}

const fileTestUUID = "u-files"

func newFileTestRoot(t *testing.T) fileTestRoot {
	t.Helper()
	tmp := t.TempDir()
	fr := fileTestRoot{
		tmp:      tmp,
		dataRoot: filepath.Join(tmp, "data"),
		root:     filepath.Join(tmp, "data", fileTestUUID),
		secret:   filepath.Join(tmp, "secret"),
	}
	fr.victim = filepath.Join(fr.secret, "victim.txt")
	mkdirAllT(t, filepath.Join(fr.root, "world"))
	mkdirAllT(t, fr.secret)
	writeFileT(t, filepath.Join(fr.root, "server.properties"), "motd=hi")
	writeFileT(t, filepath.Join(fr.root, "world", "level.dat"), "world-data")
	writeFileT(t, fr.victim, "top-secret")
	return fr
}

func mkdirAllT(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFileT(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// assertVictimIntact 確認根外受害者檔仍存在且內容未變(證明沒有副作用,而不只是回了錯誤)。
func (fr fileTestRoot) assertVictimIntact(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(fr.victim)
	if err != nil {
		t.Fatalf("根外檔案不應被動到: %v", err)
	}
	if string(b) != "top-secret" {
		t.Fatalf("根外檔案內容被改寫: %q", string(b))
	}
}

// ---- (a) 語法層:.. / 絕對路徑 / NUL / sibling-prefix,四種操作各驗 ----

// traversalPaths 是應被一律拒絕的 rel。含 Windows 與 POSIX 兩種分隔,以及 URL 解碼後常見的變體。
func traversalPaths() []string {
	return []string{
		"../secret/victim.txt",
		`..\secret\victim.txt`,
		"world/../../secret/victim.txt",
		`world\..\..\secret\victim.txt`,
		"..",
		"../",
		"/etc/passwd",
		`\windows\system32\config`,
		`C:\Windows\System32\drivers\etc\hosts`,
		"C:secret",
		`\\server\share\x`,
		"world/\x00evil",
		"a/../../..",
	}
}

func TestFiles_RejectsTraversal_AllOperations(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}

	for _, rel := range traversalPaths() {
		if _, err := b.ListFiles(ctx, fileTestUUID, rel); !errors.Is(err, ErrInvalidFilePath) {
			t.Errorf("ListFiles(%q) want ErrInvalidFilePath, got %v", rel, err)
		}
		if _, _, err := b.ReadFile(ctx, fileTestUUID, rel); !errors.Is(err, ErrInvalidFilePath) {
			t.Errorf("ReadFile(%q) want ErrInvalidFilePath, got %v", rel, err)
		}
		if err := b.WriteFile(ctx, fileTestUUID, rel, strings.NewReader("pwned")); !errors.Is(err, ErrInvalidFilePath) {
			t.Errorf("WriteFile(%q) want ErrInvalidFilePath, got %v", rel, err)
		}
		if err := b.DeleteFile(ctx, fileTestUUID, rel, true); !errors.Is(err, ErrInvalidFilePath) {
			t.Errorf("DeleteFile(%q) want ErrInvalidFilePath, got %v", rel, err)
		}
	}
	fr.assertVictimIntact(t)
	if _, err := os.Stat(fr.secret); err != nil {
		t.Fatalf("根外目錄不應被刪: %v", err)
	}
}

// uuid 本身亦是路徑片段:不安全 uuid 於進入路徑解析前即被 validateInstanceUUID 擋下。
func TestFiles_RejectsUnsafeUUID(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}

	for _, uuid := range []string{"", "..", `..\..\secret`, "../../secret", ".hidden", "a/b"} {
		if _, err := b.ListFiles(ctx, uuid, ""); !errors.Is(err, ErrInvalidInstanceUUID) {
			t.Errorf("ListFiles(uuid=%q) want ErrInvalidInstanceUUID, got %v", uuid, err)
		}
		if err := b.DeleteFile(ctx, uuid, "victim.txt", true); !errors.Is(err, ErrInvalidInstanceUUID) {
			t.Errorf("DeleteFile(uuid=%q) want ErrInvalidInstanceUUID, got %v", uuid, err)
		}
	}
	fr.assertVictimIntact(t)
}

// sibling-prefix:root=<data>/u-files,而 <data>/u-files-evil 以 root 為字串前綴。字串前綴式的
// 邊界檢查會誤放行,filepath.Rel 式不會。此處直接驗拘束助手(rel 無法表達,只能繞過 Join 直測)。
func TestResolveWithinRoot_RejectsSiblingPrefix(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "ab")
	sibling := filepath.Join(tmp, "abc")
	mkdirAllT(t, root)
	mkdirAllT(t, sibling)

	if withinRoot(root, sibling) {
		t.Fatal("withinRoot 誤把 sibling-prefix 目錄判為在內(字串前綴漏洞)")
	}
	if withinRoot(root, filepath.Join(sibling, "x.txt")) {
		t.Fatal("withinRoot 誤把 sibling-prefix 下的檔案判為在內")
	}
	if !withinRoot(root, root) {
		t.Fatal("withinRoot 應允許根本身")
	}
	if !withinRoot(root, filepath.Join(root, "a", "b")) {
		t.Fatal("withinRoot 應允許根下的路徑")
	}
}

// ---- (b) 連結層:symlink/junction 逃逸被拒 ----

// mkSymlink 建立 symlink;Windows 未開啟開發者模式/無 SeCreateSymbolicLink 權限時會失敗 → skip。
func mkSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("本機無法建立 symlink(Windows 需開發者模式或系統管理員權限): %v", err)
	}
}

// mkDirLink 建立指向目錄的連結。Windows 上 os.Symlink 需特權,但「目錄接合點(junction)」一般
// 使用者即可建立——而 junction 正是本專案 Windows 部署最實際的逃逸向量(使用者可自行在資料根裡
// 接一個指向 C:\ 的接合點),故此處退回 mklink /J,使符號連結拘束在本機真的被驗到而非一律 skip。
func mkDirLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS != "windows" {
		t.Skipf("本機無法建立目錄 symlink")
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("本機無法建立 symlink 或 junction: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

// 資料根內的 symlink 指向根外目錄:list/read/write/delete 皆須拒絕,且根外目標不受影響。
func TestFiles_RejectsSymlinkEscape(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}
	mkDirLink(t, fr.secret, filepath.Join(fr.root, "escape"))

	// 經 symlink 目錄讀根外檔案。
	if _, _, err := b.ReadFile(ctx, fileTestUUID, "escape/victim.txt"); !errors.Is(err, ErrInvalidFilePath) {
		t.Errorf("ReadFile 經 symlink 逃逸 want ErrInvalidFilePath, got %v", err)
	}
	// 經 symlink 目錄刪根外檔案。
	if err := b.DeleteFile(ctx, fileTestUUID, "escape/victim.txt", false); !errors.Is(err, ErrInvalidFilePath) {
		t.Errorf("DeleteFile 經 symlink 逃逸 want ErrInvalidFilePath, got %v", err)
	}
	// 經 symlink 目錄寫入根外(新檔,走「目標不存在→對父目錄求值」分支)。
	if err := b.WriteFile(ctx, fileTestUUID, "escape/planted.txt", strings.NewReader("x")); !errors.Is(err, ErrInvalidFilePath) {
		t.Errorf("WriteFile 經 symlink 逃逸 want ErrInvalidFilePath, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fr.secret, "planted.txt")); !os.IsNotExist(err) {
		t.Fatalf("不應在根外植入檔案, stat err=%v", err)
	}
	// 列舉 symlink 目錄本身。
	if _, err := b.ListFiles(ctx, fileTestUUID, "escape"); !errors.Is(err, ErrInvalidFilePath) {
		t.Errorf("ListFiles 經 symlink 逃逸 want ErrInvalidFilePath, got %v", err)
	}
	fr.assertVictimIntact(t)
}

// 檔案 symlink 直接指向根外檔案:讀取須拒(否則等同任意檔案讀取)。
func TestFiles_RejectsFileSymlinkEscape(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}
	mkSymlink(t, fr.victim, filepath.Join(fr.root, "leak.txt"))

	if _, _, err := b.ReadFile(ctx, fileTestUUID, "leak.txt"); !errors.Is(err, ErrInvalidFilePath) {
		t.Errorf("ReadFile 檔案 symlink 逃逸 want ErrInvalidFilePath, got %v", err)
	}
	if err := b.WriteFile(ctx, fileTestUUID, "leak.txt", strings.NewReader("pwned")); !errors.Is(err, ErrInvalidFilePath) {
		t.Errorf("WriteFile 檔案 symlink 逃逸 want ErrInvalidFilePath, got %v", err)
	}
	fr.assertVictimIntact(t)
}

// ---- (c) 刪除 symlink 本身被拒(不跟隨)----

// 指向「根內」的 symlink 通得過逃逸拘束,但刪除仍須拒絕:跟隨刪會刪到目標、刪連結本身又與
// 使用者「刪掉這個資料夾」的意圖不符,故一律拒絕並保留連結與目標。
func TestFiles_RejectsDeletingSymlinkItself(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}
	link := filepath.Join(fr.root, "world-link")
	mkDirLink(t, filepath.Join(fr.root, "world"), link)

	if err := b.DeleteFile(ctx, fileTestUUID, "world-link", false); !errors.Is(err, ErrInvalidFilePath) {
		t.Fatalf("DeleteFile(symlink) want ErrInvalidFilePath, got %v", err)
	}
	if err := b.DeleteFile(ctx, fileTestUUID, "world-link", true); !errors.Is(err, ErrInvalidFilePath) {
		t.Fatalf("DeleteFile(symlink, recursive) want ErrInvalidFilePath, got %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink 本身不應被刪: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fr.root, "world", "level.dat")); err != nil {
		t.Fatalf("symlink 目標不應被刪: %v", err)
	}
	// 指向根外的 symlink 亦不可刪(先被逃逸拘束擋下)。
	mkDirLink(t, fr.secret, filepath.Join(fr.root, "escape"))
	if err := b.DeleteFile(ctx, fileTestUUID, "escape", true); !errors.Is(err, ErrInvalidFilePath) {
		t.Fatalf("DeleteFile(根外 symlink) want ErrInvalidFilePath, got %v", err)
	}
	fr.assertVictimIntact(t)
}

// ---- (d) 實例中繼檔不可讀/寫/刪,且不列入列舉 ----

func TestFiles_ProtectsInstanceMetaFiles(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}
	// 完全封鎖(不列舉、不可讀寫刪):instance.json 含明文機密、native/proc.json 為 runtime 中繼。
	fullyProtected := []string{instanceSpecFile, nativeMetaFile, procMetaFile}
	// 唯讀(可列舉、可下載,但不可寫/刪):伺服器日誌無機密,下載做故障排除為典型用途。
	readOnly := []string{nativeLogFile, nativeLogFile + ".1"}
	metas := append(append([]string{}, fullyProtected...), readOnly...)
	for _, m := range metas {
		writeFileT(t, filepath.Join(fr.root, m), "meta-"+m)
	}

	for _, m := range fullyProtected {
		if _, _, err := b.ReadFile(ctx, fileTestUUID, m); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("ReadFile(%q) want ErrProtectedFile, got %v", m, err)
		}
		if err := b.WriteFile(ctx, fileTestUUID, m, strings.NewReader("pwned")); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("WriteFile(%q) want ErrProtectedFile, got %v", m, err)
		}
		if err := b.DeleteFile(ctx, fileTestUUID, m, false); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("DeleteFile(%q) want ErrProtectedFile, got %v", m, err)
		}
	}
	for _, m := range readOnly {
		rc, _, rerr := b.ReadFile(ctx, fileTestUUID, m)
		if rerr != nil {
			t.Errorf("ReadFile(%q) 伺服器日誌應可下載, got %v", m, rerr)
		} else {
			_ = rc.Close()
		}
		if err := b.WriteFile(ctx, fileTestUUID, m, strings.NewReader("pwned")); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("WriteFile(%q) 日誌應唯讀, got %v", m, err)
		}
		if err := b.DeleteFile(ctx, fileTestUUID, m, false); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("DeleteFile(%q) 日誌應唯讀, got %v", m, err)
		}
	}
	for _, m := range metas {
		got, err := os.ReadFile(filepath.Join(fr.root, m))
		if err != nil || string(got) != "meta-"+m {
			t.Errorf("中繼檔 %q 應原封不動, content=%q err=%v", m, string(got), err)
		}
	}

	// 完全封鎖者不列入列舉(前端不該看到按了必然失敗的項目);日誌則應可見(可下載)。
	entries, err := b.ListFiles(ctx, fileTestUUID, "")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		seen[e.Name] = true
	}
	for _, m := range fullyProtected {
		if seen[m] {
			t.Errorf("列舉不應含完全封鎖的中繼檔 %q", m)
		}
	}
	for _, m := range readOnly {
		if !seen[m] {
			t.Errorf("列舉應含可下載的伺服器日誌 %q", m)
		}
	}

	// 子目錄下的同名檔不是中繼檔(僅頂層受保護),應可正常操作。
	writeFileT(t, filepath.Join(fr.root, "world", instanceSpecFile), "user-data")
	rc, _, err := b.ReadFile(ctx, fileTestUUID, "world/"+instanceSpecFile)
	if err != nil {
		t.Fatalf("子目錄同名檔應可讀: %v", err)
	}
	_ = rc.Close()
	if err := b.DeleteFile(ctx, fileTestUUID, "world/"+instanceSpecFile, false); err != nil {
		t.Fatalf("子目錄同名檔應可刪: %v", err)
	}
}

// 必修 1(名比對層):Windows 檔名別名(大小寫、尾點、尾空白)不得繞過中繼檔保護。NTFS 大小寫
// 不敏感且開檔忽略尾端點與空白,故 `Instance.json`/`INSTANCE.JSON`/`instance.json.`/尾空白 都會
// 開到同一個 instance.json;名比對經 normalizeWinName 後一律擋下,且跨平台一致(Linux 上這些別名
// 雖是不同檔,仍保守封鎖)。原檔內容須不變。
func TestFiles_ProtectsMetaFile_NameAliases(t *testing.T) {
	ctx := context.Background()
	for _, base := range []string{instanceSpecFile, nativeMetaFile, procMetaFile} {
		fr := newFileTestRoot(t)
		b := &NativeBackend{dataRoot: fr.dataRoot}
		writeFileT(t, filepath.Join(fr.root, base), "secret-"+base)
		aliases := []string{
			strings.ToUpper(base[:1]) + base[1:], // Instance.json
			strings.ToUpper(base),                // INSTANCE.JSON
			base + ".",                           // instance.json.
			base + " ",                           // instance.json + 尾空白
			base + "  ",
			base + ". ",
		}
		for _, a := range aliases {
			if _, _, err := b.ReadFile(ctx, fileTestUUID, a); !errors.Is(err, ErrProtectedFile) {
				t.Errorf("ReadFile(%q) want ErrProtectedFile, got %v", a, err)
			}
			if err := b.WriteFile(ctx, fileTestUUID, a, strings.NewReader("pwned")); !errors.Is(err, ErrProtectedFile) {
				t.Errorf("WriteFile(%q) want ErrProtectedFile, got %v", a, err)
			}
			if err := b.DeleteFile(ctx, fileTestUUID, a, false); !errors.Is(err, ErrProtectedFile) {
				t.Errorf("DeleteFile(%q) want ErrProtectedFile, got %v", a, err)
			}
		}
		if got, _ := os.ReadFile(filepath.Join(fr.root, base)); string(got) != "secret-"+base {
			t.Errorf("%q 經別名被改寫: %q", base, string(got))
		}
	}

	// server.log 的名別名(如 Server.log / server.log. )read 可成功,write/delete 為 ErrProtectedFile。
	fr := newFileTestRoot(t)
	b := &NativeBackend{dataRoot: fr.dataRoot}
	writeFileT(t, filepath.Join(fr.root, nativeLogFile), "log-data")
	for _, a := range []string{"Server.log", "SERVER.LOG", "server.log.", "server.log "} {
		rc, _, rerr := b.ReadFile(ctx, fileTestUUID, a)
		if rerr != nil {
			t.Errorf("ReadFile(%q) 日誌別名應可讀, got %v", a, rerr)
		} else {
			_ = rc.Close()
		}
		if err := b.WriteFile(ctx, fileTestUUID, a, strings.NewReader("x")); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("WriteFile(%q) 日誌別名應唯讀, got %v", a, err)
		}
		if err := b.DeleteFile(ctx, fileTestUUID, a, false); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("DeleteFile(%q) 日誌別名應唯讀, got %v", a, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(fr.root, nativeLogFile)); string(got) != "log-data" {
		t.Errorf("server.log 經別名被改寫: %q", string(got))
	}
}

// 必修 1(身分比對層):以硬連結建立「檔名完全不符任何中繼檔、卻指向同一實體」的別名(模擬 8.3
// 短名效果),須經 os.SameFile 命中而擋下。這是名比對抓不到、唯一驗到身分比對路徑的正向測試;
// 硬連結在 NTFS 與一般 Linux fs 皆可建,建不出(如跨裝置)則 t.Skip。
func TestFiles_ProtectsMetaFile_IdentityAlias(t *testing.T) {
	ctx := context.Background()
	fr := newFileTestRoot(t)
	b := &NativeBackend{dataRoot: fr.dataRoot}
	meta := filepath.Join(fr.root, instanceSpecFile)
	writeFileT(t, meta, "secret-spec")
	alias := filepath.Join(fr.root, "alias.dat") // 名不符任何中繼檔
	if err := os.Link(meta, alias); err != nil {
		t.Skipf("本機無法建立硬連結(身分比對正向測試需要): %v", err)
	}
	if _, _, err := b.ReadFile(ctx, fileTestUUID, "alias.dat"); !errors.Is(err, ErrProtectedFile) {
		t.Errorf("ReadFile(硬連結別名) want ErrProtectedFile, got %v", err)
	}
	if err := b.WriteFile(ctx, fileTestUUID, "alias.dat", strings.NewReader("pwned")); !errors.Is(err, ErrProtectedFile) {
		t.Errorf("WriteFile(硬連結別名) want ErrProtectedFile, got %v", err)
	}
	if err := b.DeleteFile(ctx, fileTestUUID, "alias.dat", false); !errors.Is(err, ErrProtectedFile) {
		t.Errorf("DeleteFile(硬連結別名) want ErrProtectedFile, got %v", err)
	}
	if got, _ := os.ReadFile(meta); string(got) != "secret-spec" {
		t.Errorf("instance.json 經硬連結別名被改寫: %q", string(got))
	}

	// server.log 的硬連結別名:唯讀身分比對(可讀,寫/刪為 ErrProtectedFile)。
	logf := filepath.Join(fr.root, nativeLogFile)
	writeFileT(t, logf, "log-data")
	logAlias := filepath.Join(fr.root, "logalias.txt")
	if err := os.Link(logf, logAlias); err == nil {
		rc, _, rerr := b.ReadFile(ctx, fileTestUUID, "logalias.txt")
		if rerr != nil {
			t.Errorf("ReadFile(日誌硬連結別名) 應可讀, got %v", rerr)
		} else {
			_ = rc.Close()
		}
		if err := b.WriteFile(ctx, fileTestUUID, "logalias.txt", strings.NewReader("x")); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("WriteFile(日誌硬連結別名) 應唯讀, got %v", err)
		}
		if err := b.DeleteFile(ctx, fileTestUUID, "logalias.txt", false); !errors.Is(err, ErrProtectedFile) {
			t.Errorf("DeleteFile(日誌硬連結別名) 應唯讀, got %v", err)
		}
	}
}

// partialThenErrReader 先送出一段內容,再回傳錯誤——模擬上傳/傳輸中途中斷。
type partialThenErrReader struct {
	chunk []byte
	sent  bool
	err   error
}

func (p *partialThenErrReader) Read(b []byte) (int, error) {
	if !p.sent {
		p.sent = true
		return copy(b, p.chunk), nil
	}
	return 0, p.err
}

// 必修 2(原子上傳):覆寫既有檔時若傳輸中斷,既有目標內容須不變、同目錄不留殘檔。
func TestFiles_UploadAtomic_InterruptPreservesTarget(t *testing.T) {
	ctx := context.Background()
	fr := newFileTestRoot(t)
	b := &NativeBackend{dataRoot: fr.dataRoot}
	target := filepath.Join(fr.root, "server.properties")
	orig, _ := os.ReadFile(target)

	r := &partialThenErrReader{chunk: []byte("partial-new-content"), err: io.ErrUnexpectedEOF}
	if err := b.WriteFile(ctx, fileTestUUID, "server.properties", r); err == nil {
		t.Fatal("中斷的上傳應回錯")
	}
	if got, _ := os.ReadFile(target); string(got) != string(orig) {
		t.Errorf("中斷後既有目標被損毀: got %q want %q", string(got), string(orig))
	}
	des, _ := os.ReadDir(fr.root)
	for _, de := range des {
		if strings.Contains(de.Name(), "gsm-upload") {
			t.Errorf("殘留上傳暫存檔: %s", de.Name())
		}
	}

	// 成功路徑仍原子替換為新內容。
	if err := b.WriteFile(ctx, fileTestUUID, "server.properties", strings.NewReader("brand-new")); err != nil {
		t.Fatalf("正常上傳: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "brand-new" {
		t.Errorf("成功上傳後內容 %q", string(got))
	}
}

// 不可刪除實例資料根本身(空 rel)。
func TestFiles_RejectsDeletingDataRoot(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}

	for _, rel := range []string{"", ".", "/", `\`} {
		if err := b.DeleteFile(ctx, fileTestUUID, rel, true); !errors.Is(err, ErrInvalidFilePath) {
			t.Errorf("DeleteFile(root=%q) want ErrInvalidFilePath, got %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fr.root, "server.properties")); err != nil {
		t.Fatalf("資料根不應被刪: %v", err)
	}
}

// ---- 正常路徑(happy path)與型別拘束 ----

func TestFiles_HappyPath(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}

	// 列根:目錄在前、再依名稱排序。
	entries, err := b.ListFiles(ctx, fileTestUUID, "")
	if err != nil {
		t.Fatalf("ListFiles(root): %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "world" || !entries[0].IsDir || entries[1].Name != "server.properties" {
		t.Fatalf("列舉順序/內容不符: %+v", entries)
	}
	if entries[1].Path != "server.properties" || entries[1].SizeBytes != int64(len("motd=hi")) {
		t.Fatalf("FileEntry 欄位不符: %+v", entries[1])
	}
	if entries[0].Path != "world" {
		t.Fatalf("目錄 Path 應為相對路徑: %+v", entries[0])
	}

	// 列子目錄:Path 為 "/" 分隔的相對路徑。
	sub, err := b.ListFiles(ctx, fileTestUUID, "world")
	if err != nil || len(sub) != 1 || sub[0].Path != "world/level.dat" {
		t.Fatalf("ListFiles(world) 不符: %+v err=%v", sub, err)
	}
	// 反斜線分隔的 rel 亦應被接受(前端契約為 "/",但代理容忍)。
	if _, err := b.ListFiles(ctx, fileTestUUID, `world`); err != nil {
		t.Fatalf("ListFiles(world) 反斜線容忍: %v", err)
	}

	// 讀檔。
	rc, entry, err := b.ReadFile(ctx, fileTestUUID, "world/level.dat")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "world-data" || entry.Name != "level.dat" || entry.SizeBytes != 10 {
		t.Fatalf("ReadFile 內容/中繼不符: %q %+v", string(data), entry)
	}

	// 寫檔(新建 + 覆寫)。
	if err := b.WriteFile(ctx, fileTestUUID, "world/new.txt", strings.NewReader("v1")); err != nil {
		t.Fatalf("WriteFile(new): %v", err)
	}
	if err := b.WriteFile(ctx, fileTestUUID, "world/new.txt", strings.NewReader("v2-longer")); err != nil {
		t.Fatalf("WriteFile(overwrite): %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(fr.root, "world", "new.txt"))
	if string(got) != "v2-longer" {
		t.Fatalf("覆寫後內容 %q", string(got))
	}

	// 刪檔。
	if err := b.DeleteFile(ctx, fileTestUUID, "world/new.txt", false); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fr.root, "world", "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("檔案應已刪除: %v", err)
	}
	// 再刪 → ErrNotFound。
	if err := b.DeleteFile(ctx, fileTestUUID, "world/new.txt", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重複刪除 want ErrNotFound, got %v", err)
	}
}

func TestFiles_TypeConstraints(t *testing.T) {
	fr := newFileTestRoot(t)
	ctx := context.Background()
	b := &NativeBackend{dataRoot: fr.dataRoot}

	// 刪目錄需 recursive=true。
	if err := b.DeleteFile(ctx, fileTestUUID, "world", false); !errors.Is(err, ErrInvalidFilePath) {
		t.Fatalf("刪目錄未帶 recursive want ErrInvalidFilePath, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(fr.root, "world")); err != nil {
		t.Fatalf("目錄不應被刪: %v", err)
	}
	if err := b.DeleteFile(ctx, fileTestUUID, "world", true); err != nil {
		t.Fatalf("DeleteFile(recursive): %v", err)
	}
	if _, err := os.Stat(filepath.Join(fr.root, "world")); !os.IsNotExist(err) {
		t.Fatalf("目錄應已遞迴刪除: %v", err)
	}

	// 讀目錄 → ErrInvalidFilePath;列舉檔案 → ErrInvalidFilePath。
	mkdirAllT(t, filepath.Join(fr.root, "logs"))
	if _, _, err := b.ReadFile(ctx, fileTestUUID, "logs"); !errors.Is(err, ErrInvalidFilePath) {
		t.Fatalf("ReadFile(dir) want ErrInvalidFilePath, got %v", err)
	}
	if _, err := b.ListFiles(ctx, fileTestUUID, "server.properties"); !errors.Is(err, ErrInvalidFilePath) {
		t.Fatalf("ListFiles(file) want ErrInvalidFilePath, got %v", err)
	}
	// 覆寫目錄 → ErrInvalidFilePath。
	if err := b.WriteFile(ctx, fileTestUUID, "logs", strings.NewReader("x")); !errors.Is(err, ErrInvalidFilePath) {
		t.Fatalf("WriteFile(dir) want ErrInvalidFilePath, got %v", err)
	}
	// 父目錄不存在 → ErrNotFound(不自動建目錄)。
	if err := b.WriteFile(ctx, fileTestUUID, "no/such/dir/f.txt", strings.NewReader("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("WriteFile(缺父目錄) want ErrNotFound, got %v", err)
	}
	// 實例資料根不存在 → ErrNotFound。
	if _, err := b.ListFiles(ctx, "u-does-not-exist", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListFiles(未知實例) want ErrNotFound, got %v", err)
	}
}

// ---- (e) 跨後端契約:docker 與 native 對同一資料根行為一致 ----

// docker 與 native 的實例資料落在宿主端同一位置,兩者的 FileManager 必須是同一份實作的薄包裝。
// 本測試以同一個資料根跑同一組操作,逐項比對兩後端結果——任一方日後被就地改寫(例如 docker 改走
// exec 進容器)而語意漂移,此測試即紅。不需 Docker daemon:檔案操作不觸及 docker client。
func TestFileManagerContract_DockerNativeEquivalent(t *testing.T) {
	ctx := context.Background()
	backends := []struct {
		name string
		make func(dataRoot string) FileManager
	}{
		{"docker", func(dataRoot string) FileManager { return &DockerBackend{dataRoot: dataRoot} }},
		{"native", func(dataRoot string) FileManager { return &NativeBackend{dataRoot: dataRoot} }},
	}

	type result struct {
		listNames []string
		readData  string
		errs      map[string]string
	}
	results := make([]result, 0, len(backends))

	for _, bc := range backends {
		fr := newFileTestRoot(t)
		writeFileT(t, filepath.Join(fr.root, instanceSpecFile), "secret-spec")
		fm := bc.make(fr.dataRoot)
		res := result{errs: map[string]string{}}

		entries, err := fm.ListFiles(ctx, fileTestUUID, "")
		if err != nil {
			t.Fatalf("[%s] ListFiles: %v", bc.name, err)
		}
		for _, e := range entries {
			res.listNames = append(res.listNames, e.Name)
		}

		rc, _, err := fm.ReadFile(ctx, fileTestUUID, "server.properties")
		if err != nil {
			t.Fatalf("[%s] ReadFile: %v", bc.name, err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		res.readData = string(data)

		if err := fm.WriteFile(ctx, fileTestUUID, "world/added.txt", strings.NewReader("added")); err != nil {
			t.Fatalf("[%s] WriteFile: %v", bc.name, err)
		}
		if err := fm.DeleteFile(ctx, fileTestUUID, "world/added.txt", false); err != nil {
			t.Fatalf("[%s] DeleteFile: %v", bc.name, err)
		}

		// 拒絕情境的錯誤語意也須一致。
		record := func(key string, err error) {
			switch {
			case err == nil:
				res.errs[key] = "<nil>"
			case errors.Is(err, ErrInvalidFilePath):
				res.errs[key] = "invalid-path"
			case errors.Is(err, ErrProtectedFile):
				res.errs[key] = "protected"
			case errors.Is(err, ErrNotFound):
				res.errs[key] = "not-found"
			case errors.Is(err, ErrInvalidInstanceUUID):
				res.errs[key] = "invalid-uuid"
			default:
				res.errs[key] = "other:" + err.Error()
			}
		}
		_, _, rerr := fm.ReadFile(ctx, fileTestUUID, "../secret/victim.txt")
		record("read-traversal", rerr)
		record("delete-traversal", fm.DeleteFile(ctx, fileTestUUID, `..\secret`, true))
		record("write-meta", fm.WriteFile(ctx, fileTestUUID, instanceSpecFile, strings.NewReader("x")))
		record("delete-meta", fm.DeleteFile(ctx, fileTestUUID, instanceSpecFile, false))
		record("delete-dir-norecursive", fm.DeleteFile(ctx, fileTestUUID, "world", false))
		record("delete-root", fm.DeleteFile(ctx, fileTestUUID, "", true))
		record("bad-uuid", fm.DeleteFile(ctx, `..\..`, "x", true))
		_, lerr := fm.ListFiles(ctx, "u-missing", "")
		record("list-missing-instance", lerr)

		fr.assertVictimIntact(t)
		results = append(results, res)
	}

	want, got := results[0], results[1]
	if strings.Join(want.listNames, ",") != strings.Join(got.listNames, ",") {
		t.Errorf("列舉結果不一致: docker=%v native=%v", want.listNames, got.listNames)
	}
	if want.readData != got.readData {
		t.Errorf("讀檔結果不一致: docker=%q native=%q", want.readData, got.readData)
	}
	for k, v := range want.errs {
		if got.errs[k] != v {
			t.Errorf("錯誤語意不一致 %s: docker=%s native=%s", k, v, got.errs[k])
		}
	}
	// 順帶鎖住預期值本身(避免兩邊「一致地錯」)。
	for k, v := range map[string]string{
		"read-traversal":         "invalid-path",
		"delete-traversal":       "invalid-path",
		"write-meta":             "protected",
		"delete-meta":            "protected",
		"delete-dir-norecursive": "invalid-path",
		"delete-root":            "invalid-path",
		"bad-uuid":               "invalid-uuid",
		"list-missing-instance":  "not-found",
	} {
		if want.errs[k] != v {
			t.Errorf("錯誤語意 %s want %s, got %s", k, v, want.errs[k])
		}
	}
}

// ---- 端點層:信任邊界的第一道防禦 ----

// fileServerBackend 是實作 FileManager 的受測後端替身(其餘 RuntimeBackend 方法由 fakeBackend 供應),
// 檔案操作委派共用實作,故端點測試驗到的是真實路徑拘束而非假件。
type fileServerBackend struct {
	*fakeBackend
	dataRoot string
}

func (f *fileServerBackend) root(uuid string) string { return filepath.Join(f.dataRoot, uuid) }

func (f *fileServerBackend) ListFiles(_ context.Context, uuid, rel string) ([]protocol.FileEntry, error) {
	if err := validateInstanceUUID(uuid); err != nil {
		return nil, err
	}
	return listInstanceFiles(f.root(uuid), rel)
}

func (f *fileServerBackend) ReadFile(_ context.Context, uuid, rel string) (io.ReadCloser, protocol.FileEntry, error) {
	if err := validateInstanceUUID(uuid); err != nil {
		return nil, protocol.FileEntry{}, err
	}
	return readInstanceFile(f.root(uuid), rel)
}

func (f *fileServerBackend) WriteFile(_ context.Context, uuid, rel string, r io.Reader) error {
	if err := validateInstanceUUID(uuid); err != nil {
		return err
	}
	return writeInstanceFile(f.root(uuid), rel, r)
}

func (f *fileServerBackend) DeleteFile(_ context.Context, uuid, rel string, recursive bool) error {
	if err := validateInstanceUUID(uuid); err != nil {
		return err
	}
	return deleteInstanceFile(f.root(uuid), rel, recursive)
}

func TestFilesEndpoints(t *testing.T) {
	fr := newFileTestRoot(t)
	writeFileT(t, filepath.Join(fr.root, instanceSpecFile), "secret-spec")
	backend := &fileServerBackend{fakeBackend: newFakeBackend("files"), dataRoot: fr.dataRoot}
	hs := newTestServer(t, backend)
	base := hs.URL + apiBase + "/instances/" + fileTestUUID + "/files"

	do := func(method, url string, body io.Reader) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, url, body)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}

	// GET 列舉:200 + 中繼檔已濾除。
	resp, body := do(http.MethodGet, base+"?path=", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), instanceSpecFile) {
		t.Fatalf("列舉不應含中繼檔: %s", body)
	}

	// GET 下載:200 + octet-stream + Content-Length。
	resp, body = do(http.MethodGet, base+"/content?path=server.properties", nil)
	if resp.StatusCode != http.StatusOK || string(body) != "motd=hi" {
		t.Fatalf("download status=%d body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("download Content-Type=%q", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "7" {
		t.Fatalf("download Content-Length=%q want 7", cl)
	}

	// PUT 上傳:204。
	resp, body = do(http.MethodPut, base+"?path=world/up.txt", strings.NewReader("uploaded"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status=%d body=%s", resp.StatusCode, body)
	}
	if got, _ := os.ReadFile(filepath.Join(fr.root, "world", "up.txt")); string(got) != "uploaded" {
		t.Fatalf("上傳內容 %q", string(got))
	}

	// DELETE:204;再刪 404。
	resp, body = do(http.MethodDelete, base+"?path=world/up.txt", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", resp.StatusCode, body)
	}
	resp, _ = do(http.MethodDelete, base+"?path=world/up.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重複 delete status=%d want 404", resp.StatusCode)
	}

	// 路徑遍歷:四個端點皆 400(含 URL 編碼的 ..\ 與 ../)。
	for _, q := range []string{"..%2Fsecret%2Fvictim.txt", "..%5Csecret", "%2Fetc%2Fpasswd", "world%2F..%2F..%2Fsecret"} {
		for _, tc := range []struct{ method, url string }{
			{http.MethodGet, base + "?path=" + q},
			{http.MethodGet, base + "/content?path=" + q},
			{http.MethodPut, base + "?path=" + q},
			{http.MethodDelete, base + "?path=" + q + "&recursive=true"},
		} {
			resp, body := do(tc.method, tc.url, strings.NewReader("pwned"))
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s %s status=%d want 400 body=%s", tc.method, tc.url, resp.StatusCode, body)
			}
		}
	}
	fr.assertVictimIntact(t)

	// 中繼檔:讀/寫/刪皆 409(ERR_CONFLICT)。
	for _, tc := range []struct{ method, url string }{
		{http.MethodGet, base + "/content?path=" + instanceSpecFile},
		{http.MethodPut, base + "?path=" + instanceSpecFile},
		{http.MethodDelete, base + "?path=" + instanceSpecFile},
	} {
		resp, body := do(tc.method, tc.url, strings.NewReader("pwned"))
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s %s status=%d want 409 body=%s", tc.method, tc.url, resp.StatusCode, body)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(fr.root, instanceSpecFile)); string(got) != "secret-spec" {
		t.Fatalf("中繼檔被改寫: %q", string(got))
	}

	// 不安全 uuid:400(在後端之前即由 validateInstanceUUID 擋下)。用 ".hidden" 而非 ".."——
	// 後者會先被 HTTP 層的路徑正規化吃掉(根本到不了 handler),證明不了本層防禦。
	resp, body = do(http.MethodGet, hs.URL+apiBase+"/instances/.hidden/files?path=", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsafe uuid status=%d want 400 body=%s", resp.StatusCode, body)
	}

	// 刪目錄未帶 recursive → 400;帶了 → 204。
	resp, _ = do(http.MethodDelete, base+"?path=world", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("刪目錄未帶 recursive status=%d want 400", resp.StatusCode)
	}
	resp, _ = do(http.MethodDelete, base+"?path=world&recursive=true", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("刪目錄帶 recursive status=%d want 204", resp.StatusCode)
	}
}

// 必修 3(上傳上限):body 超過 maxUploadBytes → 413,且不留下任何檔案(原子寫的暫存檔已刪)。
// maxUploadBytes 為 var 以便此處注入小值,免真的送 16GiB。
func TestFilesEndpoints_UploadSizeLimit(t *testing.T) {
	fr := newFileTestRoot(t)
	backend := &fileServerBackend{fakeBackend: newFakeBackend("files"), dataRoot: fr.dataRoot}
	hs := newTestServer(t, backend)

	orig := maxUploadBytes
	maxUploadBytes = 8
	t.Cleanup(func() { maxUploadBytes = orig })

	u := hs.URL + apiBase + "/instances/" + fileTestUUID + "/files?path=big.bin"
	req, _ := http.NewRequest(http.MethodPut, u, strings.NewReader("way more than eight bytes"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限上傳 status=%d want 413", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(fr.root, "big.bin")); !os.IsNotExist(err) {
		t.Fatalf("超限上傳不應留下檔案: %v", err)
	}
	des, _ := os.ReadDir(fr.root)
	for _, de := range des {
		if strings.Contains(de.Name(), "gsm-upload") {
			t.Fatalf("超限上傳殘留暫存檔: %s", de.Name())
		}
	}

	// 上限內的上傳仍正常成功(確認上限機制未誤傷正常路徑)。
	maxUploadBytes = 1 << 20
	req2, _ := http.NewRequest(http.MethodPut, hs.URL+apiBase+"/instances/"+fileTestUUID+"/files?path=ok.txt", strings.NewReader("small"))
	req2.Header.Set("Authorization", "Bearer "+testToken)
	resp2, err := hs.Client().Do(req2)
	if err != nil {
		t.Fatalf("do2: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("上限內上傳 status=%d want 204", resp2.StatusCode)
	}
}

// 後端未實作 FileManager(如僅 MockBackend)→ 501 ERR_UNSUPPORTED。
func TestFilesEndpoints_UnsupportedBackend(t *testing.T) {
	hs := newTestServer(t, NewMockBackend())
	base := hs.URL + apiBase + "/instances/u-x/files"
	for _, tc := range []struct{ method, url string }{
		{http.MethodGet, base + "?path="},
		{http.MethodGet, base + "/content?path=a.txt"},
		{http.MethodPut, base + "?path=a.txt"},
		{http.MethodDelete, base + "?path=a.txt"},
	} {
		req, _ := http.NewRequest(tc.method, tc.url, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s status=%d want 501", tc.method, tc.url, resp.StatusCode)
		}
	}
}

// dispatchBackend 轉發:無任何子後端實作 FileManager → ErrUnsupported;有則路由過去。
func TestDispatch_FileManager(t *testing.T) {
	ctx := context.Background()
	plain := newFakeBackend("native") // 不實作 FileManager
	d, err := NewDispatchBackend(plain, nil)
	if err != nil {
		t.Fatalf("NewDispatchBackend: %v", err)
	}
	if _, err := d.ListFiles(ctx, "u-x", ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ListFiles want ErrUnsupported, got %v", err)
	}
	if _, _, err := d.ReadFile(ctx, "u-x", "a"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ReadFile want ErrUnsupported, got %v", err)
	}
	if err := d.WriteFile(ctx, "u-x", "a", strings.NewReader("x")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("WriteFile want ErrUnsupported, got %v", err)
	}
	if err := d.DeleteFile(ctx, "u-x", "a", false); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DeleteFile want ErrUnsupported, got %v", err)
	}
	_ = d.Close()

	fr := newFileTestRoot(t)
	fm := &fileServerBackend{fakeBackend: newFakeBackend("docker"), dataRoot: fr.dataRoot}
	d2, err := NewDispatchBackend(plain, fm)
	if err != nil {
		t.Fatalf("NewDispatchBackend: %v", err)
	}
	defer d2.Close()
	entries, err := d2.ListFiles(ctx, fileTestUUID, "")
	if err != nil || len(entries) != 2 {
		t.Fatalf("dispatch ListFiles 轉發不符: %+v err=%v", entries, err)
	}
}

// cleanRelPath 的單元邊界(正規化與拒絕規則)。
func TestCleanRelPath(t *testing.T) {
	// 空/當前目錄視為資料根本身;"/" 與 "\" 屬「絕對路徑」一律拒(不視為根)。
	for _, rel := range []string{"", ".", "./"} {
		got, err := cleanRelPath(rel)
		if err != nil || got != "" {
			t.Errorf("cleanRelPath(%q) = %q, %v; want \"\", nil", rel, got, err)
		}
	}
	for _, rel := range []string{"/", `\`, "//", "/a"} {
		if _, err := cleanRelPath(rel); !errors.Is(err, ErrInvalidFilePath) {
			t.Errorf("cleanRelPath(%q) want ErrInvalidFilePath, got %v", rel, err)
		}
	}
	for in, want := range map[string]string{
		"a/b":       "a/b",
		`a\b`:       "a/b",
		"./a//b/":   "a/b",
		"a/./b":     "a/b",
		"world/x.t": "world/x.t",
	} {
		got, err := cleanRelPath(in)
		if err != nil || got != want {
			t.Errorf("cleanRelPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, rel := range append(traversalPaths(), "a/../../b") {
		if _, err := cleanRelPath(rel); !errors.Is(err, ErrInvalidFilePath) {
			t.Errorf("cleanRelPath(%q) want ErrInvalidFilePath, got %v", rel, err)
		}
	}
	if runtime.GOOS != "windows" {
		return
	}
	// Windows 專屬:磁碟區相對路徑(C:x)與 UNC。
	for _, rel := range []string{`C:x`, `\\host\share`, `c:/Windows`} {
		if _, err := cleanRelPath(rel); !errors.Is(err, ErrInvalidFilePath) {
			t.Errorf("cleanRelPath(%q) want ErrInvalidFilePath, got %v", rel, err)
		}
	}
}

// httptest 需要的最小型別檢查:確保受測替身確實滿足 FileManager。
var _ FileManager = (*fileServerBackend)(nil)

var _ = httptest.NewServer
