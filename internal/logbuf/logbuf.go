// Package logbuf 提供进程内日志环形缓冲，供 Web/API/CLI 查看最近日志。
package logbuf

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// Entry 是一条内存日志记录。
type Entry struct {
	Time  string `json:"time"`
	Line  string `json:"line"`
	Level string `json:"level,omitempty"`
}

// Ring 是固定容量的线程安全日志环形缓冲。
type Ring struct {
	mu      sync.RWMutex
	entries []Entry
	next    int
	full    bool
}

// New 创建指定容量的日志环形缓冲。
//
// 参数：
//   - capacity: int，最多保存的日志条数；小于 1 时修正为 1。
//
// 返回值：
//   - *Ring，可并发写入和读取的环形缓冲。
//
// 错误情况：无；非法容量会被修正。
func New(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{entries: make([]Entry, capacity)}
}

// Add 写入一条日志。
//
// 参数：
//   - line: string，日志原文；首尾空白会被清理。
//
// 返回值：无。
//
// 错误情况：
//   - 空行会被忽略；其它情况不返回错误。
func (r *Ring) Add(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[r.next] = Entry{
		Time:  time.Now().Format(time.RFC3339),
		Line:  line,
		Level: detectLevel(line),
	}
	r.next = (r.next + 1) % len(r.entries)
	if r.next == 0 {
		r.full = true
	}
}

// Tail 返回最近 n 条日志，可按 level 做宽松过滤。
//
// 参数：
//   - n: int，期望返回条数；小于等于 0 或超过容量时返回全部可用记录。
//   - level: string，过滤等级；空字符串表示不过滤。
//
// 返回值：
//   - []Entry，按时间从旧到新排列的日志副本。
//
// 错误情况：无；未知 level 只会导致匹配不到记录。
func (r *Ring) Tail(n int, level string) []Entry {
	level = strings.ToLower(strings.TrimSpace(level))
	r.mu.RLock()
	defer r.mu.RUnlock()

	var ordered []Entry
	if r.full {
		ordered = append(ordered, r.entries[r.next:]...)
		ordered = append(ordered, r.entries[:r.next]...)
	} else {
		ordered = append(ordered, r.entries[:r.next]...)
	}
	filtered := make([]Entry, 0, len(ordered))
	for _, entry := range ordered {
		if level == "" || entry.Level == level || strings.Contains(strings.ToLower(entry.Line), level) {
			filtered = append(filtered, entry)
		}
	}
	if n <= 0 || n > len(filtered) {
		n = len(filtered)
	}
	out := make([]Entry, n)
	copy(out, filtered[len(filtered)-n:])
	return out
}

// Writer 把 io.Writer 写入拆分为日志行并写入 Ring。
type Writer struct {
	ring *Ring
	mu   sync.Mutex
	buf  bytes.Buffer
}

// NewWriter 创建写入 Ring 的 io.Writer。
//
// 参数：
//   - ring: *Ring，目标日志缓冲。
//
// 返回值：
//   - io.Writer，适合传给 log.SetOutput 或 io.MultiWriter。
//
// 错误情况：
//   - ring 为 nil 时仍返回 writer，但写入会被丢弃。
func NewWriter(ring *Ring) io.Writer {
	return &Writer{ring: ring}
}

// Write 实现 io.Writer，把字节流按行写入环形缓冲。
//
// 参数：
//   - p: []byte，日志字节。
//
// 返回值：
//   - int，已消费字节数。
//   - error，固定为 nil。
//
// 错误情况：
//   - 不返回错误；最后一段无换行内容会暂存在缓冲里等待下一次写入。
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			_, _ = w.buf.Write(p)
			break
		}
		_, _ = w.buf.Write(p[:i])
		if w.ring != nil {
			w.ring.Add(w.buf.String())
		}
		w.buf.Reset()
		p = p[i+1:]
	}
	return n, nil
}

// detectLevel 从日志文本里推断等级。
//
// 参数：
//   - line: string，日志原文。
//
// 返回值：
//   - string，`debug|info|warning|error` 之一；无法识别时默认为 info。
//
// 错误情况：无；这是展示层过滤辅助，不影响日志写入。
func detectLevel(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "level=debug") || strings.Contains(lower, "[debug]"):
		return "debug"
	case strings.Contains(lower, "level=warning") || strings.Contains(lower, "[warn]") || strings.Contains(lower, "[warning]"):
		return "warning"
	case strings.Contains(lower, "level=error") || strings.Contains(lower, "[error]") || strings.Contains(lower, " error:"):
		return "error"
	default:
		return "info"
	}
}

// Default 是 proxyd 进程内共享日志缓冲。
var Default = New(1000)
