package tui

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// sshMux makes an ssh argv share one connection per host (ControlMaster).
// Every Grid tile, preview and window-size probe used to open its own ssh
// connection, so a busy Grid made dozens of handshakes a minute from one
// address; on a loaded host they exceeded LoginGraceTime and sshd's
// per-source penalties then refused every connection from that address,
// the owner's own shell included (server, 2026-10-08). Argv that already
// chooses a ControlPath/ControlMaster, or that is not ssh, is returned as is.
func sshMux(argv []string) []string {
	if len(argv) < 2 || filepath.Base(argv[0]) != "ssh" {
		return argv
	}
	for _, a := range argv {
		l := strings.ToLower(a)
		if strings.Contains(l, "controlpath") || strings.Contains(l, "controlmaster") {
			return argv
		}
	}
	dir := sshMuxDir()
	if dir == "" {
		return argv
	}
	out := make([]string, 0, len(argv)+6)
	out = append(out, argv[0],
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(dir, "%C"),
		"-o", "ControlPersist=10m")
	return append(out, argv[1:]...)
}

// sshMuxDir is a private, short directory for control sockets (unix socket
// paths are limited to ~104 bytes on macOS); "" disables multiplexing.
func sshMuxDir() string {
	if runtime.GOOS == "windows" {
		return "" // Windows OpenSSH has no ControlMaster multiplexing
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	dir := filepath.Join(home, ".ssh", "pyre-cm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm()&0o077 != 0 {
		return ""
	}
	if len(dir)+42 > 100 {
		return ""
	}
	return dir
}
