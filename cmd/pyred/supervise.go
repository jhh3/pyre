package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/supervisor"
)

// `pyred supervise [--log FILE] [--hide-console] -- run <flags>` keeps a
// pyred daemon running: it starts the given pyred command as a child,
// restarts it with backoff when it crashes, and carries out the daemon's
// self-update exec requests (W-winport).
//
// Windows has no execve, so the launcher handoff of internal/updates
// (doExec in handoff.go) cannot replace the process in place. Instead every
// daemon the supervisor starts gets envTrampoline (a request file) and
// envTrampolinePID; doExec writes the target (path + the handoff variables,
// never secrets) to the file and exits with exitExecRequest, and the
// supervisor starts that target next. A crash or an unexplained exit
// restarts the launcher (the installed pyred), the same recovery launchd
// and systemd perform on Unix. The Windows logon task (pyred service
// install) runs this command; a daemon started by hand becomes its own
// supervisor on its first exec request. The child watches the supervisor's
// pid and shuts down when it is gone (service stop ends the supervisor).
const (
	envTrampoline    = "PYRE_EXEC_TRAMPOLINE"
	envTrampolinePID = "PYRE_EXEC_TRAMPOLINE_PID"
	exitExecRequest  = 75 // EX_TEMPFAIL: "run the requested pyred next"
)

// Trampoline settings read (and removed from the environment, so runs and
// hooks never inherit them) when `pyred run` starts.
var trampolineFile, trampolinePID string

func takeTrampolineEnv() {
	trampolineFile, trampolinePID = os.Getenv(envTrampoline), os.Getenv(envTrampolinePID)
	_ = os.Unsetenv(envTrampoline)
	_ = os.Unsetenv(envTrampolinePID)
}

// trampolineRequest is the exec request a child hands its supervisor.
type trampolineRequest struct {
	Path string            `json:"path"`
	Why  string            `json:"why,omitempty"`
	Set  map[string]string `json:"set,omitempty"` // handoff variables only
}

// handoffVars are the only variables an exec request carries; everything
// else comes from the supervisor's own environment.
var handoffVars = []string{envLauncher, envLauncherVersion, envHandoffWatch}

func requestFromExec(r *execRequest) trampolineRequest {
	t := trampolineRequest{Path: absExecPath(r.path), Why: r.why, Set: map[string]string{}}
	for _, kv := range r.env {
		k, v, _ := strings.Cut(kv, "=")
		for _, h := range handoffVars {
			if strings.EqualFold(k, h) {
				t.Set[h] = v
			}
		}
	}
	return t
}

func writeTrampolineRequest(path string, t trampolineRequest) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readTrampolineRequest(path string) (trampolineRequest, bool) {
	var t trampolineRequest
	b, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil || json.Unmarshal(b, &t) != nil || t.Path == "" || !filepath.IsAbs(t.Path) {
		return t, false
	}
	return t, true
}

// envWithout returns env minus the named variables (case-insensitive, as
// Windows environment names are).
func envWithout(env []string, names ...string) []string {
	var out []string
outer:
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		for _, n := range names {
			if strings.EqualFold(k, n) {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}

func cmdSupervise(args []string) error {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	logPath := fs.String("log", "", "append the daemon's stdout/stderr to this file (rotated at 5 MB; default: inherit)")
	hide := fs.Bool("hide-console", false, "Windows: detach from the console window the logon task opens")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		rest = []string{"run"}
	}
	if rest[0] != "run" {
		return errors.New("supervise runs `pyred run …`: pass -- run <flags>")
	}
	if *hide {
		hideConsole()
	}
	var out io.Writer = os.Stderr
	if *logPath != "" {
		if err := os.MkdirAll(filepath.Dir(*logPath), 0o700); err != nil {
			return err
		}
		rf := &supervisor.RotatingFile{Path: *logPath, MaxBytes: 5 << 20, Backups: 3}
		defer rf.Close()
		out = rf
	} else if *hide {
		out = io.Discard
	}
	launcher, err := os.Executable()
	if err != nil {
		return err
	}
	return superviseLoop(launcher, rest, nil, out, *hide)
}

// superviseLoop runs pyred children until one exits cleanly (or the
// supervisor is interrupted). first, when set, is the request to start
// before falling back to the launcher.
func superviseLoop(launcher string, args []string, first *trampolineRequest, out io.Writer, noWindow bool) error {
	f, err := os.CreateTemp("", "pyred-exec-*.json")
	if err != nil {
		return err
	}
	reqFile := f.Name()
	f.Close()
	_ = os.Remove(reqFile)
	defer os.Remove(reqFile)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	base := envWithout(os.Environ(), append([]string{envTrampoline, envTrampolinePID}, handoffVars...)...)
	next := trampolineRequest{Path: launcher}
	if first != nil {
		next = *first
	}
	logf := func(format string, a ...any) {
		fmt.Fprintf(out, "%s pyred supervise: %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}
	backoff := time.Second
	for {
		env := append(append([]string(nil), base...), envTrampoline+"="+reqFile, envTrampolinePID+"="+strconv.Itoa(os.Getpid()))
		for k, v := range next.Set {
			env = append(env, k+"="+v)
		}
		_ = os.Remove(reqFile)
		cmd := exec.Command(next.Path, args...)
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = out, out
		if !noWindow {
			cmd.Stdin = os.Stdin
		}
		childAttrs(cmd, noWindow)
		started := time.Now()
		logf("starting %s %s", next.Path, strings.Join(args, " "))
		code := -1
		if err := cmd.Start(); err != nil {
			logf("cannot start %s: %v", next.Path, err)
		} else {
			done := make(chan struct{})
			go func() {
				select {
				case <-stop:
					// Interrupted (console Ctrl+C reaches the child too):
					// let it shut down, then leave.
					logf("interrupted; waiting for the daemon to stop")
					go func() {
						select {
						case <-time.After(30 * time.Second):
							_ = cmd.Process.Kill()
						case <-done:
						}
					}()
					<-done
					os.Exit(0)
				case <-done:
				}
			}()
			_ = cmd.Wait()
			close(done)
			code = cmd.ProcessState.ExitCode()
		}
		if code == exitExecRequest {
			if t, ok := readTrampolineRequest(reqFile); ok {
				logf("exec %s (%s)", t.Path, t.Why)
				next, backoff = t, time.Second
				continue
			}
		}
		if code == 0 {
			logf("daemon exited cleanly; supervisor stops")
			return nil
		}
		if time.Since(started) > 5*time.Minute {
			backoff = time.Second
		}
		logf("daemon exited with code %d; restarting the launcher %s in %s", code, launcher, backoff)
		time.Sleep(backoff)
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
		next = trampolineRequest{Path: launcher}
	}
}
