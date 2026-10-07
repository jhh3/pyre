package digest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultWindow is the cutoff used when this client has never marked the
// digest seen.
const DefaultWindow = 12 * time.Hour

// LastSeen is this client's "last looked" mark. It lives in the client's
// own config directory (beside client.json), so every owner client (each
// CLI config, the TUI that shares it, the Android app) keeps its own.
type LastSeen struct {
	Cursor int64     `json:"cursor"`
	At     time.Time `json:"at"`
}

// StateFile is the last-seen file for a client config path.
func StateFile(clientConfigPath string) string {
	return filepath.Join(filepath.Dir(clientConfigPath), "digest-state.json")
}

// LoadLastSeen reads the mark; ok is false when there is none.
func LoadLastSeen(path string) (LastSeen, bool) {
	if path == "" {
		return LastSeen{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return LastSeen{}, false
	}
	var ls LastSeen
	if json.Unmarshal(b, &ls) != nil || (ls.Cursor <= 0 && ls.At.IsZero()) {
		return LastSeen{}, false
	}
	return ls, true
}

// SaveLastSeen writes the mark atomically (0600; it holds no secrets but
// sits beside the credential file). An older mark never replaces a newer
// one, so two clients sharing a config cannot move it backwards.
func SaveLastSeen(path string, ls LastSeen) error {
	if path == "" {
		return nil
	}
	if cur, ok := LoadLastSeen(path); ok && cur.Cursor > ls.Cursor {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Cutoff resolves a --since value into fetch options: "last" (or empty)
// is the stored mark (DefaultWindow ago when there is none); otherwise a
// duration such as "90m", "2h", "1d", or an RFC 3339 time.
func Cutoff(since string, now time.Time, statePath string) (Options, error) {
	s := strings.TrimSpace(strings.ToLower(since))
	if s == "" || s == "last" {
		if ls, ok := LoadLastSeen(statePath); ok {
			at := ls.At
			if at.IsZero() {
				at = now.Add(-DefaultWindow)
			}
			return Options{AfterCursor: ls.Cursor, Since: at, Label: "last seen", Now: now}, nil
		}
		return Options{Since: now.Add(-DefaultWindow), Label: "last " + shortDur(DefaultWindow), Now: now}, nil
	}
	if t, err := time.Parse(time.RFC3339, since); err == nil {
		return Options{Since: t, Label: "since " + t.Local().Format("Jan 2 15:04"), Now: now}, nil
	}
	d, err := ParseDuration(s)
	if err != nil {
		return Options{}, err
	}
	return Options{Since: now.Add(-d), Label: "last " + shortDur(d), Now: now}, nil
}

// ParseDuration accepts Go durations plus a "d" (day) unit: 2h, 90m, 1d, 1d12h.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	if i := strings.Index(s, "d"); i > 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (use 2h, 90m, 1d or last)", s)
		}
		total = time.Duration(n) * 24 * time.Hour
		s = s[i+1:]
	}
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (use 2h, 90m, 1d or last)", s)
		}
		total += d
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return total, nil
}

func shortDur(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return d.String()
}
