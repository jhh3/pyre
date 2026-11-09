package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/runtime/claude"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// stalledHub is the hub as a node behind a stalled link sees it: once
// stalled, every account-list read and token fetch hangs until its context
// ends (or the stall lifts).
type stalledHub struct {
	mu     sync.Mutex
	tokens map[string]string
	stall  chan struct{}
}

func (h *stalledHub) wait(ctx context.Context) error {
	h.mu.Lock()
	stall := h.stall
	h.mu.Unlock()
	if stall == nil {
		return nil
	}
	select {
	case <-stall:
		return nil
	case <-ctx.Done():
		return contracts.Errorf(contracts.ErrUnavailable, "hub request timed out: %v", ctx.Err())
	}
}

func (h *stalledHub) ListAccounts(ctx context.Context, _ contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	if err := h.wait(ctx); err != nil {
		return contracts.Page[contracts.AccountView]{}, err
	}
	return contracts.Page[contracts.AccountView]{}, nil
}

func (h *stalledHub) FetchAccountToken(ctx context.Context, id string) (string, time.Time, error) {
	if err := h.wait(ctx); err != nil {
		return "", time.Time{}, err
	}
	if t, ok := h.tokens[id]; ok {
		return t, time.Unix(1_790_000_000, 0), nil
	}
	return "", time.Time{}, contracts.Errorf(contracts.ErrNotFound, "no token")
}

// TestExecClaimWithStalledHub drives pyred's real wiring (accounts service,
// daemon, headless claude adapter, local socket handler) the way pyre run
// does when the hub link stalls (iss_…): the
// fleet view and every token refetch hang, yet the claim the CLI sends as
// soon as the hub recorded the start comes back within a few seconds with
// a binding on a token account, not a deadline error.
func TestExecClaimWithStalledHub(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	db, err := sqlitex.Open(filepath.Join(base, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	msgs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reset := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
		h := w.Header()
		h.Set("anthropic-ratelimit-unified-5h-utilization", "0.20")
		h.Set("anthropic-ratelimit-unified-5h-reset", reset)
		h.Set("anthropic-ratelimit-unified-7d-utilization", "0.20")
		h.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10))
		h.Set("anthropic-ratelimit-unified-status", "allowed")
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	t.Cleanup(msgs.Close)
	hub := &stalledHub{tokens: map[string]string{}}
	var skew atomic.Int64 // the accounts clock runs ahead to expire cached tokens
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := accounts.New(ctx, accounts.Config{NodeID: "node_stall", DB: db,
		ProfileRoot: filepath.Join(base, "state", "profiles"), SharedRoot: filepath.Join(base, "state", "shared"),
		UserClaudeDir: filepath.Join(base, "no-claude"), UserClaudeJSON: filepath.Join(base, "no-claude.json"), UserCodexDir: filepath.Join(base, "no-codex"),
		Tokens: hub, Fleet: hub, ClaudeMessagesURL: msgs.URL + "/v1/messages", HTTPClient: msgs.Client(),
		Controller: &runController{}, Reporter: &xacctReports{}, Logger: log, DefaultAccount: accounts.DefaultAccountSubscription,
		Now: func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acct_synth-s1", "acct_synth-s2", "acct_synth-s3"} {
		hub.tokens[id] = "sk-ant-oat01-pyre-synthetic-stall-" + id
		res, err := svc.Register(ctx, accounts.RegisterParams{Account: contracts.Account{AccountID: id, Provider: contracts.ProviderAnthropic,
			Label: id, Kind: "subscription"}, Harness: contracts.HarnessClaude, Source: contracts.SourceToken})
		if err != nil || !res.Verified {
			t.Fatalf("register %s: %+v %v", id, res, err)
		}
	}

	w := daemontest.NewWorld()
	sockDir, err := os.MkdirTemp("", "px")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	cb, err := claude.New(claude.Options{NodeID: "node_stall", TmuxSocket: filepath.Join(sockDir, "t.sock"), HookCommand: []string{"true"}, ProfileAsConfigDir: true})
	if err != nil {
		t.Fatal(err)
	}
	drep, err := daemontest.NewReporter(db)
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(daemon.Config{StateDir: filepath.Join(base, "state"), NodeID: "node_stall",
		Adapters: []contracts.RuntimeAdapter{daemontest.New(w, contracts.HarnessClaude)}, ExecAdapters: []contracts.RuntimeAdapter{claude.NewExec(cb)},
		DB: db, Reporter: drep, Accounts: accountsResolver{svc}, Logger: log,
		Poll: daemon.PollConfig{Interval: time.Hour}, HeartbeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	srv := httptest.NewServer(d.LocalHandler())
	t.Cleanup(srv.Close)

	// The link stalls; the cached tokens are past their refetch age.
	stall := make(chan struct{})
	hub.mu.Lock()
	hub.stall = stall
	hub.mu.Unlock()
	t.Cleanup(func() { close(stall) })
	skew.Store(int64(11 * time.Minute))

	secret := "synthetic-stall-claim-secret"
	start := time.Now()
	type answer struct {
		code int
		b    daemon.ExecBinding
		err  string
	}
	got := make(chan answer, 1)
	go func() {
		// pyre run's claim, with the wait its default 10s setup leaves.
		body, _ := json.Marshal(daemon.ExecClaimRequest{RunID: "run_stall", Secret: secret, ClientPID: os.Getpid(), WaitMS: 8000})
		resp, err := http.Post(srv.URL+daemon.PathLocalExecClaim, "application/json", bytes.NewReader(body))
		if err != nil {
			got <- answer{err: err.Error()}
			return
		}
		defer resp.Body.Close()
		var a answer
		a.code = resp.StatusCode
		raw, _ := io.ReadAll(resp.Body)
		if a.code == http.StatusOK {
			_ = json.Unmarshal(raw, &a.b)
		} else {
			a.err = string(raw)
		}
		got <- a
	}()
	sc := contracts.StartRunCommand{Session: contracts.Session{SessionID: "ses_stall", Harness: contracts.HarnessClaude, Title: "pyre run: stall",
		Tags: []string{contracts.TagPyreRun}, CreatedAt: time.Now()},
		RunID: "run_stall", Cwd: t.TempDir(), AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible},
		Args: []string{daemon.ExecArg + "=" + sha256Hex(secret)}}
	if res, err := d.HandleCommand(ctx, cmdOf(contracts.CmdStartRun, sc)); err != nil || res.Error != nil {
		t.Fatalf("start: %v %+v", err, res.Error)
	}
	var a answer
	select {
	case a = <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("claim not answered")
	}
	took := time.Since(start)
	if a.code != http.StatusOK {
		t.Fatalf("claim: %d %s (after %s)", a.code, a.err, took)
	}
	if took > 5*time.Second {
		t.Fatalf("claim took %s with a stalled hub", took)
	}
	if a.b.RunID != "run_stall" || a.b.AccountID == "" || a.b.Env["CLAUDE_CODE_OAUTH_TOKEN"] != hub.tokens[a.b.AccountID] {
		t.Fatalf("binding: run %s account %s", a.b.RunID, a.b.AccountID)
	}
	t.Logf("claim answered in %s with account %s", took.Round(time.Millisecond), a.b.AccountID)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
