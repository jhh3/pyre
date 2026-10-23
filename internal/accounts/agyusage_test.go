package accounts

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// agyUsageJSON is the shape `agy -p "/usage" --output-format json` prints
// (agy 2026-10, numbers synthetic).
const agyUsageJSON = `{"conversation_id":"","status":"SUCCESS","response":"…","duration_seconds":0,"num_turns":0,
"command":{"name":"usage","data":{"groups":[
 {"name":"Gemini Models","buckets":[
  {"id":"gemini-weekly","window":"weekly","remaining_fraction":0.75,"reset_time":"2026-10-25T16:47:07Z"},
  {"id":"gemini-5h","window":"5h","remaining_fraction":0.9,"reset_time":"2026-10-19T02:47:07Z"}]},
 {"name":"Claude and GPT models","buckets":[
  {"id":"3p-weekly","window":"weekly","remaining_fraction":0.5,"reset_time":"2026-10-25T22:16:12Z"},
  {"id":"3p-5h","window":"5h","remaining_fraction":1,"reset_time":"2026-10-19T03:16:12Z"}]},
 {"name":"Gemini 4 Argon","buckets":[
  {"id":"gemini-argon-wtu-weekly","window":"weekly","remaining_fraction":0.96,"reset_time":"2026-10-25T16:35:56Z"}]}]}}}`

// Each window is the most used bucket across agy's model groups.
func TestParseAgyUsage(t *testing.T) {
	ws, err := ParseAgyUsage([]byte(agyUsageJSON))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]contracts.UsageWindow{}
	for _, w := range ws {
		got[w.Name] = w
	}
	if len(ws) != 2 || got["seven_day"].UsedPercent != 50 || got["five_hour"].UsedPercent != 10 {
		t.Fatalf("windows: %+v", ws)
	}
	if r := got["seven_day"].ResetAt; r == nil || !r.Equal(time.Date(2026, 10, 25, 22, 16, 12, 0, time.UTC)) {
		t.Fatalf("weekly reset is the binding bucket's: %v", r)
	}
	for _, bad := range []string{`not json`, `{"status":"SUCCESS"}`, `{"status":"ERROR","command":{"name":"usage"}}`,
		`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"buckets":[{"window":"daily","remaining_fraction":0.5}]}]}}}`} {
		if _, err := ParseAgyUsage([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// fakeAgy is a synthetic agy for the usage read: `--version` prints
// version; otherwise it records "$PWD|args|env" to calls (one line) and
// prints out (after running pre, a shell snippet).
type fakeAgy struct {
	e                     *testEnv
	dir, bin, calls, outF string
	verF, preF            string
}

func newFakeAgy(t *testing.T, version string) *fakeAgy {
	t.Helper()
	f := &fakeAgy{}
	f.e = newEnv(t, func(c *Config) {
		base := filepath.Dir(c.UserCodexDir)
		f.dir = filepath.Join(base, ".gemini", "antigravity-cli")
		f.bin = filepath.Join(base, "bin", "agy")
		c.UserAgyDir, c.AgyBin = f.dir, f.bin
	})
	bindir := filepath.Dir(f.bin)
	f.calls, f.outF, f.verF, f.preF = filepath.Join(bindir, "calls"), filepath.Join(bindir, "out.json"), filepath.Join(bindir, "version"), filepath.Join(bindir, "pre.sh")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(t, f.outF, agyUsageJSON)
	f.write(t, f.verF, version)
	f.write(t, f.preF, "")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo \"agy $(cat " + f.verF + ")\"; exit 0; fi\n" +
		"echo \"$PWD|$*|$(env | sort | tr '\\n' ' ')\" >> " + f.calls + "\n. " + f.preF + "\ncat " + f.outF + "\n"
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeAgy) write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// signIn writes a synthetic agy sign-in for email and marks agy used now.
func (f *fakeAgy) signIn(t *testing.T, email string) {
	t.Helper()
	_ = os.MkdirAll(f.dir, 0o700)
	writeJSON(t, filepath.Join(f.dir, "antigravity-oauth-token"), map[string]any{"token": map[string]any{"access_token": "pyre-synthetic-access"},
		"id_token": synthJWT(map[string]any{"email": email, "sub": "sub-" + email})}, 0o600)
	f.write(t, filepath.Join(f.dir, "history.jsonl"), "{}\n")
}

func (f *fakeAgy) lines() []string {
	b, _ := os.ReadFile(f.calls)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// next makes the next pass due.
func (f *fakeAgy) next() { f.e.clock.Advance(agyUsageEvery) }

// The observer reads agy's quota with a fake agy: the exact read-only
// command, in a scratch directory (removed after) with its log there, a
// minimal environment, at most once per 5 minutes, only where agy is
// signed in; the output is never logged.
func TestObserveAgyUsage(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "pyre-synthetic-key")
	t.Setenv("AGY_ADC_AUTH", "1")
	// The fake records its physical cwd; the scratch dir comes from
	// os.TempDir, a symlink on macOS (/var -> /private/var).
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	f := newFakeAgy(t, "1.4.2")
	e, ctx := f.e, context.Background()

	// Not signed in: agy is not offered here, nothing runs.
	e.svc.observeAgyUsage(ctx)
	if len(f.lines()) != 0 || e.svc.OwnUsage(contracts.HarnessAgy) != nil {
		t.Fatal("agy ran without a sign-in")
	}
	f.signIn(t, "synth-agy@example.invalid")
	f.next()
	e.svc.observeAgyUsage(ctx)
	u := e.svc.OwnUsage(contracts.HarnessAgy)
	if u == nil || len(u.Windows) != 2 || u.Source != "agy:/usage" || !u.ObservedAt.Equal(e.clock.Now()) {
		t.Fatalf("reading: %+v", u)
	}
	if e.svc.OwnUsage(contracts.HarnessClaude) != nil {
		t.Fatal("claude has no harness-reported usage")
	}
	c := f.lines()
	if len(c) != 1 {
		t.Fatalf("calls: %q", c)
	}
	parts := strings.SplitN(c[0], "|", 3)
	dir, args, env := parts[0], parts[1], parts[2]
	if args != "-p /usage --output-format json --print-timeout 15s --log-file "+filepath.Join(dir, "agy.log") {
		t.Fatalf("args: %q", args)
	}
	if !strings.Contains(filepath.Base(dir), "pyre-agy-usage-") {
		t.Fatalf("not run in a scratch dir: %s", dir)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("scratch dir left behind: %s", dir)
	}
	if strings.Contains(env, "GEMINI_API_KEY") || strings.Contains(env, "AGY_ADC_AUTH") || !strings.Contains(env, "HOME=") {
		t.Fatalf("env: %s", env)
	}

	// Throttled: a second pass within 5 minutes does not run agy.
	e.clock.Advance(agyUsageEvery - time.Second)
	e.svc.observeAgyUsage(ctx)
	if n := len(f.lines()); n != 1 {
		t.Fatalf("calls within 5m: %d", n)
	}
	// A failed read keeps the last reading (it ages) and logs no output.
	f.write(t, f.outF, `{"token":"pyre-synthetic-leak"}`)
	e.clock.Advance(time.Second)
	e.svc.observeAgyUsage(ctx)
	if n := len(f.lines()); n != 2 {
		t.Fatalf("calls after 5m: %d", n)
	}
	if u2 := e.svc.OwnUsage(contracts.HarnessAgy); u2 == nil || !u2.ObservedAt.Equal(u.ObservedAt) {
		t.Fatalf("failed read replaced the reading: %+v", u2)
	}
	if l := e.logs.String(); strings.Contains(l, "pyre-synthetic") {
		t.Fatalf("output logged: %s", l)
	}
	// Another Google login on agy: the old reading is not reported for it.
	f.signIn(t, "synth-other@example.invalid")
	if u3 := e.svc.OwnUsage(contracts.HarnessAgy); u3 != nil {
		t.Fatalf("reading of the previous login reported: %+v", u3)
	}
	// Signed out: the reading goes.
	f.signIn(t, "synth-agy@example.invalid")
	_ = os.Remove(filepath.Join(f.dir, "antigravity-oauth-token"))
	f.next()
	e.svc.observeAgyUsage(ctx)
	f.signIn(t, "synth-agy@example.invalid")
	if u4 := e.svc.OwnUsage(contracts.HarnessAgy); u4 != nil {
		t.Fatalf("reading kept after sign-out: %+v", u4)
	}
}

// Older agy sends "/usage" to the model as a prompt: never read there.
// A reply that shows a turn or a conversation disables the read for that
// version (one warning); a new version reads again.
func TestObserveAgyUsageVersionAndTripwire(t *testing.T) {
	f := newFakeAgy(t, "1.1.10")
	e, ctx := f.e, context.Background()
	f.signIn(t, "synth-agy@example.invalid")
	e.svc.observeAgyUsage(ctx)
	if len(f.lines()) != 0 {
		t.Fatalf("read on agy 1.1.10: %q", f.lines())
	}
	f.write(t, f.verF, "1.1.11")
	f.write(t, f.outF, strings.Replace(agyUsageJSON, `"num_turns":0`, `"num_turns":1`, 1))
	f.next()
	e.svc.observeAgyUsage(ctx)
	if len(f.lines()) != 1 || e.svc.OwnUsage(contracts.HarnessAgy) != nil {
		t.Fatalf("turn reply accepted: %q", f.lines())
	}
	if !strings.Contains(e.logs.String(), "disabled for this agy version") {
		t.Fatalf("no warning: %s", e.logs.String())
	}
	f.next()
	e.svc.observeAgyUsage(ctx)
	if len(f.lines()) != 1 {
		t.Fatal("read again on the disabled version")
	}
	f.write(t, f.verF, "1.4.2")
	f.write(t, f.outF, agyUsageJSON)
	f.next()
	e.svc.observeAgyUsage(ctx)
	if len(f.lines()) != 2 || e.svc.OwnUsage(contracts.HarnessAgy) == nil {
		t.Fatal("new version not read")
	}
}

// A machine where agy was not used for 7 days is left alone (each agy
// start may run its auto-updater).
func TestObserveAgyUsageOnlyWhenUsed(t *testing.T) {
	f := newFakeAgy(t, "1.4.2")
	e, ctx := f.e, context.Background()
	f.signIn(t, "synth-agy@example.invalid")
	old := e.clock.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(f.dir, "history.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	e.svc.observeAgyUsage(ctx)
	if len(f.lines()) != 0 {
		t.Fatal("read on a machine where agy is not used")
	}
	_ = os.MkdirAll(filepath.Join(f.dir, "conversations"), 0o700)
	f.write(t, filepath.Join(f.dir, "conversations", "c.pb"), "x")
	f.next()
	e.svc.observeAgyUsage(ctx)
	if len(f.lines()) != 1 {
		t.Fatal("a fresh conversation did not count as use")
	}
}

// A descendant of agy that keeps stdout open cannot hold the read past
// its timeout: the process group is killed, and a descendant outside the
// group (setsid) only holds it for the bounded wait. The observer's kick
// returns at once either way.
func TestObserveAgyUsageLingeringChild(t *testing.T) {
	cases := map[string]string{"in group": "sleep 60 &\nsleep 60\n"}
	if _, err := exec.LookPath("setsid"); err == nil {
		cases["own session"] = "setsid sleep 9 &\nsleep 60\n"
	}
	for name, pre := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeAgy(t, "1.4.2")
			e := f.e
			f.signIn(t, "synth-agy@example.invalid")
			f.write(t, f.preF, pre)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			e.svc.kickAgyUsage(ctx)
			if d := time.Since(start); d > 100*time.Millisecond {
				t.Fatalf("kick blocked %v", d)
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				e.svc.agy.mu.Lock()
				busy := e.svc.agy.inflight
				e.svc.agy.mu.Unlock()
				if !busy {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("agy read still running 15s after a 1s deadline")
				}
				time.Sleep(50 * time.Millisecond)
			}
			if d := time.Since(start); d > time.Second+agyUsageWaitDelay+3*time.Second {
				t.Fatalf("read took %v", d)
			}
			if e.svc.OwnUsage(contracts.HarnessAgy) != nil {
				t.Fatal("a killed read produced a reading")
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{{"1.1.11", true}, {"1.1.10", false}, {"1.4.2", true}, {"1.10.0", true}, {"0.9.99", false}, {"2.0.0", true}} {
		if got := versionAtLeast(tc.v, "1.1.11"); got != tc.want {
			t.Errorf("%s: %v", tc.v, got)
		}
	}
}
