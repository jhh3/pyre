package sentinel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/transport"
)

// DefaultMaintenancePath is ~/.local/state/pyre/maintenance.json, the file
// sentinels read on each watched host over ssh.
func DefaultMaintenancePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, remoteMaintenanceFile)
}

// WriteMaintenance records a maintenance window until now+d.
func WriteMaintenance(path string, now time.Time, d time.Duration, reason string) (*Window, error) {
	if d <= 0 {
		return nil, errors.New("maintenance: duration must be positive")
	}
	w := &Window{Until: now.Add(d).UTC().Truncate(time.Second), Reason: reason, Set: now.UTC().Truncate(time.Second)}
	if err := fileperm.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(w)
	if err := transport.WriteFileAtomic(path, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return w, nil
}

// ClearMaintenance removes the window file.
func ClearMaintenance(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ReadMaintenance reads the window file; a missing file is (nil, nil).
func ReadMaintenance(path string) (*Window, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseWindow(b)
}

// parseWindow decodes maintenance.json; empty input is no window.
func parseWindow(b []byte) (*Window, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, nil
	}
	var w Window
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if w.Until.IsZero() {
		return nil, errors.New("maintenance: no until")
	}
	return &w, nil
}

// maintenanceScript writes (or, with w nil, removes) maintenance.json on a
// target. The window is single-line JSON fed through a heredoc with a
// random terminator.
func maintenanceScript(w *Window) (string, error) {
	if w == nil {
		return fmt.Sprintf("rm -f \"$HOME/%s\" && echo @@ok\n", remoteMaintenanceFile), nil
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	var r [8]byte
	_, _ = rand.Read(r[:])
	eof := "PYRE_MAINT_" + hex.EncodeToString(r[:])
	return fmt.Sprintf("f=\"$HOME/%s\"; mkdir -p \"$(dirname \"$f\")\" && cat > \"$f.tmp.$$\" <<'%s' && mv -f \"$f.tmp.$$\" \"$f\" && echo @@ok\n%s\n%s\n",
		remoteMaintenanceFile, eof, b, eof), nil
}

// SetRemoteMaintenance records a window of d on node (a sentinel target)
// over the sentinel's ssh aliases and identity, or clears it when d is 0.
// Every sentinel watching node reads it on its next probe.
func SetRemoteMaintenance(ctx context.Context, cfg *Config, node string, now time.Time, d time.Duration, reason, sshBin string) (*Window, error) {
	t, ok := cfg.target(node)
	if !ok {
		return nil, fmt.Errorf("maintenance: %q is not a target or sentinel in the sentinel config", node)
	}
	if d < 0 {
		return nil, errors.New("maintenance: duration must be positive")
	}
	var w *Window
	if d > 0 {
		w = &Window{Until: now.Add(d).UTC().Truncate(time.Second), Reason: reason, Set: now.UTC().Truncate(time.Second)}
	}
	script, err := maintenanceScript(w)
	if err != nil {
		return nil, err
	}
	p := &SSHProber{Self: cfg.Self, SSHBin: sshBin, Identity: cfg.SSHIdentity}
	var last error
	for _, alias := range t.SSH {
		out, stderr, err := p.ssh(ctx, alias, script, sshTimeout)
		if strings.Contains(out, "@@ok") {
			return w, nil
		}
		_, detail := classifySSH(stderr, err)
		last = fmt.Errorf("ssh %s: %s", alias, detail)
	}
	if last == nil {
		last = errors.New("no ssh alias")
	}
	return nil, fmt.Errorf("maintenance on %s: %w", node, last)
}
