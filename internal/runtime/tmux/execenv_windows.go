//go:build windows

package tmux

import (
	"errors"
	"os"
	"os/exec"
)

// execReplace: Windows has no exec(2). The launcher runs argv as a child
// with the inherited console and exits with its status. (The tmux backend
// is not used on Windows; termhost passes the environment over its private
// socket instead and needs no launcher.)
func execReplace(path string, argv, env []string) (int, error) {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 127, err
	}
	return 0, nil
}

// ownedByMe: the file lives in the per-user state directory, whose ACL
// admits only this user (and SYSTEM/Administrators); Windows has no uid to
// compare.
func ownedByMe(file string, st os.FileInfo) error { return nil }
