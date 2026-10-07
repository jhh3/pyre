// Package cli implements the `pyre` command line and its MCP server.
//
// Humans get readable output; every command also accepts --json and then
// prints the contract type (internal/contracts) on stdout, with all
// diagnostics on stderr. Exit codes are stable (see exit.go). Commands and
// MCP tools share one operation layer (Ops), so agents and humans get the
// same semantics.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/notifications"
)

type contextT = context.Context

// App holds process-level dependencies, injectable for tests.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// Exec runs argv in the foreground with the App's stdio (attach, $EDITOR).
	Exec func(argv []string) error
	// RunTUI launches the terminal UI for `pyre` with no arguments on a TTY.
	RunTUI func(ctx context.Context, api contracts.API, opts TUIOptions) error
	// Notify shows a desktop notification (`pyre watch`).
	Notify func(title, body string) error
	// IsTerminal reports whether stdin and stdout are a terminal.
	IsTerminal func() bool
	// API, when non-nil, replaces connection resolution (in-process tests).
	API contracts.API
	// PollInterval paces follow/wait loops (default 1s).
	PollInterval time.Duration

	// per-invocation state
	jsonOut    bool
	hubFlag    string
	pinFlag    string
	configFlag string
	opID       string
	conn       *Connection
	ops        *Ops
	ran        bool
	cached     *contracts.Freshness
}

// NewApp returns an App wired to the real process environment.
func NewApp() *App {
	a := &App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv,
		RunTUI: RunTUI, Notify: notifications.Desktop}
	a.IsTerminal = func() bool { return isTTY(os.Stdin) && isTTY(os.Stdout) }
	return a
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (a *App) defaults() {
	if a.Stdin == nil {
		a.Stdin = strings.NewReader("")
	}
	if a.Stdout == nil {
		a.Stdout = io.Discard
	}
	if a.Stderr == nil {
		a.Stderr = io.Discard
	}
	if a.Getenv == nil {
		a.Getenv = os.Getenv
	}
	if a.Exec == nil {
		a.Exec = a.execForeground
	}
	if a.RunTUI == nil {
		a.RunTUI = RunTUI
	}
	if a.Notify == nil {
		a.Notify = func(string, string) error { return nil }
	}
	if a.IsTerminal == nil {
		a.IsTerminal = func() bool { return false }
	}
	if a.PollInterval <= 0 {
		a.PollInterval = time.Second
	}
}

// Main runs one invocation and returns the process exit code.
func (a *App) Main(ctx context.Context, args []string) int {
	a.defaults()
	root := a.rootCmd()
	if args == nil {
		// cobra reads os.Args[1:] when given nil args; Main's caller owns
		// the argument list (a test binary's flags are not pyre arguments).
		args = []string{}
	}
	root.SetArgs(args)
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	err := root.ExecuteContext(client.WithFreshnessSink(ctx, a.noteCached))
	if err == nil {
		a.printCached()
		return ExitOK
	}
	code := ExitCodeFor(err)
	var ce *contracts.Error
	var st *statusError
	if !a.ran && !errors.As(err, &ce) && !errors.As(err, &st) {
		code = ExitUsage // cobra argument/flag errors happen before RunE
	}
	if errors.Is(err, context.Canceled) {
		code = ExitError
	}
	a.reportError(err, code)
	return code
}

// noteCached records that an answer came from a node's read cache while the
// hub was unreachable; printCached tells the user (on stderr, so --json
// output stays parseable) once the command succeeded, with the copy's age.
// A failed command prints only its error.
func (a *App) noteCached(op string, f contracts.Freshness) {
	if a.cached == nil || f.ObservedAt.Before(a.cached.ObservedAt) {
		a.cached = &f // report the oldest copy used
	}
}

func (a *App) printCached() {
	f := a.cached
	if f == nil {
		return
	}
	age := "-"
	if !f.ObservedAt.IsZero() {
		age = ago(f.ObservedAt)
	}
	fmt.Fprintf(a.Stderr, "pyre: hub unavailable; showing the copy cached on node %s, last confirmed with the hub %s (%s)\n",
		strings.TrimPrefix(f.Source, "node-cache:"), age, f.ObservedAt.UTC().Format(time.RFC3339))
}

func (a *App) reportError(err error, code int) {
	if a.jsonOut {
		var e *contracts.Error
		if !errors.As(err, &e) {
			e = &contracts.Error{Code: codeForExit(code), Message: err.Error()}
		}
		b, _ := json.Marshal(contracts.ErrorBody{Error: e})
		fmt.Fprintln(a.Stderr, string(b))
		return
	}
	fmt.Fprintf(a.Stderr, "pyre: %s\n", err)
	a.reportEndpoint(err)
	switch code {
	case ExitUsage:
		if !a.ran {
			fmt.Fprintln(a.Stderr, "run `pyre --help` for usage")
		}
	case ExitIndeterminate:
		fmt.Fprintln(a.Stderr, "the outcome is unknown: inspect it before retrying; never blindly resend")
	}
}

// reportEndpoint says, after a failed round trip, which endpoint was tried
// (and where that choice came from) and what to do.
func (a *App) reportEndpoint(err error) {
	var ce *contracts.Error
	if a.conn == nil || !errors.As(err, &ce) {
		return
	}
	switch ce.Details[client.DetailUnreachable] {
	case "hub":
		fmt.Fprintf(a.Stderr, "tried %s\n", a.conn.Describe())
		if a.conn.Mode == "socket" { // pyred answered; its hub link failed
			fmt.Fprintln(a.Stderr, "the local pyred could not reach its hub: check the hub, and `pyred status` on this machine")
			return
		}
		fmt.Fprintln(a.Stderr, "check that the hub is running and reachable from this machine (`pyre hello` tests the connection), or use --hub / PYRE_HUB for another address")
	case "local_daemon":
		fmt.Fprintf(a.Stderr, "tried %s\n", a.conn.Describe())
	}
}

func codeForExit(code int) contracts.ErrorCode {
	switch code {
	case ExitUsage:
		return contracts.ErrInvalid
	case ExitConflict:
		return contracts.ErrConflict
	case ExitNotFound:
		return contracts.ErrNotFound
	case ExitUnavailable:
		return contracts.ErrUnavailable
	case ExitUnsupported:
		return contracts.ErrUnsupported
	case ExitIndeterminate:
		return contracts.ErrOutcomeIndeterminate
	}
	return contracts.ErrInternal
}

// run wraps a RunE so usage-vs-runtime errors are classified correctly.
func (a *App) run(fn func(cmd *cobra.Command, args []string) error) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		a.ran = true
		return fn(cmd, args)
	}
}

// connect resolves the connection lazily (once per invocation).
func (a *App) connect() (*Ops, error) {
	if a.ops != nil {
		return a.ops, nil
	}
	api := a.API
	if api == nil {
		conn, err := Resolve(ConnOptions{HubFlag: a.hubFlag, PinFlag: a.pinFlag, ConfigPath: a.configFlag, Getenv: a.Getenv})
		if err != nil {
			return nil, err
		}
		a.conn = conn
		api = conn.Client
	}
	self := IdentityFromEnv(a.Getenv)
	if self.NodeID == "" {
		// Not inside a session: ask a pyred on this machine who it is, so
		// attach/start know the client's node (local vs ssh attach).
		self.NodeID = discoverLocalNode(a.Getenv)
	}
	a.ops = &Ops{API: api, Self: self, PollInterval: a.PollInterval}
	return a.ops, nil
}

func (a *App) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "pyre",
		Short: "Pyre: sessions, messages and shared work across your machines",
		Long: `Pyre connects your Claude Code / Codex sessions and terminals across Mac and
Linux machines. Run with no arguments on a terminal for the interactive UI.

Every command accepts --json: stdout then carries the contract type, and
diagnostics go to stderr. IDs are discoverable (pyre nodes, pyre sessions,
pyre project ls); names are accepted where unambiguous.

Connection: --hub/PYRE_HUB, else PYRE_SOCKET (local daemon), else
~/.config/pyre/client.json. Credential: PYRE_SESSION_TOKEN, else PYRE_TOKEN,
else the client config.

` + exitCodesHelp,
		Version:       buildinfo.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if !a.IsTerminal() || a.jsonOut {
				return cmd.Help()
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			return a.RunTUI(cmd.Context(), ops.API, TUIOptions{Stdin: a.Stdin, Stdout: a.Stdout, Stderr: a.Stderr, Self: ops.Self})
		}),
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return &usageError{msg: err.Error()} })
	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", false, "print the contract type as JSON on stdout (diagnostics on stderr)")
	pf.StringVar(&a.hubFlag, "hub", "", "hub URL (overrides PYRE_HUB, PYRE_SOCKET and the client config)")
	pf.StringVar(&a.pinFlag, "pin", "", "hub TLS certificate SHA-256 pin (or PYRE_HUB_PIN)")
	pf.StringVar(&a.configFlag, "config", "", "client config path (default $PYRE_CONFIG or ~/.config/pyre/client.json)")
	pf.StringVar(&a.opID, "operation-id", "", "stable operation ID for a mutating command, so a retried invocation is not repeated")
	root.SetVersionTemplate("pyre {{.Version}}\n")

	for _, add := range []func(*cobra.Command){
		a.addSystemCommands, a.addFleetCommands, a.addSessionCommands, a.addMessageCommands,
		a.addApprovalCommands, a.addProjectCommands, a.addArtifactCommands, a.addNodeActionCommands,
		a.addAttentionCommands, a.addAgentCommands, a.addDigestCommands, a.addSearchCommands,
		a.addQuickLaunchCommands, // W-quicklaunch
		a.addAuditCommands,       // W-audit
	} {
		add(root)
	}
	rejectUnknownSubcommands(root)
	return root
}

// rejectUnknownSubcommands makes every command group (subcommands, no run
// function) fail with a usage error on an unknown subcommand instead of
// printing help and exiting 0; with no arguments it still prints help.
func rejectUnknownSubcommands(c *cobra.Command) {
	for _, sub := range c.Commands() {
		rejectUnknownSubcommands(sub)
	}
	if c.Parent() == nil || !c.HasSubCommands() || c.Runnable() {
		return
	}
	c.Args = cobra.ArbitraryArgs
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return usagef("unknown command %q for %q (see %s --help)", args[0], cmd.CommandPath(), cmd.CommandPath())
		}
		return cmd.Help()
	}
}

// ---- output ----

// emit prints v as JSON in --json mode, else calls human.
func (a *App) emit(v any, human func(w io.Writer)) error {
	if a.jsonOut {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human(a.Stdout)
	return nil
}

// emitLine prints v as one JSON line (streams) or calls human.
func (a *App) emitLine(v any, human func(w io.Writer)) error {
	if a.jsonOut {
		return json.NewEncoder(a.Stdout).Encode(v)
	}
	human(a.Stdout)
	return nil
}

// warnf writes a diagnostic to stderr (never stdout).
func (a *App) warnf(format string, args ...any) {
	fmt.Fprintf(a.Stderr, format+"\n", args...)
}

func table(w io.Writer, header string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		return "in " + short(-d)
	}
	return short(d) + " ago"
}

// agoFrom is ago measured against ref (a server clock reading) instead of
// this machine's clock; a zero ref falls back to ago.
func agoFrom(ref, t time.Time) string {
	if ref.IsZero() || t.IsZero() {
		return ago(t)
	}
	d := ref.Sub(t)
	if d < 0 {
		d = 0 // the same clock stamped both; never "in the future"
	}
	return short(d) + " ago"
}

func agoPtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return ago(*t)
}

func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func strPtr(p *string) string {
	if p == nil || *p == "" {
		return "-"
	}
	return *p
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// readArg returns s, or stdin when s is "-".
func (a *App) readArg(s string) (string, error) {
	if s != "-" {
		return s, nil
	}
	b, err := io.ReadAll(io.LimitReader(a.Stdin, 16<<20))
	return string(b), err
}

// execForeground runs argv with the App's stdio attached.
func (a *App) execForeground(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	return cmd.Run()
}
