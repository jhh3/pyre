package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Automatic stops spare live work (decision I62).
//
// Codex kills the process group of every exec session a thread holds,
// exited ones included, when the thread's background terminals are cleaned
// or the thread is unloaded (I61: an archive-when-done stop killed a render
// left in an exited exec's group). Pyre does both when it stops a run and
// when it sweeps a thread no run owns. A stop the owner or an agent asked
// for still does (and says how many groups it killed). A stop Pyre makes by
// itself does not kill live work:
//
//   - Pyre records each exec's process group (= its top process, Codex's
//     osPid in thread/backgroundTerminals/list, which lists only commands
//     still running): shortly after an exec starts, after its yield, at
//     every turn end, every deferRecheck while commands run, and right
//     before a stop or sweep decides. The record (<dir>/execgroups/<thread>)
//     keeps a group after its command exited for as long as the group has
//     members, since that is what Codex's kill reaches.
//   - An automatic stop (StopSparing: archive when done, the archive
//     cascade) or the idle-thread sweep that finds such a group with live
//     members (/proc on Linux, ps on macOS) ends the run as usual but does
//     not clean or unload the thread. The thread stays loaded and
//     subscribed (Codex unloads a thread 30 minutes after its last
//     subscriber left), no run owns it (no wake, delivery or turn), its TUI
//     is closed, and the deferral (<dir>/deferred/<thread>, plus an
//     app-server ref) survives a pyred restart.
//   - Every deferRecheck Pyre checks the groups again and unloads the
//     thread once they are empty, or after deferCap (then killing what is
//     left). The deferral and its end are audited (node.stop_deferred,
//     node.stop_resumed) and logged.
//
// A new run that resumes the thread ends the deferral: its launch reloads
// the thread (I51), which kills the groups as before.

var (
	// deferRecheck is how often deferred threads are checked (and running
	// commands' groups refreshed).
	deferRecheck = time.Minute
	// deferCap bounds how long a thread stays loaded for its groups.
	deferCap = 24 * time.Hour
	// scanDelays: an exec's group is read this long after it starts (its
	// process is listed once Codex stores it; 12 s is past the default
	// 10 s yield).
	scanDelays = []time.Duration{time.Second, 12 * time.Second}
)

// groupsPerThread bounds a thread's recorded groups (the oldest go first).
const groupsPerThread = 128

// groupsSupported: Pyre reads process groups here (Linux /proc, macOS ps).
// Elsewhere (Windows, termhost) nothing is recorded and stops are unchanged.
const groupsSupported = runtime.GOOS == "linux" || runtime.GOOS == "darwin"

// execGroup is the process group of one exec session.
type execGroup struct {
	PGID      int       `json:"pgid"`
	ProcessID string    `json:"process_id,omitempty"` // the model's session id
	ItemID    string    `json:"item_id,omitempty"`
	Command   string    `json:"command,omitempty"`
	SeenAt    time.Time `json:"seen_at"`
}

// liveGroup is an exec group with live members.
type liveGroup struct {
	execGroup
	Members []procMember
}

// deferral is a thread kept loaded for its live groups.
type deferral struct {
	ThreadID   string    `json:"thread_id"`
	RunID      string    `json:"run_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Home       string    `json:"codex_home"`
	Executable string    `json:"executable,omitempty"`
	Since      time.Time `json:"since"`
	By         string    `json:"by"` // "stop" | "sweep"
	PGIDs      []int     `json:"pgids"`
}

// bgTerminal is one thread/backgroundTerminals/list entry.
type bgTerminal struct {
	ItemID    string `json:"itemId"`
	ProcessID string `json:"processId"`
	Command   string `json:"command"`
	OsPid     *int64 `json:"osPid"`
}

// deferLimit is deferCap (tests shorten it per adapter).
func (a *Adapter) deferLimit() time.Duration {
	if a.deferMax > 0 {
		return a.deferMax
	}
	return deferCap
}

func (a *Adapter) log() *slog.Logger {
	if a.o.Logger != nil {
		return a.o.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// listTerminals reads the thread's running exec sessions.
func listTerminals(ctx context.Context, c *rpcClient, threadID string) ([]bgTerminal, error) {
	var out []bgTerminal
	cursor := ""
	for range 20 {
		p := map[string]any{"threadId": threadID}
		if cursor != "" {
			p["cursor"] = cursor
		}
		var r struct {
			Data       []bgTerminal `json:"data"`
			NextCursor *string      `json:"nextCursor"`
		}
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.call(lctx, "thread/backgroundTerminals/list", p, &r)
		cancel()
		if err != nil {
			return out, err
		}
		out = append(out, r.Data...)
		if r.NextCursor == nil || *r.NextCursor == "" {
			break
		}
		cursor = *r.NextCursor
	}
	return out, nil
}

func (hs *homeServer) groupsDir() string   { return filepath.Join(hs.dir, "execgroups") }
func (hs *homeServer) deferredDir() string { return filepath.Join(hs.dir, "deferred") }

func deferRef(threadID string) string { return "deferred-" + threadID }

// groupsLocked returns the thread's recorded groups, loading them from disk
// on first use. Called with hs.groupsMu held.
func (hs *homeServer) groupsLocked(threadID string) []execGroup {
	if gs, ok := hs.groups[threadID]; ok {
		return gs
	}
	var gs []execGroup
	if refNameRe.MatchString(threadID) {
		if b, err := os.ReadFile(filepath.Join(hs.groupsDir(), threadID+".json")); err == nil {
			_ = json.Unmarshal(b, &gs)
		}
	}
	hs.groups[threadID] = gs
	return gs
}

// saveGroupsLocked persists the thread's groups. Called with hs.groupsMu held.
func (hs *homeServer) saveGroupsLocked(threadID string) {
	if !refNameRe.MatchString(threadID) {
		return
	}
	path := filepath.Join(hs.groupsDir(), threadID+".json")
	gs := hs.groups[threadID]
	if len(gs) == 0 {
		_ = os.Remove(path)
		return
	}
	if os.MkdirAll(hs.groupsDir(), 0o700) != nil {
		return
	}
	b, _ := json.Marshal(gs)
	_ = writeFileAtomic(path, b)
}

// forgetGroups drops the thread's record (it was unloaded: Codex holds none
// of its commands any more).
func (hs *homeServer) forgetGroups(threadID string) {
	hs.groupsMu.Lock()
	defer hs.groupsMu.Unlock()
	hs.groups[threadID] = nil
	hs.saveGroupsLocked(threadID)
}

// liveGroups refreshes the thread's exec groups from Codex (when c is set)
// and returns those that still have live members. A group whose command
// exited and that has no members is forgotten. ok is false when the process
// table cannot be read here (nothing is known; callers kill as before).
func (hs *homeServer) liveGroups(ctx context.Context, c *rpcClient, threadID string) ([]liveGroup, bool) {
	if !groupsSupported {
		return nil, false
	}
	var terms []bgTerminal
	if c != nil {
		terms, _ = listTerminals(ctx, c, threadID)
	}
	now := time.Now()
	listed := map[int]bool{}
	hs.groupsMu.Lock()
	gs := slices.Clone(hs.groupsLocked(threadID))
	changed := false
	for _, t := range terms {
		if t.OsPid == nil || *t.OsPid <= 1 {
			continue
		}
		pg := int(*t.OsPid)
		listed[pg] = true
		i := slices.IndexFunc(gs, func(g execGroup) bool { return g.PGID == pg })
		g := execGroup{PGID: pg, ProcessID: t.ProcessID, ItemID: t.ItemID, Command: clipCmd(t.Command), SeenAt: now}
		if i < 0 {
			gs = append(gs, g)
			changed = true
		} else {
			gs[i] = g
		}
	}
	if len(gs) > groupsPerThread {
		slices.SortStableFunc(gs, func(x, y execGroup) int { return x.SeenAt.Compare(y.SeenAt) })
		gs = gs[len(gs)-groupsPerThread:]
		changed = true
	}
	pgids := make([]int, 0, len(gs))
	for _, g := range gs {
		pgids = append(pgids, g.PGID)
	}
	hs.groups[threadID] = gs
	if changed {
		hs.saveGroupsLocked(threadID)
	}
	hs.groupsMu.Unlock()
	members, ok := groupMembers(pgids)
	if !ok {
		return nil, false
	}
	var live []liveGroup
	kept := make([]execGroup, 0, len(gs))
	for _, g := range gs {
		if m := members[g.PGID]; len(m) > 0 {
			live = append(live, liveGroup{execGroup: g, Members: m})
			kept = append(kept, g)
		} else if listed[g.PGID] {
			kept = append(kept, g) // its top process is starting or exiting
		}
	}
	hs.groupsMu.Lock()
	if len(kept) != len(hs.groups[threadID]) {
		// Groups recorded meanwhile are kept; empty ones go.
		var merged []execGroup
		for _, g := range hs.groups[threadID] {
			if slices.ContainsFunc(kept, func(k execGroup) bool { return k.PGID == g.PGID }) || !slices.Contains(pgids, g.PGID) {
				merged = append(merged, g)
			}
		}
		hs.groups[threadID] = merged
		hs.saveGroupsLocked(threadID)
	}
	hs.groupsMu.Unlock()
	return live, true
}

// scheduleGroupScan reads the thread's exec groups after d (once per
// thread and delay at a time).
func (a *Adapter) scheduleGroupScan(hs *homeServer, threadID string, d time.Duration) {
	if !groupsSupported {
		return
	}
	key := threadID + "/" + d.String()
	hs.groupsMu.Lock()
	if hs.scanPending[key] {
		hs.groupsMu.Unlock()
		return
	}
	hs.scanPending[key] = true
	hs.groupsMu.Unlock()
	time.AfterFunc(d, func() {
		hs.groupsMu.Lock()
		delete(hs.scanPending, key)
		hs.groupsMu.Unlock()
		c := hs.connected()
		if c == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		hs.liveGroups(ctx, c, threadID)
	})
}

// noteExecStart schedules the group reads of an exec_command that started.
func (a *Adapter) noteExecStart(hs *homeServer, threadID string, it item) {
	if it.Type != "commandExecution" || it.Source != execStartup || it.ProcessID == "" {
		return
	}
	for _, d := range scanDelays {
		a.scheduleGroupScan(hs, threadID, d)
	}
}

// ---- deferrals ----

// deferredLocked returns the home's deferrals, loading them on first use.
// Called with hs.groupsMu held.
func (hs *homeServer) deferredLocked() map[string]deferral {
	if hs.deferred != nil {
		return hs.deferred
	}
	hs.deferred = map[string]deferral{}
	ents, _ := os.ReadDir(hs.deferredDir())
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(hs.deferredDir(), e.Name()))
		if err != nil {
			continue
		}
		var d deferral
		if json.Unmarshal(b, &d) == nil && d.ThreadID != "" && e.Name() == d.ThreadID+".json" {
			hs.deferred[d.ThreadID] = d
		}
	}
	return hs.deferred
}

// deferrals lists the home's deferred threads.
func (hs *homeServer) deferrals() []deferral {
	hs.groupsMu.Lock()
	defer hs.groupsMu.Unlock()
	var out []deferral
	for _, d := range hs.deferredLocked() {
		out = append(out, d)
	}
	slices.SortFunc(out, func(x, y deferral) int { return strings.Compare(x.ThreadID, y.ThreadID) })
	return out
}

func (hs *homeServer) isDeferred(threadID string) bool {
	hs.groupsMu.Lock()
	defer hs.groupsMu.Unlock()
	_, ok := hs.deferredLocked()[threadID]
	return ok
}

// addDeferral records d and takes an app-server ref for it, so the
// app-server (which holds the groups' commands) stays up.
func (hs *homeServer) addDeferral(d deferral) error {
	if !refNameRe.MatchString(d.ThreadID) {
		return fmt.Errorf("codex: bad thread id %q", d.ThreadID)
	}
	if err := hs.addRef(deferRef(d.ThreadID)); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	err := os.MkdirAll(hs.deferredDir(), 0o700)
	if err == nil {
		err = writeFileAtomic(filepath.Join(hs.deferredDir(), d.ThreadID+".json"), b)
	}
	if err != nil {
		hs.dropRef(deferRef(d.ThreadID))
		return err
	}
	hs.groupsMu.Lock()
	hs.deferredLocked()[d.ThreadID] = d
	hs.groupsMu.Unlock()
	return nil
}

// dropDeferral forgets the thread's deferral and returns the app-server
// refs left (0: nothing else uses it).
func (hs *homeServer) dropDeferral(threadID string) int {
	hs.groupsMu.Lock()
	delete(hs.deferredLocked(), threadID)
	hs.groupsMu.Unlock()
	if refNameRe.MatchString(threadID) {
		_ = os.Remove(filepath.Join(hs.deferredDir(), threadID+".json"))
	}
	return hs.dropRef(deferRef(threadID))
}

// describeGroups is a short line naming the groups, their commands and
// members.
func describeGroups(live []liveGroup) string {
	var b strings.Builder
	for i, g := range live {
		if i == 4 {
			fmt.Fprintf(&b, "; and %d more", len(live)-i)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "pgid %d", g.PGID)
		if g.Command != "" {
			fmt.Fprintf(&b, " (exec `%s`)", clipCmd(g.Command))
		}
		for j, m := range g.Members {
			if j == 3 {
				fmt.Fprintf(&b, ", +%d", len(g.Members)-j)
				break
			}
			sep := ", "
			if j == 0 {
				sep = ": "
			}
			fmt.Fprintf(&b, "%s%d %s", sep, m.PID, clipCmd(firstN(m.Cmd, 60)))
		}
	}
	return b.String()
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func groupIDs(live []liveGroup) []int {
	out := make([]int, 0, len(live))
	for _, g := range live {
		out = append(out, g.PGID)
	}
	return out
}

// killedLine says what a clean or unload did to the groups that had live
// members before it (Codex kills them right after it answers).
func killedLine(before []liveGroup) string {
	left := map[int][]procMember{}
	if len(before) > 0 {
		for range 20 {
			m, ok := groupMembers(groupIDs(before))
			if !ok {
				break
			}
			left = m
			if len(m) == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	var killed, alive []liveGroup
	for _, g := range before {
		if m := left[g.PGID]; len(m) > 0 {
			alive = append(alive, liveGroup{execGroup: g.execGroup, Members: m})
		} else {
			killed = append(killed, g)
		}
	}
	s := fmt.Sprintf("%d background process group(s) were killed", len(killed))
	if len(killed) > 0 {
		s += " (" + describeGroups(killed) + ")"
	}
	if len(alive) > 0 {
		s += fmt.Sprintf("; %d still have live processes (%s)", len(alive), describeGroups(alive))
	}
	return s
}

// deferThread keeps a loaded thread for its live groups. It reports false
// when the deferral could not be recorded (the caller then unloads).
func (a *Adapter) deferThread(hs *homeServer, d deferral, live []liveGroup) bool {
	d.Home, d.Executable, d.PGIDs = hs.home, hs.exe, groupIDs(live)
	if d.Since.IsZero() {
		d.Since = time.Now().UTC()
	}
	if err := hs.addDeferral(d); err != nil {
		a.log().Warn("codex: could not defer a thread unload; unloading", "thread", d.ThreadID, "err", err)
		return false
	}
	desc := describeGroups(live)
	a.log().Info("codex: automatic stop spared live background processes; thread kept loaded",
		"thread", d.ThreadID, "run", d.RunID, "session", d.SessionID, "by", d.By, "pgids", d.PGIDs, "groups", desc)
	if a.o.OnAutomation != nil {
		go a.o.OnAutomation(contracts.NodeAutomation{RunID: d.RunID, SessionID: d.SessionID, Harness: contracts.HarnessCodex,
			Action: contracts.AutomationStopDeferred, Detail: fmt.Sprintf("%d process group(s) still running: %s", len(live), desc)})
	}
	return true
}

// endDeferral reports the end of a thread's deferral.
func (a *Adapter) endDeferral(d deferral, why string) {
	a.log().Info("codex: deferred thread unload done", "thread", d.ThreadID, "run", d.RunID, "session", d.SessionID, "why", why)
	if a.o.OnAutomation != nil {
		go a.o.OnAutomation(contracts.NodeAutomation{RunID: d.RunID, SessionID: d.SessionID, Harness: contracts.HarnessCodex,
			Action: contracts.AutomationStopResumed, Detail: why})
	}
}

// subscribeLoaded attaches this connection to a loaded thread without
// overrides (which would rebuild it and kill its commands) so Codex keeps
// it loaded. A thread that is not loaded is left alone.
func subscribeLoaded(ctx context.Context, c *rpcClient, threadID string) bool {
	loaded, err := threadLoaded(ctx, c, threadID)
	if err != nil || !loaded {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.call(rctx, "thread/resume", threadResumeParams{ThreadID: threadID, ExcludeTurns: true}, nil) == nil
}

// cancelDeferral ends a deferral without unloading (a new run takes the
// thread over, or the app-server is gone).
func (a *Adapter) cancelDeferral(ctx context.Context, hs *homeServer, threadID, why string) {
	hs.groupsMu.Lock()
	d, ok := hs.deferredLocked()[threadID]
	hs.groupsMu.Unlock()
	if !ok {
		return
	}
	if hs.dropDeferral(threadID) == 0 {
		hs.stop(ctx)
	}
	a.endDeferral(d, why)
}

// deferLoop rechecks deferred threads and refreshes the groups of running
// commands, until ctx ends.
func (a *Adapter) deferLoop(ctx context.Context) {
	if deferRecheck <= 0 {
		return
	}
	t := time.NewTicker(deferRecheck)
	defer t.Stop()
	found := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !found {
			a.findDeferred() // a previous pyred's deferrals
			found = true
		}
		a.checkDeferred(ctx)
		a.refreshRunningGroups(ctx)
	}
}

// findDeferred loads the homes that hold deferrals from an earlier pyred.
func (a *Adapter) findDeferred() {
	root := filepath.Join(a.o.StateRoot, "codex")
	dirs, _ := os.ReadDir(root)
	for _, e := range dirs {
		if !e.IsDir() {
			continue
		}
		ents, _ := os.ReadDir(filepath.Join(root, e.Name(), "deferred"))
		for _, f := range ents {
			b, err := os.ReadFile(filepath.Join(root, e.Name(), "deferred", f.Name()))
			var d deferral
			if err != nil || json.Unmarshal(b, &d) != nil {
				continue
			}
			if _, err := a.homeAt(e.Name(), d.Home, d.Executable); err == nil {
				break // the home loads all its deferrals
			}
		}
	}
}

// checkDeferred unloads each deferred thread whose groups are empty or
// whose deferral reached deferCap.
func (a *Adapter) checkDeferred(ctx context.Context) {
	a.mu.Lock()
	homes := make([]*homeServer, 0, len(a.homes))
	for _, hs := range a.homes {
		homes = append(homes, hs)
	}
	a.mu.Unlock()
	for _, hs := range homes {
		for _, d := range hs.deferrals() {
			a.checkDeferral(ctx, hs, d)
		}
	}
}

func (a *Adapter) checkDeferral(ctx context.Context, hs *homeServer, d deferral) {
	if ok, _ := hs.running(ctx); !ok {
		hs.forgetGroups(d.ThreadID)
		a.cancelDeferral(ctx, hs, d.ThreadID, "the app-server is gone; nothing left to unload")
		return
	}
	c, err := hs.ensure(ctx, nil) // reconnects (re-subscribing deferred threads)
	if err != nil {
		return // next time
	}
	hs.mu.Lock()
	owned := hs.threads[d.ThreadID] != nil
	hs.mu.Unlock()
	if owned {
		a.cancelDeferral(ctx, hs, d.ThreadID, "a new run took the thread over")
		return
	}
	loaded, err := threadLoaded(ctx, c, d.ThreadID)
	if err != nil {
		return
	}
	if !loaded {
		hs.forgetGroups(d.ThreadID)
		a.cancelDeferral(ctx, hs, d.ThreadID, "the thread is no longer loaded")
		return
	}
	live, ok := hs.liveGroups(ctx, c, d.ThreadID)
	age, limit := time.Since(d.Since), a.deferLimit()
	why := ""
	switch {
	case !ok:
		why = "the process table cannot be read"
	case len(live) == 0:
		why = "its background processes have exited"
	case age >= limit:
		why = fmt.Sprintf("kept for %s (the limit); %d process group(s) killed: %s", limit, len(live), describeGroups(live))
	default:
		return
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = c.call(uctx, "thread/unsubscribe", threadIDParams{ThreadID: d.ThreadID}, nil)
	cancel()
	if err := unloadThread(ctx, c, d.ThreadID); err != nil {
		a.log().Warn("codex: deferred thread unload failed; retrying later", "thread", d.ThreadID, "err", err)
		return
	}
	hs.forgetGroups(d.ThreadID)
	if hs.dropDeferral(d.ThreadID) == 0 {
		hs.stop(ctx)
	}
	a.endDeferral(d, "thread unloaded: "+why)
}

// refreshRunningGroups records the groups of the commands runs have
// running now.
func (a *Adapter) refreshRunningGroups(ctx context.Context) {
	a.mu.Lock()
	var rrs []*runRef
	for _, rr := range a.runs {
		rrs = append(rrs, rr)
	}
	a.mu.Unlock()
	for _, rr := range rrs {
		rr.ts.mu.Lock()
		running := len(rr.ts.runningCmds) > 0 && !rr.ts.closing
		rr.ts.mu.Unlock()
		if !running {
			continue
		}
		if c := rr.hs.connected(); c != nil {
			rr.hs.liveGroups(ctx, c, rr.meta.ThreadID)
		}
	}
}

// resubscribeDeferred re-attaches a new connection to the deferred
// threads, so Codex does not unload them as unsubscribed.
func (hs *homeServer) resubscribeDeferred(ctx context.Context, c *rpcClient) {
	for _, d := range hs.deferrals() {
		subscribeLoaded(ctx, c, d.ThreadID)
	}
}
