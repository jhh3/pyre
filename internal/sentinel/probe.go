package sentinel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/transport"
)

// SSHProber is the real Prober and Recoverer: ssh (BatchMode, no prompts),
// HTTPS GETs and TCP dials. Nothing it runs on a target changes state there
// except the push of this sentinel's own state file into
// ~/.local/state/pyre/sentinel/peers/ and, for Recover, an ssh login.
type SSHProber struct {
	Self string
	Net  NetworkConfig
	// SSHBin defaults to "ssh".
	SSHBin string
	// Identity is passed as ssh -i when set.
	Identity string

	mu      sync.Mutex
	clients map[string]*http.Client
}

const (
	sshTimeout     = 45 * time.Second
	hubTimeout     = 8 * time.Second
	publicTimeout  = 15 * time.Second
	dialTimeout    = 3 * time.Second
	recoverTimeout = 60 * time.Second
)

func (p *SSHProber) sshArgs(alias string) []string {
	args := []string{
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3",
	}
	if p.Identity != "" {
		args = append(args, "-i", p.Identity)
	}
	return append(args, "-T", alias, "/bin/sh", "-s")
}

func (p *SSHProber) ssh(ctx context.Context, alias, script string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bin := p.SSHBin
	if bin == "" {
		bin = "ssh"
	}
	cmd := exec.CommandContext(ctx, bin, p.sshArgs(alias)...)
	cmd.Stdin = strings.NewReader(script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitWriter{w: &out, n: maxStateBytes * 2}, &limitWriter{w: &errb, n: 64 << 10}
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil && err != nil {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return out.String(), errb.String(), err
}

// classifySSH maps an ssh failure to unreachable / auth / error.
func classifySSH(stderr string, err error) (kind, detail string) {
	detail = lastLine(stderr)
	if i := strings.LastIndex(detail, ": "); i >= 0 && strings.HasPrefix(detail, "ssh: ") {
		detail = detail[i+2:]
	}
	if detail == "" && err != nil {
		detail = err.Error()
	}
	low := strings.ToLower(stderr + " " + detail)
	switch {
	case strings.Contains(low, "permission denied"), strings.Contains(low, "host key verification failed"),
		strings.Contains(low, "remote host identification has changed"), strings.Contains(low, "too many authentication failures"):
		return SSHAuth, detail
	case strings.Contains(low, "timed out"), strings.Contains(low, "connection refused"), strings.Contains(low, "no route to host"),
		strings.Contains(low, "network is unreachable"), strings.Contains(low, "could not resolve"),
		strings.Contains(low, "connection closed"), strings.Contains(low, "connection reset"), strings.Contains(low, "host is down"),
		strings.Contains(low, "broken pipe"), strings.Contains(low, "timeout"):
		return SSHUnreachable, detail
	}
	return SSHError, detail
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// probeScript is the POSIX sh script one probe runs on the target. Every
// interpolated word was validated against safeWord; push is single-line
// JSON fed through a heredoc with a random terminator.
func probeScript(t Target, self string, push []byte) string {
	var b strings.Builder
	// to: every systemctl call is bounded (a frozen manager or D-Bus must not
	// run into the ssh timeout and read as "host off"); exit 124 = timeout.
	b.WriteString(`to() { if command -v timeout >/dev/null 2>&1; then timeout 5 "$@"; else "$@"; fi; }
os=$(uname -s)
echo "@@os $os"
echo "@@now $(date +%s)"
if [ "$os" = Linux ]; then
  echo "@@manager"; out=$(to systemctl --user is-system-running 2>&1); [ $? -eq 124 ] && out=timeout; echo "$out" | head -n 3
  echo "@@managerstart"; to systemctl --user show --timestamp=unix --value -p UserspaceTimestamp 2>/dev/null | head -n 1
fi
echo "@@units"
`)
	for _, u := range t.Units {
		fmt.Fprintf(&b, "if [ \"$os\" = Linux ]; then s=$(to systemctl --user is-active '%s' 2>/dev/null); [ $? -eq 124 ] && s=timeout; s=$(echo \"$s\" | head -n 1); elif launchctl list '%s' >/dev/null 2>&1; then s=active; else s=inactive; fi; echo \"%s ${s:-unknown}\"\n", u, u, u)
	}
	b.WriteString("echo \"@@tcp\"\n")
	for _, hp := range t.TCP {
		i := strings.LastIndex(hp, ":")
		h, port := strings.Trim(hp[:i], "[]"), hp[i+1:]
		fmt.Fprintf(&b, "if nc -z -w 3 '%s' '%s' >/dev/null 2>&1; then echo '%s open'; elif command -v bash >/dev/null 2>&1 && bash -c 'exec 3<>/dev/tcp/%s/%s' >/dev/null 2>&1; then echo '%s open'; else echo '%s closed'; fi\n", h, port, hp, h, port, hp, hp)
	}
	fmt.Fprintf(&b, "echo \"@@maintenance\"; cat \"$HOME/%s\" 2>/dev/null; echo\n", remoteMaintenanceFile)
	fmt.Fprintf(&b, "echo \"@@sentinel\"; cat \"$HOME/%s\" 2>/dev/null; echo\n", remoteStateFile)
	if len(push) > 0 && !bytes.ContainsAny(push, "\n\r") && safeWord.MatchString(self) {
		var r [8]byte
		_, _ = rand.Read(r[:])
		eof := "PYRE_SENTINEL_" + hex.EncodeToString(r[:])
		fmt.Fprintf(&b, "if [ -d \"$HOME/.local/state/pyre/sentinel\" ]; then (umask 077; d=\"$HOME/%s\"; mkdir -p \"$d\" && cat > \"$d/.%s.json.tmp\" <<'%s' && mv -f \"$d/.%s.json.tmp\" \"$d/%s.json\"\n%s\n%s\n); fi\n",
			remotePeersDir, self, eof, self, self, push, eof)
	}
	b.WriteString("echo \"@@end\"\n")
	return b.String()
}

// parsed is the target-side part of an observation.
type parsed struct {
	os, managerRaw, managerStart string
	remoteNow                    time.Time
	unitsDown, tcpDown           []string
	maintenance                  []byte
	sentinel                     []byte
	complete                     bool
}

func parseProbe(out string) parsed {
	var p parsed
	section := ""
	var maint, sent bytes.Buffer
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), maxStateBytes+1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "@@") {
			f := strings.Fields(strings.TrimPrefix(line, "@@"))
			if len(f) == 0 {
				continue
			}
			section = f[0]
			switch section {
			case "os":
				if len(f) > 1 {
					p.os = f[1]
				}
			case "now":
				if len(f) > 1 {
					p.remoteNow = parseUnixStamp(f[1])
				}
			case "end":
				p.complete = true
			}
			continue
		}
		switch section {
		case "manager":
			if p.managerRaw == "" {
				p.managerRaw = strings.TrimSpace(line)
			}
		case "managerstart":
			if p.managerStart == "" {
				p.managerStart = strings.TrimSpace(line)
			}
		case "units":
			if f := strings.Fields(line); len(f) >= 2 && f[1] != "active" && f[1] != "reloading" {
				p.unitsDown = append(p.unitsDown, f[0]+" ("+f[1]+")")
			}
		case "tcp":
			if f := strings.Fields(line); len(f) >= 2 && f[1] != "open" {
				p.tcpDown = append(p.tcpDown, f[0])
			}
		case "maintenance":
			maint.WriteString(line)
		case "sentinel":
			sent.WriteString(line)
		}
	}
	p.maintenance = bytes.TrimSpace(maint.Bytes())
	p.sentinel = bytes.TrimSpace(sent.Bytes())
	return p
}

// managerState maps is-system-running output to running / absent.
func managerState(os, raw string) string {
	if os != "Linux" {
		return ManagerNA
	}
	switch raw {
	case "running", "degraded", "starting", "initializing", "maintenance", "stopping":
		return ManagerRunning
	case "timeout":
		return ManagerTimeout
	}
	low := strings.ToLower(raw)
	if raw == "offline" || strings.Contains(low, "failed to connect to bus") || strings.Contains(low, "no medium") ||
		strings.Contains(low, "no such file") || strings.Contains(low, "xdg_runtime_dir") {
		return ManagerAbsent
	}
	return raw
}

// Probe implements Prober.
func (p *SSHProber) Probe(ctx context.Context, t Target, push []byte) Observation {
	o := Observation{CheckedAt: time.Now()}
	var wg sync.WaitGroup
	o.Hub = make([]Check, len(t.Hub))
	for i, e := range t.Hub {
		wg.Go(func() { o.Hub[i] = p.hub(ctx, t.HubPin, e) })
	}
	o.Public = make([]Check, len(t.Public))
	for i, e := range t.Public {
		wg.Go(func() { o.Public[i] = p.public(ctx, e) })
	}
	script := probeScript(t, p.Self, push)
	o.SSH = SSHError
	for _, alias := range t.SSH {
		out, stderr, err := p.ssh(ctx, alias, script, sshTimeout)
		pr := parseProbe(out)
		if !pr.complete && pr.os == "" {
			o.SSH, o.SSHDetail = classifySSH(stderr, err)
			o.SSHAlias = alias
			continue
		}
		o.SSH, o.SSHAlias, o.SSHDetail = SSHOK, alias, ""
		if !pr.complete {
			// It printed @@os: the host answered, then something on it hung.
			o.SSH, o.SSHDetail = SSHHung, "connected, probe did not finish"
		}
		o.OS, o.RemoteNow = pr.os, pr.remoteNow
		o.ManagerRaw = pr.managerRaw
		o.Manager = managerState(pr.os, pr.managerRaw)
		if o.Manager == ManagerRunning {
			o.ManagerStart = pr.managerStart
		}
		o.UnitsDown, o.TCPDown = pr.unitsDown, pr.tcpDown
		if w, err := parseWindow(pr.maintenance); err == nil {
			o.Maintenance = w
		}
		o.PeerState = pr.sentinel
		break
	}
	wg.Wait()
	if len(o.Hub) == 0 {
		o.Hub = nil
	}
	if len(o.Public) == 0 {
		o.Public = nil
	}
	return o
}

// CheckPublic implements PublicChecker: this host's own check of its
// public URLs, the same GET a watcher makes.
func (p *SSHProber) CheckPublic(ctx context.Context, eps []Endpoint) []Check {
	out := make([]Check, len(eps))
	var wg sync.WaitGroup
	for i, e := range eps {
		wg.Go(func() { out[i] = p.public(ctx, e) })
	}
	wg.Wait()
	return out
}

func (p *SSHProber) client(pin string) *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*http.Client{}
	}
	if c := p.clients[pin]; c != nil {
		return c
	}
	var cfg *tls.Config
	switch pin {
	case "public":
		cfg = transport.CATLSConfig(nil)
	case "":
		// /healthz is unauthenticated and carries no secret; without a pin
		// the self-signed hub certificate cannot be verified.
		cfg = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec // healthz only
	default:
		var err error
		if cfg, err = transport.PinnedTLSConfig(pin); err != nil {
			// Normalize rejects a malformed pin; never fall back to no check.
			cfg = &tls.Config{MinVersion: tls.VersionTLS12, VerifyConnection: func(tls.ConnectionState) error { return err }, InsecureSkipVerify: true} //nolint:gosec // refuses every connection
		}
	}
	c := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	p.clients[pin] = c
	return c
}

func (p *SSHProber) get(ctx context.Context, c *http.Client, url string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pyre-sentinel")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp, nil
}

func (p *SSHProber) hub(ctx context.Context, pin string, e Endpoint) Check {
	c := Check{Label: e.name(), URL: e.URL}
	resp, err := p.get(ctx, p.client(pin), strings.TrimRight(e.URL, "/")+"/healthz", hubTimeout)
	switch {
	case err != nil:
		c.Detail = shortErr(err)
	case resp.StatusCode != http.StatusOK:
		c.Detail = "HTTP " + strconv.Itoa(resp.StatusCode)
	default:
		c.OK = true
	}
	return c
}

func (p *SSHProber) public(ctx context.Context, e Endpoint) Check {
	c := Check{Label: e.name(), URL: e.URL}
	resp, err := p.get(ctx, p.client("public"), e.URL, publicTimeout)
	switch {
	case err != nil:
		c.Detail = shortErr(err)
	case resp.Header.Get("Ngrok-Error-Code") != "":
		// ngrok answers 404 ERR_NGROK_3200 for an offline endpoint.
		c.Detail = "ngrok " + resp.Header.Get("Ngrok-Error-Code")
	case resp.StatusCode >= 500:
		c.Detail = "HTTP " + strconv.Itoa(resp.StatusCode)
	default:
		c.OK = true
	}
	return c
}

func shortErr(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// Network implements Prober: one public address, else the default gateway
// (a TCP dial that connects or is refused proves the path). Public first: a
// gateway dial is local-network access, which macOS gates per app, so a
// healthy uplink needs none.
func (p *SSHProber) Network(ctx context.Context) (bool, string) {
	var tried []string
	for _, a := range p.Net.Public {
		if reachable(ctx, a) {
			return true, a
		}
		tried = append(tried, a)
	}
	if !p.Net.NoGateway {
		if gw := defaultGateway(); gw != "" {
			for _, port := range []string{"53", "80", "443"} {
				if reachable(ctx, net.JoinHostPort(gw, port)) {
					return true, "gateway " + gw
				}
			}
			tried = append(tried, "gateway "+gw)
		} else {
			tried = append(tried, "no default gateway")
		}
	}
	return false, "unreachable: " + strings.Join(tried, ", ")
}

func reachable(ctx context.Context, addr string) bool {
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err == nil {
		c.Close()
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

func defaultGateway() string {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return ""
		}
		return gatewayFromProcRoute(string(b))
	}
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
		if err != nil {
			return ""
		}
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "gateway:" {
				return f[1]
			}
		}
	}
	return ""
}

// gatewayFromProcRoute reads the default route's gateway from
// /proc/net/route (little-endian hex).
func gatewayFromProcRoute(s string) string {
	for _, l := range strings.Split(s, "\n")[1:] {
		f := strings.Fields(l)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String()
	}
	return ""
}

// recoverScript starts the user's default target. The ssh login itself
// (pam_systemd) starts user@UID.service; nothing is stopped or killed and
// nothing runs as root.
const recoverScript = "systemctl --user start default.target 2>&1 | tail -n 2; echo \"@@state $(systemctl --user is-system-running 2>&1 | head -n 1)\"\n"

// Recover implements Recoverer.
func (p *SSHProber) Recover(ctx context.Context, t Target) (string, error) {
	var last error
	for _, alias := range t.SSH {
		out, stderr, err := p.ssh(ctx, alias, recoverScript, recoverTimeout)
		if i := strings.Index(out, "@@state "); i >= 0 {
			return "user manager " + strings.TrimSpace(out[i+len("@@state "):]), nil
		}
		_, d := classifySSH(stderr, err)
		last = fmt.Errorf("ssh %s: %s", alias, d)
	}
	if last == nil {
		last = errors.New("no ssh alias")
	}
	return "", last
}

// limitWriter drops output beyond n bytes.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(b []byte) (int, error) {
	if l.n <= 0 {
		return len(b), nil
	}
	k := min(len(b), l.n)
	l.n -= k
	if _, err := l.w.Write(b[:k]); err != nil {
		return 0, err
	}
	return len(b), nil
}
