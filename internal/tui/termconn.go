package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/jhh3/pyre/internal/contracts"
)

// TermConn is a running attach command on a pseudo-terminal: Read yields
// what the command draws, Write types into it, Resize changes the pty size
// (the tmux client then resizes the window under window-size latest), and
// Close ends the command (a tmux client detaches; the session keeps
// running).
type TermConn interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// TermStarter starts an attach descriptor's command on a pty of cols x
// rows. The embedded terminal calls it for local_tmux and ssh_tmux
// descriptors; tests substitute an in-memory terminal. Never log
// d.Ticket.
type TermStarter func(d contracts.AttachDescriptor, cols, rows int) (TermConn, error)

// StartPTY is the default TermStarter: it runs d.Command (tmux attach, or
// ssh -t host tmux attach) on a new pty, with TERM=xterm-256color (what
// the embedded emulator understands) and without $TMUX, so a TUI running
// inside tmux can still embed a session of Pyre's own tmux server.
func StartPTY(d contracts.AttachDescriptor, cols, rows int) (TermConn, error) {
	switch d.Mechanism {
	case contracts.AttachLocalTmux, contracts.AttachSSHTmux:
	default:
		return nil, fmt.Errorf("attach mechanism %q cannot be embedded", d.Mechanism)
	}
	if len(d.Command) == 0 {
		return nil, fmt.Errorf("attach descriptor for run %s has no command", d.RunID)
	}
	if _, err := exec.LookPath(d.Command[0]); err != nil {
		return nil, fmt.Errorf("embedding needs %q on this machine: %v", d.Command[0], err)
	}
	cmd := exec.Command(d.Command[0], d.Command[1:]...)
	cmd.Env = termEnv(os.Environ())
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(max(1, cols)), Rows: uint16(max(1, rows))})
	if err != nil {
		return nil, err
	}
	p := &ptyConn{f: f, cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// termEnv is the environment of an embedded attach command.
func termEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "TMUX", "TMUX_PANE", "TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
}

type ptyConn struct {
	f    *os.File
	cmd  *exec.Cmd
	once sync.Once
	done chan struct{}
}

func (p *ptyConn) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *ptyConn) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *ptyConn) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(max(1, cols)), Rows: uint16(max(1, rows))})
}

// Close hangs up the pty (SIGHUP to the client) and terminates the command,
// killing it if it has not exited within two seconds. It does not wait.
func (p *ptyConn) Close() error {
	p.once.Do(func() {
		_ = p.f.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			go func() {
				select {
				case <-p.done:
				case <-time.After(2 * time.Second):
					_ = p.cmd.Process.Kill()
				}
			}()
		}
	})
	return nil
}
