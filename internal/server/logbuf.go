package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LogBuffer is an io.Writer for the standard logger: it keeps a ring of
// recent lines and fans new ones out to live subscribers (the web UI's
// Daemon Log window). main tees it with stderr via io.MultiWriter, so the
// terminal log is unchanged. With Persist it is also backed by a file, so
// the ring survives daemon restarts.
type LogBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
	subs  map[chan string]struct{}
	file  *os.File
}

func NewLogBuffer(max int) *LogBuffer {
	return &LogBuffer{max: max, subs: make(map[chan string]struct{})}
}

// Persist seeds the ring with the tail of path and appends future writes to
// it, so the log carries across restarts. The file is trimmed at startup
// once it outgrows 512 KB. Any lines already in the ring stay after the
// restored ones.
func (b *LogBuffer) Persist(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(data) > 512<<10 {
		cut := data[len(data)-(256<<10):]
		if i := bytes.IndexByte(cut, '\n'); i >= 0 {
			cut = cut[i+1:]
		}
		if os.WriteFile(path, cut, 0o644) == nil {
			data = cut
		}
	}
	var restored []string
	if len(data) > 0 {
		restored = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if n := len(restored) - b.max; n > 0 {
			restored = restored[n:]
		}
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.lines = append(restored[:len(restored):len(restored)], b.lines...)
	if n := len(b.lines) - b.max; n > 0 {
		b.lines = b.lines[n:]
	}
	b.file = f
	b.mu.Unlock()
	return nil
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file != nil {
		b.file.Write(p)
	}
	for _, ln := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		b.lines = append(b.lines, ln)
		for ch := range b.subs {
			select {
			case ch <- ln:
			default: // slow reader: drop its line rather than stall the logger
			}
		}
	}
	if n := len(b.lines) - b.max; n > 0 {
		b.lines = append([]string(nil), b.lines[n:]...)
	}
	return len(p), nil
}

// Subscribe returns the backlog and a channel of lines logged after it —
// taken under one lock, so a follower sees every line exactly once. cancel
// unregisters the channel; the writer never closes it.
func (b *LogBuffer) Subscribe() (backlog []string, ch chan string, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	backlog = append([]string(nil), b.lines...)
	ch = make(chan string, 256)
	b.subs[ch] = struct{}{}
	cancel = func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
	return backlog, ch, cancel
}
