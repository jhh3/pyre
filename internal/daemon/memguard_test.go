package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/memguard"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/supervisor"
)

// A resolved guard action is audited (a node.automation event with action
// memory_guard_kill, command redacted) and the session gets the note, as
// itself, held in the outbox while there is no hub.
func TestMemoryGuardKillAuditAndNote(t *testing.T) {
	ma := mock.NewAdapter()
	n := startNode(t, shortDir(t), Config{Adapters: []contracts.RuntimeAdapter{ma}, NodeName: "synth-box"})
	sc := startCmd("ses_synth-guard", "run_synth-g1", contracts.HarnessMock, t.TempDir())
	sc.SessionToken = "tok-synthetic-guard"
	n.start(sc)
	chosen := time.Unix(1_800_000_000, 0)
	k := memguard.Kill{PID: 4242, Name: "python3", Command: "python3 -I measure.py --token=sk-ant-synthetic0123456789abcdefghij", RSS: 23 << 30, Swap: 1 << 30,
		RunID: "run_synth-g1", SessionID: "ses_synth-guard", Node: "available 1.2G of 30G (4.0%), swap 7.2G of 8.0G (90%) used",
		Outcome: "killed with SIGKILL 10s after SIGTERM", SIGKILL: true, Chosen: chosen, Resolved: chosen.Add(10 * time.Second)}
	n.d.onGuardKill(k)

	var got *contracts.NodeAutomation
	for _, r := range n.rep.Reports(contracts.ReportEvent) {
		var ev contracts.Event
		if json.Unmarshal(r.Payload, &ev) != nil || ev.Kind != contracts.EvNodeAutomation {
			continue
		}
		var a contracts.NodeAutomation
		if json.Unmarshal(ev.Payload, &a) == nil && a.Action == contracts.AutomationMemoryGuardKill {
			got = &a
		}
	}
	if got == nil || got.RunID != "run_synth-g1" || got.SessionID != "ses_synth-guard" || !strings.Contains(got.Detail, "pid 4242 `python3 -I measure.py") ||
		!strings.Contains(got.Detail, "rss 23G swap 1.0G") || !strings.Contains(got.Detail, "SIGKILL") || strings.Contains(got.Detail, "sk-ant-synthetic") {
		t.Fatalf("automation report %+v", got)
	}
	key := resumenote.IDPart("ses_synth-guard") + "-4242-" + "tro8w0" // base36 of 1_800_000_000
	var body string
	waitFor(t, "guard note", func() bool {
		var ok bool
		body, ok = n.outboxBody("msg_memguard-" + key)
		return ok
	})
	for _, want := range []string{"[Pyre memory guard]", "keep synth-box responsive", "(pid 4242) held 23G RSS + 1.0G swap", "MemoryMax=<about twice its real peak, e.g. 32G>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("note lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "sk-ant-synthetic") {
		t.Fatalf("note leaks the token:\n%s", body)
	}

	// No session: audited, no note. A failed signal: audited, no note.
	n.d.onGuardKill(memguard.Kill{PID: 7, Name: "cargo", Outcome: "exited after SIGTERM", Chosen: chosen})
	n.d.onGuardKill(memguard.Kill{PID: 8, Name: "cargo", SessionID: "ses_synth-guard", Outcome: "signal failed: operation not permitted", Chosen: chosen})
	time.Sleep(100 * time.Millisecond)
	if _, ok := n.outboxBody("msg_memguard-" + resumenote.IDPart("ses_synth-guard") + "-8-tro8w0"); ok {
		t.Fatal("note sent for a failed signal")
	}
	if c := len(n.rep.Reports(contracts.ReportEvent)); c < 3 {
		t.Fatalf("%d event reports", c)
	}
}

func TestMemoryProtectionStatus(t *testing.T) {
	d := &Daemon{cfg: Config{}}
	if got := d.memoryProtection(context.Background()); got != "guard off" {
		t.Fatalf("off: %q", got)
	}
	d.cfg.MemoryGuard = &memguard.Config{AvailFrac: 0.05, SwapFrac: 0.8}
	if got := d.memoryProtection(context.Background()); memguard.Supported() && !strings.Contains(got, "guard on (") {
		t.Fatalf("on: %q", got)
	}
	d.cfg.RunScopes = &supervisor.RunScopeLimits{MemoryMax: "off"}
	if got := d.memoryProtection(context.Background()); d.cfg.RunScopes != nil && strings.Contains(got, "per-run scopes Memory") {
		t.Fatalf("scopes off but described: %q", got)
	}
}

func TestGuardRedact(t *testing.T) {
	for _, in := range []string{
		"tool --token abcdefSECRET1 --x", "tool --password hunter2SECRET", "tool --api-key=SECRETvalue123",
		"API_KEY=SECRETvalue123 python3 run.py", "curl https://synth:SECRETpass@example.invalid/x",
		"tool sk-ant-api03-SECRETsyntheticsyntheticsynthetic", "MY_SERVICE_TOKEN=SECRETabc tool",
	} {
		if out := guardRedact(in); strings.Contains(out, "SECRET") {
			t.Errorf("%q => %q", in, out)
		}
	}
	if out := guardRedact("python3 -I measure.py --rows 100"); out != "python3 -I measure.py --rows 100" {
		t.Fatalf("plain command changed: %q", out)
	}
	// Redaction happens before the audit text is clipped.
	long := strings.Repeat("x", 150) + " --token SECRETabcdefghijklmnop"
	if d := guardDetail(memguard.Kill{PID: 1, Command: long, Outcome: "exited after SIGTERM"}); strings.Contains(d, "SECRET") {
		t.Fatalf("clipped detail leaks: %q", d)
	}
}
