package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/stablepath"
	"github.com/jhh3/pyre/internal/wintask"
)

// `pyred service install|uninstall|status|start|stop` manages the Windows
// node service: a per-user Task Scheduler task (W-winport).
//
// Why a logon task and not a Windows service: services run in session 0,
// which has no interactive desktop, so agents could not launch or test GUI
// (Electron) apps. The task starts at the user's logon in the interactive
// session with the user's own token (LogonType InteractiveToken, least
// privilege), runs `pyred supervise --hide-console -- run …`, and repeats
// its trigger every 5 minutes with MultipleInstancesPolicy IgnoreNew: a
// watchdog that restarts the supervisor if it ever dies, without ever
// running two. RestartOnFailure covers a failed launch. On macOS and Linux
// the service is installed by scripts/install/install.sh (launchd/systemd).

// serviceOpts are the parsed `pyred service` flags.
type serviceOpts struct {
	action, state, logDir, task, exe string
	noStart, asJSON                  bool
	runArgs                          []string
}

func parseServiceArgs(args []string) (serviceOpts, error) {
	var o serviceOpts
	if len(args) == 0 {
		return o, errors.New("usage: pyred service install|uninstall|status|start|stop [flags] [-- <pyred run flags>]")
	}
	o.action = args[0]
	fs := flag.NewFlagSet("service "+o.action, flag.ContinueOnError)
	fs.StringVar(&o.state, "state", daemon.DefaultStateDir(), "node state directory")
	fs.StringVar(&o.logDir, "log-dir", "", "service log directory (default <state>/logs)")
	fs.StringVar(&o.task, "task", "", "task name (default \"Pyre pyred <user>\")")
	fs.StringVar(&o.exe, "exe", "", "pyred.exe the task runs (default: this pyred through the install's current link)")
	fs.BoolVar(&o.noStart, "no-start", false, "install: register the task without starting it")
	fs.BoolVar(&o.asJSON, "json", false, "status: print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return o, err
	}
	o.runArgs = fs.Args()
	switch o.action {
	case "install", "uninstall", "status", "start", "stop":
	default:
		return o, fmt.Errorf("unknown service action %q (install, uninstall, status, start, stop)", o.action)
	}
	if o.action != "install" && len(o.runArgs) > 0 {
		return o, fmt.Errorf("service %s takes no run flags", o.action)
	}
	for _, a := range o.runArgs {
		if a == "--state" || strings.HasPrefix(a, "--state=") || a == "-state" || strings.HasPrefix(a, "-state=") {
			return o, errors.New("pass the state directory as `pyred service install --state DIR`, not after --")
		}
	}
	if o.logDir == "" {
		o.logDir = filepath.Join(o.state, "logs")
	}
	if o.exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return o, err
		}
		o.exe = stablepath.Of(exe)
	}
	return o, nil
}

// defaultTaskName is "Pyre pyred <user>": one task per user on a shared
// machine (Task Scheduler names are machine-wide).
func defaultTaskName(user string) string { return "Pyre pyred " + wintask.SafeName(user) }

// taskArgv is the command line the task runs.
func (o serviceOpts) taskArgv() []string {
	argv := []string{"supervise", "--hide-console", "--log", filepath.Join(o.logDir, "pyred-service.log"), "--", "run", "--state", o.state}
	return append(argv, o.runArgs...)
}

func winQuoteArg(s string) string { return wintask.QuoteArg(s) }

func winJoin(argv []string) string { return wintask.Join(argv) }

func xmlText(s string) string { return wintask.XMLText(s) }

// taskXML renders the Task Scheduler definition (schema 1.2, Windows 7+)
// for user (a SID string or DOMAIN\name).
func taskXML(o serviceOpts, user string) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Author>Pyre</Author>
    <Description>Pyre node daemon (pyred) in this user's interactive session; managed by pyred service install</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + xmlText(user) + `</UserId>
      <Repetition>
        <Interval>PT5M</Interval>
        <StopAtDurationEnd>false</StopAtDurationEnd>
      </Repetition>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlText(user) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlText(winQuoteArg(o.exe)) + `</Command>
      <Arguments>` + xmlText(winJoin(o.taskArgv())) + `</Arguments>
      <WorkingDirectory>` + xmlText(o.state) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

// serviceStatus is `pyred service status --json`.
type serviceStatus struct {
	Task       string `json:"task"`
	Registered bool   `json:"registered"`
	Daemon     bool   `json:"daemon_running"`
	Socket     string `json:"socket"`
	Detail     string `json:"detail,omitempty"`
}

// socketAnswers reports whether a pyred serves the state dir's socket.
func socketAnswers(state string) (string, bool) {
	sock := filepath.Join(state, "pyred.sock")
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return sock, false
	}
	c.Close()
	return sock, true
}

func printServiceStatus(o serviceOpts, st serviceStatus) error {
	if o.asJSON {
		b, _ := json.Marshal(st)
		fmt.Println(string(b))
		return nil
	}
	reg := "not registered"
	if st.Registered {
		reg = "registered"
	}
	run := "not running"
	if st.Daemon {
		run = "running"
	}
	fmt.Printf("task %q: %s; pyred %s (%s)\n", st.Task, reg, run, st.Socket)
	if st.Detail != "" {
		fmt.Println(st.Detail)
	}
	return nil
}

func cmdService(args []string) error {
	o, err := parseServiceArgs(args)
	if err != nil {
		return err
	}
	return runService(o)
}
