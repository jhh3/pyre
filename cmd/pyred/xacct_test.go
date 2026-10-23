package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Synthetic setup tokens (obviously fake).
const (
	xacctTokA = "sk-ant-oat01-pyre-synthetic-xacct-a-0000"
	xacctTokB = "sk-ant-oat01-pyre-synthetic-xacct-b-0000"
)

type xacctTokens map[string]string

func (x xacctTokens) FetchAccountToken(_ context.Context, id string) (string, time.Time, error) {
	if t, ok := x[id]; ok {
		return t, time.Unix(1_790_000_000, 0), nil
	}
	return "", time.Time{}, contracts.Errorf(contracts.ErrNotFound, "no token")
}

type xacctReports struct {
	mu    sync.Mutex
	plans []contracts.FallbackPlan
}

func (r *xacctReports) Report(_ context.Context, kind contracts.ReportKind, payload any) error {
	if kind == contracts.ReportFallback {
		r.mu.Lock()
		r.plans = append(r.plans, payload.(contracts.FallbackPlan))
		r.mu.Unlock()
	}
	return nil
}

// TestAutoContinueOnLimitAcrossTokenAccounts drives pyred's real wiring
// (accounts service + daemon + accountsResolver/runController) with a fake
// Claude runtime: a run on token account A reports Claude's session limit;
// the node quiesces it and, with no human, resumes the SAME conversation
// natively on token account B (shared transcript store) with a short
// "continue where you left off" prompt.
func TestAutoContinueOnLimitAcrossTokenAccounts(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	db, err := sqlitex.Open(filepath.Join(base, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Usage probe: A has more headroom than B.
	msgs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := "0.30"
		if r.Header.Get("Authorization") == "Bearer "+xacctTokA {
			u = "0.10"
		}
		reset := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
		h := w.Header()
		h.Set("anthropic-ratelimit-unified-5h-utilization", u)
		h.Set("anthropic-ratelimit-unified-5h-reset", reset)
		h.Set("anthropic-ratelimit-unified-7d-utilization", "0.20")
		h.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10))
		h.Set("anthropic-ratelimit-unified-status", "allowed")
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	t.Cleanup(msgs.Close)

	ctl := &runController{}
	rep := &xacctReports{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := accounts.New(ctx, accounts.Config{NodeID: "node_xacct", DB: db,
		ProfileRoot: filepath.Join(base, "state", "profiles"), SharedRoot: filepath.Join(base, "state", "shared"),
		UserClaudeDir: filepath.Join(base, "no-claude"), UserClaudeJSON: filepath.Join(base, "no-claude.json"), UserCodexDir: filepath.Join(base, "no-codex"),
		Tokens: xacctTokens{"acct_synth-xa": xacctTokA, "acct_synth-xb": xacctTokB}, ClaudeMessagesURL: msgs.URL + "/v1/messages", HTTPClient: msgs.Client(),
		Controller: ctl, Reporter: rep, Logger: log, DefaultAccount: accounts.DefaultAccountSubscription})
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{}
	for id, label := range map[string]string{"acct_synth-xa": "synth-a", "acct_synth-xb": "synth-b"} {
		res, err := svc.Register(ctx, accounts.RegisterParams{Account: contracts.Account{AccountID: id, Provider: contracts.ProviderAnthropic,
			Label: label, Kind: "subscription"}, Harness: contracts.HarnessClaude, Source: contracts.SourceToken})
		if err != nil || !res.Verified {
			t.Fatalf("register %s: %+v %v", id, res, err)
		}
		dirs[id] = res.Profile.Dir
	}

	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	fa.Caps = append(fa.Caps, contracts.CapAccountProfile, contracts.CapAccountCrossResume)
	fa.ReachableOnly = true
	drep, err := daemontest.NewReporter(db)
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(daemon.Config{StateDir: filepath.Join(base, "state"), NodeID: "node_xacct", Adapters: []contracts.RuntimeAdapter{fa},
		DB: db, Reporter: drep, Accounts: accountsResolver{svc}, Logger: log,
		Poll: daemon.PollConfig{Interval: time.Hour}, HeartbeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctl.set(d)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	sc := contracts.StartRunCommand{Session: contracts.Session{SessionID: "ses_xacct", Harness: contracts.HarnessClaude, Title: "xacct", CreatedAt: time.Now()},
		RunID: "run_xacct1", Cwd: t.TempDir(), AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}}
	if res, err := d.HandleCommand(ctx, cmdOf(contracts.CmdStartRun, sc)); err != nil || res.Error != nil {
		t.Fatalf("start: %v %+v", err, res.Error)
	}
	d.WaitLaunches()
	p1, _ := w.Proc("run_xacct1")
	if p1.Launch.ProfileDir != dirs["acct_synth-xa"] {
		t.Fatalf("first run not on A (most headroom): %s", p1.Launch.ProfileDir)
	}
	// The conversation Claude writes under A is visible under B's profile.
	tx := filepath.Join(dirs["acct_synth-xa"], "projects", "-synth-work", "00000000-0000-4000-8000-00000000xacc.jsonl")
	if err := os.MkdirAll(filepath.Dir(tx), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tx, []byte(`{"type":"user","synthetic":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dirs["acct_synth-xb"], "projects", "-synth-work", filepath.Base(tx))); err != nil {
		t.Fatalf("transcript not shared with B's profile: %v", err)
	}

	// Claude hits the session limit (as its StopFailure hook reports it).
	reset := time.Now().Add(90 * time.Minute)
	w.Mutate("run_xacct1", func(p *daemontest.Proc) {
		p.Limit = &contracts.LimitEvent{Kind: "usage_limit", At: time.Now().UTC(), ResetAt: &reset,
			Detail: "rate_limit: You've hit your session limit · resets 3pm (America/Los_Angeles)"}
	})
	d.PollNow(ctx, "run_xacct1")
	d.WaitLaunches()

	rep.mu.Lock()
	plans := append([]contracts.FallbackPlan(nil), rep.plans...)
	rep.mu.Unlock()
	if len(plans) != 1 {
		t.Fatalf("plans %+v", plans)
	}
	pl := plans[0]
	if pl.Outcome != contracts.FallbackNativeResume || pl.FromAccountID != "acct_synth-xa" || pl.ToAccountID != "acct_synth-xb" || pl.NewRunID == "" || pl.UncertainWork {
		t.Fatalf("plan %+v", pl)
	}
	p2, ok := w.Proc(pl.NewRunID)
	if !ok {
		t.Fatalf("successor %s not launched", pl.NewRunID)
	}
	if p2.Launch.ProfileDir != dirs["acct_synth-xb"] || p2.Launch.Resume == nil || p2.Launch.Resume.ID != p1.Handle.NativeRef.ID {
		t.Fatalf("successor launch: profile %s resume %+v (want %s)", p2.Launch.ProfileDir, p2.Launch.Resume, p1.Handle.NativeRef.ID)
	}
	want := "Continue where you left off (switched from account \"synth-a\" to account \"synth-b\" because of the usage limit)"
	if !strings.Contains(p2.Launch.InitialPrompt, want) || strings.Contains(p2.Launch.InitialPrompt, "handoff") {
		t.Fatalf("successor prompt %q", p2.Launch.InitialPrompt)
	}
	cur, err := d.CurrentRun(ctx, "ses_xacct")
	if err != nil || cur.RunID != pl.NewRunID || cur.AccountID != "acct_synth-xb" {
		t.Fatalf("current run %+v %v", cur, err)
	}
	if p, _ := w.Proc("run_xacct1"); p.State != contracts.RunExited {
		t.Fatalf("limited run not quiesced: %s", p.State)
	}
	// A is held out of selection until its reset; a new run lands on B.
	sel, err := svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic})
	if err != nil || sel.AccountID != "acct_synth-xb" {
		t.Fatalf("selection after the limit: %+v %v", sel, err)
	}
	b, _ := json.Marshal(plans)
	if strings.Contains(string(b), "sk-ant-") {
		t.Fatal("token in fallback report")
	}
}
