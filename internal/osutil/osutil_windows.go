//go:build windows

package osutil

import (
	"context"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is GetExitCodeProcess's STILL_ACTIVE (259).
const stillActive = 259

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists (another user's, or a
		// protected one), like EPERM from kill(pid, 0).
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

func terminate(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

func lock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

func unlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

// NewProcessGroup starts cmd in a new process group (no console Ctrl+C
// propagation from pyred); KillGroup then ends its whole tree.
func NewProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// KillGroup ends pid and every process it started (taskkill /T /F). Windows
// console programs have no catchable SIGTERM, so force is implied.
func KillGroup(pid int, force bool) error {
	_ = force
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}

// Exec emulates execve, which Windows lacks: it runs path as a child with
// this process's stdio, waits, and exits with the child's exit code. It only
// returns when the child cannot be started.
func Exec(path string, argv, env []string) error {
	cmd := exec.Command(path)
	cmd.Args = argv
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Wait()
	code := 1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	os.Exit(code)
	return nil
}

// SignalZero emulates kill(pid, 0): nil when pid is a live process this
// user can open.
func SignalZero(pid int) error {
	if pid > 0 && processAlive(pid) {
		return nil
	}
	return syscall.ESRCH
}

// ParentPID returns the parent process id of pid (Toolhelp snapshot).
func ParentPID(pid int) (int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if int(e.ProcessID) == pid {
			return int(e.ParentProcessID), nil
		}
	}
	return 0, fmt.Errorf("process %d not found", pid)
}

const (
	fsctlSetReparsePoint = 0x000900A4
	reparseTagMountPoint = 0xA0000003
)

// Junction makes link an NTFS junction (mount point reparse point) to the
// directory target. Unlike symbolic links, junctions need neither
// administrator rights nor Developer Mode. target is made absolute.
func Junction(target, link string) error {
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	sub := utf16.Encode([]rune(`\??\` + abs))
	prn := utf16.Encode([]rune(abs))
	// MOUNT_POINT_REPARSE_BUFFER: four uint16 offsets/lengths, then the
	// substitute and print names, each NUL-terminated.
	pathBytes := (len(sub) + 1 + len(prn) + 1) * 2
	dataLen := 8 + pathBytes
	buf := make([]byte, 8+dataLen)
	binary.LittleEndian.PutUint32(buf[0:], reparseTagMountPoint)
	binary.LittleEndian.PutUint16(buf[4:], uint16(dataLen))
	binary.LittleEndian.PutUint16(buf[8:], 0)                       // SubstituteNameOffset
	binary.LittleEndian.PutUint16(buf[10:], uint16(len(sub)*2))     // SubstituteNameLength
	binary.LittleEndian.PutUint16(buf[12:], uint16((len(sub)+1)*2)) // PrintNameOffset
	binary.LittleEndian.PutUint16(buf[14:], uint16(len(prn)*2))     // PrintNameLength
	off := 16
	for _, c := range sub {
		binary.LittleEndian.PutUint16(buf[off:], c)
		off += 2
	}
	off += 2
	for _, c := range prn {
		binary.LittleEndian.PutUint16(buf[off:], c)
		off += 2
	}
	if err := os.Mkdir(link, 0o755); err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(link)
	if err != nil {
		os.Remove(link)
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		os.Remove(link)
		return err
	}
	var n uint32
	err = windows.DeviceIoControl(h, fsctlSetReparsePoint, &buf[0], uint32(len(buf)), nil, 0, &n, nil)
	windows.CloseHandle(h)
	if err != nil {
		os.Remove(link)
		return fmt.Errorf("create junction %s -> %s: %w", link, abs, err)
	}
	return nil
}

// LinkModes are the file mode bits Lstat reports for a link Symlink
// creates: Go reports junctions (mount points) as ModeIrregular.
const LinkModes = fs.ModeSymlink | fs.ModeIrregular

// Symlink creates link -> target: a junction when target is a directory
// (no privilege needed), else a file symbolic link, which Windows allows
// only with Developer Mode or SeCreateSymbolicLinkPrivilege.
func Symlink(target, link string) error {
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		return Junction(target, link)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("%w (file symbolic links on Windows need Developer Mode or the Create symbolic links privilege)", err)
	}
	return nil
}
