package enrollment

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/transport"
)

// DefaultRemoteDir is where the bootstrap installs pyred on the target, in
// a user-owned directory (no root, no service installation; WP20 owns
// service install).
const DefaultRemoteDir = "~/.local/share/pyre"

// Step is one local command of a bootstrap plan. Stdin carries secret
// material (the enrollment secret) so it never appears in argv, remote
// process listings, or printed plans.
type Step struct {
	Name  string
	Argv  []string
	Stdin string
}

// Plan is the exact sequence of scp/ssh invocations that installs pyred on a
// host reached through the user's existing SSH alias and enrolls it. Host
// keys are verified by ssh's normal behaviour: the plan never passes
// StrictHostKeyChecking or UserKnownHostsFile options.
type Plan struct {
	Alias          string
	HubURL         string
	NodeID         string
	RemoteDir      string
	RemoteBinary   string
	CredentialFile string
	Steps          []Step
}

var (
	aliasRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)
	remoteDirRe = regexp.MustCompile(`^(~/|/)[A-Za-z0-9._/-]*$`)
)

// SSHBootstrapPlan builds the plan:
//
//  1. ssh <alias> 'umask 077 && mkdir -p <dir>/bin'
//  2. scp <localBinaryPath> <alias>:<dir>/bin/.pyred.new
//  3. ssh <alias> 'chmod 700 <tmp> && mv -f <tmp> <dir>/bin/pyred && <bin> version'
//  4. ssh <alias> '<bin> enroll --hub <url> [--pin <sha256>] --node-id <id>
//     --credential-file <dir>/node-credential.json --secret-stdin'  (secret on stdin)
//
// `pyred enroll` redeems the ticket at contracts.PathEnrollExchange and
// writes the credential file with mode 0600 (see SaveNodeCredential).
// hubURL and pin override the ticket's fields when non-empty. remoteDir must
// be absolute or start with "~/"; empty uses DefaultRemoteDir.
func SSHBootstrapPlan(alias, localBinaryPath, hubURL, pin string, ticket contracts.EnrollmentTicket, remoteDir string) (Plan, error) {
	if !aliasRe.MatchString(alias) {
		return Plan{}, contracts.Errorf(contracts.ErrInvalid, "ssh alias %q must be a plain host alias from your ssh config", alias)
	}
	if localBinaryPath == "" {
		return Plan{}, contracts.Errorf(contracts.ErrInvalid, "local pyred binary path is required")
	}
	if hubURL == "" {
		hubURL = ticket.HubURL
	}
	if pin == "" {
		pin = ticket.HubCertSHA256
	}
	if hu, err := url.Parse(hubURL); err != nil || hu.Scheme != "https" || hu.Host == "" {
		return Plan{}, contracts.Errorf(contracts.ErrInvalid, "hub URL must be https://host[:port] reachable from the node")
	}
	if pin != "" {
		var err error
		if pin, err = transport.NormalizeFingerprint(pin); err != nil {
			return Plan{}, contracts.Errorf(contracts.ErrInvalid, "%v", err)
		}
	}
	if !contracts.ValidID(ticket.NodeID, contracts.PrefixNode) || !strings.HasPrefix(ticket.Secret, transport.PrefixEnrollSecret) {
		return Plan{}, contracts.Errorf(contracts.ErrInvalid, "enrollment ticket is missing its node id or secret")
	}
	if remoteDir == "" {
		remoteDir = DefaultRemoteDir
	}
	remoteDir = strings.TrimSuffix(remoteDir, "/")
	if !remoteDirRe.MatchString(remoteDir+"/") || strings.Contains(remoteDir, "..") {
		return Plan{}, contracts.Errorf(contracts.ErrInvalid, "remote dir must be absolute or ~/relative and use only [A-Za-z0-9._/-]")
	}
	shellPath := func(p string) string { // for the remote shell
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			return `"$HOME"/` + rest
		}
		return p
	}
	scpPath := func(p string) string { // scp: relative paths are relative to $HOME
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			return rest
		}
		return p
	}
	binDir := remoteDir + "/bin"
	bin := binDir + "/pyred"
	tmp := binDir + "/.pyred.new"
	cred := remoteDir + "/node-credential.json"

	enroll := []string{shellPath(bin), "enroll", "--hub", shellQuote(hubURL)}
	if pin != "" {
		enroll = append(enroll, "--pin", pin)
	}
	enroll = append(enroll, "--node-id", ticket.NodeID, "--credential-file", shellPath(cred), "--secret-stdin")

	return Plan{
		Alias: alias, HubURL: hubURL, NodeID: ticket.NodeID, RemoteDir: remoteDir, RemoteBinary: bin, CredentialFile: cred,
		Steps: []Step{
			{Name: "prepare remote directory", Argv: []string{"ssh", alias, "umask 077 && mkdir -p " + shellPath(binDir)}},
			{Name: "copy pyred", Argv: []string{"scp", "-q", localBinaryPath, alias + ":" + scpPath(tmp)}},
			{Name: "install pyred", Argv: []string{"ssh", alias, fmt.Sprintf("chmod 700 %s && mv -f %s %s && %s version", shellPath(tmp), shellPath(tmp), shellPath(bin), shellPath(bin))}},
			{Name: "enroll node", Argv: []string{"ssh", alias, strings.Join(enroll, " ")}, Stdin: ticket.Secret + "\n"},
		},
	}, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// String renders the plan for display (dry run); secrets are never shown.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bootstrap %s (node %s) -> %s\n", p.Alias, p.NodeID, p.HubURL)
	for i, s := range p.Steps {
		fmt.Fprintf(&b, "  %d. %s: %s", i+1, s.Name, transport.Redact(strings.Join(s.Argv, " ")))
		if s.Stdin != "" {
			b.WriteString("  (enrollment secret on stdin)")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Runner executes one local command. Implementations must not log stdin.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// ExecRunner runs commands with os/exec, inheriting the user's environment
// (SSH agent, config, known_hosts).
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// ExecuteOptions control Execute.
type ExecuteOptions struct {
	DryRun bool      // print the plan, run nothing
	Out    io.Writer // progress and (redacted) command output; nil discards
}

type redactWriter struct{ w io.Writer }

func (r redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(r.w, transport.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Execute runs the plan's steps in order with runner, stopping at the first
// failure. Output is redacted. In dry-run mode it only prints the plan.
func Execute(ctx context.Context, plan Plan, runner Runner, opts ExecuteOptions) error {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	if opts.DryRun {
		_, err := io.WriteString(out, "dry run: "+plan.String())
		return err
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	rw := redactWriter{w: out}
	for i, s := range plan.Steps {
		fmt.Fprintf(out, "==> [%d/%d] %s\n", i+1, len(plan.Steps), s.Name)
		var stdin io.Reader = strings.NewReader("")
		if s.Stdin != "" {
			stdin = strings.NewReader(s.Stdin)
		}
		if err := runner.Run(ctx, s.Argv, stdin, rw, rw); err != nil {
			return fmt.Errorf("bootstrap step %q failed: %s", s.Name, transport.Redact(err.Error()))
		}
	}
	fmt.Fprintf(out, "==> %s enrolled as %s; credential at %s:%s\n", plan.Alias, plan.NodeID, plan.Alias, plan.CredentialFile)
	return nil
}
