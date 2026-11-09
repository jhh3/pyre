package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// codexWatch is pyre run's view of `codex exec --json` output, for the
// caller reading its stderr: a codex that printed nothing (not even
// thread.started, its first event, which a healthy codex prints within
// seconds) is said to be stuck before its thread began, and the error
// events codex prints in its JSON stream (a retry, a capacity or rate-limit
// failure, a failed turn) are repeated as one line each. Two runs that hung
// for 20 and 45 minutes showed only "Reading additional input from
// stdin..." on stderr and an empty JSON file; nothing told their caller
// that codex had never started.
type codexWatch struct {
	stderr io.Writer
	file   *os.File // a regular-file stdout, whose writes pyre run does not see (nil for a pipe, which feed gets from the copier)
	off    int64    // where codex's output begins in file
	then   string   // what pyre run does about a codex that stays quiet ("": nothing)
	// stdinOpen reports that the caller has not finished codex's stdin
	// (nil: it has): codex exec reads stdin to its end before it starts,
	// and the stop and second start count from that end.
	stdinOpen func() bool
	mu        sync.Mutex
	seen      int64  // output bytes so far
	line      []byte // the current incomplete line (bounded)
	echoed    int    // error lines repeated
	quiet     bool   // a no-output notice was printed
}

var (
	// codexQuietAfter is how long codex may print nothing before pyre run
	// says so; the notice repeats every codexQuietEvery while it lasts.
	codexQuietAfter = 3 * time.Minute
	codexQuietEvery = 10 * time.Minute
	// codexWatchPoll is how often a file stdout is read for new output.
	codexWatchPoll = 2 * time.Second
)

const (
	codexEchoMax = 20      // error lines repeated per run
	codexLineMax = 1 << 16 // longer lines are not inspected (an error event is short)
)

// codexJSONOutput: the codex command prints JSON events on stdout.
func codexJSONOutput(args []string) bool {
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[:i]
	}
	return slices.Contains(args, "--json") || slices.Contains(args, "--experimental-json")
}

// feed takes output bytes as codex wrote them.
func (w *codexWatch) feed(p []byte) {
	if len(p) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.saw(int64(len(p)))
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(w.line)+len(p) <= codexLineMax {
				w.line = append(w.line, p...)
			} else {
				w.line = append(w.line[:0], "x"...) // too long: skipped to its end
			}
			return
		}
		if len(w.line)+i <= codexLineMax {
			w.line = append(w.line, p[:i]...)
			w.event(w.line)
		}
		w.line, p = w.line[:0], p[i+1:]
	}
}

// saw counts n output bytes; w.mu is held.
func (w *codexWatch) saw(n int64) {
	if w.quiet && w.seen == 0 {
		fmt.Fprintln(w.stderr, "pyre run: codex has started printing")
	}
	w.seen += n
}

// event repeats a codex error event on stderr. Only the top-level
// "error" (codex retrying or failing a request) and "turn.failed" events
// count: an item of type error is a warning (e.g. an unknown config key).
func (w *codexWatch) event(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte(`{"type":"error"`)) && !bytes.HasPrefix(line, []byte(`{"type":"turn.failed"`)) {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	msg := ev.Message
	if ev.Type == "turn.failed" {
		msg = "turn failed: " + ev.Error.Message
	}
	if w.echoed++; w.echoed > codexEchoMax {
		if w.echoed == codexEchoMax+1 {
			fmt.Fprintln(w.stderr, "pyre run: codex: further errors are only in its JSON output")
		}
		return
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > 400 {
		msg = string(r[:400]) + "…"
	}
	fmt.Fprintf(w.stderr, "pyre run: codex: %s\n", msg)
}

// quietNotice prints the no-output notice when codex has printed nothing
// for d. It reports whether codex has printed anything.
func (w *codexWatch) quietNotice(d time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen > 0 {
		return true
	}
	w.quiet = true
	if w.stdinOpen != nil && w.stdinOpen() {
		fmt.Fprintf(w.stderr, "pyre run: codex has printed nothing in %s and its stdin is still open: codex exec reads stdin to its end before it starts, "+
			"and the caller has not closed it. Close stdin (or pass </dev/null when there is no input); pyre run's stop and second start count from its end.\n", d.Round(time.Second))
		return false
	}
	then := w.then
	if then == "" {
		then = "Pyre leaves it running; stop it and run it again"
	}
	fmt.Fprintf(w.stderr, "pyre run: codex has printed nothing in %s (a working codex exec --json prints thread.started within seconds): "+
		"it is stuck before its thread began (network, OpenAI capacity, login or a lock). %s.\n", d.Round(time.Second), then)
	return false
}

// printed reports whether codex has written any output, reading a file
// stdout's size now rather than at the watcher's next poll.
func (w *codexWatch) printed() bool {
	w.mu.Lock()
	seen := w.seen
	w.mu.Unlock()
	if seen > 0 {
		return true
	}
	if w.file != nil {
		if fi, err := w.file.Stat(); err == nil && fi.Size() > w.off {
			return true
		}
	}
	return false
}

// run watches until stop: it reads new output from w.file and prints the
// no-output notices.
func (w *codexWatch) run(stop <-chan struct{}) {
	file, off := w.file, w.off
	var r *os.File
	if file != nil {
		// A shell's `> file` opens stdout write-only: read the same file
		// through a new read-only descriptor where the OS allows.
		r = file
		if rr, err := reopenRead(file); err == nil {
			r = rr
			defer rr.Close()
		}
	}
	start := time.Now()
	next := codexQuietAfter
	poll := time.NewTicker(codexWatchPoll)
	defer poll.Stop()
	buf := make([]byte, 64<<10)
	off0 := off
	for {
		stopped := false
		select {
		case <-stop:
			stopped = true // one last read: codex's final events
		case <-poll.C:
		}
		if r != nil {
			for {
				n, err := r.ReadAt(buf, off)
				off += int64(n)
				w.feed(buf[:n])
				if err != nil || n == 0 {
					break
				}
			}
			if off == off0 {
				// Unreadable here (write-only, no reopen): its size still
				// shows whether codex printed.
				if fi, err := file.Stat(); err == nil && fi.Size() > off0 {
					w.mu.Lock()
					w.saw(fi.Size() - off0)
					w.mu.Unlock()
					off0 = fi.Size()
				}
			}
		}
		if stopped {
			return
		}
		if next > 0 {
			if el := time.Since(start); el >= next {
				if w.quietNotice(el) {
					next = 0 // codex printed: no more notices
				} else {
					next += codexQuietEvery
				}
			}
		}
	}
}
