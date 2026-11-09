package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/apikeylogin"
	c "github.com/jhh3/pyre/internal/contracts"
)

// pyre run (W-pyrerun): a headless one-shot wrapper. `pyre run -- claude -p
// ...` (or `-- codex exec ...`) runs the command as this process's direct
// child with stdin, stdout and stderr passed through and exits with the
// child's code, so a caller cannot tell it from calling the harness
// directly; the only difference is the account: the local node binds its
// best eligible account (the profile's CLAUDE_CONFIG_DIR or CODEX_HOME, a
// token account's variable) and records a session and run in the hub
// (tag "pyre run", headless), ended with the exit code and the native
// conversation id (Claude's session id, Codex's thread id; the CLI records
// it with its own hub credential when it has one, else the node with its
// own). Without a reachable hub or node, or with no eligible account, the
// command runs exactly as given on this machine's own login after one
// warning line on stderr, unless that login bills an API key: then it does
// not run at all (exit 75; subscriptions only, internal/apikeylogin), and a
// fallback child never inherits the harness's API-key variables.
//
// Local-socket protocol: daemon.PathLocalExec* (kept as literals so the CLI
// does not link the daemon; internal/daemon/execrun.go).
const (
	localExecPath        = "/v1/local/exec"
	localExecStartPath   = "/v1/local/exec/start"
	localExecClaimPath   = "/v1/local/exec/claim"
	localExecStartedPath = "/v1/local/exec/started"
	localExecExitedPath  = "/v1/local/exec/exited"
	localExecNativePath  = "/v1/local/exec/native"
	execArg              = "--pyre-exec"
	execResumeArg        = "--pyre-exec-resume"
)

// EnvRunSetupTimeout overrides how long pyre run waits for the hub and
// node before running the command without Pyre (a Go duration).
const EnvRunSetupTimeout = "PYRE_RUN_SETUP_TIMEOUT"

type runOpts struct {
	project, title, account, harness string
	setup                            time.Duration
	fallback                         string // own | fail
	keep                             bool   // keep the session listed after the run
	codexStall                       time.Duration
	codexStallSet                    bool // --codex-stall given
}

// EnvRunFallback sets pyre run's default --fallback.
const EnvRunFallback = "PYRE_RUN_FALLBACK"

// ExitPyreUnavailable is pyre run's exit code with --fallback=fail when
// Pyre could not run the command (EX_TEMPFAIL).
const ExitPyreUnavailable = 75

// EnvRunCodexStall sets pyre run's default --codex-stall (a Go duration;
// 0 turns the stop and second start off).
const EnvRunCodexStall = "PYRE_RUN_CODEX_STALL"

// ExitCodexStalled is pyre run's exit code when codex exec --json printed
// nothing within --codex-stall twice (timeout(1)'s code).
const ExitCodexStalled = 124

var (
	// codexStallDefault is --codex-stall's default: a working codex exec
	// --json prints thread.started within seconds, and runs that printed
	// nothing for this long never started (they hung 20-45 minutes).
	codexStallDefault = 5 * time.Minute
	// codexStallGrace is how long a stalled codex has to end after SIGTERM
	// before SIGKILL.
	codexStallGrace = 5 * time.Second
)

// runDownTTL: after Pyre was unreachable (no pyred, no hub: not a slow
// claim), later calls that can fall back to a working own login skip the
// setup for this long and run the command on it at once.
const runDownTTL = 60 * time.Second

// runSetupDefault is how long pyre run waits for the hub and node by
// default. A loaded node can take well over 10 s to bind an account (a
// rota checkout plus a Codex rate-limit probe took 37 s on server), and a
// fallback is worse than the wait.
const runSetupDefault = 60 * time.Second

// silentExit ends the process with code and prints nothing (pyre run
// mirrors its child's exit status).
type silentExit struct{ code int }

func (e *silentExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (a *App) addRunCommand(root *cobra.Command) {
	var o runOpts
	cmd := &cobra.Command{
		Use:   "run [--project P] [--title T] [--account A] [--harness claude|codex] -- claude|codex ARGS...",
		Short: "Run a headless claude -p or codex exec command on the best eligible account, recorded as a session",
		Long: `Supports claude (claude -p ...) and codex (codex exec ...).

Run a headless command (claude -p ..., codex exec ...) as this process's
child, with stdin, stdout and stderr passed through byte for byte and the
child's exit code (or signal) as pyre's own: a caller cannot tell it from
calling claude or codex directly.

A codex exec --json whose output is a pipe or file and that prints nothing
(not even thread.started, which a working codex prints within seconds) for
--codex-stall (default 5m, or $PYRE_RUN_CODEX_STALL; 0: never) is stuck before
its thread began, so it did no work: pyre run stops it and starts it once more
with the same stdin, saying so on stderr. When that one stalls too, pyre run
stops it and exits 124. The window starts once a piped stdin is closed.

The node on this machine picks the best eligible account the way placement
does (weekly headroom first; limited or needs-reauth accounts are skipped) and
the command runs with that account's profile (CLAUDE_CONFIG_DIR for claude,
CODEX_HOME for codex). With claude --resume ID, or codex exec resume ID, the
account that reaches that conversation: any when the profiles share the
transcript store (codex then prefers the account that ran the thread), else
the profile that holds it, else the machine's own login. The
run is recorded in the hub as a session (harness claude or codex, tag "pyre
run", --project when given) with Claude's session id or Codex's thread id
(seen in --output-format json / codex --json output, else looked up in the
transcript store), so pyre sessions, search and read show it; it ends exited
with the command's exit code. It never reports to a parent session and never
raises owner attention.

Codex: the command is codex exec (any of its forms: a prompt, resume, fork,
review), with exec as the first argument (codex options go after it); for an
interactive codex session use pyre start --harness codex.

Inside a Pyre session the command does not run as that session: the session's
identity (PYRE_SESSION_ID, PYRE_RUN_ID, PYRE_SESSION_TOKEN, PYRE_PARENT_*, ...)
is removed from the command's environment, so its hooks and a pyre MCP server
it starts never act as, or change the state of, the calling session.

No hub login is needed on the machine: without one (no client config, no
PYRE_SESSION_TOKEN) this machine's pyred records the run with its own node
credential, and the session is archived when the run ends (--keep needs a
login). pyred's socket is $PYRE_SOCKET or the default state path.

When the hub or this machine's pyred is unreachable, or no account is
eligible, the command runs as given on this machine's own login after one
warning line on stderr, without the harness's API-key variables
(ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, OPENAI_API_KEY, CODEX_API_KEY). An
own login that bills an API key (Codex auth.json in API-key mode, Claude's
apiKeyHelper or a Console key) is never used: pyre run exits 75 instead.

Codex stdin: codex exec reads stdin to its end before it starts. When the
prompt is an argument (not "-"), a terminal stdin is replaced by /dev/null,
and a pipe that stays open with no input for 30s gives codex an empty stdin
(later input is dropped, said on stderr); input the caller pipes and closes
still reaches codex. With the prompt on stdin ("-" or none), stdin is passed
whole.

  pyre run --title "triage" -- claude -p "summarize" --output-format json --model haiku
  pyre run -- codex exec --json --sandbox read-only -m gpt-6-luna -c model_reasoning_effort=xhigh -o last.txt - < prompt.txt
  pyre run -- codex exec --json -o last.txt resume 0199a213-81c0-7800-8aa1-bbab2a035a53 - < followup.txt`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("give the command after --: pyre run -- claude -p ... (or -- codex exec ...)")
			}
			if cmd.ArgsLenAtDash() != 0 {
				return usagef("put the command after --: pyre run [flags] -- %s ...", args[0])
			}
			return nil
		},
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			o.codexStallSet = cmd.Flags().Changed("codex-stall")
			return a.runHeadless(cmd.Context(), o, args)
		}),
	}
	f := cmd.Flags()
	f.StringVar(&o.project, "project", "", "record the session in this project (ID or name)")
	f.StringVar(&o.title, "title", "", "session title (default: pyre run: <folder>)")
	f.StringVar(&o.account, "account", "", "run on this account (ID, label or rota name) instead of the best eligible one")
	f.StringVar(&o.harness, "harness", "", "claude or codex (default: from the command's name)")
	f.DurationVar(&o.setup, "setup-timeout", 0, "how long to wait for the hub and node before running without Pyre (default 60s, or $"+EnvRunSetupTimeout+")")
	f.StringVar(&o.fallback, "fallback", "", "own (default): when Pyre is unavailable or declines (no eligible account), run on this machine's own login (never an API-key login: exit 75 then); fail: exit 75 instead (or $"+EnvRunFallback+")")
	f.BoolVar(&o.keep, "keep", false, "keep the session listed after the run (default: archived when the run ends; still searchable and resumable)")
	f.DurationVar(&o.codexStall, "codex-stall", codexStallDefault, "codex exec --json that prints nothing for this long is stopped and started once more, then pyre run exits 124 (0: never; or $"+EnvRunCodexStall+")")
	root.AddCommand(cmd)
}

func (a *App) runHeadless(ctx context.Context, o runOpts, argv []string) error {
	h := o.harness
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	if h == "" {
		h = base
	}
	harness := c.Harness(h)
	switch harness {
	case c.HarnessClaude:
	case c.HarnessCodex:
		if _, err := codexExecResume(argv[1:]); err != nil {
			return err
		}
	default:
		return usagef("pyre run runs claude or codex exec (got %q; name the harness with --harness claude|codex when the command is a wrapper)", argv[0])
	}
	exe, err := exec.LookPath(argv[0])
	if err != nil {
		return usagef("%s: %v", argv[0], err)
	}
	if o.setup <= 0 {
		o.setup = runSetupDefault
		if d, err := time.ParseDuration(a.Getenv(EnvRunSetupTimeout)); err == nil && d > 0 {
			o.setup = d
		}
	}
	if o.fallback == "" {
		o.fallback = a.Getenv(EnvRunFallback)
	}
	switch o.fallback {
	case "", "own":
		o.fallback = "own"
	case "fail":
	default:
		return usagef("--fallback must be own or fail (got %q)", o.fallback)
	}
	if !o.codexStallSet {
		o.codexStall = codexStallDefault
		if v := a.Getenv(EnvRunCodexStall); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return usagef("$%s must be a duration such as 5m, or 0 (got %q)", EnvRunCodexStall, v)
			}
			o.codexStall = d
		}
	}
	if o.codexStall < 0 {
		return usagef("--codex-stall must not be negative (got %s)", o.codexStall)
	}
	// Why the own login cannot take a fallback ("" when it can): it bills
	// an API key, or there is none at all (a provider 401 otherwise).
	home := a.Getenv(homeVarFor(harness))
	noOwn := apikeylogin.Check(string(harness), home)
	noOwnKey := noOwn != ""
	if !noOwnKey {
		noOwn = apikeylogin.NoLogin(string(harness), home, a.Getenv)
	}
	var run *headlessRun
	var reason string
	refused := false
	down := runDownMarker(a.Getenv)
	// The marker only lets a call that would fall back anyway skip the
	// wait; one that cannot fall back always tries Pyre in full.
	if fi, err := os.Stat(down); err == nil && time.Since(fi.ModTime()) < runDownTTL && o.fallback == "own" && noOwn == "" {
		reason = "Pyre was unavailable moments ago (rechecked after " + runDownTTL.String() + ")"
	} else {
		var reached bool
		run, reason, refused, reached = a.prepareHeadless(ctx, o, harness, argv)
		if run == nil && !refused && !reached && ctx.Err() == nil {
			_ = os.WriteFile(down, nil, 0o600) // remembered briefly: calls in a burst do not each wait
		} else if run != nil || reached {
			_ = os.Remove(down)
		}
	}
	if run == nil && ctx.Err() != nil {
		return &silentExit{code: 130} // interrupted before the command started
	}
	if run == nil {
		what := "Pyre is unavailable: " + reason
		if refused {
			what = "Pyre declined the run: " + reason
		}
		if o.fallback == "fail" {
			fmt.Fprintf(a.Stderr, "pyre run: %s; not running it (--fallback fail)\n", what)
			return &silentExit{code: ExitPyreUnavailable}
		}
		// Subscriptions only: an own login that bills an API key is never
		// run on, and the caller's API-key variables never reach the child.
		if noOwnKey {
			fmt.Fprintf(a.Stderr, "pyre run: %s; not running it on this machine's own login: %s. "+
				"Pyre runs Claude and Codex on subscription accounts only (exit %d)\n", what, noOwn, ExitPyreUnavailable)
			return &silentExit{code: ExitPyreUnavailable}
		}
		// No own login at all: the provider would answer 401.
		if noOwn != "" {
			fmt.Fprintf(a.Stderr, "pyre run: %s; no subscription account bound and %s; retry (exit %d)\n", what, noOwn, ExitPyreUnavailable)
			return &silentExit{code: ExitPyreUnavailable}
		}
		fmt.Fprintf(a.Stderr, "pyre run: %s; running the command without Pyre on this machine's own login (%s)\n", what, ownLogin(harness, a.Getenv))
		return a.execChild(ctx, harness, exe, argv[1:], withoutAPIKeys(harness, withoutSessionIdentity(os.Environ())), nil, o.codexStall)
	}
	return a.execChild(ctx, harness, exe, argv[1:], childEnv(withoutSessionIdentity(os.Environ()), run.binding), run, o.codexStall)
}

// homeVarFor is the variable naming harness h's config home.
func homeVarFor(h c.Harness) string {
	if h == c.HarnessCodex {
		return "CODEX_HOME"
	}
	return "CLAUDE_CONFIG_DIR"
}

// fallbackAPIKeyEnv are the variables that put harness h on API billing
// (an API key, a bearer token for a gateway, a cloud provider switch). A
// fallback child never inherits them; the other harness's keys stay (a
// claude's tools may use OPENAI_API_KEY), as with a bound run.
var fallbackAPIKeyEnv = map[c.Harness][]string{
	c.HarnessClaude: append([]string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}, c.ClaudeProviderSwitches...),
	c.HarnessCodex:  {"OPENAI_API_KEY", "CODEX_API_KEY"},
}

// withoutAPIKeys is env without fallbackAPIKeyEnv[h].
func withoutAPIKeys(h c.Harness, env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(fallbackAPIKeyEnv[h], func(v string) bool { return strings.EqualFold(v, k) }) {
			out = append(out, kv)
		}
	}
	return out
}

// ownLogin names the login a fallback runs on.
func ownLogin(h c.Harness, getenv func(string) string) string {
	v, def := homeVarFor(h), "~/.claude"
	if h == c.HarnessCodex {
		def = "~/.codex"
	}
	if d := getenv(v); d != "" {
		return v + "=" + d
	}
	return def
}

// sessionIdentityEnv is the calling Pyre session's identity (pyred's
// runEnv, the Claude hook state dir). A pyre run child never inherits it:
// it is not that session, and its hooks or a pyre MCP server it starts
// would otherwise act as the caller (send as it, mark its turns, write its
// hook state). Machine-level variables (PYRE_SOCKET, PYRE_NODE_ID,
// PYRE_NODE_NAME, PYRE_HUB, PYRE_CLI, a PYRE_TOKEN login) stay.
var sessionIdentityEnv = []string{"PYRE_SESSION_ID", "PYRE_RUN_ID", "PYRE_SESSION_TOKEN", "PYRE_HARNESS", "PYRE_RUN_DIR",
	"PYRE_PROJECT_ID", "PYRE_PARENT_SESSION_ID", "PYRE_REPORT_TO_PARENT", "PYRE_PROJECT_LEAD", "PYRE_PERMISSIONS", "PYRE_ENV_GENERATION", "PYRE_CLAUDE_STATE_DIR"}

// withoutSessionIdentity is env without sessionIdentityEnv.
func withoutSessionIdentity(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(sessionIdentityEnv, k) {
			out = append(out, kv)
		}
	}
	return out
}

// runDownMarker is the per-user file remembering that Pyre was unavailable.
func runDownMarker(getenv func(string) string) string {
	dir := getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, fmt.Sprintf("pyre-run-down-%d", os.Getuid()))
}

// headlessRun is a recorded run whose binding this process claimed.
type headlessRun struct {
	sock    string
	secret  string
	binding execBinding
}

// execBinding mirrors daemon.ExecBinding.
type execBinding struct {
	RunID      string            `json:"run_id"`
	SessionID  string            `json:"session_id"`
	AccountID  string            `json:"account_id,omitempty"`
	ProfileDir string            `json:"profile_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Unset      []string          `json:"unset,omitempty"`
}

// prepareHeadless records the run and claims its binding, or says why it
// could not: refused is true when Pyre answered no (no eligible account,
// the node's capacity, a bad --account or --project), false when it could
// not be reached at all. reached is true once the run was recorded: Pyre
// is up, only the binding failed (a slow claim is not an outage).
//
// With a hub credential (a client login, or PYRE_SESSION_TOKEN inside a
// Pyre session) the CLI records the run in the hub itself. Without one,
// this machine's pyred records it with the node's own credential
// (localExecStartPath): the hub lets a node start exactly this headless
// shape on itself, so a machine needs no owner login for pyre run.
func (a *App) prepareHeadless(ctx context.Context, o runOpts, h c.Harness, argv []string) (_ *headlessRun, _ string, refused, reached bool) {
	sctx, cancel := context.WithTimeout(ctx, o.setup)
	defer cancel()
	node := localExecSocket(sctx, a.Getenv)
	if node.sock == "" {
		return nil, "no Pyre node daemon (pyred) with headless runs on this machine", false, false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "working directory: " + oneLine(err), false, false
	}
	secretB := make([]byte, 32)
	if _, err := rand.Read(secretB); err != nil {
		return nil, oneLine(err), false, false
	}
	secret := hex.EncodeToString(secretB)
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])
	resume := ""
	switch h {
	case c.HarnessCodex:
		resume, _ = codexExecResume(argv[1:])
	default:
		if id := resumeID(argv[1:]); id != "" && !slices.Contains(argv[1:], "--fork-session") {
			resume = id
		}
	}
	title := o.title
	if title == "" {
		title = "pyre run: " + filepath.Base(cwd)
	}
	var runID, reason string
	a.quietLocal = true
	ops, err := a.connect()
	noLogin := err != nil || a.conn != nil && a.conn.TokenSource == "none"
	switch {
	case noLogin && node.start:
		// No hub login on this machine: the node records the run.
		if o.keep {
			fmt.Fprintln(a.Stderr, "pyre run: --keep needs a hub login on this machine; the session is archived when the run ends")
		}
		var res struct {
			RunID string `json:"run_id"`
		}
		err := localExecPost(sctx, node.sock, localExecStartPath, map[string]any{"claim_sha256": hash, "resume": resume,
			"harness": string(h), "title": title, "cwd": cwd, "project": o.project, "account": o.account}, &res)
		if err != nil {
			return nil, "this machine's node did not record the run: " + oneLine(err), refusal(err), false
		}
		runID = res.RunID
	case err != nil:
		return nil, "no hub: " + oneLine(err) + "; " + noNodeStart, false, false
	default:
		// A login, or the local socket (pyred may hold an owner credential).
		if runID, reason, refused = a.startHeadlessInHub(sctx, ops, node.id, o, h, hash, resume, title, cwd); runID == "" {
			if noLogin && !refused {
				reason += "; no hub login on this machine, and " + noNodeStart
			}
			return nil, reason, refused, false
		}
	}
	b, err := claimBinding(ctx, sctx, node.sock, runID, secret, o.setup)
	if err != nil {
		return nil, "no account binding from this machine's node: " + oneLine(err), refusal(err), true
	}
	return &headlessRun{sock: node.sock, secret: secret, binding: b}, "", false, true
}

// execClaimAgain mirrors daemon.ExecClaimAgain: the claim's answer is not
// final (the run is still on its way to the node, or still launching).
const execClaimAgain = "claim_again"

// claimBinding claims the recorded run's binding within sctx. The node
// answers inside the wait the claim gives it, ahead of this process's
// deadline, so a slow start (the hub link, account binding) comes back as
// a real answer. A transient one (that answer, or a transport failure) is
// claimed again once with a fresh setup budget: the run is the same, the
// node hands the binding to this process again until the child starts,
// and nothing is ever started twice.
func claimBinding(ctx, sctx context.Context, sock, runID, secret string, setup time.Duration) (execBinding, error) {
	b, err := claimOnce(sctx, sock, runID, secret)
	if err == nil || !claimTransient(err) || ctx.Err() != nil {
		return b, err
	}
	rctx, cancel := context.WithTimeout(ctx, setup)
	defer cancel()
	return claimOnce(rctx, sock, runID, secret)
}

func claimOnce(ctx context.Context, sock, runID, secret string) (execBinding, error) {
	body := map[string]any{"run_id": runID, "secret": secret, "client_pid": os.Getpid()}
	if dl, ok := ctx.Deadline(); ok {
		left := time.Until(dl)
		body["wait_ms"] = max((left - min(time.Second, left/5)).Milliseconds(), 1)
	}
	var b execBinding
	err := localExecPost(ctx, sock, localExecClaimPath, body, &b)
	return b, err
}

// claimTransient: the claim may succeed when sent again (the node said so,
// or it did not answer at all: a timeout or a dropped connection).
func claimTransient(err error) bool {
	var ce *c.Error
	if errors.As(err, &ce) {
		again, _ := ce.Details[execClaimAgain].(bool)
		return again
	}
	return true
}

// noNodeStart explains why the node could not record a run itself.
const noNodeStart = "this machine's pyred cannot record a run itself (it predates that, or is not enrolled with a hub)"

// startHeadlessInHub records the run with the CLI's own hub credential and
// returns its run id, or why not.
func (a *App) startHeadlessInHub(ctx context.Context, ops *Ops, nodeID string, o runOpts, h c.Harness, hash, resume, title, cwd string) (string, string, bool) {
	if n, err := ops.API.GetNode(ctx, c.GetRequest{ID: nodeID}); err != nil {
		return "", "the hub is unreachable: " + oneLine(err), false
	} else if n.Connection.State != c.ConnConnected {
		return "", "this machine's node is not connected to the hub", false
	}
	args := []string{execArg + "=" + hash}
	if resume != "" {
		args = append(args, execResumeArg+"="+resume)
	}
	acct, err := ops.accountRef(ctx, o.account, c.ProviderFor(h))
	if err != nil {
		return "", "account: " + oneLine(err), refusal(err)
	}
	no := false
	tags := []string{c.TagPyreRun, c.TagReportNone}
	if !o.keep {
		tags = append(tags, c.TagArchiveWhenDone) // the hub archives it when the run ends
	}
	req := c.StartSessionRequest{Mutation: mut(""), NodeID: nodeID, Harness: h, Title: title, Cwd: cwd,
		Tags: tags, AccountPolicy: accountPolicy(acct, false, false), Args: args, ReportToParent: &no}
	if req.ProjectID, err = ops.projectPtr(ctx, o.project); err != nil {
		return "", "project: " + oneLine(err), refusal(err)
	}
	res, err := ops.API.StartSession(ctx, req)
	if err != nil {
		return "", "the hub did not record the run: " + oneLine(err), refusal(err)
	}
	if res.Session.Run == nil {
		return "", "the hub recorded no run", false
	}
	return res.Session.Run.RunID, "", false
}

// refusal: Pyre answered (a contracts error other than "unavailable"),
// as opposed to a transport failure or timeout.
func refusal(err error) bool {
	var ce *c.Error
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Code != c.ErrUnavailable || strings.Contains(ce.Message, "eligible") || strings.Contains(ce.Message, "subscription")
}

// resumeID returns the conversation a claude command resumes (--resume ID,
// --resume=ID, -r ID), "" when none.
func resumeID(args []string) string {
	for i, a := range args {
		if a == "--" {
			return ""
		}
		if v, ok := strings.CutPrefix(a, "--resume="); ok {
			return v
		}
		if (a == "--resume" || a == "-r") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			return args[i+1]
		}
	}
	return ""
}

// codexExecValueFlags are the options of codex exec (and of its resume and
// fork subcommands) that take the next argument as their value.
var codexExecValueFlags = map[string]bool{"-c": true, "--config": true, "--enable": true, "--disable": true, "-i": true, "--image": true,
	"-m": true, "--model": true, "--local-provider": true, "-p": true, "--profile": true, "-s": true, "--sandbox": true, "-C": true, "--cd": true,
	"--add-dir": true, "--thread-source": true, "--cyber-access-program": true, "--output-schema": true, "--color": true,
	"-o": true, "--output-last-message": true}

// threadIDRe matches a Codex thread id (a UUID).
var threadIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// codexExecResume checks that a codex command (args after the program) is
// codex exec and returns the thread it continues: the id of `exec resume
// ID` when it is a thread id ("" for a new thread, a fork, which starts a
// new thread, --last, or a thread name). After "--" everything is the
// prompt. Anything but exec is a usage error: pyre run
// is for headless commands, and only exec is one.
func codexExecResume(args []string) (string, error) {
	if len(args) == 0 || args[0] != "exec" && args[0] != "e" {
		got := "codex"
		if len(args) > 0 {
			got += " " + args[0]
		}
		return "", usagef("pyre run runs codex exec only, with exec first (got %q): pyre run -- codex exec [options] [resume ID] PROMPT; for an interactive codex use pyre start --harness codex", got)
	}
	var pos []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if len(pos) == 0 {
				return "", nil // the prompt follows: a new thread
			}
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if codexExecValueFlags[a] {
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) >= 2 && pos[0] == "resume" && threadIDRe.MatchString(pos[1]) {
		return pos[1], nil
	}
	return "", nil
}

// childEnv is the caller's environment with the binding applied: the
// account's variables set, other logins and API keys removed.
func childEnv(base []string, b execBinding) []string {
	drop := map[string]bool{}
	for _, k := range b.Unset {
		drop[k] = true
	}
	for k := range b.Env {
		drop[k] = true
	}
	out := make([]string, 0, len(base)+len(b.Env))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	for k, v := range b.Env {
		out = append(out, k+"="+v)
	}
	return out
}

// execChild runs the command with this process's stdio and mirrors its
// exit. With a recorded run it reports the child's start and end (and the
// session id seen in the output) to the node. A codex exec --json that
// prints nothing within stall (it never began its thread, so it did no
// work) is stopped and started once more; a second such stall exits
// ExitCodexStalled.
func (a *App) execChild(ctx context.Context, h c.Harness, exe string, args, env []string, run *headlessRun, stall time.Duration) error {
	var scan *sessionScanner
	watched := h == c.HarnessCodex && codexJSONOutput(args)
	stream := false
	var fileOut *os.File
	var fileOff int64 = -1
	if run != nil || watched {
		switch stdoutKind() {
		case outStream:
			// A pipe or socket: read the output on its way through for
			// Claude's session_id. WaitDelay: a background process claude
			// left holding the pipe cannot keep pyre (and the exit report)
			// waiting long after claude exited.
			if run != nil {
				scan = newIDScanner(h)
			}
			stream = true
		case outFile:
			// A regular file is passed as is (same file type for the child,
			// no pipe to hold open); the id is read from what it wrote.
			if off, err := os.Stdout.Seek(0, io.SeekCurrent); err == nil {
				fileOut, fileOff = os.Stdout, off
			}
			if fi, err := os.Stdout.Stat(); fileOut == nil || err != nil || !fi.Mode().IsRegular() {
				watched = false // a device (/dev/null) shows no output to watch
			}
		default:
			watched = false // a terminal: the caller sees codex's output itself
		}
	}
	// The child dies with pyre run (Linux parent-death signal); a headless
	// caller's child leads its own process group so the node can stop it
	// and its tools if pyre run is killed (macOS has no parent-death signal).
	group := run != nil && !isTTY(os.Stdin)
	sigs := make(chan os.Signal, 8)
	for _, s := range forwardedSignals {
		if !signal.Ignored(s) { // keep a caller's ignored SIGHUP/SIGINT (nohup) ignored
			signal.Notify(sigs, s)
		}
	}
	defer signal.Stop(sigs)
	// A closed stdout must fail the write (EPIPE; the child then sees the
	// broken pipe), not kill pyre before it reports the exit.
	ignorePipeSignal()
	defer restorePipeSignal()
	if run != nil && (scan != nil || fileOut != nil) {
		find := func() string { return scan.ID() }
		if scan == nil {
			find = func() string { return sessionIDInFile(h, fileOut, fileOff) }
		}
		stopWatch := make(chan struct{})
		defer close(stopWatch)
		go a.watchNative(run, find, stopWatch)
	}
	tty := openControllingTTY()
	defer tty.close()
	retry := watched && stall > 0
	in := passStdin()
	switch {
	case h == c.HarnessCodex:
		in = codexStdin(args, retry)
	case retry:
		in = replayableStdin(0)
	}
	for attempt := 1; ; attempt++ {
		var watch *codexWatch
		if watched {
			watch = &codexWatch{stderr: a.Stderr, file: fileOut, off: fileOff, stdinOpen: in.open}
			if retry && attempt == 1 {
				watch.then = fmt.Sprintf("pyre run stops it at %s and starts it once more", stall)
			} else if retry {
				watch.then = fmt.Sprintf("pyre run stops it at %s and gives up", stall)
			}
		}
		cmd := exec.Command(exe, args...)
		cmd.Env = env
		cmd.Stdin, cmd.Stderr, cmd.Stdout = os.Stdin, os.Stderr, os.Stdout
		var ready <-chan struct{}
		{
			var err error
			if cmd.Stdin, ready, err = in.next(); err != nil {
				fmt.Fprintf(a.Stderr, "pyre run: stdin: %v\n", err)
				if run != nil {
					a.reportExit(run, nil, "", "")
				}
				return &silentExit{code: 127}
			}
		}
		if stream {
			cmd.Stdout = &teeWriter{w: os.Stdout, scan: scan, watch: watch}
			cmd.WaitDelay = 2 * time.Second
		}
		cmd.SysProcAttr = childSysProcAttr(group)
		werr, stalled, signaled, err := a.waitChild(ctx, cmd, run, group, tty, sigs, watch, retryWindow(retry, stall, ready))
		if err != nil {
			if run != nil {
				a.reportExit(run, nil, "", "")
			}
			fmt.Fprintf(a.Stderr, "pyre run: %v\n", err)
			return &silentExit{code: 127}
		}
		if stalled && !signaled && !watch.printed() {
			if attempt == 1 {
				fmt.Fprintf(a.Stderr, "pyre run: codex printed nothing in %s (a working codex exec --json prints thread.started within seconds), so it never began its thread "+
					"and did no work; stopped it, starting it once more (--codex-stall, $%s)\n", stall, EnvRunCodexStall)
				if err := in.rewind(); err != nil {
					fmt.Fprintf(a.Stderr, "pyre run: cannot give codex its stdin again (%v); not starting it again\n", err)
				} else {
					continue
				}
			} else {
				fmt.Fprintf(a.Stderr, "pyre run: codex printed nothing in %s again, so it never began its thread; stopped it and giving up (exit %d). "+
					"It is stuck before its thread begins on this login (network, OpenAI capacity, login or a lock); try again later or on another account\n", stall, ExitCodexStalled)
			}
			if run != nil {
				code := ExitCodexStalled
				a.reportExit(run, &code, "", "")
			}
			return &silentExit{code: ExitCodexStalled}
		}
		if stalled && !signaled {
			fmt.Fprintln(a.Stderr, "pyre run: codex began printing as pyre run stopped it; not starting it again")
		}
		code, sig := exitStatus(cmd.ProcessState, werr)
		if run != nil {
			id := ""
			switch {
			case scan != nil:
				id = scan.ID()
			case fileOut != nil:
				id = sessionIDInFile(h, fileOut, fileOff)
			}
			var cp *int
			if sig == "" {
				cp = &code
			}
			a.reportExit(run, cp, sig, id)
		}
		if sig != "" {
			reraise(cmd.ProcessState) // dies by the same signal where the OS allows
		}
		return &silentExit{code: code}
	}
}

// stallWindow is when a child that printed nothing counts as stuck: after
// d, counted from its start or, for a piped stdin, from when the caller
// closed it (a codex still waiting for its input is not stuck). Zero d: never.
type stallWindow struct {
	d     time.Duration
	ready <-chan struct{} // nil: counted from the start
}

func retryWindow(retry bool, d time.Duration, ready <-chan struct{}) stallWindow {
	if !retry {
		return stallWindow{}
	}
	return stallWindow{d: d, ready: ready}
}

// waitChild starts cmd and waits for it, forwarding signals (signaled:
// the caller sent one, so a stalled child is not started again). With a
// stall window it stops a child that printed nothing in it (SIGTERM, then
// SIGKILL after codexStallGrace) and says so (stalled). err: it did not
// start.
func (a *App) waitChild(ctx context.Context, cmd *exec.Cmd, run *headlessRun, group bool, tty controllingTTY, sigs <-chan os.Signal,
	watch *codexWatch, sw stallWindow) (werr error, stalled, signaled bool, err error) {
	started, done := make(chan error, 1), make(chan error, 1)
	go func() {
		// The parent-death signal follows the thread that started the
		// child: keep that thread until the child is gone.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		done <- cmd.Wait()
	}()
	err = <-started
	if f, ok := cmd.Stdin.(*os.File); ok && f != os.Stdin {
		f.Close() // the child holds its own copy of a replayed stdin
	}
	if err != nil {
		return nil, false, false, err
	}
	if run != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		err := localExecPost(rctx, run.sock, localExecStartedPath, map[string]any{"run_id": run.binding.RunID, "secret": run.secret,
			"pid": cmd.Process.Pid, "group": group}, nil)
		cancel()
		var ce *c.Error
		if errors.As(err, &ce) && ce.Code == c.ErrConflict {
			_ = cmd.Process.Signal(stopSignal) // stopped (pyre stop) before it started
		}
	}
	var watchStop, watchDone chan struct{}
	if watch != nil {
		watchStop, watchDone = make(chan struct{}), make(chan struct{})
		go func() {
			defer close(watchDone)
			watch.run(watchStop)
		}()
	}
	var stallAt, hardKill <-chan time.Time
	ready := sw.ready
	if sw.d > 0 && ready == nil {
		stallAt = time.After(sw.d)
	}
wait:
	for {
		select {
		case s := <-sigs:
			signaled = true
			if !group && fromTerminal(s) && tty.foreground() {
				continue // the terminal sent it to the whole foreground group: the child has it
			}
			_ = cmd.Process.Signal(s)
		case <-ready:
			ready, stallAt = nil, time.After(sw.d)
		case <-stallAt:
			stallAt = nil
			if !watch.printed() {
				stalled = true
				signalStalled(cmd.Process, group, false)
				hardKill = time.After(codexStallGrace)
			}
		case <-hardKill:
			hardKill = nil
			signalStalled(cmd.Process, group, true)
		case werr = <-done:
			break wait
		}
	}
	if watch != nil {
		close(watchStop)
		<-watchDone // its last read of a file stdout is on stderr before pyre exits
	}
	return werr, stalled, signaled, nil
}

// childStdin is what each start of the child reads on stdin.
type childStdin struct {
	next   func() (io.Reader, <-chan struct{}, error) // the stdin, and when the caller's input is complete (nil: now)
	rewind func() error                               // before a start again: the same input from its start
	open   func() bool                                // the caller's input is not complete yet (nil: never)
}

// passStdin: the caller's stdin as is, for one start.
func passStdin() childStdin {
	return childStdin{
		next:   func() (io.Reader, <-chan struct{}, error) { return os.Stdin, nil, nil },
		rewind: func() error { return errors.New("stdin is not kept") },
	}
}

// codexStdinIdle is how long a piped stdin may stay open with no input
// before a codex exec whose prompt is an argument gets an empty stdin.
var codexStdinIdle = 30 * time.Second

// codexStdin is what codex exec reads on stdin. codex exec reads its stdin
// to the end before it starts whenever stdin is not a terminal ("Reading
// additional input from stdin..."): as the prompt for `-` or no prompt,
// appended to an argument prompt otherwise. A caller that leaves stdin
// open without meaning to (a background shell's inherited pipe) left
// codex waiting forever. The rule, for a codex exec whose prompt is an
// argument (not "-"):
//   - a terminal: codex gets /dev/null (it would wait on the terminal);
//   - a regular file or a device (/dev/null): passed as is;
//   - a pipe or socket: passed (relayed), but when it stays open with no
//     input for codexStdinIdle, codex gets end-of-input there and anything
//     sent later is dropped (said on stderr). Input the caller pipes
//     (`git diff | codex exec "review this"`) still reaches codex.
//
// With the prompt on stdin (`-`, or none) stdin is passed whole, as the
// caller sent it. retry: the input is kept for a second start.
func codexStdin(args []string, retry bool) childStdin {
	promptArg := codexPromptArg(args)
	if promptArg && isTTY(os.Stdin) {
		return childStdin{
			next: func() (io.Reader, <-chan struct{}, error) {
				f, err := os.Open(os.DevNull)
				return f, nil, err
			},
			rewind: func() error { return nil },
		}
	}
	fi, err := os.Stdin.Stat()
	piped := err == nil && fi.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0
	if !piped && !retry {
		return passStdin()
	}
	idle := time.Duration(0)
	if promptArg {
		idle = codexStdinIdle
	}
	return replayableStdin(idle)
}

// codexPromptArg reports whether a codex exec command (args after the
// program) gives its prompt as an argument other than "-".
func codexPromptArg(args []string) bool {
	if len(args) == 0 {
		return false
	}
	var pos []string
	last := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest := args[i+1:]
			if len(rest) == 0 {
				break
			}
			return !(len(rest) == 1 && rest[0] == "-")
		}
		if a == "--last" {
			last = true
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if codexExecValueFlags[a] {
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	need := 0 // positionals before the prompt
	if len(pos) > 0 {
		switch pos[0] {
		case "resume", "fork":
			need = 2 // the subcommand and the thread
			if last {
				need = 1
			}
		case "review":
			return false // its own input rules: stdin passed as is
		}
	}
	return len(pos) > need && pos[len(pos)-1] != "-"
}

// replayableStdin gives a child started again the caller's stdin from its
// start: a terminal or device (/dev/null) is passed as is, a regular file
// is passed as is and read again from where it began, and a pipe or
// socket (which cannot be read twice) is relayed from a copy: each start
// gets its own pipe holding everything the caller sent, then the rest as
// it arrives. The caller's stdin is read as it arrives and kept in
// memory (codex reads all of it anyway); a codex that never reads stdin
// (exec resume ID PROMPT) still never waits for it.
//
// idle > 0: a piped stdin that stays open with no input for idle counts
// as complete (empty) from then on (codexStdin).
func replayableStdin(idle time.Duration) childStdin {
	fi, err := os.Stdin.Stat()
	switch {
	case err != nil || isTTY(os.Stdin) || fi.Mode()&os.ModeCharDevice != 0:
		return childStdin{next: func() (io.Reader, <-chan struct{}, error) { return os.Stdin, nil, nil }, rewind: func() error { return nil }}
	case fi.Mode().IsRegular():
		off, err := os.Stdin.Seek(0, io.SeekCurrent)
		return childStdin{
			next: func() (io.Reader, <-chan struct{}, error) { return os.Stdin, nil, nil },
			rewind: func() error {
				if err != nil {
					return err
				}
				_, serr := os.Stdin.Seek(off, io.SeekStart)
				return serr
			},
		}
	}
	s := newStdinCopy(os.Stdin)
	s.idle = idle
	return childStdin{
		next: func() (io.Reader, <-chan struct{}, error) {
			r, err := s.pipe()
			return r, s.done, err
		},
		rewind: func() error { return nil },
		open: func() bool {
			select {
			case <-s.done:
				return false
			default:
				return true
			}
		},
	}
}

// stdinCopy keeps what the caller sent on a stdin that cannot be read
// twice: one reader takes it as it arrives (once a child is started),
// each pipe() replays it from the start.
type stdinCopy struct {
	mu       sync.Mutex
	more     *sync.Cond
	buf      []byte
	eof      bool
	cut      bool          // ended by the idle cutoff: later input is dropped
	done     chan struct{} // closed at the caller's EOF (or a read error), or at the idle cutoff
	doneOnce sync.Once
	once     sync.Once
	src      io.Reader
	idle     time.Duration // >0: no input and no EOF for this long ends the input (empty)
	warn     io.Writer     // where the cutoff is said (nil: os.Stderr)
}

func newStdinCopy(src io.Reader) *stdinCopy {
	s := &stdinCopy{src: src, done: make(chan struct{})}
	s.more = sync.NewCond(&s.mu)
	return s
}

func (s *stdinCopy) read() {
	if s.idle > 0 {
		t := time.AfterFunc(s.idle, s.cutIdle)
		defer t.Stop()
	}
	b := make([]byte, 64<<10)
	dropped := false
	for {
		n, err := s.src.Read(b)
		s.mu.Lock()
		if s.cut {
			if n > 0 && !dropped {
				dropped = true
				fmt.Fprintf(s.warnTo(), "pyre run: stdin input arrived after codex was given an empty stdin (no input for %s); it was dropped\n", s.idle)
			}
			s.mu.Unlock()
			if err != nil {
				return
			}
			continue
		}
		s.buf = append(s.buf, b[:n]...)
		s.eof = err != nil
		s.more.Broadcast()
		s.mu.Unlock()
		if err != nil {
			s.doneOnce.Do(func() { close(s.done) })
			return
		}
	}
}

func (s *stdinCopy) warnTo() io.Writer {
	if s.warn != nil {
		return s.warn
	}
	return os.Stderr
}

// cutIdle ends an input that is still open and empty after s.idle.
func (s *stdinCopy) cutIdle() {
	s.mu.Lock()
	if s.eof || len(s.buf) > 0 {
		s.mu.Unlock()
		return
	}
	s.cut, s.eof = true, true
	s.more.Broadcast()
	s.mu.Unlock()
	fmt.Fprintf(s.warnTo(), "pyre run: stdin stayed open with no input for %s; codex exec gets an empty stdin (its prompt is an argument). "+
		"Pass </dev/null when there is no input\n", s.idle)
	s.doneOnce.Do(func() { close(s.done) })
}

// pipe is a new read end yielding the caller's stdin from its start.
func (s *stdinCopy) pipe() (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	s.once.Do(func() { go s.read() })
	go func() {
		defer w.Close()
		for off := 0; ; {
			s.mu.Lock()
			for off == len(s.buf) && !s.eof {
				s.more.Wait()
			}
			chunk, eof := s.buf[off:], s.eof
			s.mu.Unlock()
			if len(chunk) == 0 && eof {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return // the child is gone
			}
			off += len(chunk)
		}
	}()
	return r, nil
}

// sessionIDInFile reads the native id from what the child wrote to the
// regular file f after offset: Claude's session_id from at most the last
// 1 MiB, Codex's thread_id (its first event) from at most the first.
func sessionIDInFile(h c.Harness, f *os.File, off int64) string {
	fi, err := f.Stat()
	if err != nil || fi.Size() <= off {
		return ""
	}
	start, n := off, fi.Size()-off
	if n > 1<<20 {
		n = 1 << 20
		if h != c.HarnessCodex {
			start = fi.Size() - n
		}
	}
	b := make([]byte, n)
	got, err := f.ReadAt(b, start)
	if err != nil && got == 0 {
		// A shell's `> file` opens stdout write-only: read the same file
		// through a new read-only descriptor.
		if r, oerr := reopenRead(f); oerr == nil {
			got, _ = r.ReadAt(b, start)
			r.Close()
		}
	}
	n = int64(got)
	s := newIDScanner(h)
	s.feed(b[:n])
	return s.id
}

// nativeWatchMax bounds the interval at which pyre run looks for the
// conversation id in the child's output while it runs.
var nativeWatchMax = 5 * time.Second

// watchNative tells the node the conversation id as soon as the child's
// output shows it (Codex's thread.started is its first event), so the run's
// transcript and activity show while it works: with only the exit report
// naming it, a 20-minute review read as a hung run with no transcript. It
// reports once; the exit report still carries the final id. A node without
// the endpoint answers an error, which is ignored.
func (a *App) watchNative(run *headlessRun, find func() string, stop <-chan struct{}) {
	wait := 200 * time.Millisecond
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		if id := find(); id != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = localExecPost(ctx, run.sock, localExecNativePath, map[string]any{"run_id": run.binding.RunID, "secret": run.secret, "native_id": id}, nil)
			cancel()
			return
		}
		wait = min(wait*2, nativeWatchMax)
	}
}

func (a *App) reportExit(run *headlessRun, code *int, sig, nativeID string) {
	// The node answers as soon as it wrote the state; the bound keeps a
	// stuck socket from delaying the caller's exit code.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = localExecPost(ctx, run.sock, localExecExitedPath, map[string]any{"run_id": run.binding.RunID, "secret": run.secret,
		"exit_code": code, "signal": sig, "native_id": nativeID}, nil)
}

// teeWriter passes output through and scans it. A failed write (the
// caller closed its end) is returned, so the child sees the broken pipe as
// it would writing directly.
type teeWriter struct {
	w     io.Writer
	scan  *sessionScanner // nil without a recorded run
	watch *codexWatch     // nil unless codex exec --json
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if t.scan != nil {
		t.scan.feed(p[:n])
	}
	if t.watch != nil {
		t.watch.feed(p[:n])
	}
	return n, err
}

var (
	sessionIDRe = regexp.MustCompile(`"session_id"\s*:\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
	threadIDOut = regexp.MustCompile(`"thread_id"\s*:\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
)

// sessionScanner finds the native id in the output with bounded memory (a
// 128-byte carry-over between writes): the last "session_id" in Claude's
// JSON or stream-json output (the zero value), or with codex the first
// "thread_id" in codex exec --json output (its thread.started event; a
// later one may name another thread, e.g. a subagent's).
type sessionScanner struct {
	mu    sync.Mutex // feed runs on the output copier; ID on the watcher
	tail  []byte
	id    string
	codex bool
}

// ID is the id seen so far ("" before it).
func (s *sessionScanner) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func newIDScanner(h c.Harness) *sessionScanner {
	return &sessionScanner{codex: h == c.HarnessCodex}
}

func (s *sessionScanner) feed(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) == 0 || s.codex && s.id != "" {
		return
	}
	buf := append(s.tail, p...)
	key, re := `"session_id"`, sessionIDRe
	if s.codex {
		key, re = `"thread_id"`, threadIDOut
	}
	if bytes.Contains(buf, []byte(key)) {
		if m := re.FindAllSubmatch(buf, -1); len(m) > 0 {
			s.id = string(m[len(m)-1][1])
			if s.codex {
				s.id = string(m[0][1])
			}
		}
	}
	if len(buf) > 128 {
		buf = buf[len(buf)-128:]
	}
	s.tail = append(s.tail[:0], buf...)
}

// localExecNode is this machine's pyred serving headless runs.
type localExecNode struct {
	sock, id string
	start    bool // it records runs itself with the node's credential
}

// localExecSocket finds this machine's pyred socket that serves headless
// runs: $PYRE_SOCKET, else the default state path (localSockets). The
// zero value when none answers.
func localExecSocket(ctx context.Context, getenv func(string) string) localExecNode {
	for _, sock := range localSockets(getenv) {
		var info struct {
			NodeID string `json:"node_id"`
			Start  bool   `json:"start"`
		}
		if localExecGet(ctx, sock, &info) == nil && info.NodeID != "" {
			return localExecNode{sock: sock, id: info.NodeID, start: info.Start}
		}
	}
	return localExecNode{}
}

func localHTTP(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

func localExecGet(ctx context.Context, sock string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://pyred"+localExecPath, nil)
	resp, err := localHTTP(sock).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pyred: %s", resp.Status) // an older pyred: no headless runs
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func localExecPost(ctx context.Context, sock, path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://pyred"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := localHTTP(sock).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error *c.Error `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e) == nil && e.Error != nil {
			return e.Error
		}
		return fmt.Errorf("pyred: %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func oneLine(err error) string {
	var ce *c.Error
	msg := err.Error()
	if errors.As(err, &ce) {
		msg = ce.Message
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return msg
}
