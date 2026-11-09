package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// limitAccounts is an account service that serves session.limit_resume.
type limitAccounts struct {
	fakeBinder
	mu      sync.Mutex
	room    LimitRoom
	resumed []string
}

func (a *limitAccounts) ResumeAfterLimit(ctx context.Context, sessionID, runID string) (contracts.LimitResumeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumed = append(a.resumed, runID)
	return contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: "acct_synth-b", NewRunID: "run_synth-cont", Kind: string(contracts.ContinuationNativeResume)}, nil
}

func (a *limitAccounts) LimitRoom(ctx context.Context, runID string) (LimitRoom, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.room, nil
}

func (n *node) limitResume(sessionID, runID, mode string) contracts.LimitResumeResult {
	n.t.Helper()
	raw, _ := json.Marshal(contracts.LimitResumeParams{SessionID: sessionID, RunID: runID, Mode: mode})
	v, err := n.d.actionLimitResume(context.Background(), raw)
	if err != nil {
		n.t.Fatalf("limit resume: %v", err)
	}
	return v.(contracts.LimitResumeResult)
}

func (n *node) outboxCount(messageID string) int {
	var c int
	n.db.QueryRow(`SELECT COUNT(*) FROM daemon_outbox WHERE message_id=?`, messageID).Scan(&c)
	return c
}

// Decision I70: a node with an account service advertises
// session.limit_resume; a live run held by its limit is asked to continue
// only once the hold lapsed and its account has room, with one stable
// message per run and limit; an ended run is continued only after a limit
// stop, through the account service.
func TestLimitResumeAction(t *testing.T) {
	w := daemontest.NewWorld()
	acc := &limitAccounts{}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{daemontest.New(w, contracts.HarnessClaude)}, Accounts: acc})
	ctx := context.Background()
	if hello, _ := n.d.Hello(ctx); !contracts.HasCap(hello.Capabilities, contracts.NodeCapLimitResume) {
		t.Fatalf("capabilities %v", hello.Capabilities)
	}
	var ambient AccountResolver = AmbientAccounts{}
	if _, ok := ambient.(LimitResumer); ok {
		t.Fatal("the ambient resolver must not advertise limit resumes")
	}
	sc := startCmd("ses_synth-lr", "run_synth-lr", contracts.HarnessClaude, t.TempDir())
	sc.SessionToken = "synthetic-session-token-lr"
	n.start(sc)

	// Its turn fails on the account's limit: held, and the limit is recorded.
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	w.Mutate("run_synth-lr", func(p *daemontest.Proc) {
		p.Limit = &contracts.LimitEvent{Kind: "usage_limit", At: now, ResetAt: &reset, Detail: "synthetic spend limit"}
	})
	n.d.PollNow(ctx, "run_synth-lr")
	if at := n.adapterData("run_synth-lr")[contracts.AdapterDataLimitAt]; at == "" {
		t.Fatal("limit time not recorded on the run")
	}
	if res := n.limitResume("ses_synth-lr", "run_synth-lr", contracts.LimitResumeInPlace); res.Outcome != contracts.LimitResumeNoRoom || res.ResetAt == nil {
		t.Fatalf("held: %+v", res)
	}
	if res := n.limitResume("ses_synth-lr", "run_synth-lr", contracts.LimitResumeContinue); res.Outcome != contracts.LimitResumeRefused {
		t.Fatalf("continue on a live run: %+v", res)
	}

	// The hold lapsed (the adapter no longer reports the limit) but the
	// account service still sees it at its limit: not yet.
	n.d.PollNow(ctx, "run_synth-lr")
	acc.room = LimitRoom{AccountID: "acct_synth-a", ResetAt: &reset, Why: "cooldown"}
	if res := n.limitResume("ses_synth-lr", "run_synth-lr", contracts.LimitResumeInPlace); res.Outcome != contracts.LimitResumeNoRoom {
		t.Fatalf("no room: %+v", res)
	}
	prompt := `[pyre] Continue where you left off (account "A" has room again after the usage limit).`
	acc.room = LimitRoom{OK: true, AccountID: "acct_synth-a", Prompt: prompt}
	res := n.limitResume("ses_synth-lr", "run_synth-lr", contracts.LimitResumeInPlace)
	if res.Outcome != contracts.LimitResumed || res.Kind != contracts.LimitResumeInPlace || res.NewRunID != "run_synth-lr" {
		t.Fatalf("in place: %+v", res)
	}
	id := limitResumeMessageID(n.run("run_synth-lr").Run)
	if body, ok := n.outboxBody(id); !ok || body != prompt {
		t.Fatalf("continue prompt %q (%v) under %s", body, ok, id)
	}
	// Asked again (the hub lost the answer): the same message, not a second.
	n.limitResume("ses_synth-lr", "run_synth-lr", contracts.LimitResumeInPlace)
	if c := n.outboxCount(id); c != 1 {
		t.Fatalf("%d continue prompts", c)
	}

	// The limit fallback stops it: only a continue resume applies now.
	if q, err := n.d.QuiesceRun(ctx, "ses_synth-lr", "run_synth-lr", contracts.AccountSwitchLimit, false); err != nil || !q.Exited {
		t.Fatalf("quiesce %+v %v", q, err)
	}
	if res := n.limitResume("ses_synth-lr", "run_synth-lr", contracts.LimitResumeInPlace); res.Outcome != contracts.LimitResumeRefused {
		t.Fatalf("in place on an ended run: %+v", res)
	}
	if res := n.limitResume("ses_synth-lr", "run_synth-old", contracts.LimitResumeContinue); res.Outcome != contracts.LimitResumeRefused || !strings.Contains(res.Detail, "run_synth-lr") {
		t.Fatalf("not the current run: %+v", res)
	}
	res = n.limitResume("ses_synth-lr", "run_synth-lr", contracts.LimitResumeContinue)
	if res.Outcome != contracts.LimitResumed || res.NewRunID != "run_synth-cont" || len(acc.resumed) != 1 {
		t.Fatalf("continue: %+v (account service asked %d times)", res, len(acc.resumed))
	}

	// A run stopped for something else is never continued.
	n.start(startCmd("ses_synth-other", "run_synth-other", contracts.HarnessClaude, t.TempDir()))
	if q, err := n.d.QuiesceRun(ctx, "ses_synth-other", "run_synth-other", contracts.AccountSwitchProactive, false); err != nil || !q.Exited {
		t.Fatalf("quiesce %+v %v", q, err)
	}
	if res := n.limitResume("ses_synth-other", "run_synth-other", contracts.LimitResumeContinue); res.Outcome != contracts.LimitResumeRefused || len(acc.resumed) != 1 {
		t.Fatalf("proactive stop: %+v", res)
	}
}
