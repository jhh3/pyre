package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/updates"
)

// Daemon self-update through the launcher (internal/updates/daemon.go).
//
// The service manager starts the installed pyred: the launcher. Before it
// serves, the launcher asks the updates manager whether a managed pyred is
// active over it and, if so, execs it (same PID, so launchd/systemd and
// their restart policy see one process) with these variables set. The
// managed daemon then proves its health once (hub connected, or started when
// standalone) or hands back to the launcher, which rolls it back.
const (
	envLauncher        = "PYRE_DAEMON_LAUNCHER"         // absolute path of the launcher pyred
	envLauncherVersion = "PYRE_DAEMON_LAUNCHER_VERSION" // its version
	envHandoffWatch    = "PYRE_DAEMON_HANDOFF_WATCH"    // "1": first boot, prove health
	// envHandoffTimeout overrides how long a newly activated pyred has to
	// become healthy (Go duration; default 90s).
	envHandoffTimeout = "PYRE_DAEMON_HANDOFF_TIMEOUT"
)

// execRequest asks cmdRun to replace this process (after every deferred
// close in runDaemon has run).
type execRequest struct {
	path, why string
	env       []string
}

func (e *execRequest) Error() string { return "exec " + e.path + ": " + e.why }

// launcherInfo returns the launcher's path and version: this process when it
// was started by the service manager, else what the launcher passed down.
func launcherInfo() (path, version string, managed bool) {
	if p := os.Getenv(envLauncher); p != "" {
		return p, os.Getenv(envLauncherVersion), true
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return exe, buildinfo.Version, false
}

// daemonBoot runs the launcher's handoff decision. It returns an exec
// request when this process must become another pyred, or nil to continue.
func daemonBoot(state string, log *slog.Logger) *execRequest {
	launcher, launcherVer, managed := launcherInfo()
	um, err := updates.New(updates.Options{StateDir: state, DaemonBase: launcherVer})
	if err != nil {
		log.Warn("daemon handoff check unavailable", "err", err)
		return nil
	}
	if managed {
		// A managed daemon must be the active managed version; otherwise the
		// pointer moved (rollback, supersede) and the launcher decides again.
		if act := um.ActivePyred(); act != buildinfo.Version {
			log.Warn("this managed pyred is no longer the active version; handing back to the launcher", "self", buildinfo.Version, "active", act)
			return &execRequest{path: launcher, why: "managed pyred no longer active", env: launcherEnv()}
		}
		return nil
	}
	dec, err := um.DaemonBoot(buildinfo.Version)
	if err != nil {
		log.Warn("daemon handoff check failed; running the installed pyred", "err", err)
		return nil
	}
	for _, n := range dec.Notes {
		log.Warn(n)
	}
	if dec.Exec == "" {
		return nil
	}
	env := append(launcherEnv(), envLauncher+"="+launcher, envLauncherVersion+"="+launcherVer)
	if dec.Watch {
		env = append(env, envHandoffWatch+"=1")
	}
	log.Info("handing off to the active managed pyred", "version", dec.Version, "exe", dec.Exec, "launcher", buildinfo.Version, "first_boot", dec.Watch)
	return &execRequest{path: dec.Exec, why: "managed pyred " + dec.Version, env: env}
}

// launcherEnv is the environment without the handoff variables.
func launcherEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case envLauncher, envLauncherVersion, envHandoffWatch:
			continue
		}
		out = append(out, kv)
	}
	return out
}

// restartViaLauncher is the exec request a running daemon uses to restart
// itself after a pyred activation/rollback (or a failed first boot).
func restartViaLauncher(why string) *execRequest {
	launcher, _, _ := launcherInfo()
	return &execRequest{path: launcher, why: why, env: launcherEnv()}
}

func handoffTimeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(envHandoffTimeout)); err == nil && d > 0 {
		return d
	}
	return 90 * time.Second
}

// selfRestart carries restart requests into runDaemon's main loop.
var selfRestart = make(chan *execRequest, 1)

func requestRestart(r *execRequest) {
	select {
	case selfRestart <- r:
	default:
	}
}

// absExecPath resolves an exec request's path against the working directory.
func absExecPath(p string) string {
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return p
}
