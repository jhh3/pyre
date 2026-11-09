package tmux

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A run's process tree (iss_…).
//
// Signalling the pane's process group is not enough to end a run: a
// harness starts plugin and MCP servers that may leave its group or
// terminal session (a wrapper such as `bun run start` whose server
// outlives it, a server spawned detached), and after a graceful /exit the
// pane is dead, so its group used to be skipped altogether. Those servers
// kept running after the session was archived.
//
// TerminateSession therefore ends the tree as it stood while the harness
// was alive: the pane process, every descendant (by parent PID; on Linux
// only those still in the pane's cgroup, so a command an agent moved to a
// scope or unit of its own with systemd-run is left alone), and the
// members of its process group and terminal session. A stop that first
// asks the harness to exit (Claude's and agy's /exit) records the tree
// beforehand with MarkTree, since the exit orphans it. Each recorded
// process is signalled only while its start time still matches.
//
// Detached jobs survive by construction: `setsid -f` and `systemd-run
// --user --unit` reparent the job away from the harness at once, so it is
// no descendant when the tree is taken, and it has a session and group of
// its own.
//
// A process can also leave the tree before the stop
// (iss_…): `pyre run -- claude -p` from a Bash
// tool runs a nested Claude in a process group (and the tool's terminal
// session) of its own; when it exits, its plugin server is reparented to
// the subreaper, in neither the pane's group nor its session, and no
// descendant. On Linux it is still in the run's own scope
// (pyre-run-<session>-<suffix>.scope, supervisor runscope.go), so
// TerminateSession also ends that scope's leftovers: its members whose
// terminal session has no live leader any more. A `setsid -f` job leads a
// session of its own and keeps it while it runs, so it is spared, as is
// anything in another scope or unit (systemd-run). The shared tmux server
// scope is never used.

// procInfo is one row of the process table.
type procInfo struct {
	pid, ppid, pgid, sid int // sid -1: unknown (macOS ps)
	start                string
	zombie               bool
}

// procRef names one process for a later signal: pid and start time.
type procRef struct {
	pid   int
	start string
}

// Test seams: the process table, a process's cgroup ("" unknown) and the
// signal sender.
var (
	listProcs  = platformListProcs
	procCgroup = platformProcCgroup
	signalProc = platformSignal
)

// treeMarks holds the trees MarkTree recorded, per server socket and
// session name, until TerminateSession takes them.
var treeMarks = struct {
	sync.Mutex
	m map[string]treeMark
}{m: map[string]treeMark{}}

type treeMark struct {
	refs []procRef
	at   time.Time
}

// treeMarkTTL bounds how long a recorded tree is used (a stop that was
// refused after MarkTree leaves its record behind).
const treeMarkTTL = 2 * time.Minute

// MarkTree records the session's process tree for its TerminateSession,
// for a stop that first asks the harness to exit (which orphans its
// children). Best effort: nothing is recorded without a live pane.
func (s *Server) MarkTree(ctx context.Context, name string) {
	p, err := s.Pane(ctx, name)
	if err != nil || p.PID <= 0 || p.Dead {
		return
	}
	refs := snapshotTree(p.PID, true)
	now := time.Now()
	treeMarks.Lock()
	defer treeMarks.Unlock()
	for k, m := range treeMarks.m {
		if now.Sub(m.at) > treeMarkTTL {
			delete(treeMarks.m, k)
		}
	}
	treeMarks.m[s.Socket+"\x00"+name] = treeMark{refs: refs, at: now}
}

func (s *Server) takeTree(name string) []procRef {
	treeMarks.Lock()
	defer treeMarks.Unlock()
	k := s.Socket + "\x00" + name
	m, ok := treeMarks.m[k]
	delete(treeMarks.m, k)
	if !ok || time.Since(m.at) > treeMarkTTL {
		return nil
	}
	return m.refs
}

// snapshotTree lists root's tree: root (when alive), its descendants
// (alive: true; on Linux in root's cgroup) and the members of the process
// group and terminal session root leads. pyred itself is never listed.
func snapshotTree(root int, alive bool) []procRef {
	procs, err := listProcs()
	if err != nil {
		return nil
	}
	return treeOf(procs, root, alive, procCgroup)
}

func treeOf(procs []procInfo, root int, alive bool, cgroupOf func(int) string) []procRef {
	self := os.Getpid()
	children := map[int][]procInfo{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p)
	}
	seen := map[int]bool{}
	var out []procRef
	add := func(p procInfo) {
		if p.pid <= 1 || p.pid == self || p.zombie || seen[p.pid] {
			return
		}
		seen[p.pid] = true
		out = append(out, procRef{pid: p.pid, start: p.start})
	}
	for _, p := range procs {
		if p.pgid == root || (p.sid > 0 && p.sid == root) {
			add(p)
		}
	}
	if !alive {
		return out
	}
	rootCg := cgroupOf(root)
	queue := []int{root}
	visited := map[int]bool{root: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if visited[c.pid] {
				continue
			}
			visited[c.pid] = true
			queue = append(queue, c.pid)
			if rootCg != "" {
				if cg := cgroupOf(c.pid); cg != "" && cg != rootCg {
					continue // moved to a scope or unit of its own (its children follow it)
				}
			}
			add(c)
		}
	}
	return out
}

// liveRefs returns the refs whose process still runs with the same start
// time (not a zombie, not a reused PID).
func liveRefs(refs []procRef) []procRef {
	if len(refs) == 0 {
		return nil
	}
	procs, err := listProcs()
	if err != nil {
		return nil
	}
	by := make(map[int]procInfo, len(procs))
	for _, p := range procs {
		by[p.pid] = p
	}
	var out []procRef
	for _, r := range refs {
		if p, ok := by[r.pid]; ok && !p.zombie && p.start == r.start {
			out = append(out, r)
		}
	}
	return out
}

// leaderReused reports whether pid, the process of a pane tmux reports
// dead, is a live process again: tmux reaped the pane process and the PID
// went to another one. While any process still has the old PID as its
// group or session ID the kernel cannot hand that PID out, so when the PID
// is free (or a zombie, the pane itself) the group's members are the
// pane's own.
func leaderReused(pid int) bool {
	procs, err := listProcs()
	if err != nil {
		return true // unknown: do not signal
	}
	for _, p := range procs {
		if p.pid == pid {
			return !p.zombie
		}
	}
	return false
}

// scopeLeftovers lists the leftovers of session name's own per-run
// scopes (see above). Nothing without cgroups (macOS) or session IDs.
func scopeLeftovers(name string) []procRef {
	procs, err := listProcs()
	if err != nil {
		return nil
	}
	return scopeOrphans(procs, name, procCgroup)
}

func scopeOrphans(procs []procInfo, name string, cgroupOf func(int) string) []procRef {
	if name == "" {
		return nil
	}
	self := os.Getpid()
	led := map[int]bool{} // live processes (a zombie leads nothing)
	for _, p := range procs {
		if !p.zombie {
			led[p.pid] = true
		}
	}
	var out []procRef
	for _, p := range procs {
		// While any process holds a session ID the kernel cannot reuse
		// that PID, so a missing leader is a session whose leader exited.
		if p.pid <= 1 || p.pid == self || p.zombie || p.sid <= 0 || led[p.sid] {
			continue
		}
		if isRunScope(cgroupOf(p.pid), name) {
			out = append(out, procRef{pid: p.pid, start: p.start})
		}
	}
	return out
}

// isRunScope reports whether cgroup path cg is a per-run scope of session
// name: pyre-run-<name>-<base36 suffix>.scope (supervisor.RunScopeUnit).
func isRunScope(cg, name string) bool {
	base := cg[strings.LastIndexByte(cg, '/')+1:]
	rest, ok := strings.CutPrefix(base, "pyre-run-"+name+"-")
	if !ok {
		return false
	}
	suffix, ok := strings.CutSuffix(rest, ".scope")
	if !ok || suffix == "" {
		return false
	}
	for _, c := range suffix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

func signalRefs(refs []procRef, force bool) {
	for _, r := range liveRefs(refs) {
		_ = signalProc(r.pid, force)
	}
}

// parsePSTable parses "pid ppid pgid stat lstart..." rows (lstart has
// spaces: "Thu Nov  5 10:09:02 2026").
func parsePSTable(out string) []procInfo {
	var procs []procInfo
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		procs = append(procs, procInfo{pid: pid, ppid: ppid, pgid: pgid, sid: -1, start: strings.Join(f[4:], " "), zombie: strings.HasPrefix(f[3], "Z")})
	}
	return procs
}
