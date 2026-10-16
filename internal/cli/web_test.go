package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/web"
)

func TestWebServeFlagsAndOwnerCheck(t *testing.T) {
	var got []web.Options
	orig := serveWeb
	serveWeb = func(ctx context.Context, o web.Options) error { got = append(got, o); return nil }
	t.Cleanup(func() { serveWeb = orig })

	e := newEnv(t, nil)
	ownerHub := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	r := run(t, e, &App{API: ownerHub}, "web", "serve")
	expectCode(t, r, 0)
	if len(got) != 1 || got[0].Listen != web.DefaultListen || got[0].Dev || got[0].API == nil ||
		got[0].StateDir != filepath.Join(e["HOME"], ".local", "state", "pyre", "web") || len(got[0].Origins) != 1 || got[0].Origins[0] != web.DefaultOrigin {
		t.Fatalf("defaults: %+v\n%s", got, r)
	}
	r = run(t, e, &App{API: ownerHub}, "web", "serve", "--dev", "--listen", "127.0.0.1:7999", "--state-dir", "/tmp/synth-web-state")
	expectCode(t, r, 0)
	if o := got[1]; !o.Dev || o.Listen != "127.0.0.1:7999" || o.StateDir != "/tmp/synth-web-state" || !strings.Contains(r.stderr, "dev") {
		t.Fatalf("flags: %+v\n%s", o, r)
	}

	// Not loopback: refused before anything runs.
	r = run(t, e, &App{API: ownerHub}, "web", "serve", "--listen", "0.0.0.0:7390")
	if r.code == 0 || !strings.Contains(r.stderr, "loopback") {
		t.Fatalf("public listen should fail:\n%s", r)
	}
	// A session credential is not the owner.
	sess := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"}
	r = run(t, e, &App{API: mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &sess})}, "web", "serve")
	if r.code == 0 || !strings.Contains(r.stderr, "owner's client credentials") {
		t.Fatalf("session credential should fail:\n%s", r)
	}
	if len(got) != 2 {
		t.Fatalf("serve ran for a refused invocation: %d", len(got))
	}
}
