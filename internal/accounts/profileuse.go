package accounts

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// profileEnvVars are the variables that bind a process to a harness profile.
var profileEnvVars = []string{"CLAUDE_CONFIG_DIR=", "CODEX_HOME="}

// ProfileUsers returns the IDs of live processes (of any user whose
// environment this process may read) bound to profileDir through
// CLAUDE_CONFIG_DIR or CODEX_HOME. A harness, or a helper it started that
// outlived the run (for example a memory tool's detached enrichment worker
// that calls `claude -p`), can still read or refresh the profile's
// credential; a rota check-in must wait for them.
//
// Linux reads /proc/<pid>/environ. Other platforms return supported=false:
// macOS does not expose other processes' environments reliably (hardened
// binaries hide it from kern.procargs2), so there the check-in relies on
// the harness adapters' own busy checks.
func ProfileUsers(ctx context.Context, profileDir string) (pids []int, supported bool, err error) {
	if runtime.GOOS != "linux" {
		return nil, false, nil
	}
	pids, err = profileUsersProc(ctx, "/proc", profileDir, os.Getpid())
	return pids, true, err
}

func profileUsersProc(ctx context.Context, procRoot, profileDir string, self int) ([]int, error) {
	dir := filepath.Clean(profileDir)
	if dir == "" || dir == "." || dir == "/" {
		return nil, nil
	}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var want [][]byte
	for _, v := range profileEnvVars {
		want = append(want, []byte(v+dir), []byte(v+dir+"/"))
	}
	var out []int
	for _, e := range ents {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "environ"))
		if err != nil || len(b) == 0 {
			continue // exited, another user's process, or a kernel thread
		}
		for _, kv := range bytes.Split(b, []byte{0}) {
			if matchesAny(kv, want) {
				out = append(out, pid)
				break
			}
		}
	}
	sort.Ints(out)
	return out, nil
}

func matchesAny(kv []byte, want [][]byte) bool {
	for _, w := range want {
		if bytes.Equal(kv, w) {
			return true
		}
	}
	return false
}

// pidList renders a bounded pid list for a check-in detail.
func pidList(pids []int) string {
	s := make([]string, 0, len(pids))
	for i, p := range pids {
		if i == 8 {
			s = append(s, "…")
			break
		}
		s = append(s, strconv.Itoa(p))
	}
	return strings.Join(s, ",")
}

// HoldProfile marks profileDir as still in use by this daemon (for example
// a background capture of an ended run's transcript). A rota check-in of the
// profile stays pending until every hold is released, like it does for an
// external process bound to the profile. The returned function releases
// the hold (idempotent) and retries pending check-ins at once.
func (s *Service) HoldProfile(profileDir string) (release func()) {
	dir := filepath.Clean(profileDir)
	if profileDir == "" {
		return func() {}
	}
	s.hmu.Lock()
	if s.holds == nil {
		s.holds = map[string]int{}
	}
	s.holds[dir]++
	s.hmu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.hmu.Lock()
			if s.holds[dir]--; s.holds[dir] <= 0 {
				delete(s.holds, dir)
			}
			s.hmu.Unlock()
			if s.cfg.Rota.Enabled {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					s.SyncCheckouts(ctx)
				}()
			}
		})
	}
}

// profileHolds reports the number of in-process holds on profileDir.
func (s *Service) profileHolds(profileDir string) int {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	return s.holds[filepath.Clean(profileDir)]
}
