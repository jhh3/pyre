package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Unreadable plugin marketplaces (live on laptop, 2026-10-06). The owner's
// settings may name a local "directory" marketplace (extraKnownMarketplaces,
// plugins/known_marketplaces.json) under a folder macOS privacy protection
// (TCC) guards, such as ~/Documents. pyred runs as a LaunchAgent; a process
// it starts that reads such a folder without a grant blocks on a privacy
// prompt nobody answers on a headless machine. Claude Code reads every
// known marketplace while it loads commands, before it draws anything, so
// the run sat in "starting" forever with a blank pane. The same profile
// started from an ssh shell works, because ssh sessions are not subject to
// the same prompt.
//
// So composition probes each local marketplace path (bounded; a blocked
// probe counts as unreadable and is never repeated while it is in flight)
// and drops an unreadable marketplace and the plugins enabled from it from
// the profile's per-launch copies only. The owner's files are untouched and
// the gap is reported (ProfileInfo.Unavailable, logged), not silent.

// A readable marketplace is always kept (with Full Disk Access granted to
// Pyre, laptop 2026-10, a ~/Documents marketplace reads fine and its
// plugins, development channels included, work in every profile). The
// probe reads what Claude reads: the directory and, for a directory
// marketplace, its .claude-plugin/marketplace.json (on laptop the folder
// listed while that manifest blocked).

// marketplaceProbeTimeout bounds one readability probe.
var marketplaceProbeTimeout = 2 * time.Second

// readDirProbe lists one entry of dir; replaced in tests.
var readDirProbe = func(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && !st.IsDir() {
		if _, err = f.Read(make([]byte, 1)); errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if _, err = f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	m, err := os.Open(filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // empty directory or no manifest (Claude reports that itself)
	}
	if err != nil {
		return err
	}
	defer m.Close()
	if _, err = m.Read(make([]byte, 1)); errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// probeCall is one in-flight probe of a path; every caller waits on it.
type probeCall struct {
	done    chan struct{}
	err     error
	started time.Time
}

var probes = struct {
	mu       sync.Mutex
	inflight map[string]*probeCall
}{inflight: map[string]*probeCall{}}

// probeReadable reports whether path can be read within the timeout.
// Concurrent callers (launches binding the same account at once) share one
// probe and its answer, so a probe another launch started a moment earlier
// is not mistaken for a blocked path; a probe still blocked after the
// timeout costs at most one goroutine however many launches ask.
func probeReadable(path string) error {
	// The probe goroutine may outlive this call (that is its point), so it
	// gets the probe function now instead of reading the package variable
	// later, unsynchronised with whoever replaces it (tests do).
	probe := readDirProbe
	probes.mu.Lock()
	pc, busy := probes.inflight[path]
	if !busy {
		pc = &probeCall{done: make(chan struct{}), started: time.Now()}
		probes.inflight[path] = pc
		go func() {
			pc.err = probe(path)
			probes.mu.Lock()
			delete(probes.inflight, path)
			probes.mu.Unlock()
			close(pc.done)
		}()
	}
	probes.mu.Unlock()
	wait := marketplaceProbeTimeout - time.Since(pc.started)
	if wait <= 0 {
		return fmt.Errorf("still blocked from an earlier read (macOS privacy prompt or hung mount?)")
	}
	select {
	case <-pc.done:
		return pc.err
	case <-time.After(wait):
		return fmt.Errorf("no answer within %s (macOS privacy prompt or hung mount?)", marketplaceProbeTimeout)
	}
}

// localMarketplacePath returns the local path of a marketplace entry
// ({"source":{"source":"directory"|"file","path":...}}), or "".
func localMarketplacePath(raw json.RawMessage) string {
	var e struct {
		Source struct {
			Source string `json:"source"`
			Path   string `json:"path"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	switch e.Source.Source {
	case "directory", "file":
		if filepath.IsAbs(e.Source.Path) {
			return e.Source.Path
		}
	}
	return ""
}

// readJSONObject reads a JSON object file as raw members; ok=false when the
// file is absent or not an object.
func readJSONObject(p string) (map[string]json.RawMessage, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

func writeJSONObject(p string, m map[string]json.RawMessage) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'), 0o600)
}

// dropUnreadableMarketplaces removes local marketplaces the daemon cannot
// read (and plugins enabled from them) from the Claude profile copies in dir.
func dropUnreadableMarketplaces(info *ProfileInfo, dir string) {
	settingsPath := filepath.Join(dir, "settings.json")
	knownPath := filepath.Join(dir, "plugins", "known_marketplaces.json")
	settings, haveSettings := readJSONObject(settingsPath)
	known, haveKnown := readJSONObject(knownPath)

	var extra map[string]json.RawMessage
	if haveSettings {
		if raw, ok := settings["extraKnownMarketplaces"]; ok {
			_ = json.Unmarshal(raw, &extra)
		}
	}
	paths := map[string]string{} // marketplace -> local path
	for name, raw := range extra {
		if p := localMarketplacePath(raw); p != "" {
			paths[name] = p
		}
	}
	for name, raw := range known {
		if p := localMarketplacePath(raw); p != "" && paths[name] == "" {
			paths[name] = p
		}
	}
	if len(paths) == 0 {
		return
	}
	names := make([]string, 0, len(paths))
	for n := range paths {
		names = append(names, n)
	}
	sort.Strings(names)
	bad := map[string]bool{}
	for _, name := range names {
		if err := probeReadable(paths[name]); err != nil {
			bad[name] = true
			info.Unavailable = append(info.Unavailable, fmt.Sprintf(
				"plugin marketplace %q (%s) is not readable by Pyre: %v; it and its plugins are left out of this profile. On macOS, allow pyred to access that folder (System Settings > Privacy & Security > Files and Folders or Full Disk Access), or move the marketplace out of a protected folder",
				name, paths[name], err))
		}
	}
	if len(bad) == 0 {
		return
	}
	if haveSettings {
		changed := false
		if extra != nil {
			for n := range bad {
				if _, ok := extra[n]; ok {
					delete(extra, n)
					changed = true
				}
			}
			if b, err := json.Marshal(extra); err == nil {
				settings["extraKnownMarketplaces"] = b
			}
		}
		var enabled map[string]json.RawMessage
		if raw, ok := settings["enabledPlugins"]; ok && json.Unmarshal(raw, &enabled) == nil {
			for id := range enabled {
				if i := strings.LastIndex(id, "@"); i >= 0 && bad[id[i+1:]] {
					delete(enabled, id)
					changed = true
				}
			}
			if b, err := json.Marshal(enabled); err == nil {
				settings["enabledPlugins"] = b
			}
		}
		if changed {
			if err := writeJSONObject(settingsPath, settings); err != nil {
				info.Conflicts = append(info.Conflicts, "settings.json: "+err.Error())
			}
		}
	}
	if haveKnown {
		changed := false
		for n := range bad {
			if _, ok := known[n]; ok {
				delete(known, n)
				changed = true
			}
		}
		if changed {
			if err := writeJSONObject(knownPath, known); err != nil {
				info.Conflicts = append(info.Conflicts, "plugins/known_marketplaces.json: "+err.Error())
			}
		}
	}
}
