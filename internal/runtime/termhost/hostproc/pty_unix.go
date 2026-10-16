//go:build !windows

package hostproc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// unixProc is a program on a Unix PTY. It leads its own session and
// process group (pty.Start sets Setsid/Setctty), like a tmux pane.
type unixProc struct {
	cmd  *exec.Cmd
	f    *os.File
	done chan struct{}
}

func startProc(argv []string, cwd string, env []string, cols, rows int) (proc, error) {
	path, err := lookPathEnv(argv[0], env)
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: path, Args: argv, Env: env, Dir: cwd}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &unixProc{cmd: cmd, f: f, done: make(chan struct{})}, nil
}

func (p *unixProc) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *unixProc) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *unixProc) Pid() int                    { return p.cmd.Process.Pid }

func (p *unixProc) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *unixProc) Wait() *int {
	err := p.cmd.Wait()
	defer close(p.done)
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil
	}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return nil
	}
	code := p.cmd.ProcessState.ExitCode()
	return &code
}

func (p *unixProc) Exited() {}

func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

func (p *unixProc) Terminate(grace time.Duration) {
	pid := p.Pid()
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	end := time.Now().Add(grace)
	for time.Now().Before(end) && groupAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if groupAlive(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
}

func (p *unixProc) Kill() {
	_ = syscall.Kill(-p.Pid(), syscall.SIGKILL)
	_ = p.cmd.Process.Kill()
}

func (p *unixProc) Close() error { return p.f.Close() }
