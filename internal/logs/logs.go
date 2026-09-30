// Package logs captures app output into size-rotated files, keeps a ring
// buffer of recent lines, and fans lines out to live subscribers (the
// dashboard streams them over SSE).
//
// File format, one line per entry:
//
//	2026-09-27T16:11:39.123Z out   hello world
//	2026-09-27T16:11:39.456Z err   something went wrong
//	2026-09-27T16:11:40.000Z dootd started (pid 1234)
package logs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Streams.
const (
	Stdout = "out"
	Stderr = "err"
	System = "dootd" // lines written by dootd about the app (start, exit, OOM)
)

// Defaults (docs/architecture.md).
const (
	DefaultMaxSize  = 10 << 20
	DefaultKeep     = 3
	DefaultRingSize = 1000
	MaxLineBytes    = 16 << 10 // longer lines are split
)

const timeFormat = "2006-01-02T15:04:05.000Z"

// Line is one log entry.
type Line struct {
	Time   time.Time
	Stream string
	Text   string
}

// String renders the line in file format (without trailing newline).
func (l Line) String() string {
	return fmt.Sprintf("%s %-5s %s", l.Time.UTC().Format(timeFormat), l.Stream, l.Text)
}

// Options configures a Log. Zero values use the defaults.
type Options struct {
	MaxSize  int64
	Keep     int
	RingSize int
}

// Log is a concurrency-safe rotating log with a ring buffer.
type Log struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	maxSize  int64
	keep     int
	ring     []Line
	next     int
	full     bool
	subs     map[chan Line]struct{}
	errShown bool
	closed   bool
}

// Open opens (appending to) the log at path and preloads the ring buffer
// from the tail of the existing file so history survives dootd restarts.
func Open(path string, o Options) (*Log, error) {
	if o.MaxSize <= 0 {
		o.MaxSize = DefaultMaxSize
	}
	if o.Keep <= 0 {
		o.Keep = DefaultKeep
	}
	if o.RingSize <= 0 {
		o.RingSize = DefaultRingSize
	}
	l := &Log{
		path:    path,
		maxSize: o.MaxSize,
		keep:    o.Keep,
		ring:    make([]Line, o.RingSize),
		subs:    map[chan Line]struct{}{},
	}
	if err := l.openFile(); err != nil {
		return nil, err
	}
	l.preload()
	return l, nil
}

func (l *Log) openFile() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("logs: open %s: %w", l.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("logs: stat %s: %w", l.path, err)
	}
	l.f, l.size = f, st.Size()
	return nil
}

// preload fills the ring from the tail of app.log.1 + app.log (the newest
// ~512 KB), so history survives dootd restarts and recent rotations.
func (l *Log) preload() {
	const budget = 512 << 10
	cur := readTail(l.path, budget)
	var prev []byte
	if len(cur) < budget {
		prev = readTail(l.path+".1", budget-len(cur))
	}
	b := append(prev, cur...)
	for _, raw := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if ln, ok := parseLine(raw); ok {
			l.push(ln)
		}
	}
}

// readTail returns up to n bytes from the end of path, starting at a line
// boundary. Missing files return nil.
func readTail(path string, n int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	cut := st.Size() > int64(n)
	if cut {
		if _, err := f.Seek(-int64(n), io.SeekEnd); err != nil {
			return nil
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(n)))
	if err != nil {
		return nil
	}
	if cut {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return nil
		}
		b = b[i+1:] // drop the partial first line
	}
	return b
}

func parseLine(s string) (Line, bool) {
	ts, rest, ok := strings.Cut(s, " ")
	if !ok {
		return Line{}, false
	}
	t, err := time.Parse(timeFormat, ts)
	if err != nil {
		return Line{}, false
	}
	stream, text, _ := strings.Cut(strings.TrimLeft(rest, " "), " ")
	if len(text) > 0 && stream != System {
		text = strings.TrimPrefix(text, strings.Repeat(" ", max(0, 5-len(stream))))
	}
	return Line{Time: t, Stream: stream, Text: text}, true
}

// Write appends one line.
func (l *Log) Write(stream, text string) {
	l.add(Line{Time: time.Now(), Stream: stream, Text: text})
}

// Writef appends a formatted System line.
func (l *Log) Writef(format string, args ...any) {
	l.Write(System, fmt.Sprintf(format, args...))
}

func (l *Log) add(ln Line) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.push(ln)
	l.writeFile(ln.String() + "\n")
	for ch := range l.subs {
		select {
		case ch <- ln:
		default: // slow subscriber: drop rather than block the app's output
		}
	}
}

func (l *Log) push(ln Line) {
	l.ring[l.next] = ln
	l.next = (l.next + 1) % len(l.ring)
	if l.next == 0 {
		l.full = true
	}
}

func (l *Log) writeFile(s string) {
	if l.size+int64(len(s)) > l.maxSize && l.size > 0 {
		if err := l.rotate(); err != nil {
			l.reportErr(err)
		}
	}
	if l.f == nil {
		return
	}
	n, err := l.f.WriteString(s)
	l.size += int64(n)
	if err != nil {
		l.reportErr(err)
	}
}

// rotate: app.log.(keep-1) -> app.log.keep, ..., app.log -> app.log.1.
func (l *Log) rotate() error {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	for i := l.keep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.path, i)
		if err := os.Rename(src, fmt.Sprintf("%s.%d", l.path, i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("logs: rotate %s: %w", src, err)
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("logs: rotate %s: %w", l.path, err)
	}
	return l.openFile()
}

func (l *Log) reportErr(err error) {
	if !l.errShown {
		fmt.Fprintf(os.Stderr, "dootd: log %s: %v (further errors suppressed)\n", l.path, err)
		l.errShown = true
	}
}

// Capture reads lines from r (an app's stdout or stderr pipe) until EOF.
// Lines longer than MaxLineBytes are split.
func (l *Log) Capture(stream string, r io.Reader) {
	br := bufio.NewReaderSize(r, MaxLineBytes)
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 || (err == nil && !isPrefix) {
			l.Write(stream, strings.TrimSuffix(string(chunk), "\r"))
		}
		if err != nil {
			return
		}
	}
}

// Recent returns up to n of the most recent lines, oldest first.
func (l *Log) Recent(n int) []Line {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := l.next
	if l.full {
		count = len(l.ring)
	}
	if n <= 0 || n > count {
		n = count
	}
	out := make([]Line, 0, n)
	start := (l.next - n + len(l.ring)) % len(l.ring)
	for i := 0; i < n; i++ {
		out = append(out, l.ring[(start+i)%len(l.ring)])
	}
	return out
}

// Subscribe returns a channel of new lines and a cancel function. Lines are
// dropped for subscribers that fall more than buf lines behind.
func (l *Log) Subscribe(buf int) (<-chan Line, func()) {
	ch := make(chan Line, buf)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			l.mu.Lock()
			if _, ok := l.subs[ch]; ok {
				delete(l.subs, ch)
				close(ch)
			}
			l.mu.Unlock()
		})
	}
}

// Close closes the file and all subscriber channels.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	for ch := range l.subs {
		delete(l.subs, ch)
		close(ch)
	}
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}
