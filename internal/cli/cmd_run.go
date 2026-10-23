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
	"time"

	"github.com/spf13/cobra"

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
// warning line on stderr.
//
// Local-socket protocol: daemon.PathLocalExec* (kept as literals so the CLI
// does not link the daemon; internal/daemon/execrun.go).
const (
	localExecPath        = "/v1/local/exec"
	localExecStartPath   = "/v1/local/exec/start"
	localExecClaimPath   = "/v1/local/exec/claim"
	localExecStartedPath = "/v1/local/exec/started"
	localExecExitedPath  = "/v1/local/exec/exited"
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
}

// EnvRunFallback sets pyre run's default --fallback.
const EnvRunFallback = "PYRE_RUN_FALLBACK"

// ExitPyreUnavailable is pyre run's exit code with --fallback=fail when
// Pyre could not run the command (EX_TEMPFAIL).
const ExitPyreUnavailable = 75

// runDownTTL: after Pyre was unreachable, later calls skip the setup for
// this long and run the command on the own login at once.
const runDownTTL = 60 * time.Second

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
eligible, the command runs exactly as given on this machine's own login after
one warning line on stderr.

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
			return a.runHeadless(cmd.Context(), o, args)
		}),
	}
	f := cmd.Flags()
	f.StringVar(&o.project, "project", "", "record the session in this project (ID or name)")
	f.StringVar(&o.title, "title", "", "session title (default: pyre run: <folder>)")
	f.StringVar(&o.account, "account", "", "run on this account (ID, label or rota name) instead of the best eligible one")
	f.StringVar(&o.harness, "harness", "", "claude or codex (default: from the command's name)")
	f.DurationVar(&o.setup, "setup-timeout", 0, "how long to wait for the hub and node before running without Pyre (default 10s, or $"+EnvRunSetupTimeout+")")
	f.StringVar(&o.fallback, "fallback", "", "own (default): when Pyre is unavailable or declines (no eligible account), run on this machine's own login; fail: exit 75 instead (or $"+EnvRunFallback+")")
	f.BoolVar(&o.keep, "keep", false, "keep the session listed after the run (default: archived when the run ends; still searchable and resumable)")
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
		o.setup = 10 * time.Second
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
	var run *headlessRun
	var reason string
	refused := false
	down := runDownMarker(a.Getenv)
	if fi, err := os.Stat(down); err == nil && time.Since(fi.ModTime()) < runDownTTL {
		reason = "Pyre was unavailable moments ago (rechecked after " + runDownTTL.String() + ")"
	} else {
		run, reason, refused = a.prepareHeadless(ctx, o, harness, argv)
		if run == nil && !refused && ctx.Err() == nil {
			_ = os.WriteFile(down, nil, 0o600) // remembered briefly: calls in a burst do not each wait
		} else if run != nil {
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
		fmt.Fprintf(a.Stderr, "pyre run: %s; running the command without Pyre on this machine's own login (%s)\n", what, ownLogin(harness, a.Getenv))
		return a.execChild(ctx, harness, exe, argv[1:], withoutSessionIdentity(os.Environ()), nil)
	}
	return a.execChild(ctx, harness, exe, argv[1:], childEnv(withoutSessionIdentity(os.Environ()), run.binding), run)
}

// ownLogin names the login a fallback runs on.
func ownLogin(h c.Harness, getenv func(string) string) string {
	v, def := "CLAUDE_CONFIG_DIR", "~/.claude"
	if h == c.HarnessCodex {
		v, def = "CODEX_HOME", "~/.codex"
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
// PYRE_HUB, PYRE_CLI, a PYRE_TOKEN login) stay.
var sessionIdentityEnv = []string{"PYRE_SESSION_ID", "PYRE_RUN_ID", "PYRE_SESSION_TOKEN", "PYRE_HARNESS", "PYRE_RUN_DIR",
	"PYRE_PROJECT_ID", "PYRE_PARENT_SESSION_ID", "PYRE_REPORT_TO_PARENT", "PYRE_PERMISSIONS", "PYRE_ENV_GENERATION", "PYRE_CLAUDE_STATE_DIR"}

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
// not be reached at all.
//
// With a hub credential (a client login, or PYRE_SESSION_TOKEN inside a
// Pyre session) the CLI records the run in the hub itself. Without one,
// this machine's pyred records it with the node's own credential
// (localExecStartPath): the hub lets a node start exactly this headless
// shape on itself, so a machine needs no owner login for pyre run.
func (a *App) prepareHeadless(ctx context.Context, o runOpts, h c.Harness, argv []string) (*headlessRun, string, bool) {
	sctx, cancel := context.WithTimeout(ctx, o.setup)
	defer cancel()
	node := localExecSocket(sctx, a.Getenv)
	if node.sock == "" {
		return nil, "no Pyre node daemon (pyred) with headless runs on this machine", false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "working directory: " + oneLine(err), false
	}
	secretB := make([]byte, 32)
	if _, err := rand.Read(secretB); err != nil {
		return nil, oneLine(err), false
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
	refused := false
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
			return nil, "this machine's node did not record the run: " + oneLine(err), refusal(err)
		}
		runID = res.RunID
	case err != nil:
		return nil, "no hub: " + oneLine(err) + "; " + noNodeStart, false
	default:
		// A login, or the local socket (pyred may hold an owner credential).
		if runID, reason, refused = a.startHeadlessInHub(sctx, ops, node.id, o, h, hash, resume, title, cwd); runID == "" {
			if noLogin && !refused {
				reason += "; no hub login on this machine, and " + noNodeStart
			}
			return nil, reason, refused
		}
	}
	var b execBinding
	err = localExecPost(sctx, node.sock, localExecClaimPath, map[string]any{"run_id": runID, "secret": secret, "client_pid": os.Getpid()}, &b)
	if err != nil {
		return nil, "no account binding from this machine's node: " + oneLine(err), refusal(err)
	}
	return &headlessRun{sock: node.sock, secret: secret, binding: b}, "", false
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
// session id seen in the output) to the node.
func (a *App) execChild(ctx context.Context, h c.Harness, exe string, args, env []string, run *headlessRun) error {
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stderr, cmd.Stdout = os.Stdin, os.Stderr, os.Stdout
	var scan *sessionScanner
	var fileOut *os.File
	var fileOff int64 = -1
	if run != nil {
		switch stdoutKind() {
		case outStream:
			// A pipe or socket: read the output on its way through for
			// Claude's session_id. WaitDelay: a background process claude
			// left holding the pipe cannot keep pyre (and the exit report)
			// waiting long after claude exited.
			scan = newIDScanner(h)
			cmd.Stdout = &teeWriter{w: os.Stdout, scan: scan}
			cmd.WaitDelay = 2 * time.Second
		case outFile:
			// A regular file is passed as is (same file type for the child,
			// no pipe to hold open); the id is read from what it wrote.
			if off, err := os.Stdout.Seek(0, io.SeekCurrent); err == nil {
				fileOut, fileOff = os.Stdout, off
			}
		}
	}
	// The child dies with pyre run (Linux parent-death signal); a headless
	// caller's child leads its own process group so the node can stop it
	// and its tools if pyre run is killed (macOS has no parent-death signal).
	group := run != nil && !isTTY(os.Stdin)
	cmd.SysProcAttr = childSysProcAttr(group)
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
	if err := <-started; err != nil {
		if run != nil {
			a.reportExit(run, nil, "", "")
		}
		fmt.Fprintf(a.Stderr, "pyre run: %v\n", err)
		return &silentExit{code: 127}
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
	tty := openControllingTTY()
	defer tty.close()
	var werr error
wait:
	for {
		select {
		case s := <-sigs:
			if !group && fromTerminal(s) && tty.foreground() {
				continue // the terminal sent it to the whole foreground group: the child has it
			}
			_ = cmd.Process.Signal(s)
		case werr = <-done:
			break wait
		}
	}
	code, sig := exitStatus(cmd.ProcessState, werr)
	if run != nil {
		id := ""
		switch {
		case scan != nil:
			id = scan.id
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
	w    io.Writer
	scan *sessionScanner
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.scan.feed(p[:n])
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
	tail  []byte
	id    string
	codex bool
}

func newIDScanner(h c.Harness) *sessionScanner {
	return &sessionScanner{codex: h == c.HarnessCodex}
}

func (s *sessionScanner) feed(p []byte) {
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
