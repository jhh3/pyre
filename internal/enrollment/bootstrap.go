package enrollment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/transport"
)

// DefaultRemoteDir is where the bootstrap installs pyred on a macOS or
// Linux target, in a user-owned directory (no root, no service
// installation; WP20 owns service install). It is install.sh's prefix.
const DefaultRemoteDir = "~/.local/share/pyre"

// DefaultWindowsRemoteDir is the Windows equivalent: install.ps1's prefix
// (%LOCALAPPDATA%\Programs\Pyre for a standard profile), where install.ps1
// finds bin\pyred.exe and node-credential.json from a bootstrap.
const DefaultWindowsRemoteDir = "~/AppData/Local/Programs/Pyre"

// Target operating systems a bootstrap plan is built for.
const (
	TargetUnix    = "unix"    // macOS, Linux: POSIX shell steps
	TargetWindows = "windows" // PowerShell steps (Windows OpenSSH, any login shell)
)

// Step is one local command of a bootstrap plan. Stdin carries secret
// material (the enrollment secret) so it never appears in argv, remote
// process listings, or printed plans. Display, when set, is what a printed
// plan shows instead of Argv (a readable form of an encoded PowerShell
// script).
type Step struct {
	Name    string
	Argv    []string
	Stdin   string
	Display string `json:",omitempty"`
}

// Plan is the exact sequence of scp/ssh invocations that installs pyred on a
// host reached through the user's existing SSH alias and enrolls it. Host
// keys are verified by ssh's normal behaviour: the plan never passes
// StrictHostKeyChecking or UserKnownHostsFile options.
type Plan struct {
	Alias          string
	Target         string
	HubURL         string
	NodeID         string
	RemoteDir      string
	RemoteBinary   string
	CredentialFile string
	Steps          []Step
}

var (
	aliasRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)
	remoteDirRe    = regexp.MustCompile(`^(~/|/)[A-Za-z0-9._/-]*$`)
	winRemoteDirRe = regexp.MustCompile(`^~/[A-Za-z0-9._/-]*$`)
)

// SSHBootstrapPlan builds the plan for a macOS or Linux target:
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
	return SSHBootstrapPlanFor(TargetUnix, alias, localBinaryPath, hubURL, pin, ticket, remoteDir)
}

// SSHBootstrapPlanFor builds the plan for target (TargetUnix or
// TargetWindows). A Windows plan runs every remote step as
// `powershell -NoProfile -NonInteractive -EncodedCommand <script>`, so it
// works whether the target's OpenSSH login shell is cmd.exe or PowerShell:
//
//  1. create <dir>\bin under %USERPROFILE%
//  2. scp <localBinaryPath> <alias>:<dir>/bin/.pyred.new
//  3. Move-Item it to <dir>\bin\pyred.exe, then `pyred.exe version`
//  4. read the secret from stdin and pipe it to `pyred.exe enroll ...
//     --credential-file <dir>\node-credential.json --secret-stdin`
//
// On Windows remoteDir must start with "~/" (relative to %USERPROFILE%,
// which is also where scp puts relative paths); empty uses
// DefaultWindowsRemoteDir. localBinaryPath must be a pyred.exe built for
// the target's architecture.
func SSHBootstrapPlanFor(target, alias, localBinaryPath, hubURL, pin string, ticket contracts.EnrollmentTicket, remoteDir string) (Plan, error) {
	if target != TargetUnix && target != TargetWindows {
		return Plan{}, contracts.Errorf(contracts.ErrInvalid, "bootstrap target must be %q or %q (got %q)", TargetUnix, TargetWindows, target)
	}
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
	if target == TargetWindows {
		return windowsPlan(alias, localBinaryPath, hubURL, pin, ticket, remoteDir)
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
		Alias: alias, Target: TargetUnix, HubURL: hubURL, NodeID: ticket.NodeID, RemoteDir: remoteDir, RemoteBinary: bin, CredentialFile: cred,
		Steps: []Step{
			{Name: "prepare remote directory", Argv: []string{"ssh", alias, "umask 077 && mkdir -p " + shellPath(binDir)}},
			{Name: "copy pyred", Argv: []string{"scp", "-q", localBinaryPath, alias + ":" + scpPath(tmp)}},
			{Name: "install pyred", Argv: []string{"ssh", alias, fmt.Sprintf("chmod 700 %s && mv -f %s %s && %s version", shellPath(tmp), shellPath(tmp), shellPath(bin), shellPath(bin))}},
			{Name: "enroll node", Argv: []string{"ssh", alias, strings.Join(enroll, " ")}, Stdin: ticket.Secret + "\n"},
		},
	}, nil
}

// scpPath: scp resolves relative paths against the remote home (on Windows
// OpenSSH too, %USERPROFILE%).
func scpPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return rest
	}
	return p
}

func windowsPlan(alias, localBinaryPath, hubURL, pin string, ticket contracts.EnrollmentTicket, remoteDir string) (Plan, error) {
	if remoteDir == "" {
		remoteDir = DefaultWindowsRemoteDir
	}
	remoteDir = strings.TrimSuffix(remoteDir, "/")
	if !winRemoteDirRe.MatchString(remoteDir+"/") || strings.Contains(remoteDir, "..") || remoteDir == "~" {
		return Plan{}, contracts.Errorf(contracts.ErrInvalid, "on Windows the remote dir must be ~/relative (to %%USERPROFILE%%) and use only [A-Za-z0-9._/-]")
	}
	rel := strings.ReplaceAll(strings.TrimPrefix(remoteDir, "~/"), "/", `\`)
	// $r is the install directory on the target; every script starts there.
	head := "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; $r = Join-Path $env:USERPROFILE " + psQuote(rel) + "; "
	prepare := head + "New-Item -ItemType Directory -Force -Path (Join-Path $r 'bin') | Out-Null; " +
		"$la = Join-Path $env:USERPROFILE 'AppData\\Local'; if ($env:LOCALAPPDATA -and $env:LOCALAPPDATA.TrimEnd('\\') -ne $la) { [Console]::Error.WriteLine(\"note: LOCALAPPDATA is $env:LOCALAPPDATA, not $la; pass install.ps1 -Credential $r\\node-credential.json\") }"
	// A running pyred.exe cannot be replaced but can be renamed: move it
	// aside first (re-bootstrapping a live node), like mv -f on unix.
	install := head + "$b = Join-Path $r 'bin\\pyred.exe'; if (Test-Path -LiteralPath $b) { Remove-Item -Force -LiteralPath \"$b.old\" -ErrorAction SilentlyContinue; Move-Item -Force -LiteralPath $b -Destination \"$b.old\" }; " +
		"Move-Item -Force -LiteralPath (Join-Path $r 'bin\\.pyred.new') -Destination $b; & $b version; exit $LASTEXITCODE"
	enrollCmd := "& (Join-Path $r 'bin\\pyred.exe') enroll --hub " + psQuote(hubURL)
	if pin != "" {
		enrollCmd += " --pin " + pin
	}
	enrollCmd += " --node-id " + ticket.NodeID + " --credential-file (Join-Path $r 'node-credential.json') --secret-stdin"
	// The secret arrives on ssh stdin and is piped to pyred's stdin; it is
	// never in argv or the encoded script.
	enroll := head + "$s = [Console]::In.ReadLine(); $s | " + enrollCmd + "; $c = $LASTEXITCODE; $s = $null; exit $c"
	bin := remoteDir + "/bin/pyred.exe"
	ps := func(name, script string) Step {
		return Step{Name: name, Argv: []string{"ssh", alias, "powershell -NoProfile -NonInteractive -EncodedCommand " + EncodePowerShell(script)}, Display: "ssh " + alias + " powershell: " + script}
	}
	steps := []Step{
		ps("prepare remote directory", prepare),
		{Name: "copy pyred", Argv: []string{"scp", "-q", localBinaryPath, alias + ":" + scpPath(remoteDir+"/bin/.pyred.new")}},
		ps("install pyred", install),
		ps("enroll node", enroll),
	}
	steps[3].Stdin = ticket.Secret + "\n"
	return Plan{Alias: alias, Target: TargetWindows, HubURL: hubURL, NodeID: ticket.NodeID, RemoteDir: remoteDir, RemoteBinary: bin,
		CredentialFile: remoteDir + "/node-credential.json", Steps: steps}, nil
}

// EncodePowerShell encodes script for powershell -EncodedCommand (base64 of
// UTF-16LE), which survives cmd.exe and PowerShell as the remote login
// shell without any quoting.
func EncodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[2*i:], r)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ProbeTarget learns whether alias is a Windows or a POSIX host: `uname -s`
// naming Linux, Darwin or a BSD -> TargetUnix; MINGW/MSYS/CYGWIN, Windows_NT
// (busybox-w32, uutils) or any *_NT -> TargetWindows. Anything else, or no
// uname, is settled by a PowerShell probe ([Environment]::OSVersion.Platform
// = Win32NT -> TargetWindows); an unrecognised uname that PowerShell does not
// answer stays unix. It runs read-only commands only.
func ProbeTarget(ctx context.Context, runner Runner, alias string) (string, error) {
	if !aliasRe.MatchString(alias) {
		return "", contracts.Errorf(contracts.ErrInvalid, "ssh alias %q must be a plain host alias from your ssh config", alias)
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	var out, errb bytes.Buffer
	unameErr := runner.Run(ctx, []string{"ssh", alias, "uname -s"}, strings.NewReader(""), &out, &errb)
	other := false // uname answered, but with no OS we know
	if unameErr == nil {
		switch u := strings.ToUpper(strings.TrimSpace(out.String())); {
		case strings.HasPrefix(u, "MINGW"), strings.HasPrefix(u, "MSYS"), strings.HasPrefix(u, "CYGWIN"),
			strings.HasPrefix(u, "WINDOWS"), strings.Contains(u, "_NT"):
			return TargetWindows, nil
		case u == "LINUX", u == "DARWIN", strings.HasSuffix(u, "BSD"), u == "DRAGONFLY", u == "SUNOS":
			return TargetUnix, nil
		case u != "":
			other = true
		}
	}
	out.Reset()
	errb.Reset()
	psErr := runner.Run(ctx, []string{"ssh", alias, "powershell -NoProfile -NonInteractive -Command [Environment]::OSVersion.Platform"}, strings.NewReader(""), &out, &errb)
	if psErr == nil && strings.Contains(out.String(), "Win32NT") {
		return TargetWindows, nil
	}
	if other {
		return TargetUnix, nil
	}
	detail := strings.TrimSpace(errb.String())
	if unameErr != nil && detail == "" {
		detail = unameErr.Error()
	}
	return "", contracts.Errorf(contracts.ErrUnavailable, "could not tell the operating system of %s (neither `uname -s` nor a PowerShell probe answered: %s); pass --target-os", alias, transport.Redact(credentialsBounded(detail)))
}

func credentialsBounded(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// String renders the plan for display (dry run); secrets are never shown.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bootstrap %s (node %s) -> %s\n", p.Alias, p.NodeID, p.HubURL)
	for i, s := range p.Steps {
		shown := s.Display
		if shown == "" {
			shown = strings.Join(s.Argv, " ")
		}
		fmt.Fprintf(&b, "  %d. %s: %s", i+1, s.Name, transport.Redact(shown))
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
