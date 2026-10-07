package diagnostics

import (
	"bytes"
	"strings"
	"sync"
)

// Ring is a bounded, goroutine-safe line buffer. It keeps at most MaxLines
// lines and MaxBytes bytes (oldest dropped first) and counts everything it
// was ever given, so tests can both inspect recent log lines and measure log
// growth without unbounded memory. It implements io.Writer, so it can back a
// slog handler: slog.New(slog.NewTextHandler(ring, nil)).
type Ring struct {
	mu       sync.Mutex
	lines    []string
	bytes    int
	maxLines int
	maxBytes int
	partial  []byte
	redact   func(string) string

	totalLines int64
	totalBytes int64
	dropped    int64
}

// NewRing returns a ring keeping maxLines lines and maxBytes bytes (each
// <=0 means a default of 1000 lines / 1 MiB). If redact is non-nil every
// line passes through it before being stored (totals count raw input).
func NewRing(maxLines, maxBytes int, redact func(string) string) *Ring {
	if maxLines <= 0 {
		maxLines = 1000
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	return &Ring{maxLines: maxLines, maxBytes: maxBytes, redact: redact}
}

// Write splits p into lines; a trailing partial line is held until its
// newline arrives (bounded to maxBytes).
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.totalBytes += int64(len(p))
	data := p
	if len(r.partial) > 0 {
		data = append(r.partial, p...)
		r.partial = nil
	}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		r.addLocked(string(data[:i]))
		data = data[i+1:]
	}
	if len(data) > 0 {
		if len(data) > r.maxBytes {
			data = data[len(data)-r.maxBytes:]
		}
		r.partial = append([]byte(nil), data...)
	}
	return len(p), nil
}

// Add appends one line.
func (r *Ring) Add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.totalBytes += int64(len(line) + 1)
	r.addLocked(line)
}

func (r *Ring) addLocked(line string) {
	r.totalLines++
	if r.redact != nil {
		line = r.redact(line)
	}
	if len(line) > r.maxBytes {
		line = line[:r.maxBytes]
	}
	r.lines = append(r.lines, line)
	r.bytes += len(line)
	for len(r.lines) > r.maxLines || r.bytes > r.maxBytes {
		r.bytes -= len(r.lines[0])
		r.lines[0] = ""
		r.lines = r.lines[1:]
		r.dropped++
	}
	// Compact occasionally so the backing array does not grow forever.
	if cap(r.lines) > 4*r.maxLines && len(r.lines) < r.maxLines {
		r.lines = append([]string(nil), r.lines...)
	}
}

// Lines returns a copy of the retained lines, oldest first.
func (r *Ring) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// Last returns up to n most recent lines.
func (r *Ring) Last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.lines) {
		n = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-n:]...)
}

// String joins the retained lines with newlines.
func (r *Ring) String() string { return strings.Join(r.Lines(), "\n") }

// RingStats summarises volume for log-growth measurement.
type RingStats struct {
	TotalLines    int64 `json:"total_lines"`
	TotalBytes    int64 `json:"total_bytes"`
	RetainedLines int   `json:"retained_lines"`
	RetainedBytes int   `json:"retained_bytes"`
	Dropped       int64 `json:"dropped"`
}

// Stats returns totals since creation (or the last Reset).
func (r *Ring) Stats() RingStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RingStats{TotalLines: r.totalLines, TotalBytes: r.totalBytes, RetainedLines: len(r.lines), RetainedBytes: r.bytes, Dropped: r.dropped}
}

// Reset clears lines and counters.
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines, r.bytes, r.partial = nil, 0, nil
	r.totalLines, r.totalBytes, r.dropped = 0, 0, 0
}
