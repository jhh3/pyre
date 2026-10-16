//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/runtime/hostsel"
	"github.com/jhh3/pyre/internal/runtime/termhost"
	"github.com/jhh3/pyre/internal/wintask"
)

func currentUser() (sid, name string, err error) { return wintask.CurrentUser() }

func schtasks(args ...string) (string, error) { return wintask.Schtasks(args...) }

func taskRegistered(task string) bool { return wintask.Registered(task) }

func utf16File(s string) []byte { return wintask.UTF16File(s) }

// removeSessionHostTasks deletes the user's on-demand session-host tasks
// (termhost, started when pyred's job forbids breakaway). A host that is
// running keeps running with its sessions until it exits.
func removeSessionHostTasks(user, state string) {
	names := map[string]bool{}
	if all, err := wintask.List(); err == nil {
		prefix := termhost.SessionTaskPrefix(user)
		for _, n := range all {
			if strings.HasPrefix(n, prefix) {
				names[n] = true
			}
		}
	}
	if n := termhost.SessionTaskName(user, hostsel.TermhostSocket(filepath.Join(state, "tmux", "pyre.sock"))); wintask.Registered(n) {
		names[n] = true
	}
	for n := range names {
		if err := wintask.Delete(n); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			continue
		}
		fmt.Printf("removed session-host task %q\n", n)
	}
}

func runService(o serviceOpts) error {
	sid, name, err := currentUser()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	if o.task == "" {
		o.task = defaultTaskName(name)
	}
	switch o.action {
	case "status":
		sock, up := socketAnswers(o.state)
		return printServiceStatus(o, serviceStatus{Task: o.task, Registered: taskRegistered(o.task), Daemon: up, Socket: sock})
	case "start":
		if out, err := schtasks("/Run", "/TN", o.task); err != nil {
			return fmt.Errorf("start task %q: %v: %s", o.task, err, out)
		}
		fmt.Printf("started task %q\n", o.task)
		return nil
	case "stop":
		// Ending the task terminates the supervisor; the daemon notices and
		// shuts down cleanly (runs keep running and are recovered).
		if out, err := schtasks("/End", "/TN", o.task); err != nil {
			return fmt.Errorf("stop task %q: %v: %s", o.task, err, out)
		}
		fmt.Printf("stopped task %q\n", o.task)
		return nil
	case "uninstall":
		removeSessionHostTasks(name, o.state)
		if !taskRegistered(o.task) {
			fmt.Printf("task %q is not registered\n", o.task)
			return nil
		}
		_, _ = schtasks("/End", "/TN", o.task)
		if out, err := schtasks("/Delete", "/TN", o.task, "/F"); err != nil {
			return fmt.Errorf("delete task %q: %v: %s", o.task, err, out)
		}
		fmt.Printf("removed task %q (state in %s kept)\n", o.task, o.state)
		return nil
	}
	// install
	if _, err := os.Stat(o.exe); err != nil {
		return fmt.Errorf("pyred executable %s: %w", o.exe, err)
	}
	for _, d := range []string{o.state, o.logDir} {
		if err := fileperm.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	_ = fileperm.Restrict(o.state, 0o700)
	tmp, err := os.CreateTemp("", "pyred-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(utf16File(taskXML(o, sid))); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if out, err := schtasks("/Create", "/TN", o.task, "/XML", tmp.Name(), "/F"); err != nil {
		return fmt.Errorf("register task %q: %v: %s (if Task Scheduler refuses a per-user logon task here, run this once from an elevated prompt as the same user)", o.task, err, out)
	}
	fmt.Printf("registered task %q: at logon of %s, runs %s %s\n", o.task, name, o.exe, winJoin(o.taskArgv()))
	if o.noStart {
		return nil
	}
	if _, up := socketAnswers(o.state); up {
		fmt.Println("a pyred already serves this state directory; not starting another (pyred service stop, then start, to switch)")
		return nil
	}
	if out, err := schtasks("/Run", "/TN", o.task); err != nil {
		return fmt.Errorf("start task %q: %v: %s", o.task, err, out)
	}
	fmt.Printf("started task %q\n", o.task)
	return nil
}
