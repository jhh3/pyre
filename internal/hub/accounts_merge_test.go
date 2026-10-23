package hub

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func mergeTx(t *testing.T, h *Hub, fn func(tx *sql.Tx) error) {
	t.Helper()
	if err := sqlitex.Tx(context.Background(), h.db, fn); err != nil {
		t.Fatal(err)
	}
}

func accountsByLabel(t *testing.T, h *Hub, label string) []contracts.AccountView {
	t.Helper()
	page, err := h.ListAccounts(as(owner), contracts.ListAccountsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var out []contracts.AccountView
	for _, v := range page.Items {
		if strings.EqualFold(v.Account.Label, label) {
			out = append(out, v)
		}
	}
	return out
}

// An older CLI's add-token creates a token-only account with the label of
// the person's rota account; the hub folds it at once.
func TestSetTokenFoldsIntoSameLabelAccount(t *testing.T) {
	h, _ := newTokenHub(t)
	ctx := context.Background()
	nodeA := contracts.NewID(contracts.PrefixNode)
	rota, err := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic,
		Label: "synth-person", RotaRef: "synth-person@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	tokOnly, err := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic,
		Label: "synth-person", PerMachineIndependent: true})
	if err != nil {
		t.Fatal(err)
	}
	st, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: tokOnly.AccountID, Token: synthHubToken})
	if err != nil {
		t.Fatal(err)
	}
	if st.AccountID != rota.AccountID || !st.Set {
		t.Fatalf("status names %s, want the surviving %s: %+v", st.AccountID, rota.AccountID, st)
	}
	if got := accountsByLabel(t, h, "synth-person"); len(got) != 1 || got[0].Account.AccountID != rota.AccountID {
		t.Fatalf("want one account, got %+v", got)
	}
	acct, _ := h.getAccount(ctx, h.db, rota.AccountID)
	if acct.PerMachineIndependent || acct.RotaRef != "synth-person@example.invalid" {
		t.Fatalf("survivor changed: %+v", acct)
	}
	node := as(contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeA})
	// The old ID still fetches (a node learns the survivor's ID).
	f, err := h.FetchAccountToken(node, contracts.FetchAccountTokenRequest{AccountID: tokOnly.AccountID})
	if err != nil || f.Token != synthHubToken || f.AccountID != rota.AccountID {
		t.Fatalf("alias fetch: %v %v", f, err)
	}
	if l, err := h.ListAccountTokens(node, contracts.ListAccountTokensRequest{AccountID: tokOnly.AccountID}); err != nil || len(l) != 1 || l[0].AccountID != rota.AccountID {
		t.Fatalf("alias status: %+v %v", l, err)
	}
	// Replacing the token through either ID updates the survivor.
	if st, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: tokOnly.AccountID, Token: synthHubToken + "x"}); err != nil || st.AccountID != rota.AccountID {
		t.Fatalf("set via alias: %+v %v", st, err)
	}
}

// Duplicates that exist before the upgrade are folded by the pass, with
// running work repointed and later reports for the old ID accepted.
func TestFoldExistingDuplicateKeepsRunningWork(t *testing.T) {
	h, _ := newTokenHub(t)
	ctx := context.Background()
	nodeA, nodeB := contracts.NewID(contracts.PrefixNode), contracts.NewID(contracts.PrefixNode)
	// The token account first (no same-label account yet: nothing folds).
	tokOnly, _ := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic, Label: "synth-dup", PerMachineIndependent: true})
	if _, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: tokOnly.AccountID, Token: synthHubToken}); err != nil {
		t.Fatal(err)
	}
	rota, _ := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic, Label: "Synth-Dup", RotaRef: "synth-dup@example.invalid"})
	if got := accountsByLabel(t, h, "synth-dup"); len(got) != 2 {
		t.Fatalf("setup: want the duplicate, got %d", len(got))
	}
	now := h.now()
	run := contracts.Run{RunID: contracts.NewID(contracts.PrefixRun), SessionID: "ses_synth-run", NodeID: nodeA, Harness: contracts.HarnessClaude,
		AccountID: tokOnly.AccountID, State: contracts.RunRunning, StartedAt: now}
	asg := contracts.AccountAssignment{AssignmentID: contracts.NewID(contracts.PrefixAssignment), AccountID: tokOnly.AccountID, GrantID: contracts.NewID(contracts.PrefixGrant),
		CredentialKind: contracts.CredOAuthToken, RunID: run.RunID, SessionID: run.SessionID, NodeID: nodeA, AssignedAt: now}
	tokGrant := contracts.DeviceGrant{GrantID: asg.GrantID, AccountID: tokOnly.AccountID, NodeID: nodeA, Harness: contracts.HarnessClaude, Kind: contracts.CredOAuthToken, State: contracts.GrantValid, ObservedAt: now}
	rotaGrant := contracts.DeviceGrant{GrantID: contracts.NewID(contracts.PrefixGrant), AccountID: rota.AccountID, NodeID: nodeB, Harness: contracts.HarnessClaude, Kind: contracts.CredRotaCheckout, State: contracts.GrantValid, ObservedAt: now}
	obs := contracts.AccountObservation{AccountID: tokOnly.AccountID, Provider: contracts.ProviderAnthropic, ReportingNode: nodeA, ObservedAt: now,
		Source: contracts.UsageSourceMessagesProbe, Confidence: "high", Eligibility: contracts.EligibleYes, Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: 12}}}
	mergeTx(t, h, func(tx *sql.Tx) error {
		if err := h.putRunTx(ctx, tx, run, 0, ""); err != nil {
			return err
		}
		if err := h.ingestGrantTx(ctx, tx, nodeA, tokGrant, 1); err != nil {
			return err
		}
		if err := h.ingestGrantTx(ctx, tx, nodeB, rotaGrant, 1); err != nil {
			return err
		}
		if err := h.ingestObservationTx(ctx, tx, nodeA, obs, 2); err != nil {
			return err
		}
		return h.ingestAssignmentTx(ctx, tx, nodeA, asg, 3)
	})

	merged, skipped, err := h.FoldTokenAccounts(ctx)
	if err != nil || len(merged) != 1 || merged[0].From != tokOnly.AccountID || merged[0].Into != rota.AccountID || len(skipped) != 0 {
		t.Fatalf("fold: %+v %v %v", merged, skipped, err)
	}
	if again, _, _ := h.FoldTokenAccounts(ctx); len(again) != 0 {
		t.Fatalf("not idempotent: %+v", again)
	}
	views := accountsByLabel(t, h, "synth-dup")
	if len(views) != 1 || views[0].Account.AccountID != rota.AccountID {
		t.Fatalf("want one account: %+v", views)
	}
	v := views[0]
	if len(v.Assignments) != 1 || v.Assignments[0].AccountID != rota.AccountID || v.Assignments[0].RunID != run.RunID {
		t.Fatalf("assignment not repointed: %+v", v.Assignments)
	}
	if len(v.ByNode) != 1 || v.ByNode[0].ReportingNode != nodeA || v.ByNode[0].AccountID != rota.AccountID {
		t.Fatalf("observation not moved: %+v", v.ByNode)
	}
	if len(v.Grants) != 1 || v.Grants[0].GrantID != rotaGrant.GrantID {
		t.Fatalf("grants: %+v", v.Grants)
	}
	if r, err := h.getRun(ctx, h.db, run.RunID); err != nil || r.AccountID != rota.AccountID {
		t.Fatalf("run not repointed: %+v %v", r.AccountID, err)
	}
	if toks, _ := h.ListAccountTokens(as(owner), contracts.ListAccountTokensRequest{}); len(toks) != 1 || toks[0].AccountID != rota.AccountID {
		t.Fatalf("token not moved: %+v", toks)
	}

	// The node still names the old account: its reports land on the
	// survivor, the old grant is dropped, nothing re-creates the old account.
	rel := asg
	released := now.Add(time.Minute)
	rel.ReleasedAt = &released
	obs.ObservedAt = now.Add(2 * time.Minute)
	run.State = contracts.RunExited
	mergeTx(t, h, func(tx *sql.Tx) error {
		if err := h.ingestAssignmentTx(ctx, tx, nodeA, rel, 4); err != nil {
			return err
		}
		if err := h.ingestGrantTx(ctx, tx, nodeA, tokGrant, 5); err != nil {
			return err
		}
		if err := h.ingestObservationTx(ctx, tx, nodeA, obs, 6); err != nil {
			return err
		}
		return h.putRunTx(ctx, tx, run, 7, "")
	})
	if _, err := h.getAccount(ctx, h.db, tokOnly.AccountID); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("old account re-created: %v", err)
	}
	v = accountsByLabel(t, h, "synth-dup")[0]
	if len(v.Assignments) != 0 || len(v.Grants) != 1 || !v.ByNode[0].ObservedAt.Equal(obs.ObservedAt) {
		t.Fatalf("after old-ID reports: %+v", v)
	}
	if r, _ := h.getRun(ctx, h.db, run.RunID); r.AccountID != rota.AccountID {
		t.Fatalf("run report re-pointed back: %s", r.AccountID)
	}
	// An explicit policy naming the old ID selects the survivor.
	pol := contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: tokOnly.AccountID}
	if err := h.validateAccountPolicy(ctx, contracts.HarnessClaude, &pol); err != nil || pol.AccountID != rota.AccountID {
		t.Fatalf("policy: %+v %v", pol, err)
	}
}

func TestFoldSkipsAmbiguousLabels(t *testing.T) {
	h, _ := newTokenHub(t)
	ctx := context.Background()
	tokOnly, _ := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic, Label: "synth-amb"})
	if _, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: tokOnly.AccountID, Token: synthHubToken}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"one@example.invalid", "two@example.invalid"} {
		if _, err := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic, Label: "synth-amb", RotaRef: r}); err != nil {
			t.Fatal(err)
		}
	}
	// Other provider, same label: never a match.
	if _, err := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderOpenAI, Label: "synth-amb"}); err != nil {
		t.Fatal(err)
	}
	merged, skipped, err := h.FoldTokenAccounts(ctx)
	if err != nil || len(merged) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "several") {
		t.Fatalf("ambiguous: %+v %+v %v", merged, skipped, err)
	}
	if got := accountsByLabel(t, h, "synth-amb"); len(got) != 4 {
		t.Fatalf("accounts changed: %d", len(got))
	}
}

func TestSetTokenOnRotaAccountKeepsLease(t *testing.T) {
	h, _ := newTokenHub(t)
	ctx := context.Background()
	rota, _ := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic, Label: "synth-rota", RotaRef: "synth-rota@example.invalid"})
	st, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: rota.AccountID, Token: synthHubToken})
	if err != nil || st.AccountID != rota.AccountID {
		t.Fatalf("%+v %v", st, err)
	}
	if a, _ := h.getAccount(ctx, h.db, rota.AccountID); a.PerMachineIndependent {
		t.Fatal("a rota account with a token lost its fleet lease (PerMachineIndependent set)")
	}
}

// Folding a reserve account into a normal one keeps the survivor in
// reserve, and the audit log says why.
func TestFoldKeepsReserveTier(t *testing.T) {
	h, _ := newTokenHub(t)
	ctx := context.Background()
	rota, err := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic,
		Label: "synth-spare", RotaRef: "synth-spare@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	tokOnly, err := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic,
		Label: "synth-spare", PerMachineIndependent: true})
	if err != nil {
		t.Fatal(err)
	}
	reserve := contracts.TierReserve
	if _, err := h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: tokOp(), AccountID: tokOnly.AccountID, Tier: &reserve}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: tokOnly.AccountID, Token: synthHubToken}); err != nil {
		t.Fatal(err)
	}
	if acct, _ := h.getAccount(ctx, h.db, rota.AccountID); !acct.Reserve() {
		t.Fatalf("survivor lost the reserve tier: %+v", acct)
	}
	page, err := h.ListAudit(as(owner), contracts.ListAuditRequest{AccountID: rota.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Items {
		if e.Action == contracts.AuditAccountTierChanged && e.To == "reserve" && strings.Contains(e.Reason, "folded from "+tokOnly.AccountID) {
			return
		}
	}
	t.Fatalf("no tier audit on the survivor: %+v", page.Items)
}
