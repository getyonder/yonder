package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// This file is the send log: a line with the time for each step of a send
// between paired devices, so that a send that was slow on real networks can
// be read afterwards. It names devices and counts files; it holds no file
// name and nothing secret.

const (
	// TraceName is the log's file in the Config's Dir.
	TraceName = "send.log"
	// traceMax is the size past which the log starts again, when it is
	// opened and while it is written.
	traceMax = 1 << 20
	// traceLineMax is the longest line, newline included; a longer one is
	// cut, whatever its callers cut before.
	traceLineMax = 2000
)

// tracer writes the send log. A nil one writes nothing.
type tracer struct {
	mu   sync.Mutex
	f    *os.File
	size int64 // bytes in the file
}

// openTrace opens the send log in dir, or returns nil if it cannot: the log
// is never a reason for Yonder not to start.
func openTrace(dir string) *tracer {
	path := filepath.Join(dir, TraceName)
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if info, err := os.Stat(path); err == nil && info.Size() > traceMax {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return nil
	}
	t := &tracer{f: f}
	if info, err := f.Stat(); err == nil {
		t.size = info.Size()
	}
	return t
}

// logf writes one line, with the time to the millisecond in front. A control
// character becomes a space, so that no text from another device, in an
// error for example, starts a line of its own.
func (t *tracer) logf(format string, args ...any) {
	if t == nil {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05.000 ") + strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, fmt.Sprintf(format, args...))
	if len(line) > traceLineMax-1 {
		line = line[:traceLineMax-1]
	}
	line += "\n"
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return
	}
	if t.size+int64(len(line)) > traceMax && t.f.Truncate(0) == nil {
		t.size = 0
	}
	n, _ := t.f.WriteString(line)
	t.size += int64(n)
}

func (t *tracer) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.f.Close()
	t.f = nil
}

// since is the time from start, as the log shows it.
func since(start time.Time) time.Duration {
	return time.Since(start).Round(10 * time.Millisecond)
}
