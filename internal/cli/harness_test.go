package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// Synthetic credentials for the in-test mock hub.
const (
	ownerToken = "test-owner-token"
	sessBToken = "test-session-b-token"
	sessAToken = "test-session-a-token"
)

var testOwner = c.Principal{Kind: c.PrincipalOwner, ID: "cli_test-owner", Name: "test owner"}

// hub is a mock backend served over the real HTTP binding.
type hub struct {
	b   *mock.Backend
	srv *httptest.Server
}

func newHub(t *testing.T) *hub {
	t.Helper()
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	auth := httpapi.NewStaticTokens(map[string]c.Principal{
		ownerToken: testOwner,
		sessBToken: {Kind: c.PrincipalSession, ID: "ses_synth-b"},
		sessAToken: {Kind: c.PrincipalSession, ID: "ses_synth-a"},
	})
	srv := httptest.NewServer(httpapi.NewServer(b, auth, httpapi.Options{StreamWait: time.Second}))
	t.Cleanup(srv.Close)
	return &hub{b: b, srv: srv}
}

// env is an isolated process environment: never the user's config.
type env map[string]string

func newEnv(t *testing.T, h *hub) env {
	dir := t.TempDir()
	e := env{
		"HOME":            dir,
		EnvConfig:         filepath.Join(dir, "config", "client.json"),
		EnvStateDir:       filepath.Join(dir, "state"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "xdg"),
	}
	if h != nil {
		e[EnvHub] = h.srv.URL
		e[EnvToken] = ownerToken
	}
	return e
}

func (e env) with(kv ...string) env {
	out := env{}
	for k, v := range e {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(out, kv[i])
		} else {
			out[kv[i]] = kv[i+1]
		}
	}
	return out
}

func (e env) get(k string) string { return e[k] }

type result struct {
	stdout, stderr string
	code           int
}

func (r result) String() string {
	return "exit " + itoa(r.code) + "\nstdout:\n" + r.stdout + "\nstderr:\n" + r.stderr
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// run executes one pyre invocation in-process.
func run(t *testing.T, e env, app *App, args ...string) result {
	t.Helper()
	if app == nil {
		app = &App{}
	}
	var out, errb bytes.Buffer
	app.Stdout, app.Stderr, app.Getenv = &out, &errb, e.get
	if app.Stdin == nil {
		app.Stdin = strings.NewReader("")
	}
	if app.PollInterval == 0 {
		app.PollInterval = 20 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := app.Main(ctx, args)
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

func mustJSON[T any](t *testing.T, r result) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("stdout is not a %T: %v\n%s", v, err, r)
	}
	return v
}

func expectCode(t *testing.T, r result, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("want exit %d, got %s", code, r)
	}
}
