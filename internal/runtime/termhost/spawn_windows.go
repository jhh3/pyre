//go:build windows

package termhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/jhh3/pyre/internal/stablepath"
	"github.com/jhh3/pyre/internal/wintask"
)

// spawnHost starts the host detached from pyred: no console
// (DETACHED_PROCESS), its own process group (no Ctrl-C/Ctrl-Break from
// pyred's console), and out of pyred's job object (CREATE_BREAKAWAY_FROM_JOB)
// so a service manager or terminal that kills pyred's job on exit does not
// kill the runs. If the job forbids breakaway, the host starts through its
// own Task Scheduler task (launch.go); only if that fails too does it start
// inside pyred's job, living only as long as that job. The log records
// which path was taken.
func spawnHost(ctx context.Context, exe string, args, env []string, logPath, socket string) error {
	logf, err := openLog(logPath)
	if err != nil {
		return err
	}
	defer logf.Close()
	direct := func(breakaway bool) error {
		flags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
		if breakaway {
			flags |= windows.CREATE_BREAKAWAY_FROM_JOB
		}
		cmd := exec.Command(exe, args...)
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err := cmd.Start(); err != nil {
			if breakaway && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return fmt.Errorf("%w: %v", errBreakawayDenied, err)
			}
			return err
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	l := hostLauncher{
		mode:    os.Getenv(SpawnEnv),
		direct:  direct,
		viaTask: func() (string, error) { return startViaTask(exe, args, env, logPath, socket) },
		log:     logf,
	}
	return l.launch()
}

// startViaTask registers (or updates: the pyred path changes on upgrade)
// and runs the session-host task for socket. The task runs pyred through
// the install's stable "current" path, so a host started after an upgrade
// runs the new binary; a host already running keeps its own.
func startViaTask(exe string, args, env []string, logPath, socket string) (string, error) {
	sid, user, err := wintask.CurrentUser()
	if err != nil {
		return "", fmt.Errorf("current user: %w", err)
	}
	name := SessionTaskName(user, socket)
	// The environment travels in a file in the private socket directory
	// (never on the command line); the host reads and deletes it at start.
	envFile := filepath.Join(filepath.Dir(socket), "termhost-env-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".json")
	b, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(envFile, b, 0o600); err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(envFile)
		}
	}()
	def := sessionTask(sid, stablepath.Of(exe), sessionTaskArgs(args, logPath, envFile), socket)
	if err := wintask.Register(name, def.XML()); err != nil {
		return "", err
	}
	if err := wintask.Run(name); err != nil {
		return "", err
	}
	ok = true
	// Clean up if the host never came to read it.
	time.AfterFunc(time.Minute, func() { _ = os.Remove(envFile) })
	return name, nil
}

// forgetSessionTask removes the session-host task of socket after its
// host was shut down (throwaway hosts must not leave tasks behind).
func forgetSessionTask(socket string) {
	_, user, err := wintask.CurrentUser()
	if err != nil {
		return
	}
	if name := SessionTaskName(user, socket); wintask.Registered(name) {
		_ = wintask.Delete(name)
	}
}
