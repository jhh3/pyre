package cli

// Monitors (W6): a shell condition checked on a timer that wakes an agent
// session only when the condition's output changes. A monitor is a plain
// shell session (harness shell) running `pyre monitor run`, so it costs no
// model tokens between changes, survives the caller ending, and is removed
// like any session. Session lists leave monitors out and show them on the
// session they wake (contracts/monitors.go); `pyre monitor ls` and
// `pyre sessions --monitors` list them. The loop sends the target a Pyre
// message (delivered at its next idle) as the monitor session. A repeat of
// the same add (same command, machine, directory, target and caller)
// returns the live monitor instead of starting another.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Monitor tags: TagMonitor marks a monitor session; monitorToTag names the
// session it wakes; c.TagMonitorKeyPrefix tags what it watches (reuse).
const (
	TagMonitor        = c.TagMonitor
	monitorToPrefix   = c.TagMonitorToPrefix
	monitorMinEvery   = time.Minute
	monitorMaxOutput  = 3000 // bytes of new output quoted in the wake message
	monitorCmdTimeout = 10 * time.Minute
	monitorMaxCapture = 1 << 20 // bytes of check output kept
	// monitorWakeBudget wakes per rolling hour: a check noisier than that
	// is a bad monitor, so it stops with a final notice.
	monitorWakeBudget = 6
	// monitorDefaultFor is the lifetime when --for is not given.
	monitorDefaultFor = 7 * 24 * time.Hour
	// monitorMaxBetween is how many intermediate outputs a held change
	// keeps for its report (the first, then the latest).
	monitorMaxBetween = 8
)

func monitorToTag(id string) string { return monitorToPrefix + id }

// monitorKey identifies a monitor request: what it watches (command,
// machine, directory) and how (interval, lifetime, until-change, initial),
// so a reuse never returns a monitor that behaves differently from the
// one asked for. p.For is already defaulted.
func monitorKey(p MonitorAddParams, nodeID, cwd string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%t\x00%t", p.Cmd, nodeID, cwd, p.Every, p.For, p.UntilChange, p.Initial)))
	return fmt.Sprintf("%x", h[:8])
}

// MonitorAddResult is the monitor's session; Reused says an identical live
// monitor already existed and was returned instead of starting another.
type MonitorAddResult struct {
	c.StartSessionResponse
	Reused bool `json:"reused,omitempty"`
}

// MonitorAddParams creates a monitor.
type MonitorAddParams struct {
	Session     string        `json:"session,omitempty"` // session to wake (default: the caller)
	Every       time.Duration `json:"every"`
	Cmd         string        `json:"cmd"`
	Node        string        `json:"node,omitempty"` // where the command runs (default: the caller's node, else the target's)
	Cwd         string        `json:"cwd,omitempty"`
	Title       string        `json:"title,omitempty"`
	For         time.Duration `json:"for,omitempty"`          // stop after this long (default 7 days)
	UntilChange bool          `json:"until_change,omitempty"` // stop after the first change is reported
	Initial     bool          `json:"initial,omitempty"`      // also report the first result
	OperationID string        `json:"operation_id,omitempty"`
}

// MonitorAdd starts the monitor's shell session, or returns the caller's
// live monitor that watches the same command on the same machine and
// directory for the same target.
func (o *Ops) MonitorAdd(ctx context.Context, p MonitorAddParams) (MonitorAddResult, error) {
	if strings.TrimSpace(p.Cmd) == "" {
		return MonitorAddResult{}, usagef("--cmd is required (a shell command whose output is watched)")
	}
	if p.Every < monitorMinEvery {
		return MonitorAddResult{}, usagef("--every must be at least %s (got %s)", monitorMinEvery, p.Every)
	}
	target, err := o.ResolveSession(ctx, p.Session)
	if err != nil {
		return MonitorAddResult{}, err
	}
	nodeRef := p.Node
	if nodeRef == "" {
		nodeRef = o.Self.NodeID
	}
	if nodeRef == "" && target.Run != nil {
		nodeRef = target.Run.NodeID
	}
	node, err := o.ResolveNode(ctx, nodeRef)
	if err != nil {
		return MonitorAddResult{}, err
	}
	cwd := p.Cwd
	if cwd == "" && node.NodeID == o.Self.NodeID {
		cwd, _ = os.Getwd()
	}
	if cwd == "" {
		cwd = "~"
	}
	if p.For <= 0 {
		p.For = monitorDefaultFor
	}
	key := monitorKey(p, node.NodeID, cwd)
	if mon, ok, err := o.liveMonitor(ctx, key, target.Session.SessionID); err != nil {
		return MonitorAddResult{}, err
	} else if ok {
		return MonitorAddResult{StartSessionResponse: c.StartSessionResponse{Session: mon}, Reused: true}, nil
	}
	title := p.Title
	if title == "" {
		title = oneLineClip(p.Cmd, 40)
	}
	args := []string{"monitor", "run", "--to", target.Session.SessionID, "--every", p.Every.String(),
		"--cmd-b64", base64.RawURLEncoding.EncodeToString([]byte(p.Cmd)),
		"--label-b64", base64.RawURLEncoding.EncodeToString([]byte(title))}
	args = append(args, "--for", p.For.String())
	if p.UntilChange {
		args = append(args, "--until-change")
	}
	if p.Initial {
		args = append(args, "--initial")
	}
	no := false
	res, err := o.API.StartSession(ctx, c.StartSessionRequest{
		Mutation: mut(p.OperationID), NodeID: node.NodeID, Harness: c.HarnessShell, Title: c.MonitorTitlePrefix + title, Cwd: cwd,
		Args: monitorShellArgs(nodeShell(node), args), Tags: []string{TagMonitor, monitorToTag(target.Session.SessionID), c.TagMonitorKeyPrefix + key, c.TagReportNone},
		ParentSessionID: o.Self.SessionID, ReportToParent: &no,
	})
	return MonitorAddResult{StartSessionResponse: res}, err
}

// liveMonitor finds the caller's unarchived monitor whose run is running
// or waiting (contracts.MonitorLive: never unknown) that matches key for
// target.
func (o *Ops) liveMonitor(ctx context.Context, key, target string) (c.SessionView, bool, error) {
	list, err := o.MonitorList(ctx)
	if err != nil {
		return c.SessionView{}, false, err
	}
	for _, s := range list {
		if s.Session.MonitorKey() == key && s.Session.MonitorTarget() == target && s.Session.Parent() == o.Self.SessionID &&
			s.Run != nil && c.MonitorLive(s.Run.State) {
			return s, true, nil
		}
	}
	return c.SessionView{}, false, nil
}

// nodeShell is the executable the node's shell adapter starts.
func nodeShell(n c.Node) string {
	h, _ := nodeHarness(n, string(c.HarnessShell))
	if h.Executable != "" {
		return h.Executable
	}
	if n.Platform == "windows" {
		return "powershell.exe"
	}
	return "/bin/sh"
}

// monitorShellArgs are the arguments for the node's shell that run the
// pyre CLI: $PYRE_CLI, else pyre on PATH (the arguments are base64,
// duration and ID tokens, so they need no quoting).
func monitorShellArgs(shell string, args []string) []string {
	a := strings.Join(args, " ")
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(strings.ReplaceAll(shell, `\`, "/")), ".exe"))
	switch base {
	case "pwsh", "powershell":
		return []string{"-NoLogo", "-NoProfile", "-Command", "$p = if ($env:PYRE_CLI) { $env:PYRE_CLI } else { 'pyre' }; & $p " + a + "; exit $LASTEXITCODE"}
	case "cmd":
		return []string{"/D", "/C", `if defined PYRE_CLI ("%PYRE_CLI%" ` + a + `) else (pyre ` + a + `)`}
	case "fish":
		return []string{"-lc", "if set -q PYRE_CLI; exec $PYRE_CLI " + a + "; else; exec pyre " + a + "; end"}
	}
	return []string{"-lc", `exec "${PYRE_CLI:-pyre}" ` + a}
}

// MonitorList lists monitors (not archived).
func (o *Ops) MonitorList(ctx context.Context) ([]c.SessionView, error) {
	page, err := o.ListSessions(ctx, SessionFilter{Harness: string(c.HarnessShell), IncludeMonitors: true})
	if err != nil {
		return nil, err
	}
	var out []c.SessionView
	for _, s := range page.Items {
		if s.Session.IsMonitor() {
			out = append(out, s)
		}
	}
	return out, nil
}

func monitorTarget(s c.SessionView) string { return s.Session.MonitorTarget() }

// MonitorRemove stops a monitor and archives it.
func (o *Ops) MonitorRemove(ctx context.Context, ref, opID string) (c.SessionView, error) {
	sv, err := o.ResolveSession(ctx, ref)
	if err != nil {
		return c.SessionView{}, err
	}
	if !sv.Session.IsMonitor() {
		return c.SessionView{}, usagef("%s is not a monitor (see `pyre monitor ls`)", sv.Session.SessionID)
	}
	yes := true
	return o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(opID), SessionID: sv.Session.SessionID, Archived: &yes, WhenDone: true})
}

// monitorTargetState is the wake target as the loop sees it each check.
type monitorTargetState struct {
	Gone bool   // archived or deleted: the monitor ends
	Busy bool   // mid-turn: a change is held and sent once it is idle
	Why  string // for the log
}

// monitorLoop is `pyre monitor run`: check, compare, wake on change.
type monitorLoop struct {
	To, Label, Cmd string
	Every, For     time.Duration
	UntilChange    bool
	Initial        bool
	Out            io.Writer
	Now            func() time.Time
	Sleep          func(context.Context, time.Duration) error
	Check          func(ctx context.Context, cmd string) string
	Send           func(ctx context.Context, text string) error
	Target         func(ctx context.Context) monitorTargetState
	Self           string // the monitor's own session, for the stop hint
	// Load and Save (optional) keep the loop's state across a relaunch
	// (monitorState): a monitor that a reboot or crash killed resumes with
	// its deadline, its last output and its wake budget.
	Load func() (monitorState, bool)
	Save func(monitorState)
}

// monitorState is what a relaunched monitor needs to continue rather than
// start over: when it started (the --for deadline), the baseline, the
// output the target last saw (so the first check after a relaunch is not a
// spurious "changed"), whether it changed since the baseline, the recent
// wakes (the hourly budget), and Done once it ended deliberately
// (--until-change fired, --for ran out, the wake budget, the target gone),
// so a manual resume of an ended monitor ends again at once.
type monitorState struct {
	Start        time.Time   `json:"start"`
	Baseline     string      `json:"baseline,omitempty"`
	HaveBaseline bool        `json:"have_baseline,omitempty"`
	Seen         string      `json:"seen,omitempty"`
	HaveSeen     bool        `json:"have_seen,omitempty"`
	Changed      bool        `json:"changed,omitempty"`
	Wakes        []time.Time `json:"wakes,omitempty"`
	Done         string      `json:"done,omitempty"`
}

func (m *monitorLoop) run(ctx context.Context) error {
	start := m.Now()
	var last, baseline string
	// changed: some check differed from the baseline, so a delivered report
	// is a real change (an --initial report of the baseline is not).
	haveLast, haveBase, held, changed := false, false, false, false
	var h monitorHeld
	var wakes []time.Time
	logf := func(format string, a ...any) {
		fmt.Fprintf(m.Out, m.Now().Format("15:04:05")+" "+format+"\n", a...)
	}
	save := func(done string) {
		if m.Save == nil {
			return
		}
		st := monitorState{Start: start, Done: done}
		if done == "" {
			st.Baseline, st.HaveBaseline, st.Changed, st.Wakes = baseline, haveBase, changed, wakes
			st.Seen, st.HaveSeen = last, haveLast
			if held { // the target has not seen the latest output yet
				st.Seen, st.HaveSeen = h.Prev, h.HavePrev
			}
		}
		m.Save(st)
	}
	finish := func(format string, a ...any) error {
		why := fmt.Sprintf(format, a...)
		logf("done: %s", why)
		save(why)
		return nil
	}
	if m.Load != nil {
		if st, ok := m.Load(); ok {
			if st.Done != "" {
				logf("done: this monitor already ended (%s); not restarted", st.Done)
				return nil
			}
			start, baseline, haveBase, changed, wakes = st.Start, st.Baseline, st.HaveBaseline, st.Changed, st.Wakes
			last, haveLast = st.Seen, st.HaveSeen
			logf("relaunched: watching since %s; last output %s", start.Local().Format("Jan 2 15:04"), outHash(last))
			if m.For > 0 && m.Now().Sub(start) >= m.For {
				return finish("ran for %s", m.For)
			}
		}
	}
	save("")
	for {
		tgt := m.Target(ctx)
		if tgt.Gone {
			return finish("%s is %s", m.To, tgt.Why)
		}
		out := m.Check(ctx, m.Cmd)
		if !haveBase {
			baseline, haveBase = out, true
		} else if out != baseline {
			changed = true
		}
		switch {
		case !haveLast && !m.Initial:
			logf("baseline (%s): %s", outHash(out), oneLineClip(out, 120))
		case haveLast && out == last && !held:
			logf("unchanged")
		case held && h.HavePrev && out == h.Prev && out == last && len(h.Between) == 0:
			// Defensive: nothing seen since the last report differs from it.
			logf("unchanged since the last report; nothing to send")
			held = false
		default:
			switch {
			case !held:
				h = monitorHeld{Prev: last, HavePrev: haveLast}
			case out != last:
				h.add(last) // an output the target has not seen yet
			}
			held = true // coalesced: the latest output is quoted, the ones before it listed
			now := m.Now()
			recent := wakes[:0]
			for _, t := range wakes {
				if now.Sub(t) < time.Hour {
					recent = append(recent, t)
				}
			}
			wakes = recent
			switch {
			case tgt.Busy:
				logf("changed; %s is busy (%s), held until it is idle", m.To, tgt.Why)
			case len(wakes) >= monitorWakeBudget:
				text := m.message(out, h, false) + fmt.Sprintf("\nThis monitor has stopped: its output changed more than %d times in an hour, too often to be worth a wake each. Use a check whose output changes only when something you act on happens.", monitorWakeBudget)
				if err := m.Send(ctx, text); err != nil {
					logf("changed; wake budget spent; final notice failed: %v", err)
				} else {
					logf("changed; wake budget spent (%d in an hour); stopped with a final notice", monitorWakeBudget)
				}
				save(fmt.Sprintf("wake budget spent (%d in an hour)", monitorWakeBudget))
				return nil
			default:
				stop := m.UntilChange && changed
				if err := m.Send(ctx, m.message(out, h, stop)); err != nil {
					logf("changed; wake failed (retrying next check): %v", err)
				} else {
					held = false
					wakes = append(wakes, now)
					logf("changed; woke %s: %s", m.To, oneLineClip(out, 120))
					if stop {
						save("until-change: reported the change")
						return nil
					}
				}
			}
		}
		last, haveLast = out, true
		if m.For > 0 && m.Now().Sub(start) >= m.For {
			return finish("ran for %s", m.For)
		}
		save("")
		if err := m.Sleep(ctx, m.Every); err != nil {
			return err
		}
	}
}

// monitorHeld is a change not yet reported: the output last reported
// (Prev) and the distinct outputs seen after it but before the latest
// (Between), so a report names a value that came and went while the target
// was busy instead of only the latest one.
type monitorHeld struct {
	Prev     string
	HavePrev bool
	Between  []string // one-line clipped, oldest first
	Dropped  int      // intermediates left out of Between
}

// add records an intermediate output, keeping the first (what triggered
// the change) and the latest ones.
func (h *monitorHeld) add(out string) {
	h.Between = append(h.Between, oneLineClip(out, 120))
	if len(h.Between) > monitorMaxBetween {
		h.Between = slices.Delete(h.Between, 1, 2)
		h.Dropped++
	}
}

// between lists the intermediates, marking where some were left out.
func (h monitorHeld) between() []string {
	var parts []string
	for i, v := range h.Between {
		if i == 1 && h.Dropped > 0 {
			parts = append(parts, fmt.Sprintf("… (%d more)", h.Dropped))
		}
		parts = append(parts, v)
	}
	return parts
}

// sequence renders prev → intermediates → out on one line.
func (h monitorHeld) sequence(out string) string {
	var parts []string
	if h.HavePrev {
		parts = append(parts, oneLineClip(h.Prev, 120))
	}
	parts = append(append(parts, h.between()...), oneLineClip(out, 120))
	return strings.Join(parts, " → ")
}

// message renders a report; stopping says this is --until-change's last one.
func (m *monitorLoop) message(out string, h monitorHeld, stopping bool) string {
	var b strings.Builder
	back := h.HavePrev && out == h.Prev // it changed, then changed back
	what := "output changed"
	switch {
	case !h.HavePrev:
		what = "first result"
	case back:
		what = "output changed and changed back"
	}
	fmt.Fprintf(&b, "[monitor %q] %s (every %s: `%s`):\n", m.Label, what, m.Every, oneLineClip(m.Cmd, 200))
	if back {
		b.WriteString("changed to " + strings.Join(h.between(), " → ") + " and back to " + oneLineClip(out, 120) + " before this report.\n")
	}
	b.WriteString("```\n" + clipBytes(out, monitorMaxOutput) + "\n```\n")
	if h.HavePrev && !back {
		b.WriteString("previous: " + oneLineClip(h.Prev, 200) + "\n")
	}
	if len(h.Between) > 0 {
		b.WriteString("sequence: " + h.sequence(out) + "\n")
	}
	switch {
	case stopping:
		b.WriteString("This monitor has stopped (until-change).")
	case m.UntilChange:
		b.WriteString("This monitor keeps watching and stops after the first change from this.")
	case m.Self != "":
		b.WriteString("Stop it with `pyre monitor rm " + m.Self + "`.")
	}
	return b.String()
}

// monitorTargetOf reads the target session: gone when archived or deleted,
// busy while a turn runs. A failed read is neither (checked again next time).
func monitorTargetOf(ctx context.Context, api c.API, id string) monitorTargetState {
	sv, err := api.GetSession(ctx, c.GetRequest{ID: id})
	switch {
	case c.IsCode(err, c.ErrNotFound):
		return monitorTargetState{Gone: true, Why: "gone"}
	case err != nil:
		return monitorTargetState{}
	case sv.Session.Archived:
		return monitorTargetState{Gone: true, Why: "archived"}
	case sv.Session.Harness == c.HarnessShell: // no turns: never busy
	case sv.Run != nil && (sv.Run.State == c.RunRunning || sv.Run.State == c.RunStarting):
		return monitorTargetState{Busy: true, Why: string(sv.Run.State)}
	}
	return monitorTargetState{}
}

// runMonitorCheck runs cmd in the platform shell and returns its combined
// output (trailing whitespace trimmed), plus the exit status when nonzero.
func runMonitorCheck(ctx context.Context, cmd string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var x *exec.Cmd
	if runtime.GOOS == "windows" {
		x = exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-Command", cmd)
		if p, err := exec.LookPath("pwsh.exe"); err == nil {
			x = exec.CommandContext(ctx, p, "-NoLogo", "-NoProfile", "-Command", cmd)
		}
	} else {
		x = exec.CommandContext(ctx, "sh", "-c", cmd)
	}
	x.WaitDelay = 2 * time.Second // a timed-out command's children may hold the pipe
	var buf capBuffer
	x.Stdout, x.Stderr = &buf, &buf
	prepareCheck(x) // the whole process tree dies on timeout
	err := x.Run()
	b := buf.b
	if buf.dropped > 0 {
		b = append(b, fmt.Sprintf("\n[%d bytes of output dropped]", buf.dropped)...)
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		out += fmt.Sprintf("\n[timed out after %s]", timeout)
	case err != nil:
		if ee, ok := err.(*exec.ExitError); ok {
			out += fmt.Sprintf("\n[exit %d]", ee.ExitCode())
		} else {
			out += "\n[" + err.Error() + "]"
		}
	}
	return strings.TrimLeft(out, "\n")
}

func outHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}

func oneLineClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		cut := n
		for cut > 0 && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return "[… earlier output clipped]\n" + s[cut:] // the tail holds the latest lines
}

// capBuffer keeps the first monitorMaxCapture bytes of a check's output.
type capBuffer struct {
	mu      sync.Mutex
	b       []byte
	dropped int
}

func (w *capBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p) // always "written": dropping the excess is not an error
	room := monitorMaxCapture - len(w.b)
	if room < len(p) {
		w.dropped += len(p) - max(room, 0)
		p = p[:max(room, 0)]
	}
	w.b = append(w.b, p...)
	return n, nil
}
