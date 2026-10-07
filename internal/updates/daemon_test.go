package updates

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func stagePyred(t *testing.T, f *fixture, version string) {
	t.Helper()
	p := writeExe(t, filepath.Join(f.root, "rel", version, "pyred"), fakeScript(version, 0, 1, 1))
	if _, err := f.m.Stage(context.Background(), contracts.UpdateParams{Tool: "pyred", Version: version, Source: p}); err != nil {
		t.Fatal(err)
	}
}

func pyredState(t *testing.T, f *fixture, version string) string {
	t.Helper()
	m, _ := New(Options{StateDir: f.m.opt.StateDir})
	st, err := m.load()
	if err != nil {
		t.Fatal(err)
	}
	if _, r := findRec(st, "pyred", version); r != nil {
		return r.State
	}
	return ""
}

// The in-product pyred update: activation records the launcher it was made
// over and asks the daemon to restart; the launcher then hands off to the
// managed version, rolls back a first boot that never became healthy, and
// ignores an activation made over another launcher.
func TestDaemonHandoffLifecycle(t *testing.T) {
	var switched []string
	f := newFixture(t, func(o *Options) {
		o.DaemonBase = "0.1.0-synth-launcher"
		o.OnDaemonSwitch = func(v string) { switched = append(switched, v) }
	})
	ctx := context.Background()
	const launcher = "0.1.0-synth-launcher"

	// Nothing managed: the launcher runs itself.
	if dec, err := f.m.DaemonBoot(launcher); err != nil || dec.Exec != "" {
		t.Fatalf("no managed pyred: %+v %v", dec, err)
	}
	stagePyred(t, f, "0.2.0-synth")
	if dec, _ := f.m.DaemonBoot(launcher); dec.Exec != "" {
		t.Fatalf("staged (not active) pyred handed off: %+v", dec)
	}
	if _, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.2.0-synth"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(switched, ",") != "0.2.0-synth" {
		t.Fatalf("daemon not asked to restart: %v", switched)
	}
	if h := f.m.ReadHandoff(); h == nil || h.State != HandoffPending || h.Base != launcher || h.Previous != "" {
		t.Fatalf("handoff after activate = %+v", h)
	}
	// Boot 1: hand off, first boot watched.
	dec, err := f.m.DaemonBoot(launcher)
	if err != nil || !dec.Watch || dec.Version != "0.2.0-synth" || !strings.HasSuffix(dec.Exec, filepath.Join("0.2.0-synth", "bin", "pyred")) {
		t.Fatalf("handoff decision = %+v %v", dec, err)
	}
	// The managed daemon crashed (or was restarted) before it reported
	// healthy: the next launcher start rolls back to the launcher itself.
	dec, _ = f.m.DaemonBoot(launcher)
	if dec.Exec != "" || dec.Result == nil || dec.Result.Outcome != "failed_reverted" || pyredState(t, f, "0.2.0-synth") != StateFailed {
		t.Fatalf("unhealthy first boot not rolled back: %+v (state %s)", dec, pyredState(t, f, "0.2.0-synth"))
	}
	if _, err := os.Lstat(f.m.currentLink("pyred")); !os.IsNotExist(err) {
		t.Fatalf("current link kept after rollback: %v", err)
	}
	// The launcher may exec another daemon at once: the rollback result is
	// persisted and reported by the next daemon that wires its reporter.
	if n := f.m.FlushPendingResults(ctx); n != 1 || !strings.Contains(strings.Join(f.rep.outcomes(), ","), "update.result:0.2.0-synth:failed_reverted") {
		t.Fatalf("pending rollback result not reported: %d %v", n, f.rep.outcomes())
	}
	if n := f.m.FlushPendingResults(ctx); n != 0 {
		t.Fatalf("pending results reported twice: %d", n)
	}

	// A healthy version: boots directly afterwards, without a watchdog.
	stagePyred(t, f, "0.3.0-synth")
	if _, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.3.0-synth"}); err != nil {
		t.Fatal(err)
	}
	if dec, _ := f.m.DaemonBoot(launcher); dec.Version != "0.3.0-synth" || !dec.Watch {
		t.Fatalf("boot 0.3.0 = %+v", dec)
	}
	if err := f.m.MarkDaemonHealthy("0.3.0-synth"); err != nil {
		t.Fatal(err)
	}
	if dec, _ := f.m.DaemonBoot(launcher); dec.Version != "0.3.0-synth" || dec.Watch {
		t.Fatalf("healthy boot = %+v", dec)
	}

	// 0.4.0 fails its first boot (MarkDaemonFailed): back to 0.3.0, not the
	// launcher.
	stagePyred(t, f, "0.4.0-synth")
	if _, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.4.0-synth"}); err != nil {
		t.Fatal(err)
	}
	if h := f.m.ReadHandoff(); h.Previous != "0.3.0-synth" {
		t.Fatalf("previous = %+v", h)
	}
	if dec, _ := f.m.DaemonBoot(launcher); dec.Version != "0.4.0-synth" {
		t.Fatalf("boot 0.4.0 = %+v", dec)
	}
	if err := f.m.MarkDaemonFailed("0.4.0-synth", "synthetic: not connected to the hub within 1s"); err != nil {
		t.Fatal(err)
	}
	dec, _ = f.m.DaemonBoot(launcher)
	if dec.Version != "0.3.0-synth" || dec.Result == nil || dec.Result.RevertedTo != "0.3.0-synth" || !strings.Contains(dec.Result.Health, "not connected") {
		t.Fatalf("failed 0.4.0 not rolled back to 0.3.0: %+v %+v", dec, dec.Result)
	}
	if pyredState(t, f, "0.4.0-synth") != StateFailed || pyredState(t, f, "0.3.0-synth") != StateActive {
		t.Fatal("states after rollback")
	}

	// Another launcher (upgrade.sh installed a new release): the managed
	// activation is superseded, never a silent downgrade.
	dec, _ = f.m.DaemonBoot("0.5.0-synth-installed")
	if dec.Exec != "" || len(dec.Notes) != 1 || !strings.Contains(dec.Notes[0], "not handed off") || pyredState(t, f, "0.3.0-synth") != StatePrevious {
		t.Fatalf("superseded activation = %+v", dec)
	}
}

// update rollback --tool pyred with no previous managed version returns to
// the installed pyred (the launcher) and restarts the daemon on it.
func TestPyredRollbackToLauncher(t *testing.T) {
	var switched []string
	f := newFixture(t, func(o *Options) {
		o.DaemonBase = "0.1.0-synth-launcher"
		o.External = map[string]string{"pyred": "/synthetic/launcher/pyred"}
		o.OnDaemonSwitch = func(v string) { switched = append(switched, "<"+v+">") }
	})
	ctx := context.Background()
	stagePyred(t, f, "0.2.0-synth")
	if _, err := f.m.Activate(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.2.0-synth"}); err != nil {
		t.Fatal(err)
	}
	tv, err := f.m.Rollback(ctx, contracts.UpdateParams{Tool: "pyred"})
	if err != nil || tv.Version != "0.1.0-synth-launcher" || tv.Path != "/synthetic/launcher/pyred" {
		t.Fatalf("rollback to launcher = %+v %v", tv, err)
	}
	if strings.Join(switched, ",") != "<0.2.0-synth>,<>" || f.m.ReadHandoff() != nil || f.m.ActivePyred() != "" {
		t.Fatalf("switched %v handoff %+v active %q", switched, f.m.ReadHandoff(), f.m.ActivePyred())
	}
	if dec, _ := f.m.DaemonBoot("0.1.0-synth-launcher"); dec.Exec != "" {
		t.Fatalf("launcher handed off after rollback: %+v", dec)
	}
	// Rolling forward again from previous works and re-records the base.
	if _, err := f.m.Rollback(ctx, contracts.UpdateParams{Tool: "pyred", Version: "0.2.0-synth"}); err != nil {
		t.Fatal(err)
	}
	if dec, _ := f.m.DaemonBoot("0.1.0-synth-launcher"); dec.Version != "0.2.0-synth" {
		t.Fatalf("roll forward = %+v", dec)
	}
	// Other tools keep the old behaviour.
	if _, err := f.m.Rollback(ctx, contracts.UpdateParams{Tool: "claude"}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("claude rollback without previous: %v", err)
	}
}
