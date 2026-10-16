//go:build windows

package hostproc

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
	"golang.org/x/sys/windows"
)

// conPTY is a program on a Windows pseudoconsole (ConPTY, Windows 10 1809
// and later). The program and every process it starts run in a job object,
// so terminating the session ends the whole tree (Windows has no process
// groups); KILL_ON_JOB_CLOSE also ends the tree if the host itself dies.
type conPTY struct {
	hpc windows.Handle
	in  *os.File // write end of the pseudoconsole's input pipe
	out *os.File // read end of its output pipe
	pid int

	mu     sync.Mutex // guards proc and job, which Close releases
	proc   windows.Handle
	job    windows.Handle // 0 when the job could not be set up
	closed bool

	consoleOnce sync.Once
	closeOnce   sync.Once
	exited      chan struct{}
}

func startProc(argv []string, cwd string, env []string, cols, rows int) (proc, error) {
	if _, err := wire.BackendCheck(); err != nil {
		return nil, err
	}
	path, err := lookPathEnv(argv[0], env)
	if err != nil {
		return nil, err
	}
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, fmt.Errorf("create input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}
	var hpc windows.Handle
	err = windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inR, outW, 0, &hpc)
	// The pseudoconsole holds its own duplicates of its pipe ends.
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	if err != nil {
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	c := &conPTY{hpc: hpc, in: os.NewFile(uintptr(inW), "conpty-in"), out: os.NewFile(uintptr(outR), "conpty-out"), exited: make(chan struct{})}
	if err := c.spawn(path, argv, cwd, env); err != nil {
		c.closeConsole()
		_ = c.in.Close()
		_ = c.out.Close()
		return nil, err
	}
	return c, nil
}

func (c *conPTY) spawn(path string, argv []string, cwd string, env []string) error {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("attribute list: %w", err)
	}
	defer attrs.Delete()
	// The attribute's value is the HPCON itself, not a pointer to it.
	hpc := c.hpc
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		return fmt.Errorf("attach pseudoconsole: %w", err)
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	// The host's own standard handles are redirected (log file / NUL);
	// without this the program could inherit them instead of using the
	// pseudoconsole (the same precaution as wezterm's portable-pty).
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = windows.InvalidHandle, windows.InvalidHandle, windows.InvalidHandle

	app, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return err
	}
	var dir *uint16
	if cwd != "" {
		if dir, err = windows.UTF16PtrFromString(cwd); err != nil {
			return err
		}
	}
	block := envBlock(env)
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if err := windows.CreateProcess(app, cmdline, nil, nil, false, flags, &block[0], dir, &si.StartupInfo, &pi); err != nil {
		return fmt.Errorf("CreateProcess %s: %w", path, err)
	}
	c.proc, c.pid = pi.Process, int(pi.ProcessId)
	if job, err := windows.CreateJobObject(nil, nil); err == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		_, serr := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
		if serr == nil && windows.AssignProcessToJobObject(job, pi.Process) == nil {
			c.job = job
		} else {
			windows.CloseHandle(job)
		}
	}
	_, _ = windows.ResumeThread(pi.Thread)
	windows.CloseHandle(pi.Thread)
	return nil
}

// envBlock renders env as a sorted, NUL-separated UTF-16 block. SYSTEMROOT
// is kept (some programs fail without it), as os/exec does.
func envBlock(env []string) []uint16 {
	env = append([]string(nil), env...)
	if envLookup(env, "SYSTEMROOT") == "" {
		if v := os.Getenv("SYSTEMROOT"); v != "" {
			env = append(env, "SYSTEMROOT="+v)
		}
	}
	sort.SliceStable(env, func(i, j int) bool {
		return strings.ToUpper(wire.EnvKey(env[i])) < strings.ToUpper(wire.EnvKey(env[j]))
	})
	var b []uint16
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			continue
		}
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	return append(b, 0)
}

func (c *conPTY) Read(b []byte) (int, error)  { return c.out.Read(b) }
func (c *conPTY) Write(b []byte) (int, error) { return c.in.Write(b) }
func (c *conPTY) Pid() int                    { return c.pid }

func (c *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(c.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (c *conPTY) Wait() *int {
	defer close(c.exited)
	if _, err := windows.WaitForSingleObject(c.proc, windows.INFINITE); err != nil {
		return nil
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.proc, &code); err != nil {
		return nil
	}
	n := int(code)
	return &n
}

// Exited closes the pseudoconsole shortly after the program ended (its
// last output drains first), so Read sees EOF: ConPTY keeps the output
// pipe open while the pseudoconsole exists.
func (c *conPTY) Exited() {
	go func() {
		time.Sleep(200 * time.Millisecond)
		c.closeConsole()
	}()
}

// closeConsole closes the pseudoconsole once. Every process still attached
// gets CTRL_CLOSE_EVENT. On older Windows it blocks until the output pipe
// is drained; the session's reader keeps draining it.
func (c *conPTY) closeConsole() {
	c.consoleOnce.Do(func() { windows.ClosePseudoConsole(c.hpc) })
}

func (c *conPTY) Terminate(grace time.Duration) {
	go c.closeConsole()
	select {
	case <-c.exited:
	case <-time.After(grace):
		c.Kill()
		select {
		case <-c.exited:
		case <-time.After(2 * time.Second):
		}
	}
	// Children that ignored CTRL_CLOSE_EVENT go with the job.
	c.Kill()
}

func (c *conPTY) Kill() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.job != 0 {
		_ = windows.TerminateJobObject(c.job, 1)
		return
	}
	_ = windows.TerminateProcess(c.proc, 1)
}

func (c *conPTY) Close() error {
	c.closeOnce.Do(func() {
		go c.closeConsole()
		_ = c.in.Close()
		_ = c.out.Close() // takes effect once the pending Read returns (EOF after closeConsole)
		c.mu.Lock()
		c.closed = true
		if c.job != 0 {
			windows.CloseHandle(c.job) // KILL_ON_JOB_CLOSE: stragglers end too
			c.job = 0
		}
		proc := c.proc
		c.mu.Unlock()
		go func() {
			// Release the process handle once Wait no longer needs it.
			<-c.exited
			windows.CloseHandle(proc)
		}()
	})
	return nil
}
