package hive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Cmd is one bounded Hive subprocess invocation.
type Cmd struct {
	Path      string
	Args      []string
	Env       []string
	Dir       string
	Timeout   time.Duration
	MaxOutput int
	// Background marks work nobody is waiting on (transcript capture, the
	// note bridge). ExecRunner lowers such a process group's CPU priority
	// (BackgroundNice) so it yields to interactive work and to agents.
	Background bool
}

// BackgroundNice is the nice value given to background Hive subprocesses.
const BackgroundNice = 10

// Result is the captured outcome. A non-zero exit is not an error; callers
// interpret Hive's documented exit codes.
type Result struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Truncated bool // stdout exceeded MaxOutput and was cut
	Duration  time.Duration
}

// ErrTimeout is returned when a subprocess exceeds its timeout; the whole
// process group is killed.
var ErrTimeout = errors.New("hive: subprocess timed out")

// Runner executes subprocesses. Tests substitute fakes.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// ExecRunner runs real processes in their own process group so a timeout
// also kills Hive's children (python -> bash -> sqlite3).
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Timeout <= 0 {
		c.Timeout = DefaultQueryTimeout
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = DefaultMaxOutput
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	stdout := &capWriter{max: c.MaxOutput}
	stderr := &capWriter{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	start := time.Now()
	err := cmd.Start()
	if err == nil {
		if c.Background {
			// Set right after exec, before Hive's interpreter has started
			// any children (they inherit it); best effort.
			lowerPriority(cmd)
		}
		err = cmd.Wait()
	}
	res := Result{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), Truncated: stdout.truncated, Duration: time.Since(start)}
	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("%w after %s: %s %v", ErrTimeout, c.Timeout, c.Path, firstArg(c.Args))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

func firstArg(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}

// capWriter keeps the first max bytes and drains the rest so the child never
// blocks on a full pipe.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	if room <= 0 {
		w.truncated = w.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}
