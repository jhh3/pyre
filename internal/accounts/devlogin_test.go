package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/devlogin"
	"github.com/jhh3/pyre/internal/devlogin/devlogintest"
)

type dlEnv struct {
	*testEnv
	tmp     string // TMPDIR: temp homes must be gone after every login
	approve string
	liveSum [32]byte
}

func newDLEnv(t *testing.T) *dlEnv {
	e := newRotaEnv(t).testEnv
	bin, err := devlogintest.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv(EnvCodexBin, bin)
	t.Setenv("FAKE_CODEX_MODE", "approve")
	approve := filepath.Join(t.TempDir(), "approved")
	t.Setenv("FAKE_CODEX_APPROVE", approve)
	t.Setenv("FAKE_CODEX_EMAIL", "person@synthetic.invalid")
	t.Setenv("FAKE_CODEX_ACCOUNT", "synthetic-acct-new")
	d := &dlEnv{testEnv: e, tmp: tmp, approve: approve, liveSum: fileSum(t, filepath.Join(e.userCodex, "auth.json"))}
	t.Cleanup(func() {
		if got := fileSum(t, filepath.Join(e.userCodex, "auth.json")); got != d.liveSum {
			t.Error("the live ~/.codex/auth.json changed")
		}
	})
	return d
}

func fileSum(t *testing.T, p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func (d *dlEnv) assertNoTempHomes() {
	d.t.Helper()
	ents, _ := os.ReadDir(d.tmp)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "pyre-codex-login-") {
			d.t.Fatalf("temporary login home left behind: %s", e.Name())
		}
	}
}

func (d *dlEnv) waitDone(id string) devlogin.View {
	d.t.Helper()
	dl := d.svc.deviceLogins()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		v, err := dl.Status(context.Background(), devlogin.StatusParams{LoginID: id, WaitSeconds: 2})
		if err != nil {
			d.t.Fatal(err)
		}
		if v.Done() {
			return v
		}
	}
	d.t.Fatal("login did not finish")
	return devlogin.View{}
}

func assertNoSecrets(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	for _, bad := range []string{"pyre-synthetic-access", "pyre-synthetic-refresh", "id_token", "eyJ"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("view leaks %q: %s", bad, b)
		}
	}
}

func TestDeviceLoginStreamsCodeThenSavesRotaAccount(t *testing.T) {
	d := newDLEnv(t)
	h := d.svc.Handlers()[devlogin.ActionStart]
	if h == nil {
		t.Fatal("account.device_login is not registered")
	}
	raw, err := h(context.Background(), json.RawMessage(`{"provider":"openai"}`))
	if err != nil {
		t.Fatal(err)
	}
	v := raw.(devlogin.View)
	if v.State != devlogin.StateWaiting || v.VerificationURL != devlogintest.URL || v.UserCode != devlogintest.Code {
		t.Fatalf("start view = %+v", v)
	}
	if v.CodeExpiresAt == nil || time.Until(*v.CodeExpiresAt) < 14*time.Minute {
		t.Fatalf("code expiry not parsed: %v", v.CodeExpiresAt)
	}
	// Still waiting: a status read times out without a change.
	st, err := d.svc.deviceLogins().Status(context.Background(), devlogin.StatusParams{LoginID: v.LoginID, WaitSeconds: 1})
	if err != nil || st.State != devlogin.StateWaiting {
		t.Fatalf("status = %+v, %v", st, err)
	}
	os.WriteFile(d.approve, nil, 0o600)
	done := d.waitDone(v.LoginID)
	if done.State != devlogin.StateCompleted || done.Email != "person@synthetic.invalid" || done.RotaName != done.Email || done.LiveLogin || done.Replaced {
		t.Fatalf("done = %+v", done)
	}
	assertNoSecrets(t, done)
	dir := filepath.Join(d.userCodex, "accounts", "person@synthetic.invalid")
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "auth.json"), 0o600)
	si, ident := readCodexStore(dir)
	if !si.Present || ident != "synthetic-acct-new" {
		t.Fatalf("saved snapshot unusable: %+v %q", si, ident)
	}
	d.assertNoTempHomes()

	// A second login of the same account keeps a backup of the first.
	v2, err := d.svc.deviceLogins().Start(context.Background(), devlogin.Params{Provider: contracts.ProviderOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	done2 := d.waitDone(v2.LoginID)
	if done2.State != devlogin.StateCompleted || !done2.Replaced {
		t.Fatalf("second = %+v", done2)
	}
	baks, _ := filepath.Glob(filepath.Join(dir, "auth.json.replaced-*"))
	if len(baks) != 1 {
		t.Fatalf("backups = %v", baks)
	}
	d.assertNoTempHomes()
}

func assertMode(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != want {
		t.Fatalf("%s mode %v, want %v", p, st.Mode().Perm(), want)
	}
}

func TestDeviceLoginFlagsTheLiveLogin(t *testing.T) {
	d := newDLEnv(t)
	t.Setenv("FAKE_CODEX_APPROVE", "")
	t.Setenv("FAKE_CODEX_ACCOUNT", codexAcct("DEFAULT")) // the node's own default login
	v, err := d.svc.deviceLogins().Start(context.Background(), devlogin.Params{})
	if err != nil {
		t.Fatal(err)
	}
	done := d.waitDone(v.LoginID)
	if done.State != devlogin.StateCompleted || !done.LiveLogin || done.Detail != devlogin.LiveLoginRefusal {
		t.Fatalf("done = %+v", done)
	}
	// account.register refuses it too (today's rule), so a client that
	// ignored the flag still cannot register it.
	_, err = d.svc.Register(context.Background(), RegisterParams{Harness: contracts.HarnessCodex, Source: "rota", BindRotaIdentity: true,
		Account: contracts.Account{AccountID: "acct_synthetic_live", Provider: contracts.ProviderOpenAI, RotaRef: done.RotaName}})
	if !contracts.IsCode(err, contracts.ErrForbidden) || !strings.Contains(err.Error(), "live default login") {
		t.Fatalf("register of the live login: %v", err)
	}
}

func TestDeviceLoginTimeoutCancelFailure(t *testing.T) {
	d := newDLEnv(t)
	dl := d.svc.deviceLogins()

	t.Setenv("FAKE_CODEX_MODE", "hang")
	v, err := dl.Start(context.Background(), devlogin.Params{TimeoutSeconds: 1})
	if err != nil || v.State != devlogin.StateWaiting {
		t.Fatalf("start = %+v, %v", v, err)
	}
	if got := d.waitDone(v.LoginID); got.State != devlogin.StateExpired || got.UserCode != "" {
		t.Fatalf("timeout = %+v", got)
	}
	d.assertNoTempHomes()

	v, err = dl.Start(context.Background(), devlogin.Params{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := dl.Cancel(context.Background(), v.LoginID)
	if err != nil || c.State != devlogin.StateCancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	d.assertNoTempHomes()

	t.Setenv("FAKE_CODEX_MODE", "fail")
	v, err = dl.Start(context.Background(), devlogin.Params{})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.waitDone(v.LoginID); got.State != devlogin.StateFailed || !strings.Contains(got.Detail, "device code request failed") {
		t.Fatalf("fail = %+v", got)
	}
	d.assertNoTempHomes()
	if ents, _ := os.ReadDir(filepath.Join(d.userCodex, "accounts")); len(ents) != 0 {
		t.Fatalf("something was saved: %v", ents)
	}
}

func TestDeviceLoginRefusals(t *testing.T) {
	d := newDLEnv(t)
	dl := d.svc.deviceLogins()
	if _, err := dl.Start(context.Background(), devlogin.Params{Provider: contracts.ProviderAnthropic}); !contracts.IsCode(err, contracts.ErrUnsupported) || !strings.Contains(err.Error(), "device-code") {
		t.Fatalf("anthropic: %v", err)
	}
	if _, err := dl.Status(context.Background(), devlogin.StatusParams{LoginID: "dlogin_nope"}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	// A snapshot Pyre has checked out is never replaced underneath it.
	dir := filepath.Join(d.userCodex, "accounts", "person@synthetic.invalid")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "pyre-lock"), []byte(`{"holder":"pyre"}`), 0o600)
	t.Setenv("FAKE_CODEX_APPROVE", "")
	v, err := dl.Start(context.Background(), devlogin.Params{})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.waitDone(v.LoginID); got.State != devlogin.StateFailed || !strings.Contains(got.Detail, "checked out") {
		t.Fatalf("locked = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "auth.json")); err == nil {
		t.Fatal("saved over a checked-out snapshot")
	}
	// At most two logins wait at once.
	t.Setenv("FAKE_CODEX_MODE", "hang")
	a, _ := dl.Start(context.Background(), devlogin.Params{})
	b, _ := dl.Start(context.Background(), devlogin.Params{})
	if _, err := dl.Start(context.Background(), devlogin.Params{}); !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
		t.Fatalf("third login: %v", err)
	}
	dl.Cancel(context.Background(), a.LoginID)
	dl.Cancel(context.Background(), b.LoginID)
	d.assertNoTempHomes()
}

// TestDeviceLoginRealCodexParse runs the REAL `codex login --device-auth`
// (opt-in: PYRE_REAL_CODEX_DEVICE_LOGIN=1) only to check that the node
// parses the real link and code, then cancels it. Nothing is approved or
// saved; the temporary home must be gone afterwards.
func TestDeviceLoginRealCodexParse(t *testing.T) {
	if os.Getenv("PYRE_REAL_CODEX_DEVICE_LOGIN") != "1" {
		t.Skip("set PYRE_REAL_CODEX_DEVICE_LOGIN=1 to run against the real codex CLI")
	}
	e := newRotaEnv(t).testEnv
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv(EnvCodexBin, "")
	dl := e.svc.deviceLogins()
	v, err := dl.Start(context.Background(), devlogin.Params{Provider: contracts.ProviderOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("state=%s url=%s code_shape=%s expires_in=%v", v.State, v.VerificationURL,
		strings.Map(func(r rune) rune {
			if r == '-' {
				return r
			}
			return 'X'
		}, v.UserCode), v.CodeExpiresAt != nil)
	c, err := dl.Cancel(context.Background(), v.LoginID)
	if err != nil || c.State != devlogin.StateCancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if v.State != devlogin.StateWaiting || !strings.HasPrefix(v.VerificationURL, "https://") || v.UserCode == "" || v.CodeExpiresAt == nil {
		t.Fatalf("real output not parsed: %+v", v)
	}
	ents, _ := os.ReadDir(tmp)
	for _, x := range ents {
		if strings.HasPrefix(x.Name(), "pyre-codex-login-") {
			t.Fatalf("temporary home left behind: %s", x.Name())
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(e.userCodex, "accounts")); len(ents) != 0 {
		t.Fatalf("something was saved: %v", ents)
	}
}
