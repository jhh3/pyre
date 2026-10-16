//go:build !windows

package main

import "errors"

// runService: the logon task is Windows-only; Unix nodes use launchd or
// systemd units rendered by scripts/install/install.sh.
func runService(o serviceOpts) error {
	if o.action == "status" {
		sock, up := socketAnswers(o.state)
		return printServiceStatus(o, serviceStatus{Task: "(launchd/systemd)", Daemon: up, Socket: sock,
			Detail: "the macOS/Linux service is managed by scripts/install/install.sh --service node (launchctl / systemctl --user)"})
	}
	return errors.New("pyred service manages the Windows logon task; on macOS and Linux use scripts/install/install.sh --service node")
}
