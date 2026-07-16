package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// native 行程日誌落檔與 tail(native-backend R7)。
//
// 設計:rollingLog 同時(a)以 JSONL 逐行持久化到實例根下的 server.log(滿 maxBytes 即滾動,
// 保留 maxFiles 個檔)、(b)對「即時追蹤(follow)」訂閱者記憶體扇出。滾動只搬動磁碟檔、不觸碰
// 訂閱者通道,故 tail(follow)串流跨滾動不中斷(R7 驗收:日誌檔滾動不中斷串流)。歷史(tail N)
// 由磁碟檔重建。預設上限 64 MiB × 5 檔(design 定值)。
//
// 收養情境(T11:agent 重啟後接管已存在行程)沒有記憶體寫入端,其 follow 需改為檔案輪詢——
// 屬 T11 職責;本任務只處理「本 agent 生命週期內啟動的行程」的即時扇出。
const (
	nativeLogFile      = "server.log"
	defaultLogMaxBytes = 64 << 20 // 64 MiB
	defaultLogMaxFiles = 5        // server.log + server.log.1 .. server.log.4
	logSubBuffer       = 1024     // 每個 follow 訂閱者的緩衝
)

// rollingLog 是一個實例的滾動日誌 writer + 即時扇出核心。併發安全。
type rollingLog struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	maxFiles int

	f    *os.File
	size int64

	subs   map[int]chan LogLine
	subSeq int
	closed bool
}

// newRollingLog 開啟(或續接)實例根 dir 下的 server.log。maxBytes/maxFiles<=0 時採預設。
func newRollingLog(dir string, maxBytes int64, maxFiles int) (*rollingLog, error) {
	if maxBytes <= 0 {
		maxBytes = defaultLogMaxBytes
	}
	if maxFiles <= 0 {
		maxFiles = defaultLogMaxFiles
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, nativeLogFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	var size int64
	if info, serr := f.Stat(); serr == nil {
		size = info.Size()
	}
	return &rollingLog{
		dir:      dir,
		maxBytes: maxBytes,
		maxFiles: maxFiles,
		f:        f,
		size:     size,
		subs:     make(map[int]chan LogLine),
	}, nil
}

// write 落檔一行(JSONL)並扇出給即時訂閱者。滾動只發生在磁碟層,不影響訂閱通道。
func (r *rollingLog) write(stream, text string) {
	ln := LogLine{TsUTC: time.Now().UTC(), Stream: stream, Line: text}
	r.mu.Lock()
	if !r.closed && r.f != nil {
		if data, err := json.Marshal(ln); err == nil {
			data = append(data, '\n')
			if n, werr := r.f.Write(data); werr == nil {
				r.size += int64(n)
				if r.size >= r.maxBytes {
					r.rotate()
				}
			}
		}
	}
	for _, ch := range r.subs {
		select {
		case ch <- ln:
		default: // 訂閱者過慢:丟棄本行以不阻塞落檔(磁碟仍完整,消費端可重讀)
		}
	}
	r.mu.Unlock()
}

// rotate 搬動磁碟檔(呼叫端須持 r.mu):server.log.(N-1) 逐級後移、server.log→.1、開新 server.log。
func (r *rollingLog) rotate() {
	_ = r.f.Close()
	base := filepath.Join(r.dir, nativeLogFile)
	rotatedMax := r.maxFiles - 1 // .1 .. .rotatedMax
	_ = os.Remove(base + "." + strconv.Itoa(rotatedMax))
	for i := rotatedMax - 1; i >= 1; i-- {
		_ = os.Rename(base+"."+strconv.Itoa(i), base+"."+strconv.Itoa(i+1))
	}
	_ = os.Rename(base, base+".1")
	f, err := os.OpenFile(base, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		r.f = nil // 開檔失敗:停止落檔,扇出仍運作
		return
	}
	r.f = f
	r.size = 0
}

// subscribe 註冊一個即時 follow 訂閱者,回傳其 id 與通道。
func (r *rollingLog) subscribe() (int, <-chan LogLine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subSeq++
	id := r.subSeq
	ch := make(chan LogLine, logSubBuffer)
	r.subs[id] = ch
	return id, ch
}

// unsubscribe 移除訂閱者(不關閉通道,避免與 write 的 send 競態;GC 回收)。
func (r *rollingLog) unsubscribe(id int) {
	r.mu.Lock()
	delete(r.subs, id)
	r.mu.Unlock()
}

// close 停止落檔並關閉檔案(不影響已扇出的訂閱者;串流由 done/ctx 收束)。冪等。
func (r *rollingLog) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}

// readLogHistory 由磁碟檔(舊→新:server.log.N .. server.log.1, server.log)重建 LogLine 歷史;
// tail>0 時只回最後 tail 行。非 JSONL 行(如收養前的裸文字)以整行當 stdout 收納。
func readLogHistory(dir string, tail int) []LogLine {
	base := filepath.Join(dir, nativeLogFile)
	// 收集所有輪替檔並依序號由大到小(舊到新)。
	rotated := []int{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, nativeLogFile+".") {
			if n, err := strconv.Atoi(strings.TrimPrefix(name, nativeLogFile+".")); err == nil {
				rotated = append(rotated, n)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(rotated)))

	var lines []LogLine
	for _, n := range rotated {
		lines = append(lines, decodeLogFile(base+"."+strconv.Itoa(n))...)
	}
	lines = append(lines, decodeLogFile(base)...)

	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return lines
}

// decodeLogFile 讀一個日誌檔,逐行反序列化為 LogLine;非 JSONL 行退回整行 stdout。
func decodeLogFile(path string) []LogLine {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []LogLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var ln LogLine
		if err := json.Unmarshal(raw, &ln); err == nil && ln.Stream != "" {
			out = append(out, ln)
			continue
		}
		out = append(out, LogLine{TsUTC: time.Now().UTC(), Stream: "stdout", Line: string(raw)})
	}
	return out
}

// captureStream 逐行掃描行程的 stdout/stderr pipe,寫入 rollingLog(每行一筆)。至 EOF(行程結束)返回。
func captureStream(rl *rollingLog, stream string, rc io.ReadCloser) {
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		rl.write(stream, strings.TrimRight(sc.Text(), "\r"))
	}
}

// nativeLogStream 是 Logs 回傳的串流:先送歷史,follow 時續送即時扇出,至 ctx 取消 / Close /
// 行程結束(done)收束。
type nativeLogStream struct {
	out     chan LogLine
	cancel  context.CancelFunc
	onClose func()

	mu  sync.Mutex
	err error
}

// newNativeLogStream 建立串流。history 為開場補送;sub 為即時通道(nil=僅歷史,送完即關);
// done 於行程結束時關閉(nil=無行程,follow 亦僅歷史);unsub 於收束時解除訂閱。
func newNativeLogStream(ctx context.Context, history []LogLine, sub <-chan LogLine, done <-chan struct{}, unsub func()) *nativeLogStream {
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithCancel(ctx)
	s := &nativeLogStream{out: make(chan LogLine), cancel: cancel, onClose: unsub}
	go func() {
		defer close(s.out)
		if unsub != nil {
			defer unsub()
		}
		for _, ln := range history {
			select {
			case s.out <- ln:
			case <-cctx.Done():
				s.setErr(cctx.Err())
				return
			}
		}
		if sub == nil {
			return // 僅歷史
		}
		for {
			select {
			case ln := <-sub:
				select {
				case s.out <- ln:
				case <-cctx.Done():
					s.setErr(cctx.Err())
					return
				}
			case <-done:
				// 行程結束:排掉尚在緩衝的即時行後收束。
				for {
					select {
					case ln := <-sub:
						select {
						case s.out <- ln:
						case <-cctx.Done():
							return
						}
					default:
						return
					}
				}
			case <-cctx.Done():
				s.setErr(cctx.Err())
				return
			}
		}
	}()
	return s
}

func (s *nativeLogStream) Lines() <-chan LogLine { return s.out }

func (s *nativeLogStream) Close() error {
	s.cancel()
	return nil
}

func (s *nativeLogStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *nativeLogStream) setErr(err error) {
	if err == nil || err == context.Canceled {
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}
