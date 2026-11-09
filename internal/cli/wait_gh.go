package cli

// GitHub waits (`pyre wait gh-pr`, `pyre wait gh-run`): a GitHub-aware
// kind of monitor. A wait is a monitor session (shell, tagged monitor and
// c.TagWaitGitHub) running `pyre wait run`, so pyred relaunches it after a
// restart or reboot like any monitor (daemon/monitorrelaunch.go) and it is
// listed by `pyre monitor ls`. The loop (internal/ghwait) polls through the
// node's shared poller and sends ONE message: when the condition is met,
// at the timeout, or when an error persists. A wait outlives the session
// that created it (the hub's archive cascade skips it) and delivers to that
// session's parent once the session is archived or ended.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/ghguard"
	"github.com/jhh3/pyre/internal/ghwait"
)

func init() { mcpToolProviders = append(mcpToolProviders, waitGitHubMCPTools) }

// waitDefaultTimeout is --timeout when not given.
const waitDefaultTimeout = 2 * time.Hour

// WaitGitHubParams creates a GitHub wait.
type WaitGitHubParams struct {
	Kind        string `json:"kind"`              // gh-pr or gh-run
	Target      string `json:"target"`            // PR: URL or owner/repo#N; run: URL or "owner/repo RUN-ID"
	Until       string `json:"until"`             // the condition
	Timeout     string `json:"timeout,omitempty"` // e.g. "2h" (default 2h)
	Session     string `json:"session,omitempty"` // session to message (default: the caller)
	Node        string `json:"node,omitempty"`    // machine that polls (default: the caller's)
	After       string `json:"after,omitempty"`   // checks-done/checks-fail: failures before this are being rerun ("now", RFC 3339, "15:04", or "10m" ago)
	OperationID string `json:"operation_id,omitempty"`
}

// WaitGitHubResult is the wait's monitor session.
type WaitGitHubResult struct {
	WaitID  string `json:"wait_id"`
	Target  string `json:"target"`
	URL     string `json:"url"`
	Until   string `json:"until"`
	Timeout string `json:"timeout"`
	Notify  string `json:"notify"` // session that gets the message
	Node    string `json:"node,omitempty"`
	After   string `json:"after,omitempty"` // RFC 3339, when --after was given
	Reused  bool   `json:"reused,omitempty"`
}

// parseAfter resolves --after: "now", RFC 3339, "2006-01-02 15:04" or
// "15:04" (local time today), or a duration meaning that long ago.
func parseAfter(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return time.Time{}, nil
	case strings.EqualFold(s, "now"):
		return now, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("15:04", s, time.Local); err == nil {
		y, m, d := now.In(time.Local).Date()
		return time.Date(y, m, d, t.Hour(), t.Minute(), 0, 0, time.Local), nil
	}
	return time.Time{}, usagef("--after must be now, an RFC 3339 time, \"15:04\" or a duration ago such as 10m (got %q)", s)
}

// waitAfter validates and resolves p.After for cond.
func waitAfter(p WaitGitHubParams, cond ghwait.Cond, now time.Time) (time.Time, error) {
	if strings.TrimSpace(p.After) == "" {
		return time.Time{}, nil
	}
	if cond.Name != "checks-done" && cond.Name != "checks-fail" {
		return time.Time{}, usagef("--after applies to --until checks-done or checks-fail")
	}
	return parseAfter(p.After, now)
}

// parseWait validates a request into its target, condition and timeout.
func parseWait(p WaitGitHubParams) (ghwait.Target, ghwait.Cond, time.Duration, error) {
	var t ghwait.Target
	var err error
	switch ghwait.Kind(p.Kind) {
	case ghwait.KindPR:
		t, err = ghwait.ParsePR(p.Target)
	case ghwait.KindRun:
		t, err = ghwait.ParseRun(strings.Fields(p.Target))
	default:
		return t, ghwait.Cond{}, 0, usagef("kind must be gh-pr or gh-run (got %q)", p.Kind)
	}
	if err != nil {
		return t, ghwait.Cond{}, 0, usagef("%v", err)
	}
	if strings.TrimSpace(p.Until) == "" {
		return t, ghwait.Cond{}, 0, usagef("--until is required")
	}
	cond, err := ghwait.ParseCond(t.Kind, p.Until)
	if err != nil {
		return t, ghwait.Cond{}, 0, usagef("%v", err)
	}
	timeout := waitDefaultTimeout
	if s := strings.TrimSpace(p.Timeout); s != "" {
		if timeout, err = time.ParseDuration(s); err != nil || timeout < time.Minute {
			return t, ghwait.Cond{}, 0, usagef("--timeout must be a duration of at least 1m, e.g. 2h (got %q)", s)
		}
	}
	return t, cond, timeout, nil
}

func waitKey(t ghwait.Target, cond ghwait.Cond, timeout time.Duration, nodeID, to string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("wait\x00%s\x00%s\x00%s\x00%s\x00%s", t.Key(), strings.ToLower(cond.String()), timeout, nodeID, to)))
	return fmt.Sprintf("%x", h[:8])
}

// WaitGitHub starts a wait (a monitor session), or returns the live one
// that already waits for the same target and condition for the same
// session on the same machine.
func (o *Ops) WaitGitHub(ctx context.Context, p WaitGitHubParams) (WaitGitHubResult, error) {
	t, cond, timeout, err := parseWait(p)
	if err != nil {
		return WaitGitHubResult{}, err
	}
	after, err := waitAfter(p, cond, time.Now())
	if err != nil {
		return WaitGitHubResult{}, err
	}
	if p.Session == "" && o.Self.SessionID == "" {
		return WaitGitHubResult{}, usagef("outside a Pyre session there is no session to message: pass --session, or --block to wait here")
	}
	target, err := o.ResolveSession(ctx, p.Session)
	if err != nil {
		return WaitGitHubResult{}, err
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
		return WaitGitHubResult{}, err
	}
	to := target.Session.SessionID
	res := WaitGitHubResult{Target: t.String(), URL: t.URL(), Until: cond.String(), Timeout: timeout.String(), Notify: to, Node: node.Name}
	keyTo := to
	if !after.IsZero() {
		res.After = after.UTC().Format(time.RFC3339)
		keyTo += "\x00after " + res.After
	}
	key := waitKey(t, cond, timeout, node.NodeID, keyTo)
	list, err := o.MonitorList(ctx)
	if err != nil {
		return WaitGitHubResult{}, err
	}
	for _, s := range list {
		if s.Session.MonitorKey() == key && s.Session.MonitorTarget() == to && s.Run != nil && c.MonitorLive(s.Run.State) {
			res.WaitID, res.Reused = s.Session.SessionID, true
			return res, nil
		}
	}
	args := waitRunArgs(t, cond, to, timeout, after)
	title := fmt.Sprintf("wait %s %s until %s", t.Kind, t, cond)
	no := false
	start, err := o.API.StartSession(ctx, c.StartSessionRequest{
		Mutation: mut(p.OperationID), NodeID: node.NodeID, Harness: c.HarnessShell, Title: c.MonitorTitlePrefix + title, Cwd: "~",
		Args:            monitorShellArgs(nodeShell(node), args),
		Tags:            []string{TagMonitor, c.TagWaitGitHub, monitorToTag(to), c.TagMonitorKeyPrefix + key, c.TagReportNone},
		ParentSessionID: o.Self.SessionID, ReportToParent: &no,
	})
	if err != nil {
		return WaitGitHubResult{}, err
	}
	res.WaitID = start.Session.Session.SessionID
	return res, nil
}

// waitRunArgs is the wait loop's command line (`pyre wait run ...`): the
// target and condition are base64url, so they need no shell quoting (a
// condition such as review-from:x[bot] holds glob characters).
func waitRunArgs(t ghwait.Target, cond ghwait.Cond, to string, timeout time.Duration, after time.Time) []string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	targetArg := t.String()
	if t.Kind == ghwait.KindRun {
		targetArg = fmt.Sprintf("%s/%d", t.FullRepo(), t.RunID)
	}
	args := []string{"wait", "run", "--kind", string(t.Kind), "--target-b64", b64(targetArg), "--until-b64", b64(cond.String()),
		"--to", to, "--timeout", timeout.String()}
	if !after.IsZero() {
		args = append(args, "--after-unix", fmt.Sprint(after.Unix()))
	}
	return args
}

// WaitList lists GitHub waits (live and ended, not archived).
func (o *Ops) WaitList(ctx context.Context) ([]c.SessionView, error) {
	list, err := o.MonitorList(ctx)
	if err != nil {
		return nil, err
	}
	var out []c.SessionView
	for _, s := range list {
		if s.Session.IsGitHubWait() {
			out = append(out, s)
		}
	}
	return out, nil
}

// WaitCancel stops a wait and archives it.
func (o *Ops) WaitCancel(ctx context.Context, ref, opID string) (c.SessionView, error) {
	sv, err := o.ResolveSession(ctx, ref)
	if err != nil {
		return c.SessionView{}, err
	}
	if !sv.Session.IsGitHubWait() {
		return c.SessionView{}, usagef("%s is not a GitHub wait (see `pyre wait ls`)", sv.Session.SessionID)
	}
	return o.MonitorRemove(ctx, sv.Session.SessionID, opID)
}

// waitRecipient is who gets the message: to, else (archived, gone or
// ended) the nearest live session above it. Rerouted says why.
func waitRecipient(ctx context.Context, api c.API, to string) (id, rerouted string, err error) {
	cur, why := to, ""
	for range 8 {
		sv, err := api.GetSession(ctx, c.GetRequest{ID: cur})
		gone := ""
		switch {
		case c.IsCode(err, c.ErrNotFound):
			gone = "is gone"
		case err != nil:
			return "", "", err
		case sv.Session.Archived:
			gone = "was archived"
		case sv.Run != nil && (sv.Run.State == c.RunExited || sv.Run.State == c.RunFailed):
			gone = "has ended"
		}
		if gone == "" {
			return cur, why, nil
		}
		if why == "" {
			why = fmt.Sprintf("%s %s", to, gone)
		}
		parent := ""
		if err == nil {
			parent = sv.Session.Parent()
		}
		if parent == "" {
			if gone == "has ended" { // nobody above: its mail waits for it
				return cur, "", nil
			}
			return "", why, fmt.Errorf("%s and has no parent to tell", why)
		}
		cur = parent
	}
	return "", why, fmt.Errorf("no live session above %s", to)
}

// ghTokenFunc returns the node's gh token: $GH_TOKEN or $GITHUB_TOKEN,
// else `gh auth token` from the real gh (not the shim), read once.
func ghTokenFunc(getenv func(string) string) func(context.Context) (string, error) {
	var once sync.Once
	var tok string
	var terr error
	return func(ctx context.Context) (string, error) {
		once.Do(func() {
			for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
				if v := strings.TrimSpace(getenv(k)); v != "" {
					tok = v
					return
				}
			}
			self, _ := os.Executable()
			gh, err := ghguard.FindReal(getenv("PATH"), getenv(ghguard.EnvShimDir), self)
			if err != nil {
				terr = err
				return
			}
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			out, err := exec.CommandContext(cctx, gh, "auth", "token", "--hostname", "github.com").Output()
			if err != nil {
				terr = fmt.Errorf("gh auth token: %v (log in with `gh auth login` on this machine)", err)
				return
			}
			tok = strings.TrimSpace(string(out))
		})
		return tok, terr
	}
}

// newWaitPoller is this machine's shared poller.
func (a *App) newWaitPoller() *ghwait.Poller {
	dir := filepath.Join(DefaultStateDir(a.Getenv), "ghwait")
	cl := &ghwait.Client{Base: a.Getenv("PYRE_GITHUB_API"), Token: ghTokenFunc(a.Getenv), HTTP: &http.Client{Timeout: 30 * time.Second},
		CacheDir: ghwait.CacheDir(dir),
		Guard:    func(ctx context.Context) error { return ghguard.Take(ctx, "wait gh") }}
	return &ghwait.Poller{Dir: dir, Client: cl, Now: time.Now}
}

func waitStatePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "ghwait", "waits", filepath.Base(sessionID)+".json")
}

func (a *App) addWaitCommands(root *cobra.Command) {
	w := &cobra.Command{
		Use:   "wait",
		Short: "Wait on a GitHub PR or Actions run; get one message when it is done",
		Long: `pyre wait gh-pr / gh-run watches a pull request or an Actions run and sends
this session ONE Pyre message, already summarized (URL, final state, failing
checks with links and log commands, a review's author and excerpt), when the
condition is met, when --timeout passes, or when an error persists. The call
returns at once with a wait ID; --block waits here instead (scripts).

  pyre wait gh-pr https://github.com/o/r/pull/12 --until checks-done
  pyre wait gh-pr o/r#12 --until review-from:gemini-code-assist
  pyre wait gh-run https://github.com/o/r/actions/runs/123 --until failed

A wait is a monitor (pyre monitor ls shows it): it survives pyred restarts
and reboots, and it outlives the session that created it: once that session
is archived or ended, the message goes to its parent. Waits on the same PR
or run on one machine share one poller, which uses conditional requests
(unchanged answers do not count against GitHub's limit) every 30-60 s while
checks run and every 2-5 min otherwise.`,
	}
	add := func(kind ghwait.Kind, use, short string, conds []string, nargs cobra.PositionalArgs) {
		var p WaitGitHubParams
		var block bool
		cmd := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  nargs,
			RunE: a.run(func(cmd *cobra.Command, args []string) error {
				p.Kind, p.Target, p.OperationID = string(kind), strings.Join(args, " "), a.opID
				if block {
					return a.waitBlock(cmd.Context(), p)
				}
				ops, err := a.connect()
				if err != nil {
					return err
				}
				res, err := ops.WaitGitHub(cmd.Context(), p)
				if err != nil {
					return err
				}
				return a.emit(res, func(w io.Writer) {
					verb := "waiting"
					if res.Reused {
						verb = "already waiting"
					}
					fmt.Fprintf(w, "%s: wait %s on %s until %s (timeout %s); one message to %s when done\n%s\ncancel: pyre wait cancel %s\n",
						verb, res.WaitID, res.Target, res.Until, res.Timeout, res.Notify, res.URL, res.WaitID)
				})
			}),
		}
		f := cmd.Flags()
		f.StringVar(&p.Until, "until", "", "condition: "+strings.Join(conds, " | "))
		f.StringVar(&p.Timeout, "timeout", "", "give up after this long (default 2h)")
		f.StringVar(&p.Session, "to", "", "session to message (default: this session)")
		f.StringVar(&p.Node, "node", "", "machine that polls (default: this one)")
		if kind == ghwait.KindPR {
			f.StringVar(&p.After, "after", "", "checks-done/checks-fail: failures that finished before this are being rerun, so wait for their new results (now | RFC 3339 | 15:04 | 10m ago)")
		}
		f.BoolVar(&block, "block", false, "wait in the foreground and print the message (exit 0 met, 5 timeout, 1 error)")
		w.AddCommand(cmd)
	}
	add(ghwait.KindPR, "gh-pr <url | owner/repo#N> --until COND", "Wait on a pull request", ghwait.PRConds, cobra.ExactArgs(1))
	add(ghwait.KindRun, "gh-run <url | owner/repo RUN-ID> --until done|failed", "Wait on a GitHub Actions run", ghwait.RunConds, cobra.RangeArgs(1, 2))

	w.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List GitHub waits",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			list, err := ops.WaitList(cmd.Context())
			if err != nil {
				return err
			}
			if list == nil {
				list = []c.SessionView{}
			}
			return a.emit(list, func(w io.Writer) {
				var rows [][]string
				for _, s := range list {
					state := "-"
					if s.Run != nil {
						state = string(s.Run.State)
					}
					rows = append(rows, []string{s.Session.SessionID, strings.TrimPrefix(s.Session.Title, c.MonitorTitlePrefix+"wait "), orDash(s.NodeName), state, orDash(monitorTarget(s))})
				}
				table(w, "WAIT\tWHAT\tNODE\tSTATE\tNOTIFIES", rows)
			})
		}),
	})
	w.AddCommand(&cobra.Command{
		Use:   "cancel <wait>",
		Short: "Stop a GitHub wait and archive it",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			sv, err := ops.WaitCancel(cmd.Context(), args[0], a.opID)
			if err != nil {
				return err
			}
			return a.emit(sv, func(w io.Writer) { fmt.Fprintf(w, "wait %s stops and is archived\n", sv.Session.SessionID) })
		}),
	})

	var kind, targetB64, untilB64, to string
	var timeout time.Duration
	var afterUnix int64
	runCmd := &cobra.Command{
		Use:    "run --kind K --target-b64 T --until-b64 U --to S --timeout D",
		Short:  "The wait loop (started by pyre wait gh-pr/gh-run inside the wait's monitor session)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			tb, _ := base64.RawURLEncoding.DecodeString(targetB64)
			ub, _ := base64.RawURLEncoding.DecodeString(untilB64)
			t, cond, _, err := parseWait(WaitGitHubParams{Kind: kind, Target: string(tb), Until: string(ub), Timeout: timeout.String()})
			if err != nil {
				return err
			}
			wt := &ghwait.Waiter{Target: t, Cond: cond, Timeout: timeout, Poller: a.newWaitPoller(), Now: time.Now, Sleep: sleepCtx, Log: a.Stdout}
			if afterUnix > 0 {
				wt.After = time.Unix(afterUnix, 0)
			}
			wt.Deliver = func(ctx context.Context, text string) error {
				id, why, err := waitRecipient(ctx, ops.API, to)
				if err != nil {
					return err
				}
				if why != "" {
					text += "\n(Sent to you because " + why + ".)"
				}
				_, err = ops.Send(ctx, SendParams{To: id, Text: text})
				return err
			}
			if self := ops.Self.SessionID; self != "" {
				path := waitStatePath(DefaultStateDir(a.Getenv), self)
				wt.Load = func() (ghwait.WaitState, bool) { return loadWaitState(path) }
				wt.Save = func(st ghwait.WaitState) {
					if err := saveJSONAtomic(path, st); err != nil {
						fmt.Fprintf(a.Stdout, "warning: wait state not saved (a relaunch after a restart starts over): %v\n", err)
					}
				}
			}
			fmt.Fprintf(a.Stdout, "wait %s: one message to %s (timeout %s)\n%s\n", wt.Label(), to, timeout, t.URL())
			_, err = wt.Run(cmd.Context())
			return err
		}),
	}
	rf := runCmd.Flags()
	rf.StringVar(&kind, "kind", "", "gh-pr or gh-run")
	rf.StringVar(&targetB64, "target-b64", "", "target, base64url")
	rf.StringVar(&untilB64, "until-b64", "", "condition, base64url")
	rf.StringVar(&to, "to", "", "session to message")
	rf.DurationVar(&timeout, "timeout", waitDefaultTimeout, "give up after this long")
	rf.Int64Var(&afterUnix, "after-unix", 0, "--after, Unix seconds")
	w.AddCommand(runCmd)
	root.AddCommand(w)
}

// waitBlock runs a wait in the foreground and prints its message.
func (a *App) waitBlock(ctx context.Context, p WaitGitHubParams) error {
	t, cond, timeout, err := parseWait(p)
	if err != nil {
		return err
	}
	after, err := waitAfter(p, cond, time.Now())
	if err != nil {
		return err
	}
	wt := &ghwait.Waiter{Target: t, Cond: cond, Timeout: timeout, Poller: a.newWaitPoller(), Now: time.Now, Sleep: sleepCtx, Log: a.Stderr, After: after,
		Deliver: func(context.Context, string) error { return nil }}
	out, err := wt.Run(ctx)
	if err != nil {
		return err
	}
	if err := a.emit(out, func(w io.Writer) { fmt.Fprintln(w, out.Text) }); err != nil {
		return err
	}
	switch out.Kind {
	case "timeout":
		return withStatus(ExitUnavailable, fmt.Errorf("wait timed out after %s", timeout))
	case "error":
		return withStatus(ExitError, fmt.Errorf("wait stopped on an error"))
	}
	return nil
}

func loadWaitState(path string) (ghwait.WaitState, bool) {
	var st ghwait.WaitState
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &st) != nil || st.Start.IsZero() {
		return ghwait.WaitState{}, false
	}
	return st, true
}

func waitGitHubMCPTools() []MCPTool {
	return []MCPTool{
		compactly(tool("wait_github", "Wait on GitHub",
			"Wait on a PR or Actions run without polling: returns at once; you get ONE summarized message (state, failing checks with links, review excerpt) when met, on timeout or a lasting error. Survives restarts.",
			obj(props{"kind": str("gh-pr or gh-run"),
				"target":  str("PR: https://github.com/o/r/pull/N or o/r#N; run: https://github.com/o/r/actions/runs/ID or \"o/r ID\""),
				"until":   str("gh-pr: checks-done | checks-fail | merged | closed | review-from:<login> | comment[:<login>] | head-changed; gh-run: done | failed"),
				"timeout": str("give up after this long, e.g. \"2h\" (default 2h)"),
				"after":   str("checks-done/checks-fail after a rerun: failures that finished before this are being rerun, so wait for their new results (\"now\", RFC 3339, \"15:04\", or \"10m\" ago). Reruns are detected anyway; this is a stricter barrier"),
				"session": str("session to message (default: you)"), "node": str("machine that polls (default: yours)"), "operation_id": opIDProp},
				"kind", "target", "until"),
			hints("Wait on GitHub", idempotent, true),
			func(ctx context.Context, o *Ops, p WaitGitHubParams) (WaitGitHubResult, error) {
				return o.WaitGitHub(ctx, p)
			}),
			func(rc *renderCtx, r WaitGitHubResult) string {
				after := ""
				if r.After != "" {
					after = ", after " + r.After
				}
				return fmt.Sprintf("wait %s on %s until %s (timeout %s%s)%s; one message to %s. Cancel: cancel_wait %s\n", r.WaitID, r.Target, r.Until, r.Timeout, after,
					map[bool]string{true: " (already waiting)", false: ""}[r.Reused], r.Notify, r.WaitID)
			}),
		compactly(tool("cancel_wait", "Cancel a GitHub wait", "Stop a wait_github wait and archive it.",
			obj(props{"wait_id": str("the wait ID wait_github returned"), "operation_id": opIDProp}, "wait_id"),
			hints("Cancel a GitHub wait", idempotent, true),
			func(ctx context.Context, o *Ops, p struct {
				WaitID      string `json:"wait_id"`
				OperationID string `json:"operation_id,omitempty"`
			}) (c.SessionView, error) {
				return o.WaitCancel(ctx, p.WaitID, p.OperationID)
			}),
			func(rc *renderCtx, r c.SessionView) string {
				return "wait " + r.Session.SessionID + " stops and is archived\n"
			}),
	}
}

// saveJSONAtomic writes v as JSON through a temp file and a rename,
// private to the user.
func saveJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}
