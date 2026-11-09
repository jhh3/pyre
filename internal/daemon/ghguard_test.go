package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/ghguard"
)

// Runs get the gh shim first on PATH (a link to the pyre CLI), and the
// shim's calls are counted, throttled and warned per session.
func TestGHShimOnPathAndCallBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink shim")
	}
	cli := filepath.Join(t.TempDir(), "pyre")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{nodeID: "node_synth", cfg: Config{StateDir: t.TempDir(), PyreBinDir: "/opt/synth/bin", PyreCLI: cli}}
	d.cfg.defaults()
	cmd := contracts.StartRunCommand{RunID: "run_synth", Session: contracts.Session{SessionID: "ses_synth", Harness: contracts.HarnessClaude}}
	env := d.runEnv(map[string]string{"PATH": "/usr/bin"}, cmd, t.TempDir())
	dir := env[ghguard.EnvShimDir]
	if dir == "" || env["PATH"] != dir+string(os.PathListSeparator)+"/opt/synth/bin"+string(os.PathListSeparator)+"/usr/bin" {
		t.Fatalf("PATH %q shim dir %q", env["PATH"], dir)
	}
	if target, err := os.Readlink(filepath.Join(dir, "gh")); err != nil || target != cli {
		t.Fatalf("shim link -> %q, %v", target, err)
	}
	if off := d.runEnv(map[string]string{"PATH": "/usr/bin", ghguard.EnvGuard: "off"}, cmd, t.TempDir()); strings.Contains(off["PATH"], dir) {
		t.Fatalf("guard off still puts the shim on PATH: %q", off["PATH"])
	}

	// A low sample: the first call warns, later ones within 5 min do not.
	d.gh.sampleGH = func(context.Context) ([]byte, error) {
		return []byte(`{"resources":{"core":{"limit":5000,"remaining":400,"reset":1793833260},"graphql":{"limit":5000,"remaining":5000}}}`), nil
	}
	d.ghRefresh()
	deadline := time.Now().Add(5 * time.Second)
	for d.ghBudget() == nil || d.ghBudget().ObservedAt.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("no rate-limit sample")
		}
		time.Sleep(10 * time.Millisecond)
	}
	first := d.ghCall("ses_synth", ghguard.CallRequest{RunID: "run_synth", Command: "api"})
	if !strings.Contains(first.Warn, "core 400/5000 left") || first.WaitMS != 0 || first.Refuse {
		t.Fatalf("first call: %+v", first)
	}
	var last ghguard.CallResponse
	for i := 0; i < ghguard.DefaultBurst+20; i++ {
		last = d.ghCall("ses_synth", ghguard.CallRequest{RunID: "run_synth", Command: "api"})
		if last.Warn != "" {
			t.Fatalf("warned again on call %d", i)
		}
	}
	if !last.Refuse || !strings.Contains(last.Message, "ses_synth") || !strings.Contains(last.Message, "at most once a minute") {
		t.Fatalf("tight loop not refused: %+v", last)
	}
	b := d.ghBudget()
	if b == nil || b.Core.Remaining != 400 || len(b.TopSessions) != 1 || b.TopSessions[0].SessionID != "ses_synth" || b.TopSessions[0].Refused == 0 {
		t.Fatalf("budget %+v", b)
	}
}

// The shim's endpoint is mounted on the local socket handler.
func TestGHCallEndpointMounted(t *testing.T) {
	d := &Daemon{nodeID: "node_synth", cfg: Config{StateDir: t.TempDir()}}
	d.cfg.defaults()
	d.gh.sampleGH = func(context.Context) ([]byte, error) { return nil, os.ErrNotExist }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://pyred"+ghguard.PathCall, strings.NewReader(`{"run_id":"run_synth","command":"pr view"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pyre-Protocol", "1")
	d.LocalHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var resp ghguard.CallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Refuse || resp.WaitMS != 0 {
		t.Fatalf("resp %+v %v", resp, err)
	}
	if top, total := d.gh.limiter().Top(d.now(), 5); total != 1 || top[0].SessionID != "owner" {
		t.Fatalf("not counted: %+v %d", top, total)
	}
}
