package repoindex

import (
	"bufio"
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SSH host aliases in remotes: a checkout whose origin is "github:o/n"
// works only on a machine whose ~/.ssh/config maps the alias "github" to
// github.com. The index records the remote other machines clone and
// dedupe by, so an alias is resolved here (ssh -G, once per host) into
// the real host. The checkout's own config is never changed.

// sshHost is what `ssh -G` reports for a host.
type sshHost struct {
	HostName, User, Port string
}

// sshResolve resolves an ssh host; tests replace it.
var sshResolve = execSSHResolve

// sshAliasTTL bounds how long a resolution (or a failure) is reused, so an
// edited ~/.ssh/config is picked up by a later pass.
const sshAliasTTL = 10 * time.Minute

type sshCached struct {
	h  sshHost
	ok bool
	at time.Time
}

var sshCache struct {
	mu sync.Mutex
	m  map[string]sshCached
}

// resolveSSHHost is sshResolve, cached per host (failures too).
func resolveSSHHost(host string) (sshHost, bool) {
	sshCache.mu.Lock()
	defer sshCache.mu.Unlock()
	if c, ok := sshCache.m[host]; ok && time.Since(c.at) < sshAliasTTL {
		return c.h, c.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := sshResolve(ctx, host)
	ok := err == nil && h.HostName != ""
	if sshCache.m == nil {
		sshCache.m = map[string]sshCached{}
	}
	sshCache.m[host] = sshCached{h: h, ok: ok, at: time.Now()}
	return h, ok
}

// execSSHResolve runs `ssh -G host` (reads the config, no connection).
func execSSHResolve(ctx context.Context, host string) (sshHost, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-G", "--", host)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return sshHost{}, err
	}
	var h sshHost
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		switch strings.ToLower(k) {
		case "hostname":
			h.HostName = strings.TrimSpace(v)
		case "user":
			h.User = strings.TrimSpace(v)
		case "port":
			h.Port = strings.TrimSpace(v)
		}
	}
	return h, nil
}

// isAliasHost: only an unqualified name can be an alias worth resolving.
// A real host name (github.com) is left alone even when the ssh config
// redirects it (ssh.github.com:443), since other machines reach it as is.
func isAliasHost(host string) bool {
	return host != "" && host != "localhost" && !strings.ContainsAny(host, ".:[]/\\ ")
}

// CanonicalRemote rewrites an scp-like or ssh:// remote whose host is an
// ssh alias into the host it stands for: "github:o/n" becomes
// "git@github.com:o/n" (ssh://user@host:port/path when the alias sets a
// port). Anything else, or a failed resolution, is returned unchanged.
func CanonicalRemote(remote string) string {
	if i := strings.Index(remote, "://"); i >= 0 {
		scheme := strings.ToLower(remote[:i])
		if scheme != "ssh" && scheme != "git+ssh" && scheme != "ssh+git" {
			return remote
		}
		u, err := url.Parse(remote)
		if err != nil || u.Port() != "" || !isAliasHost(u.Hostname()) {
			return remote
		}
		h, ok := resolveSSHHost(u.Hostname())
		if !ok || h.HostName == u.Hostname() {
			return remote
		}
		host := h.HostName
		if h.Port != "" && h.Port != "22" {
			host += ":" + h.Port
		}
		u.Host = host
		if u.User == nil && h.User != "" {
			u.User = url.User(h.User)
		}
		return u.String()
	}
	// scp-like: [user@]host:path, no slash before the colon.
	colon := strings.IndexByte(remote, ':')
	if colon <= 0 || strings.Contains(remote[:colon], "/") {
		return remote
	}
	user, host, hasUser := strings.Cut(remote[:colon], "@")
	if !hasUser {
		user, host = "", remote[:colon]
	}
	path := remote[colon+1:]
	if !isAliasHost(host) || path == "" {
		return remote
	}
	h, ok := resolveSSHHost(host)
	if !ok || h.HostName == host {
		return remote
	}
	if user == "" {
		user = h.User
	}
	if h.Port != "" && h.Port != "22" {
		u := url.URL{Scheme: "ssh", Host: h.HostName + ":" + h.Port, Path: "/" + strings.TrimPrefix(path, "/")}
		if user != "" {
			u.User = url.User(user)
		}
		return u.String()
	}
	if user != "" {
		return user + "@" + h.HostName + ":" + path
	}
	return h.HostName + ":" + path
}
