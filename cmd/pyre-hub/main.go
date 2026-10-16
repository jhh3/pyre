//go:build !windows

// Command pyre-hub is the shared hub service (WP05).
//
//	pyre-hub init    --data DIR [--hub-url URL] [--tls-host H]...
//	pyre-hub serve   --data DIR [--listen ADDR] [--also-listen ADDR]... [--preview-listen ADDR] [--preview-url URL] [--hub-url URL]
//	                 [--fcm-service-account FILE] [--log-file FILE [--log-max-bytes N] [--log-backups N]]
//	pyre-hub backup  --data DIR --out FILE.tar.age|- [--recipient R]... [--recipients-file F]...
//	                 [--passphrase-file F] [--unencrypted] [--verify]
//	pyre-hub restore --in FILE|- (--data NEWDIR | --verify-only) [--identity F]... [--passphrase-file F]
//	pyre-hub compact --data DIR [--dry-run] [--no-vacuum]
//	pyre-hub version [--json]
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
	"github.com/jhh3/pyre/internal/events"
	"github.com/jhh3/pyre/internal/hub"
	"github.com/jhh3/pyre/internal/hub/assemble"
	"github.com/jhh3/pyre/internal/hub/backup"
	"github.com/jhh3/pyre/internal/notifications"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/internal/transport"
)

const usage = `usage:
  pyre-hub init    --data DIR [--hub-url URL] [--tls-host HOST]...
  pyre-hub serve   --data DIR [--listen ADDR] [--also-listen ADDR]... [--preview-listen ADDR] [--preview-url URL] [--hub-url URL]
                   [--fcm-service-account FILE] [--log-file FILE [--log-max-bytes N] [--log-backups N]]
  pyre-hub backup  --data DIR --out FILE.tar.age|- [--recipient age1…|age1pq1…|ssh-…]... [--recipients-file F]...
                   [--passphrase-file F] [--unencrypted] [--verify]
  pyre-hub restore --in FILE|- (--data NEWDIR | --verify-only) [--identity KEYFILE]... [--passphrase-file F]
  pyre-hub compact --data DIR [--dry-run] [--no-vacuum]   (hub stopped: retention pass + VACUUM)
  pyre-hub version [--json]
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(ctx, rest, stdout, stderr)
	case "serve":
		err = cmdServe(ctx, rest, stderr)
	case "backup":
		err = cmdBackup(ctx, rest, stdout, stderr)
	case "restore":
		err = cmdRestore(ctx, rest, stdout, stderr)
	case "compact":
		err = cmdCompact(ctx, rest, stdout, stderr)
	case "version", "--version":
		if len(rest) > 0 && rest[0] == "--json" {
			b, _ := json.Marshal(map[string]any{"version": buildinfo.Version, "protocol": contracts.ProtocolVersion, "min_protocol": contracts.MinProtocolVersion,
				"schema": contracts.SchemaVersion, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH})
			fmt.Fprintln(stdout, string(b))
		} else {
			fmt.Fprintf(stdout, "pyre-hub %s (protocol %d)\n", buildinfo.Version, contracts.ProtocolVersion)
		}
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "pyre-hub: unknown command %q\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(stderr, "pyre-hub %s: %v\n%s", cmd, err, usage)
			return 2
		}
		fmt.Fprintf(stderr, "pyre-hub %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// OwnerCredentialFile is written by init (0600). It holds the first owner
// client credential; it is never logged.
const OwnerCredentialFile = "owner-credential.json"

type ownerCredential struct {
	HubURL        string `json:"hub_url"`
	HubCertSHA256 string `json:"hub_cert_sha256"`
	ClientID      string `json:"client_id"`
	Token         string `json:"token"`
}

func cmdInit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("init", stderr)
	data := fs.String("data", "", "hub data directory (created)")
	hubURL := fs.String("hub-url", "", "URL nodes and clients use to reach the hub (e.g. https://hub.example:7443)")
	var hosts multiFlag
	fs.Var(&hosts, "tls-host", "DNS name or IP for the hub certificate (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *data == "" {
		return usageError{"--data is required"}
	}
	credPath := filepath.Join(*data, OwnerCredentialFile)
	if _, err := os.Stat(credPath); err == nil {
		return fmt.Errorf("%s already exists; this hub is initialized", credPath)
	}
	st, err := assemble.Open(ctx, assemble.Config{DataDir: *data, HubURL: *hubURL, TLSHosts: hosts, Logger: quietLogger(stderr)})
	if err != nil {
		return err
	}
	defer st.Close()
	cred, err := st.Enrollment.BootstrapOwnerClient(ctx, "owner (init)", "cli")
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(ownerCredential{HubURL: *hubURL, HubCertSHA256: st.Cert.Fingerprint, ClientID: cred.ClientID, Token: cred.Token}, "", "  ")
	if err := os.WriteFile(credPath, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, `Pyre hub initialized.
  data directory:   %s
  hub id:           %s
  TLS fingerprint:  %s
  owner credential: %s (mode 0600; contains a secret token)

Next steps:
  1. Start the hub (it listens on the port of --hub-url; give another
     --listen if a proxy or an ssh forward maps that port):
       pyre-hub serve --data %s --listen %s --hub-url %s
  2. Log in from your first client machine: copy %s there
     (e.g. with scp), feed its "token" value to
       pyre login --hub %s --pin %s --token -
     on stdin, then delete the copy.
  3. Pair other clients (the Android app, other laptops) from that client:
       pyre pair-client --name <name> [--kind android|cli|tui]
     The pairing link/QR embeds the hub URL and the TLS fingerprint above.
  4. Enroll nodes from that client:
       pyre enroll-node --name <name> --ssh <host> [--bootstrap [--execute]]
     or create a ticket with --json and run "pyred enroll" on the node.
`, *data, st.Hub.HubID(), st.Cert.Fingerprint, credPath, *data, listenFromURL(*hubURL), orPlaceholder(*hubURL),
		OwnerCredentialFile, orPlaceholder(*hubURL), st.Cert.Fingerprint)
	return nil
}

// listenFromURL is the --listen value matching the hub URL's port: ":7443"
// without a URL, ":443"/":80" for a URL without a port.
func listenFromURL(hubURL string) string {
	if hubURL == "" {
		return ":7443"
	}
	u, err := url.Parse(hubURL)
	if err != nil || u.Host == "" {
		return ":7443"
	}
	if p := u.Port(); p != "" {
		return ":" + p
	}
	if u.Scheme == "http" {
		return ":80"
	}
	return ":443"
}

func orPlaceholder(s string) string {
	if s == "" {
		return "https://<hub-host>:7443"
	}
	return s
}

func quietLogger(w io.Writer) *slog.Logger {
	return transport.NewRedactingLogger(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func cmdServe(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlags("serve", stderr)
	data := fs.String("data", "", "hub data directory (see pyre-hub init)")
	listen := fs.String("listen", ":7443", "HTTPS listen address for the API, node channel and exchanges")
	previewListen := fs.String("preview-listen", "", "separate HTTPS listen address for isolated artifact previews (disabled if empty)")
	hubURL := fs.String("hub-url", "", "URL advertised in enrollment and pairing tickets")
	fcmFile := fs.String("fcm-service-account", "", "Firebase service-account JSON key enabling FCM push (keep it outside the repo, 0600)")
	logFile := fs.String("log-file", "", "write the hub log to this file (size-rotated) instead of stderr")
	logMax := fs.Int64("log-max-bytes", 10<<20, "rotate --log-file at this size")
	logBackups := fs.Int("log-backups", 5, "rotated --log-file copies to keep")
	runtimeStats := fs.String("dev-runtime-stats", "", "testing: rewrite this file every 30s with the hub's goroutine/heap/fd counters (soak leak checks)")
	cpuProfile := fs.String("dev-cpuprofile", "", "testing: write a CPU profile of the whole serve run to this file (written at shutdown)")
	devCmdRet := fs.Duration("dev-command-retention", 0, "testing: keep finished node commands this long instead of 7 days")
	devOpRet := fs.Duration("dev-operation-retention", 0, "testing: keep idempotency records this long instead of 30 days")
	devPrune := fs.Duration("dev-prune-interval", 0, "testing: prune finished commands and idempotency records this often instead of hourly")
	devMaxEvents := fs.Int("dev-max-events", 0, "testing: keep at most this many events instead of 200000")
	auditMaxAge := fs.Duration("audit-max-age", 0, "keep audit log entries (pyre audit) this long (default 2160h = 90 days)")
	auditMaxRows := fs.Int("audit-max-rows", 0, "keep at most this many audit log entries (default 100000)")
	previewURL := fs.String("preview-url", "", "origin of the preview listener used in minted preview links (default: --hub-url host with the --preview-listen port)")
	var hosts, alsoListen multiFlag
	fs.Var(&hosts, "tls-host", "DNS name or IP for a newly generated certificate (repeatable)")
	fs.Var(&alsoListen, "also-listen", "another HTTPS listen address for the same API, e.g. the tailnet address 100.x.y.z:7443 when --listen is a LAN address (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *data == "" {
		return usageError{"--data is required"}
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	unlock, err := lockDataDir(*data) // W-dbhygiene: compact refuses while we serve
	if err != nil {
		return err
	}
	defer unlock()
	var logW io.Writer = stderr
	if *logFile != "" {
		rf := &supervisor.RotatingFile{Path: *logFile, MaxBytes: *logMax, Backups: *logBackups}
		defer rf.Close()
		logW = rf
	}
	logger := transport.NewRedactingLogger(slog.NewTextHandler(logW, nil))
	sqlitex.SetLogger(logger.With("component", "sqlite")) // slow/nested transaction warnings
	var fcm notifications.Provider
	if *fcmFile != "" {
		f, err := notifications.NewFCM(notifications.FCMConfig{ServiceAccountFile: *fcmFile})
		if err != nil {
			return err
		}
		fcm = f
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	pURL := *previewURL
	if pURL == "" && *previewListen != "" {
		pURL = derivePreviewURL(*hubURL, *previewListen)
	}
	hopts := hub.Options{CommandRetention: *devCmdRet, OperationRetention: *devOpRet, PruneInterval: *devPrune}
	if *devMaxEvents > 0 {
		r := events.DefaultRetention
		r.MaxEvents = *devMaxEvents
		hopts.EventRetention = &r
	}
	if *auditMaxAge > 0 || *auditMaxRows > 0 {
		ar := hub.DefaultAuditRetention
		if *auditMaxAge > 0 {
			ar.MaxAge = *auditMaxAge
		}
		if *auditMaxRows > 0 {
			ar.MaxRows = *auditMaxRows
		}
		hopts.AuditRetention = &ar
	}
	st, err := assemble.Open(ctx, assemble.Config{DataDir: *data, HubURL: *hubURL, PreviewURL: pURL, TLSHosts: hosts, Logger: logger, FCM: fcm, Hub: hopts})
	if err != nil {
		return err
	}
	defer st.Close()
	st.Start(ctx)
	// Requests run under a base context that shutdown cancels after a short
	// grace: http.Server.Shutdown waits for in-flight requests, and
	// long-polls (inbox waits up to 60 s) otherwise held every stop for the
	// full shutdown timeout (A20 soak: 10 s; fleet scripts SIGKILLed at 5 s).
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	onShutdown := func() { time.AfterFunc(shutdownGrace, cancelBase) }
	base := func(net.Listener) context.Context { return baseCtx }
	srv := &http.Server{Handler: st.Handler, TLSConfig: st.Cert.ServerTLSConfig(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext: base}
	srv.RegisterOnShutdown(onShutdown)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	servers := []*http.Server{srv}
	errc := make(chan error, 2+len(alsoListen))
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	// --also-listen: the same server on more addresses (W-gcp: the tailnet
	// address for cloud nodes). Clients pin the certificate, so any address works.
	for _, addr := range alsoListen {
		xl, err := net.Listen("tcp", addr)
		if err != nil {
			srv.Close()
			return fmt.Errorf("--also-listen %s: %w", addr, err)
		}
		logger.Info("pyre-hub also serving", "listen", xl.Addr().String())
		go func() { errc <- srv.ServeTLS(xl, "", "") }()
	}
	if *previewListen != "" {
		psrv := &http.Server{Handler: st.Preview, TLSConfig: st.Cert.ServerTLSConfig(), ReadHeaderTimeout: 10 * time.Second, BaseContext: base}
		psrv.RegisterOnShutdown(onShutdown)
		pln, err := net.Listen("tcp", *previewListen)
		if err != nil {
			srv.Close()
			return err
		}
		servers = append(servers, psrv)
		go func() { errc <- psrv.ServeTLS(pln, "", "") }()
	}
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return err
		}
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}
	if *runtimeStats != "" {
		go writeRuntimeStats(ctx, *runtimeStats, 30*time.Second, logger)
	}
	logger.Info("pyre-hub serving", "listen", ln.Addr().String(), "hub_id", st.Hub.HubID(), "tls_sha256", st.Cert.Fingerprint, "version", buildinfo.Version)
	select {
	case <-ctx.Done():
	case err = <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	logger.Info("pyre-hub stopped")
	return err
}

// shutdownGrace is how long in-flight requests may still finish after a
// stop before their contexts are cancelled.
const shutdownGrace = time.Second

// writeRuntimeStats rewrites path with the process's runtime counters now
// and then every interval until ctx ends (--dev-runtime-stats).
func writeRuntimeStats(ctx context.Context, path string, interval time.Duration, logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := diagnostics.WriteRuntimeStats(path); err != nil {
			logger.Warn("runtime stats not written", "path", path, "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// derivePreviewURL combines the advertised hub URL's host with the preview
// listener's port ("" if either is unusable; links then fall back to the
// hub URL, which does not serve previews when a preview listener is set, so
// pass --preview-url explicitly in that case).
func derivePreviewURL(hubURL, previewListen string) string {
	u, err := url.Parse(hubURL)
	if err != nil || u.Host == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(previewListen)
	if err != nil || port == "" || port == "0" {
		return ""
	}
	return "https://" + net.JoinHostPort(u.Hostname(), port)
}

// passphraseEnv supplies the backup passphrase when --passphrase-file is
// not given.
const passphraseEnv = "PYRE_BACKUP_PASSPHRASE"

// scryptLogN overrides age's scrypt work factor for new passphrase backups
// (0: age's default). Tests lower it; there is no flag.
var scryptLogN = 0

// readPassphrase returns the passphrase from file (trailing newline
// stripped) or the environment; nil when neither is set.
func readPassphrase(file string) ([]byte, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read passphrase file: %w", err)
		}
		b = []byte(strings.TrimRight(string(b), "\r\n"))
		if len(b) == 0 {
			return nil, fmt.Errorf("passphrase file %s is empty", file)
		}
		return b, nil
	}
	if v := os.Getenv(passphraseEnv); v != "" {
		return []byte(v), nil
	}
	return nil, nil
}

func cmdBackup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("backup", stderr)
	data := fs.String("data", "", "hub data directory")
	out := fs.String("out", "", "archive to write (must not exist), or - for stdout")
	passFile := fs.String("passphrase-file", "", "encrypt with this passphrase (age scrypt; default: $"+passphraseEnv+")")
	var recips, recipFiles multiFlag
	fs.Var(&recips, "recipient", "encrypt to this age public key: age1… (X25519), age1pq1… (post-quantum hybrid) or an ssh-ed25519/ssh-rsa line (repeatable)")
	fs.Var(&recipFiles, "recipients-file", "file with one recipient per line, # comments (repeatable)")
	plain := fs.Bool("unencrypted", false, "write a plaintext archive (it contains the hub TLS key; only when encrypting it some other way)")
	verify := fs.Bool("verify", false, "after writing, decrypt the archive as a stream and check every digest (file output only)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *data == "" || *out == "" || fs.NArg() > 0 {
		return usageError{"--data and --out are required"}
	}
	pass, err := readPassphrase(*passFile)
	if err != nil {
		return err
	}
	opts := backup.Options{ScryptLogN: scryptLogN, Unencrypted: *plain, Verify: *verify}
	for _, r := range recips {
		rc, err := backup.ParseRecipient(r)
		if err != nil {
			return usageError{"--recipient: " + err.Error()}
		}
		opts.Recipients = append(opts.Recipients, rc)
	}
	for _, f := range recipFiles {
		rs, err := backup.ReadRecipientsFile(f)
		if err != nil {
			return fmt.Errorf("--recipients-file: %w", err)
		}
		opts.Recipients = append(opts.Recipients, rs...)
	}
	haveRecips := len(opts.Recipients) > 0
	switch {
	case *plain && (pass != nil || haveRecips):
		return usageError{"--unencrypted conflicts with a passphrase (--passphrase-file or $" + passphraseEnv + ") or recipients"}
	case pass != nil && haveRecips:
		src := "--passphrase-file"
		if *passFile == "" {
			src = "$" + passphraseEnv
		}
		return usageError{"give either a passphrase (" + src + ") or recipients, not both: an age passphrase must be the only recipient"}
	case !*plain && pass == nil && !haveRecips:
		return usageError{"backups are encrypted: give --recipient/--recipients-file, or --passphrase-file FILE / $" + passphraseEnv + " (or --unencrypted to opt out explicitly)"}
	case *verify && *out == "-":
		return usageError{"--verify needs a file --out (a stream cannot be read back)"}
	}
	opts.Passphrase = pass
	if *plain {
		fmt.Fprintln(stderr, "warning: writing an UNENCRYPTED backup containing the hub TLS private key and private data; encrypt it before storing it")
	}
	var m backup.Manifest
	report := stdout
	if *out == "-" {
		// The archive owns stdout; the summary goes to stderr.
		report = stderr
		bw := bufio.NewWriterSize(os.Stdout, 256<<10)
		if m, err = backup.CreateTo(ctx, *data, bw, opts); err == nil {
			err = bw.Flush()
		}
	} else {
		m, err = backup.Create(ctx, *data, *out, opts)
	}
	if err != nil {
		return err
	}
	var total int64
	for _, e := range m.Entries {
		total += e.Size
	}
	how := "encrypted: " + opts.Mode()
	if *plain {
		how = "UNENCRYPTED; contains the hub TLS key and private data"
	}
	fmt.Fprintf(report, "wrote %s: hub %s, %d files, %d bytes (%s)\n", *out, m.HubID, len(m.Entries), total, how)
	if *verify {
		fmt.Fprintln(report, "verified: the archive decrypts and every digest matches the manifest")
	}
	return nil
}

func cmdRestore(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("restore", stderr)
	in := fs.String("in", "", "archive written by pyre-hub backup, or - for stdin")
	data := fs.String("data", "", "new hub data directory (must not exist or be empty)")
	passFile := fs.String("passphrase-file", "", "passphrase of a passphrase-encrypted archive (default: $"+passphraseEnv+")")
	var idFiles multiFlag
	fs.Var(&idFiles, "identity", "age identity file (AGE-SECRET-KEY-…, or an unencrypted OpenSSH private key) for a recipient-encrypted archive (repeatable)")
	verifyOnly := fs.Bool("verify-only", false, "decrypt and check every digest without writing anything (no --data)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *in == "" || (*data == "") != *verifyOnly || fs.NArg() > 0 {
		return usageError{"--in and --data (or --verify-only) are required"}
	}
	pass, err := readPassphrase(*passFile)
	if err != nil {
		return err
	}
	opts := backup.Options{Passphrase: pass}
	for _, f := range idFiles {
		ids, err := backup.ReadIdentityFile(f)
		if err != nil {
			return fmt.Errorf("--identity: %w", err)
		}
		opts.Identities = append(opts.Identities, ids...)
	}
	var src io.Reader = os.Stdin
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			return err
		}
		defer f.Close()
		src = f
	}
	var m backup.Manifest
	var format backup.Format
	if *verifyOnly {
		m, format, err = backup.VerifyFrom(ctx, src, opts)
	} else {
		m, format, err = backup.RestoreFrom(ctx, src, *data, opts)
	}
	switch {
	case errors.Is(err, backup.ErrPassphraseRequired):
		return fmt.Errorf("%w: give --passphrase-file FILE or set $%s", err, passphraseEnv)
	case errors.Is(err, backup.ErrKeyRequired):
		return fmt.Errorf("%w: give --identity FILE, or --passphrase-file FILE / $%s", err, passphraseEnv)
	case err != nil:
		return err
	}
	if *verifyOnly {
		fmt.Fprintf(stdout, "verified %s (%s): hub %s, %d files, backup of %s; all digests match\n", *in, format, m.HubID, len(m.Entries), m.CreatedAt.Format(time.RFC3339))
		return nil
	}
	fmt.Fprintf(stdout, "restored hub %s (%d files, backup of %s, %s) into %s; all digests verified\n", m.HubID, len(m.Entries), m.CreatedAt.Format(time.RFC3339), format, *data)
	return nil
}
