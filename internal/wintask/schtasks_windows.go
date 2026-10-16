//go:build windows

package wintask

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// CurrentUser returns this process's user SID string and account name.
func CurrentUser() (sid, name string, err error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", "", err
	}
	sid = tu.User.Sid.String()
	name = os.Getenv("USERNAME")
	if acct, _, _, err := tu.User.Sid.LookupAccount(""); err == nil {
		name = acct
	}
	return sid, name, nil
}

// Schtasks runs schtasks.exe without a console window; output is returned
// for error messages (it is localized, so it is never parsed beyond the
// task-name column).
func Schtasks(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Registered reports whether task exists.
func Registered(task string) bool {
	_, err := Schtasks("/Query", "/TN", task)
	return err == nil
}

// Register creates or replaces task from its XML definition.
func Register(task, def string) error {
	tmp, err := os.CreateTemp("", "pyre-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(UTF16File(def)); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if out, err := Schtasks("/Create", "/TN", task, "/XML", tmp.Name(), "/F"); err != nil {
		return fmt.Errorf("register task %q: %v: %s", task, err, out)
	}
	return nil
}

// Run starts task now.
func Run(task string) error {
	if out, err := Schtasks("/Run", "/TN", task); err != nil {
		return fmt.Errorf("start task %q: %v: %s", task, err, out)
	}
	return nil
}

// Delete removes task's registration (a running instance is not ended).
func Delete(task string) error {
	if out, err := Schtasks("/Delete", "/TN", task, "/F"); err != nil {
		return fmt.Errorf("delete task %q: %v: %s", task, err, out)
	}
	return nil
}

// List returns the names of the registered tasks this user can see.
func List() ([]string, error) {
	out, err := Schtasks("/Query", "/FO", "CSV", "/NH")
	if err != nil {
		return nil, fmt.Errorf("list tasks: %v: %s", err, out)
	}
	return ParseTaskNames(out), nil
}
