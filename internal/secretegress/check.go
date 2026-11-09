package secretegress

import (
	"net"
	"regexp"
	"strings"
)

// Severities.
const (
	SeverityHigh = "high" // audit, urgent inbox item and phone push
	SeverityLow  = "low"  // audit only
)

// Rules.
const (
	RuleDecoyValue     = "decoy-value"      // a decoy's value appears in a tool call
	RuleDecoyNetwork   = "decoy+network"    // a decoy variable is sent over the network
	RuleDecoyRead      = "decoy-read"       // a decoy variable is read, with no network call
	RuleSecretNetwork  = "secret+network"   // a secret goes to a host that is not its provider
	RuleEnvDumpNetwork = "env-dump+network" // the whole environment goes over the network
)

// Finding is what Check found in one command.
type Finding struct {
	Rule     string
	Severity string
	Secrets  []string // what was referenced, never a value: "$GH_TOKEN", "gh auth token", "ghp_ literal"
	Sinks    []string // "curl", "python (urllib)", ...
	Hosts    []string // destinations named in the command
	Encoded  bool     // base64/xxd/gzip/... in the same command
}

// Reason is a one-line description for the owner.
func (f *Finding) Reason() string {
	var b strings.Builder
	switch f.Rule {
	case RuleDecoyValue:
		b.WriteString("a planted decoy credential's value appears in a tool call")
	case RuleDecoyNetwork:
		b.WriteString("a planted decoy credential is sent over the network")
	case RuleDecoyRead:
		b.WriteString("a planted decoy credential was read")
	case RuleEnvDumpNetwork:
		b.WriteString("the environment (with its secrets) is sent over the network")
	default:
		b.WriteString("a secret is sent over the network")
	}
	if len(f.Secrets) > 0 {
		b.WriteString(": " + strings.Join(f.Secrets, ", "))
	}
	if len(f.Sinks) > 0 {
		b.WriteString(" via " + strings.Join(f.Sinks, ", "))
	}
	if len(f.Hosts) > 0 {
		b.WriteString(" to " + strings.Join(f.Hosts, ", "))
	}
	if f.Encoded {
		b.WriteString(" (encoded)")
	}
	return b.String()
}

// secretRef is one secret source in a command.
type secretRef struct {
	label  string   // shown to the owner
	family string   // provider family: "github", "anthropic", ... ("" = none; "local" = this machine only; "db" = a database URL)
	alts   []string // more providers the name names (VORTEX_QA_PAID_ACME_API_KEY: acme)
	decoy  bool
	dump   bool   // the whole environment
	kind   string // refVar, refLiteral or "" (a command, file or dump that runs where the text runs)
}

const (
	refVar     = "var"     // $NAME and named access from code
	refLiteral = "literal" // a token typed into the command
)

// remoteReads drops the sources an ssh remote command reads on the other
// machine (ssh host 'doppler secrets get X', ssh mac "cat ~/.config/gh/hosts.yml"):
// they run there and their output comes back, nothing leaves this one.
// Only for a call whose one sink is ssh (a pipe into curl still counts).
// Literals are sent either way; a $VAR counts unless single-quoted (else
// this shell expands it); a double-quoted word holding $( or a backtick
// runs here.
func remoteReads(scan string, d Decoys, refs []secretRef) []secretRef {
	// The quoted remote words exactly as written ('...' or "..."), so only
	// that text is dropped; an unquoted remote command keeps every source.
	var single, all []string
	for _, c := range parse(scan, 0) {
		name, rest := c.name()
		if name != "ssh" {
			continue
		}
		for _, w := range sshRemoteWords(rest) {
			if w == "" {
				continue
			}
			if q := "'" + w + "'"; strings.Contains(scan, q) {
				single = append(single, q)
				all = append(all, q)
			} else if q := `"` + w + `"`; !strings.Contains(w, "$(") && !strings.Contains(w, "`") && strings.Contains(scan, q) {
				all = append(all, q)
			}
		}
	}
	if len(all) == 0 {
		return refs
	}
	strip := func(qs []string) string {
		s := scan
		for _, q := range qs {
			s = strings.ReplaceAll(s, q, " ")
		}
		return s
	}
	var out []secretRef
	for _, r := range refs {
		if r.kind == refLiteral {
			out = append(out, r)
		}
	}
	for _, r := range findSecrets(strip(single), d) {
		if r.kind == refVar {
			out = append(out, r)
		}
	}
	for _, r := range findSecrets(strip(all), d) {
		if r.kind != refVar && r.kind != refLiteral {
			out = append(out, r)
		}
	}
	return out
}

// sshArgFlags are the ssh options that take a value.
var sshArgFlags = map[string]bool{
	"-B": true, "-b": true, "-c": true, "-D": true, "-E": true, "-e": true, "-F": true, "-I": true, "-i": true, "-J": true,
	"-L": true, "-l": true, "-m": true, "-O": true, "-o": true, "-P": true, "-p": true, "-Q": true, "-R": true, "-S": true, "-W": true, "-w": true,
}

// sshRemoteWords: the words after ssh's destination (the remote command).
func sshRemoteWords(rest []string) []string {
	for i := 0; i < len(rest); i++ {
		w := rest[i]
		if strings.HasPrefix(w, "-") {
			if sshArgFlags[w] {
				i++
			}
			continue
		}
		return rest[i+1:]
	}
	return nil
}

// Check inspects one shell command (a Bash/PowerShell tool call or a Codex
// commandExecution). It returns nil when there is nothing to flag. It never
// does I/O.
func Check(cmd string, d Decoys) *Finding {
	if cmd == "" {
		return nil
	}
	if v := d.valueIn(cmd); v {
		f := &Finding{Rule: RuleDecoyValue, Severity: SeverityHigh, Secrets: []string{"decoy value"}}
		sinks, hosts, _ := findSinks(cmd, nil)
		f.Sinks, f.Hosts = sinks, hosts
		return f
	}
	scan := withoutInertText(cmd)
	fl := newFlow(scan, d)
	sinks, hosts, _ := findSinks(cmd, fl.lits)
	decoyNames := d.namesIn(scan)
	if len(sinks) == 0 {
		if len(decoyNames) > 0 {
			return &Finding{Rule: RuleDecoyRead, Severity: SeverityLow, Secrets: dollar(decoyNames)}
		}
		return nil
	}
	refs := findSecrets(scan, d)
	if len(sinks) == 1 && sinks[0] == "ssh" {
		refs = remoteReads(scan, d, refs)
	}
	// A planted decoy has no legitimate use: any network call in the same
	// tool call is enough.
	if anyRef(refs, func(r secretRef) bool { return r.decoy }) {
		f := &Finding{Rule: RuleDecoyNetwork, Severity: SeverityHigh, Sinks: sinks, Hosts: hosts, Encoded: encodeRe.MatchString(scan)}
		f.addSecrets(refs)
		return f
	}
	// Otherwise a secret counts only where it flows into a network call:
	// the same statement or pipeline, or a variable, file or exported
	// variable an earlier statement filled from it.
	var f *Finding
	for _, st := range statements(scan) {
		ssinks, shosts, loopback := findSinks(st, fl.lits)
		srefs := fl.refsIn(st)
		if len(ssinks) == 1 && ssinks[0] == "ssh" {
			srefs = remoteReads(st, d, srefs)
		}
		srefs = append(srefs, fl.envReaders(ssinks)...)
		fl.record(st, srefs)
		if len(ssinks) == 0 || len(srefs) == 0 {
			continue
		}
		rule := RuleSecretNetwork
		switch {
		case anyRef(srefs, func(r secretRef) bool { return r.dump }):
			rule = RuleEnvDumpNetwork
		case fl.ghContent(st, srefs):
		default:
			// A secret that goes only to its own provider, this machine or a
			// placeholder host is ordinary use: not an alert.
			if rest := fl.dbRefsOK(st, srefs, ssinks); len(rest) == 0 || providerOK(rest, shosts, loopback) {
				continue
			}
		}
		if f == nil {
			f = &Finding{Severity: SeverityHigh, Rule: rule, Encoded: encodeRe.MatchString(scan)}
		} else if rule == RuleEnvDumpNetwork {
			f.Rule = rule
		}
		f.addSecrets(srefs)
		f.Sinks = appendNew(f.Sinks, ssinks...)
		f.Hosts = appendNew(f.Hosts, shosts...)
	}
	return f
}

func (f *Finding) addSecrets(refs []secretRef) {
	for _, r := range refs {
		f.Secrets = appendNew(f.Secrets, r.label)
	}
}

func appendNew(xs []string, add ...string) []string {
	for _, a := range add {
		if !hasWord(xs, a) {
			xs = append(xs, a)
		}
	}
	return xs
}

// CheckValues reports whether a decoy's value appears in any tool input
// (any tool, not only shells: an MCP call or a file write).
func CheckValues(text string, d Decoys) *Finding {
	if d.valueIn(text) {
		return &Finding{Rule: RuleDecoyValue, Severity: SeverityHigh, Secrets: []string{"decoy value"}}
	}
	return nil
}

// withoutInertText drops the bodies of quoted heredocs (<<'EOF') that feed
// a program other than a shell or interpreter: a commit message or PR body
// that mentions $GH_TOKEN is text, not a read of the variable.
func withoutInertText(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	var walk func(cmds []command)
	var inert []string
	walk = func(cmds []command) {
		for _, c := range cmds {
			name, _ := c.name()
			if shells[name] || interpreters[name] {
				continue
			}
			inert = append(inert, c.quotedDocs...)
		}
	}
	walk(parse(cmd, 0))
	for _, b := range inert {
		if b != "" {
			cmd = strings.Replace(cmd, b, "", 1)
		}
	}
	return cmd
}

func anyRef(refs []secretRef, f func(secretRef) bool) bool {
	for _, r := range refs {
		if f(r) {
			return true
		}
	}
	return false
}

func dollar(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "$" + n
	}
	return out
}

// --- sinks ---

// netPrograms send data over the network by themselves.
var netPrograms = map[string]bool{
	"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true, "socat": true, "telnet": true,
	"ssh": true, "scp": true, "sftp": true, "ftp": true, "tftp": true, "rsync": true, "http": true, "https": true,
	"xh": true, "xhs": true, "httpie": true, "aria2c": true, "lftp": true, "smbclient": true,
	"invoke-webrequest": true, "invoke-restmethod": true, "iwr": true, "irm": true, "bitsadmin": true,
	"start-bitstransfer": true, "send-mailmessage": true, "mail": true, "sendmail": true, "mutt": true,
}

// interpreters send data only when their code uses a network library.
var interpreters = map[string]bool{
	"python": true, "python2": true, "python3": true, "pypy": true, "pypy3": true, "uv": true, "uvx": true,
	"node": true, "nodejs": true, "deno": true, "bun": true, "ruby": true, "perl": true, "php": true,
	"pwsh": true, "powershell": true, "osascript": true, "lua": true, "tclsh": true,
}

// shells run their string arguments as commands.
var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "eval": true,
	"su": true, "pwsh": true, "powershell": true, "cmd": true, "watch": true,
}

var netLibRe = regexp.MustCompile(`(?i)urllib|\brequests\b|http\.client|httpx|aiohttp|\bsocket\b|websocket|\bfetch\s*\(|https?\.(?:request|get)\b|net\.(?:connect|createconnection)|xmlhttprequest|axios|got\(|net::http|open-uri|io::socket|lwp::|http::tiny|file_get_contents\s*\(\s*['"]https?:|curl_init|webclient|invoke-webrequest|invoke-restmethod|system\.net\.|tcpclient`)

var (
	urlRe      = regexp.MustCompile(`(?i)\b(?:https?|wss?|ftps?|sftp|scp|ssh|git|smtp)://(?:[^/\s@"'<>]*@)?(\[[0-9a-f:.]+\]|[^/\s:"'?#<>@]+)`)
	scpHostRe  = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?([A-Za-z0-9][A-Za-z0-9.-]*):`)
	devTCPRe   = regexp.MustCompile(`/dev/(?:tcp|udp)/([^/\s"']+)/`)
	encodeRe   = regexp.MustCompile(`(?i)(?:^|[\s|;&(])(?:base64|base32|basenc|xxd|od|hexdump|gzip|bzip2|xz|zstd|openssl\s+(?:enc|base64)|uuencode)\b|b64encode|btoa\s*\(|tobase64string|bin2hex|\.hex\(\)`)
	shellArgRe = regexp.MustCompile(`[\s|;&$]`)
)

// findSinks returns the network sinks of a command, the hosts they name
// and whether a sink talks only to this machine (a unix socket).
func findSinks(cmd string, lits map[string][]string) (sinks, hosts []string, loopback bool) {
	storeSinks := 0
	add := func(s string) {
		for _, x := range sinks {
			if x == s {
				return
			}
		}
		sinks = append(sinks, s)
	}
	addHost := func(h string) {
		h = strings.ToLower(strings.Trim(h, "[].,`"))
		if h == "" {
			return
		}
		for _, x := range hosts {
			if x == h {
				return
			}
		}
		hosts = append(hosts, h)
	}
	var walk func(cmds []command, depth int)
	walk = func(cmds []command, depth int) {
		for _, c := range cmds {
			name, rest := c.name()
			if name == "" {
				continue
			}
			sink := ""
			urls := false
			switch {
			case netPrograms[name]:
				sink = name
				urls = true
			case name == "openssl" && hasWord(rest, "s_client"):
				sink = "openssl s_client"
				urls = true
			case name == "git":
				if u := gitURLs(rest); len(u) > 0 {
					sink = "git " + firstNonFlag(rest)
					for _, h := range u {
						addHost(h)
					}
				}
			case name == "gh":
				sink, urls = ghSink(rest)
				switch sink {
				case "":
				case "gh gist":
				case "gh secret set", "gh variable set":
					// Storing a secret in GitHub Actions is what the command
					// is for; it goes to GitHub's encrypted secret store.
					storeSinks++
				default:
					addHost("github.com")
				}
			case name == "docker" && (hasWord(rest, "login") || hasWord(rest, "push")):
				sink = "docker " + firstNonFlag(rest)
				urls = true
				if h := dockerLoginHost(rest); h != "" {
					addHost(h) // echo $GITHUB_TOKEN | docker login ghcr.io --password-stdin
				}
			case interpreters[name]:
				text := inlineCode(rest) + "\n" + strings.Join(c.heredocs, "\n")
				if m := netLib(name, text); m != "" {
					sink = name + " (" + m + ")"
					urls = true
					for _, h := range c.heredocs {
						for _, m := range urlRe.FindAllStringSubmatch(h, -1) {
							addHost(m[1])
						}
					}
				}
			}
			if sink != "" {
				add(sink)
				if urls && unixSocket(rest) {
					// The URL's host is a placeholder: the request goes to a
					// local socket (curl --unix-socket "$PYRE_SOCKET" http://pyre/...).
					loopback = true
				} else if urls {
					for i := 0; i < len(rest); i++ {
						w := rest[i]
						if netPrograms[name] {
							// A header, form field or body is not where the
							// request goes (-H "Origin: https://x", -d '{"url":...}').
							if f, _, eq := strings.Cut(w, "="); valueFlags[f] {
								if !eq {
									i++
								}
								continue
							}
						}
						for _, m := range urlRe.FindAllStringSubmatch(hostWord(w, lits), -1) {
							addHost(m[1])
						}
					}
					if name == "scp" || name == "rsync" || name == "sftp" {
						for _, w := range rest {
							if m := scpHostRe.FindStringSubmatch(w); m != nil && !strings.HasPrefix(w, "-") {
								addHost(m[1])
							}
						}
					}
				}
			}
			// Commands run by a shell (bash -c "...", ssh host "...", eval)
			// are parsed as their own commands.
			if (shells[name] || name == "ssh") && depth < maxDepth {
				for _, w := range rest {
					if shellArgRe.MatchString(w) {
						walk(parse(w, depth+1), depth+1)
					}
				}
				if shells[name] {
					for _, h := range c.heredocs {
						walk(parse(h, depth+1), depth+1)
					}
				}
			}
		}
	}
	walk(parse(cmd, 0), 0)
	for _, m := range devTCPRe.FindAllStringSubmatch(cmd, -1) {
		add("/dev/tcp")
		addHost(m[1])
	}
	if storeSinks > 0 && len(hosts) == 0 {
		only := true
		for _, x := range sinks {
			only = only && (x == "gh secret set" || x == "gh variable set")
		}
		loopback = loopback || only // only secret stores: as safe as this machine
	}
	return sinks, hosts, loopback
}

// valueFlags: options of curl, wget and httpie-style programs whose value
// is request content (headers, data, forms, output), not a destination.
var valueFlags = map[string]bool{
	"-d": true, "--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true, "--data-ascii": true,
	"--json": true, "-H": true, "--header": true, "-F": true, "--form": true, "--form-string": true, "-e": true,
	"--referer": true, "-A": true, "--user-agent": true, "-b": true, "--cookie": true, "-o": true, "--output": true,
	"-w": true, "--write-out": true, "--post-data": true, "--body-data": true, "-O": true, "--output-document": true,
}

// hostWord prepares a word for host matching: a $(...) inside it becomes
// one placeholder word (https://x-access-token:$(gh auth token)@github.com),
// and $NAME / ${NAME} set to literals earlier in the command (H=x; for h
// in a b) is expanded, one copy per value.
func hostWord(w string, vars map[string][]string) string {
	for i := strings.Index(w, "$("); i >= 0; i = strings.Index(w, "$(") {
		k := matchParen(w, i+1)
		if k < 0 {
			break
		}
		w = w[:i] + "SUB" + w[k+1:]
	}
	if len(vars) == 0 || !strings.Contains(w, "$") {
		return w
	}
	for _, m := range litRe.FindAllString(w, 4) {
		vs := vars[strings.Trim(m, "${}")]
		if len(vs) == 0 {
			continue
		}
		var out []string
		for _, v := range vs {
			out = append(out, strings.ReplaceAll(w, m, v))
		}
		w = strings.Join(out, " ")
	}
	return w
}

var (
	litRe = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$[A-Za-z_][A-Za-z0-9_]*`)
	// Python sends only through a network module it imports (urllib.parse,
	// or a module named inside a string literal, is not a call).
	pyNetRe    = regexp.MustCompile(`(?:^|[\n;])[ \t]*(?:import[ \t]+[^\n;]*?\b(requests|httpx|aiohttp|socket|websockets?|urllib3|urllib\.request|urllib2|http\.client)\b|from[ \t]+(requests|httpx|aiohttp|socket|websockets?|urllib3|urllib\.request|urllib2|http\.client)\b[^\n;]*\bimport\b|from[ \t]+(?:urllib|http)[ \t]+import[^\n;]*\b(request|client)\b)|\b(urlopen)\s*\(|\b(urllib\.request)\.\w+\s*\(`)
	pyTripleRe = regexp.MustCompile(`(?s)'''.*?'''|""".*?"""|'''.*|""".*`)
)

// netLib names the network library an interpreter's code uses ("" none).
func netLib(name, code string) string {
	if strings.HasPrefix(name, "python") || strings.HasPrefix(name, "pypy") || name == "uv" || name == "uvx" {
		m := pyNetRe.FindStringSubmatch(pyTripleRe.ReplaceAllString(code, "''"))
		if m == nil {
			return ""
		}
		switch lib := firstSet(m[1:]...); lib {
		case "urllib.request", "urlopen", "urllib2", "request":
			return "urllib"
		case "client":
			return "http.client"
		default:
			return lib
		}
	}
	m := netLibRe.FindString(code)
	if m == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(strings.TrimRight(m, "( ")))
}

// codeFlags take inline code as their argument (python -c, node -e,
// deno eval, pwsh -Command, osascript -e).
var codeFlags = map[string]bool{
	"-c": true, "-e": true, "-E": true, "--eval": true, "-r": true, "-p": true, "--print": true,
	"eval": true, "-command": true, "-Command": true, "-cmd": true,
}

// inlineCode is the code an interpreter is given on its command line (not
// a script file or a package name: pip install requests is not a call).
func inlineCode(rest []string) string {
	var b strings.Builder
	for i := 0; i+1 < len(rest); i++ {
		if codeFlags[rest[i]] {
			b.WriteString(rest[i+1])
			b.WriteByte('\n')
			i++
		}
	}
	return b.String()
}

// dockerLoginHost: the registry of "docker login [flags] REGISTRY" (a
// host name, not a URL); "" for Docker Hub or another subcommand.
func dockerLoginHost(rest []string) string {
	if firstNonFlag(rest) != "login" {
		return ""
	}
	after := false
	for i := 0; i < len(rest); i++ {
		w := rest[i]
		switch {
		case !after:
			after = w == "login"
		case w == "-u" || w == "-p" || w == "--username" || w == "--password":
			i++
		case strings.HasPrefix(w, "-"):
		default:
			if m := urlRe.FindStringSubmatch(w); m != nil {
				return m[1]
			}
			if h, _, _ := strings.Cut(w, "/"); strings.Contains(h, ".") {
				h, _, _ = strings.Cut(h, ":")
				return h
			}
			return ""
		}
	}
	return ""
}

func unixSocket(rest []string) bool {
	for _, w := range rest {
		if w == "--unix-socket" || strings.HasPrefix(w, "--unix-socket=") || w == "--abstract-unix-socket" {
			return true
		}
	}
	return false
}

func hasWord(ws []string, w string) bool {
	for _, x := range ws {
		if x == w {
			return true
		}
	}
	return false
}

func firstNonFlag(ws []string) string {
	for _, w := range ws {
		if !strings.HasPrefix(w, "-") {
			return w
		}
	}
	return ""
}

// gitURLs returns the hosts of the URLs a network git subcommand names
// (push/fetch/pull/clone/ls-remote/remote add). A remote name (origin)
// is not a sink: its URL was set up separately.
func gitURLs(rest []string) []string {
	sub := firstNonFlag(rest)
	switch sub {
	case "push", "fetch", "pull", "clone", "ls-remote", "remote", "submodule":
	default:
		return nil
	}
	var hosts []string
	for _, w := range rest {
		if strings.HasPrefix(w, "-") {
			continue
		}
		w = hostWord(w, nil)
		if m := urlRe.FindStringSubmatch(w); m != nil {
			hosts = append(hosts, m[1])
		} else if m := scpHostRe.FindStringSubmatch(w); m != nil && strings.Contains(w, "@") {
			hosts = append(hosts, m[1])
		}
	}
	return hosts
}

// ghSink: gh talks to GitHub; a gist is a classic place to drop a secret,
// so it is not treated as "GitHub, the token's provider".
func ghSink(rest []string) (string, bool) {
	switch sub := firstNonFlag(rest); sub {
	case "gist":
		return "gh gist", false
	case "secret", "variable":
		if len(rest) > 1 && (hasWord(rest, "set") || hasWord(rest, "create")) {
			return "gh " + sub + " set", false
		}
		return "gh " + sub, false
	case "api", "issue", "pr", "release", "repo", "workflow", "run", "label", "discussion":
		return "gh " + sub, false
	}
	return "", false
}

// --- secrets ---

var (
	// $NAME, ${NAME}, $env:NAME, %NAME%, and named access from code.
	envRefRe = regexp.MustCompile(`(?i:\$env:)([A-Za-z_][A-Za-z0-9_]*)|\$\{?([A-Za-z_][A-Za-z0-9_]*)|%([A-Za-z_][A-Za-z0-9_]*)%|(?:environ(?:\.get)?\s*[\[(]\s*|getenv\s*\(\s*|process\.env\.|process\.env\[\s*|\bENV\[\s*|env\.get\(\s*|System\.getenv\(\s*)['"]?([A-Za-z_][A-Za-z0-9_]*)`)
	// printenv NAME
	printenvRe = regexp.MustCompile(`\bprintenv\s+([A-Za-z_][A-Za-z0-9_]*)`)

	toolSecretRes = []struct {
		re     *regexp.Regexp
		label  string
		family string
	}{
		{regexp.MustCompile(`\bgh\s+auth\s+token\b`), "gh auth token", "github"},
		{regexp.MustCompile(`\bgh\s+auth\s+status\b[^|;&\n]*(?:-t\b|--show-token)`), "gh auth status --show-token", "github"},
		{regexp.MustCompile(`\bdoppler\s+secrets(?:\s+download\b|\s*$|\s*[|;&)])`), "doppler secrets", "doppler"},
		{regexp.MustCompile(`\bdoppler\s+configure\s+(?:get\s+token|--all)`), "doppler configure", "doppler"},
		{regexp.MustCompile(`\bsecurity\s+find-(?:generic|internet)-password\b`), "macOS keychain", ""},
		{regexp.MustCompile(`\bsecret-tool\s+lookup\b`), "secret-tool lookup", ""},
		{regexp.MustCompile(`\bgit\s+credential\s+fill\b`), "git credential fill", "github"},
		{regexp.MustCompile(`\bop\s+(?:read|item\s+get)\b`), "1Password CLI", ""},
		{regexp.MustCompile(`\bgcloud\s+auth\s+(?:application-default\s+)?print-access-token\b`), "gcloud auth token", "google"},
		// An identity token is audience-bound: Cloud Run and Functions are where it goes.
		{regexp.MustCompile(`\bgcloud\s+auth\s+print-identity-token\b`), "gcloud identity token", "gcp-identity"},
		{regexp.MustCompile(`\baws\s+configure\s+get\b`), "aws configure get", "aws"},
		{regexp.MustCompile(`\bvault\s+(?:kv\s+)?read\b|\bvault\s+kv\s+get\b`), "vault read", ""},
		{regexp.MustCompile(`\bpyre\s+(?:token|auth\s+token)\b`), "pyre token", "local"},
	}

	// namedSecretCmds read secrets by name (doppler secrets get NAME): the
	// name gives the provider, as for $NAME. With no name parsed the
	// source keeps the store's own family.
	namedSecretCmds = []struct {
		re         *regexp.Regexp // group 1: the arguments
		label      string
		family     string          // when no name is given
		valueFlags map[string]bool // flags that take a value
	}{
		{regexp.MustCompile(`\bdoppler\s+secrets\s+get\b([^|;&)\n]*)`), "doppler secrets get", "doppler", map[string]bool{
			"-p": true, "--project": true, "-c": true, "--config": true, "--token": true, "--scope": true,
			"--configuration": true, "--api-host": true, "--dashboard-host": true, "--format": true,
		}},
	}

	credFileRes = []struct {
		re     *regexp.Regexp
		label  string
		family string
	}{
		{regexp.MustCompile(`\.config/gh/hosts\.ya?ml`), "~/.config/gh/hosts.yml", "github"},
		{regexp.MustCompile(`\.claude/\.credentials\.json`), "~/.claude/.credentials.json", "anthropic"},
		{regexp.MustCompile(`\.codex/auth\.json`), "~/.codex/auth.json", "openai"},
		{regexp.MustCompile(`pyre/profiles/\S*?(?:\.credentials\.json|auth\.json|hosts\.ya?ml|oauth[^/\s]*\.json)`), "Pyre account profile credentials", ""},
		{regexp.MustCompile(`\.git-credentials\b`), "~/.git-credentials", ""},
		{regexp.MustCompile(`(?:^|[\s"'=/~])\.netrc\b`), "~/.netrc", ""},
		{regexp.MustCompile(`\.aws/credentials\b`), "~/.aws/credentials", "aws"},
		{regexp.MustCompile(`\.docker/config\.json`), "~/.docker/config.json", ""},
		{regexp.MustCompile(`\.config/doppler\b|\.doppler/\.doppler\.ya?ml`), "Doppler config", "doppler"},
		{regexp.MustCompile(`\.config/gcloud/(?:credentials\.db|access_tokens\.db|application_default_credentials\.json)`), "gcloud credentials", "google"},
		{regexp.MustCompile(`(?:~|\$HOME|\$\{HOME\})/\.npmrc\b`), "~/.npmrc", "npm"},
		{regexp.MustCompile(`/proc/(?:self|\d+|\$\$|\*)/environ`), "/proc/*/environ", "dump"},
	}

	// sshKeyRe: a private key path; .pub keys are public, and a key given
	// to ssh/scp/git as an identity (-i, IdentityFile, ssh-add) is use,
	// not a read.
	sshKeyRe = regexp.MustCompile(`(-i\s*|IdentityFile[=\s]\s*|ssh-add\s+(?:-\S+\s+)*|ssh-keygen\s[^|;&\n]*-f\s*|-o\s*IdentityFile=)?\S*\.ssh/id_[A-Za-z0-9_-]+(\.pub)?`)

	// Token literals (see internal/credentials/redact.go for the same shapes).
	literalRes = []struct {
		re     *regexp.Regexp
		label  string
		family string
	}{
		{regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`), "GitHub token literal", "github"},
		{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), "GitHub token literal", "github"},
		{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), "Anthropic key literal", "anthropic"},
		{regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])sk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}`), "OpenAI key literal", "openai"},
		{regexp.MustCompile(`xox[abprse]-[A-Za-z0-9-]{10,}`), "Slack token literal", "slack"},
		{regexp.MustCompile(`\bdp\.(?:st|pt|ct|sa|scim|audit)\.[A-Za-z0-9_.-]{20,}`), "Doppler token literal", "doppler"},
		{regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), "AWS key literal", "aws"},
		{regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), "Google key literal", "google"},
		{regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{10,}`), "Stripe key literal", "stripe"},
		{regexp.MustCompile(`lin_(?:api|oauth)_[A-Za-z0-9]{20,}`), "Linear key literal", "linear"},
		{regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`), "Hugging Face token literal", "huggingface"},
		{regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), "npm token literal", "npm"},
		{regexp.MustCompile(`\bpy[cnstep]_[A-Za-z0-9_\-]{12,}`), "Pyre token literal", "local"},
		{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`), "private key", ""},
	}

	// env dumps in code that takes the whole environment (shell dumps:
	// shellDump).
	envDumpRe = regexp.MustCompile(`(?i:get-childitem\s+env:|gci\s+env:|dir\s+env:|ls\s+env:)|os\.environ\s*(?:$|[^\[.\s]|\.(?:copy|items|keys|values)\b)|process\.env\s*(?:$|[^.\[\s])|Deno\.env\.toObject|ENV\.to_h`)
)

func findSecrets(cmd string, d Decoys) []secretRef {
	var refs []secretRef
	for _, m := range envRefRe.FindAllStringSubmatch(cmd, -1) {
		name := firstSet(m[1:]...)
		if r, ok := nameRef(name, d); ok {
			r.kind = refVar
			refs = append(refs, r)
		}
	}
	for _, m := range printenvRe.FindAllStringSubmatch(cmd, -1) {
		if r, ok := nameRef(m[1], d); ok {
			refs = append(refs, r)
		}
	}
	for _, t := range toolSecretRes {
		if t.re.MatchString(cmd) {
			refs = append(refs, secretRef{label: t.label, family: t.family})
		}
	}
	var vars map[string][]string
	for _, t := range namedSecretCmds {
		for _, m := range t.re.FindAllStringSubmatch(cmd, -1) {
			if vars == nil {
				vars = shellVars(cmd)
			}
			names := secretNames(m[1], t.valueFlags, vars)
			if len(names) == 0 {
				refs = append(refs, secretRef{label: t.label, family: t.family})
			}
			for _, n := range names {
				if r, ok := storeRef(n); ok {
					r.label = t.label + " " + n
					refs = append(refs, r)
				}
			}
		}
	}
	for _, t := range credFileRes {
		if t.re.MatchString(cmd) {
			if t.family == "dump" {
				refs = append(refs, secretRef{label: t.label, dump: true})
			} else {
				refs = append(refs, secretRef{label: t.label, family: t.family})
			}
		}
	}
	for _, m := range sshKeyRe.FindAllStringSubmatch(cmd, -1) {
		if m[1] == "" && m[2] == "" {
			refs = append(refs, secretRef{label: "SSH private key", family: ""})
			break
		}
	}
	for _, t := range literalRes {
		if t.re.MatchString(cmd) {
			refs = append(refs, secretRef{label: t.label, family: t.family, kind: refLiteral})
		}
	}
	if envDumpRe.MatchString(cmd) || shellDump(cmd) {
		refs = append(refs, secretRef{label: "environment dump", dump: true})
	}
	return refs
}

// shellDump: a command that prints the whole environment (bare env,
// printenv, export -p, declare -px, set), found by parsing, so the word
// env inside a grep pattern or a PR body is not one.
func shellDump(cmd string) bool {
	if !strings.Contains(cmd, "env") && !strings.Contains(cmd, "set") && !strings.Contains(cmd, "-p") {
		return false
	}
	for _, c := range Commands(cmd) {
		args := withoutRedirects(c.Args)
		if ws := withoutRedirects(c.Words); len(ws) > 0 && len(ws) < len(c.Words) {
			// env > file: the lexer took the redirection for env's program.
			c.Name, args = progName(ws[0]), ws[1:]
		}
		switch c.Name {
		case "env", "printenv", "set":
			if len(args) == 0 {
				return true
			}
		case "export", "declare", "typeset":
			if len(args) == 1 && strings.HasPrefix(args[0], "-") && strings.ContainsAny(args[0], "px") {
				return true
			}
		}
	}
	return false
}

var redirWordRe = regexp.MustCompile(`^[0-9]*(?:[<>]{1,2}|&>{1,2}|>&|<&)(.*)$`)

// withoutRedirects drops redirections (> f, 2>/dev/null, >>f) from words.
func withoutRedirects(ws []string) []string {
	var out []string
	for i := 0; i < len(ws); i++ {
		if m := redirWordRe.FindStringSubmatch(ws[i]); m != nil {
			if m[1] == "" {
				i++ // the target is the next word
			}
			continue
		}
		out = append(out, ws[i])
	}
	return out
}

var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretNames: the secret names among a command's arguments (not flags
// or their values). A $VAR set earlier to literals (for n in A B; N=A)
// gives those names; a templated name (QA_${v}_EMAIL) is judged by its
// literal parts.
func secretNames(args string, valueFlags map[string]bool, vars map[string][]string) []string {
	var names []string
	ws := strings.Fields(args)
	for i := 0; i < len(ws); i++ {
		w := strings.Trim(ws[i], `"'`)
		switch {
		case valueFlags[w]:
			i++
		case strings.HasPrefix(w, "-"):
		case nameRe.MatchString(w):
			names = append(names, w)
		case litRe.MatchString(w) && litRe.FindString(w) == w:
			for _, v := range vars[strings.Trim(w, "${}")] {
				if nameRe.MatchString(v) {
					names = append(names, v)
				}
			}
		case strings.Contains(w, "$") && !strings.Contains(w, "$("):
			if t := litRe.ReplaceAllString(w, "X"); nameRe.MatchString(t) {
				names = append(names, t)
			}
		}
	}
	return names
}

func firstSet(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// nonSecretTail: a name ending in one of these holds a path, size or
// setting about a secret, not the secret (TOKEN_PATH, MAX_TOKENS).
var nonSecretTail = map[string]bool{
	"FILE": true, "PATH": true, "DIR": true, "URL": true, "URI": true, "NAME": true, "TYPE": true, "LEN": true,
	"LENGTH": true, "COUNT": true, "MAX": true, "MIN": true, "LIMIT": true, "EXPIRY": true, "EXPIRES": true,
	"TTL": true, "HOST": true, "PORT": true, "ENDPOINT": true, "HEADER": true, "ENV": true, "SCOPES": true,
	"USER": true, "USERNAME": true, "ID": true, "IDS": true, "TOKENS": true, "FINGERPRINT": true, "HELPER": true,
	"PROVIDER": true, "VERSION": true, "MODE": true, "SET": true, "ENABLED": true, "REGION": true,
}

var secretSegs = map[string]bool{
	"TOKEN": true, "SECRET": true, "SECRETS": true, "PASSWORD": true, "PASSWD": true, "PAT": true,
	"APIKEY": true, "CREDENTIALS": true, "CREDENTIAL": true, "PRIVATEKEY": true,
}

// IsSecretName: a variable name that holds a credential (GH_TOKEN,
// ANTHROPIC_API_KEY, DB_PASSWORD, NPMTOKEN, CLIENT_SECRET).
func IsSecretName(name string) bool {
	u := strings.ToUpper(name)
	segs := strings.Split(u, "_")
	if nonSecretTail[segs[len(segs)-1]] {
		return false
	}
	for i, s := range segs {
		if secretSegs[s] {
			return true
		}
		if s == "KEY" && i > 0 {
			switch segs[i-1] {
			case "API", "ACCESS", "SECRET", "PRIVATE", "AUTH", "ADMIN", "SIGNING", "SERVICE", "MASTER", "LICENSE":
				return true
			}
		}
	}
	return strings.HasSuffix(u, "TOKEN") || strings.HasSuffix(u, "APIKEY") || strings.HasSuffix(u, "SECRET")
}

func nameRef(name string, d Decoys) (secretRef, bool) {
	if d.isName(name) {
		return secretRef{label: "$" + name, decoy: true}, true
	}
	if isDBURLName(name) {
		return secretRef{label: "$" + name, family: "db"}, true
	}
	if !IsSecretName(name) {
		return secretRef{}, false
	}
	return secretRef{label: "$" + name, family: familyOf(name), alts: altFamilies(name)}, true
}

// storeRef: a value a secret store returns by name (doppler secrets get
// NAME). Doppler also holds configuration: a name that says it is a host,
// id, origin, email or public value is not a secret.
func storeRef(name string) (secretRef, bool) {
	if isDBURLName(name) {
		return secretRef{family: "db"}, true
	}
	u := strings.ToUpper(stripPrefix(name))
	segs := strings.Split(u, "_")
	if last := segs[len(segs)-1]; nonSecretTail[last] || configTail[last] || strings.Contains(u, "PUBLISHABLE") {
		return secretRef{}, false
	}
	for _, p := range []string{"NEXT_PUBLIC_", "VITE_", "PUBLIC_", "EXPO_PUBLIC_", "REACT_APP_"} {
		if strings.HasPrefix(u, p) {
			return secretRef{}, false
		}
	}
	return secretRef{family: familyOf(name), alts: altFamilies(name)}, true
}

// configTail: Doppler names ending in these hold configuration, not a credential.
var configTail = map[string]bool{
	"ORIGIN": true, "ORIGINS": true, "EMAIL": true, "UUID": true, "DSN": true, "DOMAIN": true, "BUCKET": true,
	"PROJECT": true, "ORG": true, "TEAM": true, "CHANNEL": true, "ENVIRONMENT": true, "LEVEL": true, "RATE": true,
}

// stripPrefix drops a project prefix from a Doppler-style name
// (ACME__STRIPE_SECRET_KEY -> STRIPE_SECRET_KEY).
func stripPrefix(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 && i+2 < len(name) {
		return name[i+2:]
	}
	return name
}

// dbURLRe: a database connection string (it holds the DB password).
var dbURLRe = regexp.MustCompile(`^(?:[A-Z0-9]+_)*(?:DATABASE|DB|POSTGRES|POSTGRESQL|PG|MYSQL|REDIS|MONGO|MONGODB)(?:_[A-Z0-9]+)*_(?:URL|URI|DSN)$`)

func isDBURLName(name string) bool {
	return dbURLRe.MatchString(strings.ToUpper(stripPrefix(name)))
}

// familyAlias maps a variable's first word to its provider family.
// Providers live here and in familyDomains; add both to extend.
var familyAlias = map[string]string{
	"GH": "github", "GITHUB": "github", "GHE": "github", "GHCR": "github", "CR": "github", // CR_PAT: GitHub's documented ghcr.io token name
	"ANTHROPIC": "anthropic", "CLAUDE": "anthropic",
	"OPENAI": "openai", "CODEX": "openai", "AZURE": "azure",
	"CF": "cloudflare", "CLOUDFLARE": "cloudflare", "WRANGLER": "cloudflare",
	"HF": "huggingface", "HUGGINGFACE": "huggingface", "HUGGING": "huggingface",
	"ELEVEN": "elevenlabs", "XI": "elevenlabs", "FALAI": "fal",
	"NPM": "npm", "NODE": "npm", "YARN": "npm",
	"AWS": "aws", "GOOGLE": "google", "GCP": "google", "GCLOUD": "google", "GEMINI": "google", "FIREBASE": "google",
	"JIRA": "atlassian", "CONFLUENCE": "atlassian", "ATLASSIAN": "atlassian", "BITBUCKET": "bitbucket",
	"PYRE": "local", "DATABASE": "", "DB": "", "POSTGRES": "", "MYSQL": "", "REDIS": "",
}

// familyOf: the provider family of a secret variable; its first word
// unless aliased (LINEAR_API_KEY -> linear, matched against host labels).
func familyOf(name string) string {
	first := strings.ToUpper(strings.SplitN(stripPrefix(name), "_", 2)[0])
	if f, ok := familyAlias[first]; ok {
		return f
	}
	if len(first) < 3 || secretSegs[first] || first == "API" || first == "ACCESS" || first == "AUTH" || first == "MY" || first == "SECRET" {
		return ""
	}
	return strings.ToLower(first)
}

// altFamilies: the other known providers a name names besides its first
// word (VORTEX_QA_PAID_ACME_API_KEY: acme; CF_VORTEX_SITE_TOKEN: vortex).
func altFamilies(name string) []string {
	segs := strings.Split(strings.ToUpper(stripPrefix(name)), "_")
	var out []string
	for _, s := range segs[1:] {
		f, ok := familyAlias[s]
		if !ok {
			if _, known := familyDomains[strings.ToLower(s)]; known {
				f, ok = strings.ToLower(s), true
			}
		}
		if ok && f != "" && f != "local" && !hasWord(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// familyDomains are the hosts (and their subdomains) a family's
// credentials legitimately go to, besides a host whose registrable label
// is the family name (LINEAR_API_KEY -> api.linear.app).
var familyDomains = map[string][]string{
	"github":       {"github.com", "githubusercontent.com", "ghcr.io", "githubapp.com"},
	"anthropic":    {"anthropic.com", "claude.ai", "claude.com"},
	"openai":       {"openai.com", "chatgpt.com"},
	"google":       {"googleapis.com", "google.com", "gcr.io", "pkg.dev", "firebaseio.com"},
	"gcp-identity": {"googleapis.com", "run.app", "cloudfunctions.net", "appspot.com"},
	"aws":          {"amazonaws.com", "aws.amazon.com"},
	"azure":        {"azure.com", "microsoft.com", "windows.net", "azure-api.net"},
	"cloudflare":   {"cloudflare.com", "workers.dev"},
	"huggingface":  {"huggingface.co", "hf.co"},
	"npm":          {"npmjs.org", "npmjs.com", "yarnpkg.com"},
	"slack":        {"slack.com"},
	"doppler":      {"doppler.com"},
	"stripe":       {"stripe.com"},
	"linear":       {"linear.app"},
	"atlassian":    {"atlassian.net", "atlassian.com"},
	"bitbucket":    {"bitbucket.org"},
	"posthog":      {"posthog.com"}, // us., eu., us.i.
	"sentry":       {"sentry.io"},
	"modal":        {"modal.com", "modal.run"},
	"fireworks":    {"fireworks.ai"},
	"runware":      {"runware.ai"},
	"replicate":    {"replicate.com", "replicate.delivery"},
	"fal":          {"fal.run", "fal.ai", "fal.media"},
	"elevenlabs":   {"elevenlabs.io"},
	"resend":       {"resend.com"},
	"intercom":     {"intercom.io", "intercom.com"},
	"notion":       {"notion.com", "notion.so"},
	"discord":      {"discord.com", "discordapp.com"},
	"clerk":        {"clerk.com", "clerk.accounts.dev", "clerk.dev"},
	"vortex":      {"vortex.so"},
	"acme":         {"acme.app"},
	"gengen":       {"gengen.farm"},
}

// providerOK: every secret goes only to its own provider, or every
// destination is this machine. A command naming no host (nc, a socket in
// code) is never "provider".
func providerOK(refs []secretRef, hosts []string, loopback bool) bool {
	if loopback && len(hosts) == 0 {
		return true
	}
	if len(hosts) == 0 {
		return false
	}
	allLocal := true
	for _, h := range hosts {
		if !isLocalHost(h) {
			allLocal = false
		}
	}
	if allLocal {
		return true
	}
	for _, r := range refs {
		if r.family == "" && len(r.alts) == 0 || r.family == "local" || r.family == "db" {
			return false
		}
		for _, h := range hosts {
			if isLocalHost(h) || hostOf(r.family, h) {
				continue
			}
			ok := false
			for _, a := range r.alts {
				ok = ok || hostOf(a, h)
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

// isLocalHost: this machine, or a placeholder host that cannot receive
// anything (example.com/.org/.net, *.test, *.invalid; RFC 2606).
func isLocalHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	for _, p := range []string{"example.com", "example.org", "example.net"} {
		if h == p || strings.HasSuffix(h, "."+p) {
			return true
		}
	}
	if strings.HasSuffix(h, ".test") || strings.HasSuffix(h, ".invalid") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func hostOf(family, host string) bool {
	for _, d := range familyDomains[family] {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	// A label of the registrable part, not a subdomain an attacker can
	// choose: "linear" in api.linear.app, not in linear.evil.dev.
	reg := labels[len(labels)-2]
	return reg == family
}
