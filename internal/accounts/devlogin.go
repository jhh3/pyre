package accounts

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/devlogin"
	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/osutil"
)

// Remote device login (account.device_login*): the node runs `codex login
// --device-auth` into a private temporary CODEX_HOME, streams the
// verification URL and one-time code back through status reads, and on
// approval saves the new login as the rota account
// <CodexHome>/accounts/<email>/auth.json (0600, directory 0700), keeping a
// backup of an earlier copy — exactly like the owner's codex-add-device.sh.
// The live <CodexHome>/auth.json is never touched, the temporary home is
// always removed, and no token ever leaves the node: views carry only the
// URL, code, state and email.

// EnvCodexBin overrides the codex executable used for device logins.
const EnvCodexBin = "PYRE_CODEX_BIN"

const (
	maxActiveDeviceLogins = 2
	deviceLoginKeep       = time.Hour // finished logins stay readable this long
	deviceLoginStartWait  = 45 * time.Second
	maxDeviceLoginTimeout = 30 * time.Minute
)

// ClaudeDeviceLoginUnsupported explains why Claude logins are not offered.
const ClaudeDeviceLoginUnsupported = "Claude Code has no device-code login: `claude auth login` needs a browser on that machine " +
	"(or a code pasted back into its own terminal). Log in on the machine itself, save it with rota, then `pyre accounts register`."

var deviceLoginMgrs sync.Map // *Service -> *deviceLogins

type deviceLogins struct {
	s    *Service
	mu   sync.Mutex
	jobs map[string]*dlJob
}

type dlJob struct {
	view    devlogin.View
	changed chan struct{} // closed and replaced on every change
	cancel  context.CancelFunc
	stopped bool // cancel requested by the owner
}

func (s *Service) deviceLogins() *deviceLogins {
	v, _ := deviceLoginMgrs.LoadOrStore(s, &deviceLogins{s: s, jobs: map[string]*dlJob{}})
	return v.(*deviceLogins)
}

// deviceLoginHandlers are merged into Handlers (actions.go).
func (s *Service) deviceLoginHandlers() map[string]ActionHandler {
	d := s.deviceLogins()
	return map[string]ActionHandler{
		devlogin.ActionStart: func(ctx context.Context, params json.RawMessage) (any, error) {
			var p devlogin.Params
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			return d.Start(ctx, p)
		},
		devlogin.ActionStatus: func(ctx context.Context, params json.RawMessage) (any, error) {
			var p devlogin.StatusParams
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			return d.Status(ctx, p)
		},
		devlogin.ActionCancel: func(ctx context.Context, params json.RawMessage) (any, error) {
			var p devlogin.StatusParams
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			return d.Cancel(ctx, p.LoginID)
		},
	}
}

func (s *Service) deviceLoginCodexHome() string {
	if s.cfg.Rota.CodexHome != "" {
		return s.cfg.Rota.CodexHome
	}
	return s.cfg.UserCodexDir
}

func codexBinForLogin() (string, error) {
	if b := strings.TrimSpace(os.Getenv(EnvCodexBin)); b != "" {
		return b, nil
	}
	p, err := exec.LookPath("codex")
	if err != nil {
		return "", contracts.Errorf(contracts.ErrUnavailable, "codex is not installed on this machine (or not on pyred's PATH)")
	}
	return p, nil
}

// Start launches a device login and returns once the URL and code are
// known (or the login ended first).
func (d *deviceLogins) Start(ctx context.Context, p devlogin.Params) (devlogin.View, error) {
	switch p.Provider {
	case contracts.ProviderOpenAI, "":
		p.Provider = contracts.ProviderOpenAI
	case contracts.ProviderAnthropic:
		return devlogin.View{}, contracts.Errorf(contracts.ErrUnsupported, "%s", ClaudeDeviceLoginUnsupported)
	default:
		return devlogin.View{}, contracts.Errorf(contracts.ErrInvalid, "provider must be openai (or anthropic, which is not supported)")
	}
	timeout := devlogin.DefaultTimeout
	if p.TimeoutSeconds > 0 {
		timeout = min(time.Duration(p.TimeoutSeconds)*time.Second, maxDeviceLoginTimeout)
	}
	home := d.s.deviceLoginCodexHome()
	if home == "" {
		return devlogin.View{}, contracts.Errorf(contracts.ErrUnavailable, "no Codex home on this machine")
	}
	bin, err := codexBinForLogin()
	if err != nil {
		return devlogin.View{}, err
	}
	d.mu.Lock()
	d.pruneLocked()
	active := 0
	for _, j := range d.jobs {
		if !j.view.Done() {
			active++
		}
	}
	if active >= maxActiveDeviceLogins {
		d.mu.Unlock()
		return devlogin.View{}, contracts.Errorf(contracts.ErrCapacityExhausted, "%d logins are already waiting on this machine; finish or cancel one first", active)
	}
	tmp, err := os.MkdirTemp("", "pyre-codex-login-")
	if err != nil {
		d.mu.Unlock()
		return devlogin.View{}, contracts.Errorf(contracts.ErrInternal, "temporary home: %v", err)
	}
	_ = fileperm.Restrict(tmp, 0o700)
	jctx, cancel := context.WithTimeout(context.Background(), timeout)
	j := &dlJob{changed: make(chan struct{}), cancel: cancel, view: devlogin.View{
		LoginID: contracts.NewID("dlogin"), Provider: p.Provider, NodeID: d.s.cfg.NodeID, State: devlogin.StateStarting, StartedAt: d.s.now(), Version: 1,
	}}
	d.jobs[j.view.LoginID] = j
	id, ch := j.view.LoginID, j.changed
	d.mu.Unlock()

	go d.run(jctx, cancel, id, bin, tmp, home)

	// Wait for the code (or an early end), bounded.
	t := time.NewTimer(deviceLoginStartWait)
	defer t.Stop()
	for {
		select {
		case <-ch:
		case <-t.C:
			v, _ := d.view(id)
			return v, nil
		case <-ctx.Done():
			v, _ := d.view(id)
			return v, nil
		}
		v, next := d.view(id)
		if v.State != devlogin.StateStarting {
			return v, nil
		}
		ch = next
	}
}

func (d *deviceLogins) view(id string) (devlogin.View, chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[id]
	if !ok {
		return devlogin.View{}, nil
	}
	return j.view, j.changed
}

func (d *deviceLogins) update(id string, fn func(v *devlogin.View)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[id]
	if !ok {
		return
	}
	fn(&j.view)
	j.view.Version++
	close(j.changed)
	j.changed = make(chan struct{})
}

func (d *deviceLogins) pruneLocked() {
	now := d.s.now()
	for id, j := range d.jobs {
		if j.view.FinishedAt != nil && now.Sub(*j.view.FinishedAt) > deviceLoginKeep {
			delete(d.jobs, id)
		}
	}
}

// Status returns the login, waiting up to WaitSeconds for a change.
func (d *deviceLogins) Status(ctx context.Context, p devlogin.StatusParams) (devlogin.View, error) {
	v, ch := d.view(p.LoginID)
	if ch == nil {
		return v, contracts.Errorf(contracts.ErrNotFound, "no such login on this machine (finished logins are kept for an hour)")
	}
	wait := min(max(p.WaitSeconds, 0), devlogin.MaxWaitSeconds)
	if wait == 0 || v.Done() {
		return v, nil
	}
	t := time.NewTimer(time.Duration(wait) * time.Second)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	v, _ = d.view(p.LoginID)
	return v, nil
}

// Cancel stops a login; nothing is saved.
func (d *deviceLogins) Cancel(ctx context.Context, id string) (devlogin.View, error) {
	d.mu.Lock()
	j, ok := d.jobs[id]
	if ok && !j.view.Done() {
		j.stopped = true
		j.cancel()
	}
	d.mu.Unlock()
	if !ok {
		return devlogin.View{}, contracts.Errorf(contracts.ErrNotFound, "no such login on this machine")
	}
	// Wait briefly for the process to end so the reply is final.
	for range 50 {
		v, ch := d.view(id)
		if v.Done() {
			return v, nil
		}
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return v, nil
		}
	}
	v, _ := d.view(id)
	return v, nil
}

// run drives one login to its end. The temporary home is always removed.
func (d *deviceLogins) run(ctx context.Context, cancel context.CancelFunc, id, bin, tmp, home string) {
	defer cancel()
	defer os.RemoveAll(tmp)
	finish := func(state, detail string, fn func(v *devlogin.View)) {
		_ = os.RemoveAll(tmp) // gone before anyone sees the final state
		d.update(id, func(v *devlogin.View) {
			now := d.s.now()
			v.State, v.Detail, v.FinishedAt = state, detail, &now
			if state != devlogin.StateWaiting && state != devlogin.StateCompleted {
				v.UserCode = "" // a dead code is noise
			}
			if fn != nil {
				fn(v)
			}
		})
	}
	cmd := exec.CommandContext(ctx, bin, "login", "--device-auth")
	cmd.Env = append(envWithout(os.Environ(), "CODEX_HOME", "NO_COLOR"), "CODEX_HOME="+tmp, "NO_COLOR=1")
	cmd.Dir = tmp
	osutil.NewProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = osutil.KillGroup(cmd.Process.Pid, false)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		finish(devlogin.StateFailed, "could not run codex: "+err.Error(), nil)
		return
	}
	var tail []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		var p devlogin.Parser
		sent := false
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			line := strings.TrimSpace(devlogin.StripANSI(sc.Text()))
			if line != "" {
				tail = append(tail, line)
				if len(tail) > 4 {
					tail = tail[1:]
				}
			}
			if p.Feed(sc.Text()) && !sent {
				sent = true
				d.update(id, func(v *devlogin.View) {
					v.State, v.VerificationURL, v.UserCode = devlogin.StateWaiting, p.URL, p.Code
					if p.ExpiresIn > 0 {
						t := d.s.now().Add(p.ExpiresIn)
						v.CodeExpiresAt = &t
					}
				})
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	werr := cmd.Wait()
	_ = pw.Close()
	<-done
	if werr == nil {
		// Belt and braces: kill anything left in the group.
		_ = osutil.KillGroup(cmd.Process.Pid, true)
	}

	d.mu.Lock()
	stopped := d.jobs[id] != nil && d.jobs[id].stopped
	d.mu.Unlock()
	switch {
	case stopped:
		finish(devlogin.StateCancelled, "", nil)
		return
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		finish(devlogin.StateExpired, "", nil)
		return
	}
	authPath := filepath.Join(tmp, "auth.json")
	if st, err := os.Stat(authPath); werr != nil || err != nil || st.Size() == 0 {
		detail := "codex exited without a login"
		if werr != nil {
			detail = "codex: " + werr.Error()
		}
		if len(tail) > 0 {
			detail += " (" + truncate(strings.Join(tail, " / "), 300) + ")"
		}
		finish(devlogin.StateFailed, detail, nil)
		return
	}
	res, err := d.s.saveDeviceLogin(authPath, home)
	if err != nil {
		finish(devlogin.StateFailed, err.Error(), nil)
		return
	}
	finish(devlogin.StateCompleted, "", func(v *devlogin.View) {
		v.Email, v.RotaName, v.Replaced, v.LiveLogin = res.email, res.email, res.replaced, res.live
		if res.live {
			v.Detail = devlogin.LiveLoginRefusal
		}
	})
}

type savedLogin struct {
	email    string
	replaced bool
	live     bool
}

// saveDeviceLogin stores the fresh auth.json as the rota account named by
// its email under <home>/accounts/, backing up an earlier copy. It refuses
// to replace a snapshot Pyre has checked out (pyre-lock present).
func (s *Service) saveDeviceLogin(authPath, home string) (savedLogin, error) {
	var out savedLogin
	b, err := os.ReadFile(authPath)
	if err != nil {
		return out, fmt.Errorf("reading the new login: %v", err)
	}
	var a codexAuth
	if json.Unmarshal(b, &a) != nil || a.Tokens == nil || a.Tokens.IDToken == "" {
		return out, fmt.Errorf("the new login is not a ChatGPT sign-in (no id token); nothing saved")
	}
	email := codexEmail(a.Tokens.IDToken)
	if email == "" || !validRotaName(email) {
		return out, fmt.Errorf("could not read the account email from the new login; nothing saved")
	}
	out.email = email
	_, newIdent := readCodexStore(filepath.Dir(authPath))
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err == nil {
		if _, liveIdent := readCodexStore(home); liveIdent != "" && liveIdent == newIdent {
			out.live = true
		}
	}
	dest := filepath.Join(home, "accounts", email)
	if _, err := os.Stat(filepath.Join(dest, "pyre-lock")); err == nil {
		return out, fmt.Errorf("%s is checked out by a Pyre session on this machine right now; try again when it is idle (nothing saved)", email)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return out, fmt.Errorf("creating %s: %v", dest, err)
	}
	_ = fileperm.Restrict(dest, 0o700)
	target := filepath.Join(dest, "auth.json")
	if _, err := os.Stat(target); err == nil {
		bak := target + ".replaced-" + s.now().Format("20060102150405")
		if err := os.Rename(target, bak); err != nil {
			return out, fmt.Errorf("backing up the earlier copy: %v", err)
		}
		out.replaced = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return out, err
	}
	if err := writeFileAtomic(target, b, 0o600); err != nil {
		return out, fmt.Errorf("saving the login: %v", err)
	}
	return out, nil
}

func codexEmail(idToken string) string {
	cl := jwtClaims(idToken)
	if cl == nil {
		return ""
	}
	if e, _ := cl["email"].(string); e != "" {
		return strings.TrimSpace(e)
	}
	if p, ok := cl["https://api.openai.com/profile"].(map[string]any); ok {
		if e, _ := p["email"].(string); e != "" {
			return strings.TrimSpace(e)
		}
	}
	return ""
}

func envWithout(env []string, keys ...string) []string {
	out := env[:0:0]
outer:
	for _, kv := range env {
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}
