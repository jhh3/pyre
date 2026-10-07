package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if _, err = f.Readdirnames(1); errors.Is(err, io.EOF) {
		return nil // empty directory
	}
	return err
}

var probes = struct {
	mu       sync.Mutex
	inflight map[string]chan error
}{inflight: map[string]chan error{}}

// probeReadable reports whether path can be read within the timeout. A
// probe still blocked from an earlier call reports unreadable at once, so a
// stuck path costs at most one goroutine however many launches ask.
func probeReadable(path string) error {
	// The probe goroutine may outlive this call (that is its point), so it
	// gets the probe function now instead of reading the package variable
	// later, unsynchronised with whoever replaces it (tests do).
	probe := readDirProbe
	probes.mu.Lock()
	ch, busy := probes.inflight[path]
	if !busy {
		ch = make(chan error, 1)
		probes.inflight[path] = ch
		go func() {
			err := probe(path)
			probes.mu.Lock()
			delete(probes.inflight, path)
			probes.mu.Unlock()
			ch <- err
		}()
	}
	probes.mu.Unlock()
	if busy {
		return fmt.Errorf("still blocked from an earlier read (macOS privacy prompt or hung mount?)")
	}
	select {
	case err := <-ch:
		return err
	case <-time.After(marketplaceProbeTimeout):
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
