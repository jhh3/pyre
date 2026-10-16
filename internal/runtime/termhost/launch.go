package termhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/wintask"
)

// On Windows the host must outlive pyred like a tmux server. A direct
// spawn with CREATE_BREAKAWAY_FROM_JOB leaves pyred's job object; when the
// job forbids breakaway (Task Scheduler runs `pyred supervise` in a job
// without JOB_OBJECT_LIMIT_BREAKAWAY_OK), the host is started through its
// own on-demand Task Scheduler task instead: the task instance gets its own
// job, in the user's interactive session with the user's token, so a pyred
// restart no longer kills every session. Win32_Process.Create (WMI) was not
// used: its processes are children of WmiPrvSE, subject to the provider
// host's job/quotas, and not reliably in the interactive desktop that GUI
// agents need — the reason the node itself runs as a logon task.

// SpawnEnv selects how the Windows host starts: "task" always goes
// through Task Scheduler, "direct" never does (when breakaway is refused
// the host then shares pyred's job); unset tries a direct breakaway spawn
// and falls back to the task.
const SpawnEnv = "PYRE_TERMHOST_SPAWN"

const (
	spawnTask   = "task"
	spawnDirect = "direct"
)

// errBreakawayDenied: pyred's job object refuses CREATE_BREAKAWAY_FROM_JOB.
var errBreakawayDenied = errors.New("termhost: pyred's job object forbids breakaway")

// hostLauncher decides how the host process is started (the decision is
// platform-independent so it is testable anywhere).
type hostLauncher struct {
	mode string // SpawnEnv value
	// direct starts the host as pyred's child, out of pyred's job when
	// breakaway is set (errBreakawayDenied when the job refuses).
	direct func(breakaway bool) error
	// viaTask starts the host through its Task Scheduler task and returns
	// the task name.
	viaTask func() (string, error)
	log     io.Writer
}

func (l hostLauncher) logf(format string, args ...any) {
	if l.log != nil {
		fmt.Fprintf(l.log, time.Now().Format("2006/01/02 15:04:05 ")+format+"\n", args...)
	}
}

const sharedJobNote = "the host shares pyred's job (it and its sessions end when pyred's job does)"

func (l hostLauncher) launch() error {
	if l.mode == spawnTask {
		l.logf("termhost: %s=task; starting the host through Task Scheduler", SpawnEnv)
	} else {
		err := l.direct(true)
		if err == nil {
			l.logf("termhost: started the host directly (out of pyred's job)")
			return nil
		}
		if !errors.Is(err, errBreakawayDenied) {
			return err
		}
		if l.mode == spawnDirect || l.viaTask == nil {
			l.logf("termhost: pyred's job object forbids breakaway (%s=%s); %s", SpawnEnv, l.mode, sharedJobNote)
			return l.direct(false)
		}
		l.logf("termhost: pyred's job object forbids breakaway; starting the host through Task Scheduler")
	}
	name, err := l.viaTask()
	if err == nil {
		l.logf("termhost: started the host through Task Scheduler task %q (its own job; it outlives pyred)", name)
		return nil
	}
	l.logf("termhost: Task Scheduler start failed: %v; %s", err, sharedJobNote)
	return l.direct(false)
}

// SessionTaskPrefix is the start of every session-host task name of user.
func SessionTaskPrefix(user string) string {
	return "Pyre Session Host " + wintask.SafeName(user) + " "
}

// SessionTaskName is the Task Scheduler task that starts the host of
// socket for user: one per socket (a task runs one instance at a time),
// named by a hash of the case-folded path (Windows paths are
// case-insensitive).
func SessionTaskName(user, socket string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(socket))))
	return SessionTaskPrefix(user) + hex.EncodeToString(sum[:4])
}

// sessionTaskArgs is the host command line the task runs: the direct
// spawn's args plus the log (a task has no stdio to redirect), the
// environment file (a task starts with the user's default environment, not
// pyred's), and detaching from the console window Task Scheduler opens.
func sessionTaskArgs(args []string, logPath, envFile string) []string {
	out := append([]string(nil), args...)
	return append(out, "--log", logPath, "--env-file", envFile, "--hide-console")
}

// sessionTask is the task definition for the host.
func sessionTask(userID, exe string, args []string, socket string) wintask.OnDemand {
	return wintask.OnDemand{
		Description: "Pyre session host for " + socket + "; started on demand by pyred so sessions outlive pyred restarts (removed by pyred service uninstall)",
		UserID:      userID,
		Command:     exe,
		Args:        args,
		WorkDir:     filepath.Dir(socket),
		Hidden:      true,
	}
}
