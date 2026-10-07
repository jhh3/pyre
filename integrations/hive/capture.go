package hive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Pyre-side capture of transcripts Hive's own hooks cannot reach.
//
// Hive 0.2.3 captures Claude sessions through its Stop/SessionEnd hooks, but
// registers no capture hook for Codex: its `tick` sweep is the whole Codex
// capture path and reads only ~/.codex/sessions. A Codex run in a Pyre
// account profile writes its rollout under that profile's CODEX_HOME, so
// Hive never sees it. The node therefore hands such a rollout to Hive's
// supported capture verb (`hive mem capture --transcript P --session S
// --cwd D --reason R`) itself: when the run ends, and at idle boundaries of a
// live run. The capture runs in a bounded background queue, never inside a
// harness hook or an adapter call. `--session` is the Codex thread ID, which
// is also the run's native reference, so search hits are attributed to the
// Pyre session and run (NativeResolver).

// CaptureRequest asks Hive to capture one harness transcript.
type CaptureRequest struct {
	Transcript string // absolute path of the transcript (Codex rollout JSONL)
	SessionID  string // harness-native session ID (Hive entry uid)
	Cwd        string // the run's working directory (Hive repo keying)
	Reason     string // e.g. "pyre-run-end", "pyre-idle"
	// Env adds variables to the capture subprocess (e.g. CODEX_HOME of the
	// run's profile, so the process is visibly bound to the profile).
	Env []string
	// Done is called once the request finished, failed or was dropped.
	Done func()
}

// CaptureResult is the outcome of one capture.
type CaptureResult struct {
	ExitCode int
	Duration time.Duration
	Bytes    int64
}

// ErrCaptureUnavailable means Hive cannot write (absent or degraded).
var ErrCaptureUnavailable = errors.New("hive capture unavailable")

// CaptureTranscript runs Hive's capture verb once (bounded by the write
// timeout). A non-zero exit is reported as an error with Hive's stderr line.
func (c *Client) CaptureTranscript(ctx context.Context, r CaptureRequest) (CaptureResult, error) {
	var res CaptureResult
	if !filepath.IsAbs(r.Transcript) || !validNativeID(r.SessionID) {
		return res, fmt.Errorf("hive capture: invalid request (transcript %q, session %q)", r.Transcript, r.SessionID)
	}
	st, err := os.Stat(r.Transcript)
	if err != nil {
		return res, fmt.Errorf("hive capture: %w", err)
	}
	res.Bytes = st.Size()
	if s := c.Status(ctx); s.Mode != ModeCLI {
		return res, fmt.Errorf("%w: %s", ErrCaptureUnavailable, s.Memory.Detail)
	}
	reason := r.Reason
	if reason == "" {
		reason = "pyre"
	}
	transcript := r.Transcript
	if shim, err := CodexRolloutNeedsShim(r.Transcript); err == nil && shim {
		staged, cleanup, err := c.stageCodexRollout(r.Transcript, res.Bytes)
		if err != nil {
			return res, fmt.Errorf("hive capture: %w", err)
		}
		defer cleanup()
		transcript = staged
	}
	args := []string{"mem", "capture", "--transcript", transcript, "--session", r.SessionID, "--reason", reason}
	if r.Cwd != "" {
		args = append(args, "--cwd", r.Cwd)
	}
	c.mu.Lock()
	bin := c.status.Bin
	c.mu.Unlock()
	dir := r.Cwd
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = ""
	}
	out, err := c.runCmd(ctx, Cmd{Path: bin, Args: args, Env: append(c.env.childEnv(), r.Env...), Dir: dir,
		Timeout: c.env.writeTimeout(), MaxOutput: 64 << 10})
	res.ExitCode, res.Duration = out.ExitCode, out.Duration
	if err != nil {
		return res, err
	}
	if out.ExitCode != 0 {
		return res, fmt.Errorf("hive mem capture exited %d: %s", out.ExitCode, oneLine(out.Stderr))
	}
	return res, nil
}

// stageCodexRollout writes the Hive-compatible copy of a codex rollout into a
// private staging dir; cleanup removes it after the capture call.
func (c *Client) stageCodexRollout(path string, size int64) (string, func(), error) {
	if size > MaxNormalizeBytes {
		return "", nil, fmt.Errorf("codex rollout is %d bytes (shim limit %d)", size, MaxNormalizeBytes)
	}
	dir := filepath.Join(c.opts.StagingRoot, "capture")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	src, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer src.Close()
	dst, err := os.CreateTemp(dir, "codex-*.jsonl")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(dst.Name()) }
	if _, err := NormalizeCodexRollout(src, dst); err != nil {
		dst.Close()
		cleanup()
		return "", nil, err
	}
	if err := dst.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return dst.Name(), cleanup, nil
}

var nativeIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func validNativeID(id string) bool { return nativeIDRe.MatchString(id) }

// CodexRollout finds the rollout file of a Codex thread under a CODEX_HOME
// (sessions/YYYY/MM/DD/rollout-<timestamp>-<thread>.jsonl, then
// archived_sessions). The newest match wins.
func CodexRollout(codexHome, threadID string) (string, error) {
	if codexHome == "" || !validNativeID(threadID) {
		return "", fmt.Errorf("codex rollout: invalid home or thread id")
	}
	var matches []string
	for _, pat := range []string{
		filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl"),
		filepath.Join(codexHome, "archived_sessions", "rollout-*-"+threadID+".jsonl"),
	} {
		m, _ := filepath.Glob(pat)
		matches = append(matches, m...)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("codex rollout for thread %s not found under %s", threadID, codexHome)
	}
	sort.Strings(matches)
	return matches[len(matches)-1], nil
}

// Capturer runs CaptureRequests one at a time from a bounded queue, as
// background work (see Background), MinGap apart. Requests
// for a transcript that is already queued are merged; a transcript whose
// size did not change since its last successful capture is skipped (Hive's
// capture is idempotent, this only saves the subprocess).
type Capturer struct {
	c      *Client
	logf   func(string, ...any)
	max    int
	minGap time.Duration
	last   time.Time // when the last capture subprocess finished

	mu      sync.Mutex
	queue   []*CaptureRequest
	index   map[string]*CaptureRequest
	done    map[string][]func()
	lastLen map[string]int64
	wake    chan struct{}
	stats   CaptureStats
}

// CaptureStats counts outcomes (for status and tests).
type CaptureStats struct {
	OK, Failed, Skipped, Dropped int
	LastError                    string
	// MaxPending is the queue high-water mark (never above the cap).
	MaxPending int
}

// CapturerOptions bound a Capturer. Zero values select the defaults.
type CapturerOptions struct {
	// MaxQueue is the number of distinct transcripts that may wait
	// (default 32). Requests for a queued transcript coalesce; a new one
	// beyond the cap is dropped (Done is called). Hive's sweep and the next
	// idle or run-end capture pick the transcript up again: capture is an
	// index of a transcript that stays on disk, not durable data.
	MaxQueue int
	// MinGap is the pause between two captures run from the queue (default
	// DefaultCaptureGap), so a burst of ending runs is spread out instead of
	// running Hive back to back. Negative disables it.
	MinGap time.Duration
	Logf   func(string, ...any)
}

// DefaultCaptureGap is CapturerOptions.MinGap's default.
const DefaultCaptureGap = time.Second

// NewCapturer returns a capturer with a queue of at most max requests
// (default 32) and the default gap between captures.
func NewCapturer(c *Client, max int, logf func(string, ...any)) *Capturer {
	return NewCapturerWith(c, CapturerOptions{MaxQueue: max, Logf: logf})
}

// NewCapturerWith returns a capturer bounded by o. Captures run one at a
// time, as background work of c (one of its BackgroundSlots, lowered CPU
// priority).
func NewCapturerWith(c *Client, o CapturerOptions) *Capturer {
	if o.MaxQueue <= 0 {
		o.MaxQueue = 32
	}
	switch {
	case o.MinGap == 0:
		o.MinGap = DefaultCaptureGap
	case o.MinGap < 0:
		o.MinGap = 0
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &Capturer{c: c, logf: o.Logf, max: o.MaxQueue, minGap: o.MinGap, index: map[string]*CaptureRequest{}, done: map[string][]func(){},
		lastLen: map[string]int64{}, wake: make(chan struct{}, 1)}
}

// Enqueue queues r without blocking. It returns false (and calls r.Done)
// when the queue is full.
func (k *Capturer) Enqueue(r CaptureRequest) bool {
	k.mu.Lock()
	if cur, ok := k.index[r.Transcript]; ok {
		if r.Done != nil {
			k.done[r.Transcript] = append(k.done[r.Transcript], r.Done)
		}
		if r.Reason == "pyre-run-end" {
			cur.Reason = r.Reason
		}
		k.mu.Unlock()
		return true
	}
	if len(k.queue) >= k.max {
		k.stats.Dropped++
		k.mu.Unlock()
		k.logf("capture queue full; dropped %s (%s)", r.SessionID, r.Reason)
		if r.Done != nil {
			r.Done()
		}
		return false
	}
	rr := r
	k.queue = append(k.queue, &rr)
	k.index[r.Transcript] = &rr
	if r.Done != nil {
		k.done[r.Transcript] = append(k.done[r.Transcript], r.Done)
	}
	k.stats.MaxPending = max(k.stats.MaxPending, len(k.queue))
	k.mu.Unlock()
	select {
	case k.wake <- struct{}{}:
	default:
	}
	return true
}

// Stats returns a snapshot of the outcome counters.
func (k *Capturer) Stats() CaptureStats {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.stats
}

// Run processes the queue until ctx ends; pending requests are released
// (Done called) on exit.
func (k *Capturer) Run(ctx context.Context) {
	for {
		if !k.pause(ctx) {
			k.drain()
			return
		}
		if r := k.next(); r != nil {
			k.process(ctx, r)
			continue
		}
		select {
		case <-ctx.Done():
			k.drain()
			return
		case <-k.wake:
		}
	}
}

// Drain processes every queued request now (tests, shutdown paths).
func (k *Capturer) Drain(ctx context.Context) {
	for r := k.next(); r != nil; r = k.next() {
		k.process(ctx, r)
	}
}

// pause waits out MinGap since the last capture; false when ctx ended.
func (k *Capturer) pause(ctx context.Context) bool {
	k.mu.Lock()
	wait := time.Until(k.last.Add(k.minGap))
	k.mu.Unlock()
	if wait <= 0 || k.Pending() == 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (k *Capturer) next() *CaptureRequest {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.queue) == 0 {
		return nil
	}
	r := k.queue[0]
	k.queue = k.queue[1:]
	return r
}

func (k *Capturer) finish(r *CaptureRequest) {
	k.mu.Lock()
	delete(k.index, r.Transcript)
	fns := k.done[r.Transcript]
	delete(k.done, r.Transcript)
	k.mu.Unlock()
	for _, f := range fns {
		f()
	}
}

func (k *Capturer) drain() {
	for r := k.next(); r != nil; r = k.next() {
		k.finish(r)
	}
}

func (k *Capturer) process(ctx context.Context, r *CaptureRequest) {
	defer k.finish(r)
	if fi, err := os.Stat(r.Transcript); err == nil {
		k.mu.Lock()
		same := k.lastLen[r.Transcript] == fi.Size()
		if same {
			k.stats.Skipped++
		}
		k.mu.Unlock()
		if same {
			return
		}
	}
	res, err := k.c.CaptureTranscript(Background(ctx), *r)
	k.mu.Lock()
	k.last = time.Now()
	if err != nil {
		k.stats.Failed++
		k.stats.LastError = err.Error()
	} else {
		k.stats.OK++
		k.lastLen[r.Transcript] = res.Bytes
	}
	k.mu.Unlock()
	if err != nil {
		k.logf("capture of %s (%s) failed: %v", r.SessionID, r.Reason, err)
		return
	}
	k.logf("captured %s into hive (%s, %d bytes, %s)", r.SessionID, r.Reason, res.Bytes, res.Duration.Round(time.Millisecond))
}

// MaxQueue is the queue cap.
func (k *Capturer) MaxQueue() int { return k.max }

// Pending is the number of queued requests.
func (k *Capturer) Pending() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.queue)
}
