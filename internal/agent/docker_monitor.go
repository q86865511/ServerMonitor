package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"servermonitor/internal/protocol"
)

// Logs 回傳容器 stdout/stderr 串流(R6)。Docker 多工串流由本地解多工(8-byte header),
// 每行標注 stream 與時間戳(Timestamps)。
func (b *DockerBackend) Logs(ctx context.Context, id protocol.RuntimeID, opts LogOpts) (LogStream, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	lopts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Timestamps: true,
	}
	if opts.Tail > 0 {
		lopts.Tail = strconv.Itoa(opts.Tail)
	} else {
		lopts.Tail = "all"
	}
	if !opts.Since.IsZero() {
		lopts.Since = strconv.FormatInt(opts.Since.UTC().Unix(), 10)
	}
	rc, err := b.cli.ContainerLogs(ctx, string(id), lopts)
	if err != nil {
		return nil, mapDockerErr(err)
	}
	return newDockerLogStream(ctx, rc), nil
}

// Stats 取樣資源使用(R6):CPU% 正規化為容器 CPU/可用核心;記憶體扣除 page cache;
// 資料磁碟用量量測 host bind mount 路徑(明確與 Docker block I/O 區分)。
func (b *DockerBackend) Stats(ctx context.Context, id protocol.RuntimeID) (protocol.ResourceStats, error) {
	if err := ctxErr(ctx); err != nil {
		return protocol.ResourceStats{}, err
	}
	resp, err := b.cli.ContainerStats(ctx, string(id), false)
	if err != nil {
		return protocol.ResourceStats{}, mapDockerErr(err)
	}
	defer resp.Body.Close()
	var v container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return protocol.ResourceStats{}, fmt.Errorf("解析 stats 失敗: %w", err)
	}
	stats := protocol.ResourceStats{
		TsUTC:       time.Now().UTC(),
		CPUPercent:  calcCPUPercent(v),
		MemoryBytes: calcMemUsage(v),
		MemoryLimit: v.MemoryStats.Limit,
	}
	// 資料磁碟:由 gsm.uuid 定位 host bind mount 根量測。
	if j, err := b.cli.ContainerInspect(ctx, string(id)); err == nil && j.Config != nil {
		if uuid := j.Config.Labels[labelUUID]; uuid != "" {
			if used, derr := b.dataDiskUsage(uuid); derr == nil {
				stats.DataDiskBytes = &used
			}
		}
	}
	return stats, nil
}

// ExecProcess 在容器內執行程序(緩衝式;非遊戲指令)。
func (b *DockerBackend) ExecProcess(ctx context.Context, id protocol.RuntimeID, cmd ExecCmd) (ExecResult, error) {
	if err := ctxErr(ctx); err != nil {
		return ExecResult{}, err
	}
	ec, err := b.cli.ContainerExecCreate(ctx, string(id), container.ExecOptions{
		Cmd:          cmd.Cmd,
		Env:          cmd.Env,
		WorkingDir:   cmd.WorkDir,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, mapDockerErr(err)
	}
	att, err := b.cli.ContainerExecAttach(ctx, ec.ID, container.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, mapDockerErr(err)
	}
	defer att.Close()

	var outBuf, errBuf bytes.Buffer
	if _, err := stdcopy.StdCopy(&outBuf, &errBuf, att.Reader); err != nil {
		return ExecResult{}, fmt.Errorf("讀取 exec 輸出失敗: %w", err)
	}
	insp, err := b.cli.ContainerExecInspect(ctx, ec.ID)
	if err != nil {
		return ExecResult{}, mapDockerErr(err)
	}
	return ExecResult{ExitCode: insp.ExitCode, Stdout: outBuf.String(), Stderr: errBuf.String()}, nil
}

// calcCPUPercent 由 stats 算正規化 CPU%(容器 CPU/可用核心;0..100,100=用滿全部核心)。
// (cpuDelta/systemDelta) 已把容器 CPU 正規化到全機容量,*100 即得。
func calcCPUPercent(v container.StatsResponse) float64 {
	cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage) - float64(v.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(v.CPUStats.SystemUsage) - float64(v.PreCPUStats.SystemUsage)
	if sysDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	pct := (cpuDelta / sysDelta) * 100.0
	if pct < 0 {
		return 0
	}
	return pct
}

// calcMemUsage 由 stats 算實際記憶體用量(扣除 page cache)。
func calcMemUsage(v container.StatsResponse) uint64 {
	usage := v.MemoryStats.Usage
	for _, key := range []string{"inactive_file", "total_inactive_file", "cache"} {
		if val, ok := v.MemoryStats.Stats[key]; ok {
			if val <= usage {
				usage -= val
			}
			break
		}
	}
	return usage
}

// dataDiskUsage 遞迴量測實例 host 資料根的檔案總量(排除 spec 快照與 .gsm-* 暫存目錄)。
func (b *DockerBackend) dataDiskUsage(uuid string) (uint64, error) {
	root := b.instanceDataRoot(uuid)
	var total uint64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(filepath.Base(p), ".gsm-") {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, rerr := filepath.Rel(root, p); rerr == nil && rel == instanceSpecFile {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}

// ---- log 串流 ----

// dockerLogStream 解多工 Docker log 串流為 LogLine。單一 pump goroutine 擁有來源與生命週期。
type dockerLogStream struct {
	lines     chan LogLine
	src       io.ReadCloser
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	mu  sync.Mutex
	err error
}

func newDockerLogStream(ctx context.Context, rc io.ReadCloser) *dockerLogStream {
	cctx, cancel := context.WithCancel(ctx)
	s := &dockerLogStream{
		lines:  make(chan LogLine, 256),
		src:    rc,
		ctx:    cctx,
		cancel: cancel,
	}
	go s.pump()
	return s
}

// pump 讀 Docker 多工串流:每幀 8-byte header(byte0=stream,byte4:8=big-endian size)+ payload,
// 依 stream 累積跨幀部分行、逐行輸出。ctx 取消或來源結束時收束並關閉 lines。
func (s *dockerLogStream) pump() {
	defer close(s.lines)
	defer s.src.Close()

	go func() { // ctx 取消時關來源以喚醒阻塞的讀取
		<-s.ctx.Done()
		s.src.Close()
	}()

	r := bufio.NewReader(s.src)
	var hdr [8]byte
	partial := map[byte]*bytes.Buffer{}
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			s.flushPartial(partial)
			s.setErr(err)
			return
		}
		stype := hdr[0]
		n := binary.BigEndian.Uint32(hdr[4:8])
		if n == 0 {
			continue
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			s.setErr(err)
			return
		}
		buf := partial[stype]
		if buf == nil {
			buf = &bytes.Buffer{}
			partial[stype] = buf
		}
		buf.Write(payload)
		for {
			data := buf.Bytes()
			idx := bytes.IndexByte(data, '\n')
			if idx < 0 {
				break
			}
			line := string(data[:idx])
			rest := append([]byte(nil), data[idx+1:]...)
			buf.Reset()
			buf.Write(rest)
			if !s.emit(streamName(stype), line) {
				return
			}
		}
	}
}

// flushPartial 在串流結束時輸出各 stream 殘留的最後一行(無換行結尾)。
func (s *dockerLogStream) flushPartial(partial map[byte]*bytes.Buffer) {
	for stype, buf := range partial {
		if buf.Len() == 0 {
			continue
		}
		s.emit(streamName(stype), buf.String())
	}
}

func (s *dockerLogStream) emit(stream, raw string) bool {
	raw = strings.TrimRight(raw, "\r")
	ts, text := splitLogTimestamp(raw)
	select {
	case s.lines <- LogLine{TsUTC: ts, Stream: stream, Line: text}:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *dockerLogStream) Lines() <-chan LogLine { return s.lines }

func (s *dockerLogStream) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.src.Close()
	})
	return nil
}

func (s *dockerLogStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *dockerLogStream) setErr(err error) {
	if err == nil || err == io.EOF || err == io.ErrClosedPipe || err == context.Canceled {
		return
	}
	if s.ctx.Err() != nil { // 因取消/Close 導致的來源錯誤不算異常
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func streamName(stype byte) string {
	if stype == 2 {
		return "stderr"
	}
	return "stdout"
}

// splitLogTimestamp 由 "RFC3339Nano <text>" 拆出時間與內容;無法解析則回零時間與原行。
func splitLogTimestamp(line string) (time.Time, string) {
	i := strings.IndexByte(line, ' ')
	if i <= 0 {
		return time.Time{}, line
	}
	ts, err := time.Parse(time.RFC3339Nano, line[:i])
	if err != nil {
		return time.Time{}, line
	}
	return ts.UTC(), line[i+1:]
}
