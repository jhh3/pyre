package hub

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestUpdateAccountLabelOnly(t *testing.T) {
	x := newHarness(t)
	op := func() contracts.Mutation {
		return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
	}
	a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: op(), Provider: contracts.ProviderAnthropic,
		Label: "synth-laptop", RotaRef: "synth@example.invalid", ProviderRef: "synthetic-uuid-1", PerMachineIndependent: true})
	if err != nil {
		t.Fatal(err)
	}
	label := "  synth "
	got, err := x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: op(), AccountID: a.AccountID, Label: &label})
	if err != nil || got.Label != "synth" || got.RotaRef != a.RotaRef || got.ProviderRef != a.ProviderRef || !got.PerMachineIndependent || got.Disabled {
		t.Fatalf("update: %+v %v", got, err)
	}
	page, err := x.h.ListAccounts(as(owner), contracts.ListAccountsRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Account.Label != "synth" {
		t.Fatalf("list after update: %+v %v", page.Items, err)
	}
	off := true
	if got, err := x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: op(), AccountID: a.AccountID, Disabled: &off}); err != nil || !got.Disabled || got.Label != "synth" {
		t.Fatalf("disable: %+v %v", got, err)
	}
	empty := " "
	for name, req := range map[string]contracts.UpdateAccountRequest{
		"empty label": {Mutation: op(), AccountID: a.AccountID, Label: &empty},
		"nothing":     {Mutation: op(), AccountID: a.AccountID},
		"bad id":      {Mutation: op(), AccountID: "nope", Label: &label},
	} {
		if _, err := x.h.UpdateAccount(as(owner), req); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	missing := contracts.NewID(contracts.PrefixAccount)
	if _, err := x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: op(), AccountID: missing, Label: &label}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	sess := contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"}
	if _, err := x.h.UpdateAccount(as(sess), contracts.UpdateAccountRequest{Mutation: op(), AccountID: a.AccountID, Label: &label}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Errorf("non-owner: %v", err)
	}
}

// A google (agy) account can be recorded, e.g. naming a machine's own agy
// login by its email; unknown providers stay refused.
func TestUpsertAccountProviders(t *testing.T) {
	x := newHarness(t)
	op := func() contracts.Mutation {
		return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
	}
	if a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: op(), Provider: contracts.ProviderGoogle,
		Label: "synth-agy", RotaRef: "synth-agy@example.invalid"}); err != nil || a.Provider != contracts.ProviderGoogle {
		t.Fatalf("google: %+v %v", a, err)
	}
	if _, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: op(), Provider: "synthetic-cloud", Label: "x"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("unknown provider: %v", err)
	}
}

// The tier is set by accounts.update ("normal" is stored as empty), kept by
// a later upsert (re-registration), refused when unknown, and audited.
func TestUpdateAccountTier(t *testing.T) {
	x := newHarness(t)
	op := func() contracts.Mutation {
		return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
	}
	a, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: op(), Provider: contracts.ProviderAnthropic, Label: "synth"})
	if err != nil {
		t.Fatal(err)
	}
	reserve, normal, bad := contracts.TierReserve, contracts.TierNormal, contracts.AccountTier("spare")
	got, err := x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: op(), AccountID: a.AccountID, Tier: &reserve})
	if err != nil || !got.Reserve() || got.Label != "synth" {
		t.Fatalf("reserve: %+v %v", got, err)
	}
	if got, err := x.h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: op(), AccountID: a.AccountID, Provider: contracts.ProviderAnthropic, Label: "synth"}); err != nil || !got.Reserve() {
		t.Fatalf("upsert dropped the tier: %+v %v", got, err)
	}
	page, err := x.h.ListAccounts(as(owner), contracts.ListAccountsRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Account.Tier != contracts.TierReserve {
		t.Fatalf("list: %+v %v", page.Items, err)
	}
	if _, err := x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: op(), AccountID: a.AccountID, Tier: &bad}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("unknown tier: %v", err)
	}
	got, err = x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: op(), AccountID: a.AccountID, Tier: &normal})
	if err != nil || got.Tier != "" || got.TierName() != contracts.TierNormal {
		t.Fatalf("normal: %+v %v", got, err)
	}
	// Setting the same tier again records nothing.
	if _, err := x.h.UpdateAccount(as(owner), contracts.UpdateAccountRequest{Mutation: op(), AccountID: a.AccountID, Tier: &normal}); err != nil {
		t.Fatal(err)
	}
	var tiers []string
	for _, e := range x.audit(contracts.ListAuditRequest{AccountID: a.AccountID}) {
		if e.Action == contracts.AuditAccountTierChanged {
			tiers = append(tiers, e.From+">"+e.To)
			if e.Actor.Kind != "owner" {
				t.Fatalf("actor %+v", e.Actor)
			}
		}
	}
	if len(tiers) != 2 || tiers[0] != "reserve>normal" || tiers[1] != "normal>reserve" {
		t.Fatalf("tier audit (newest first): %v", tiers)
	}
	wantSummary(t, findAudit(t, x.audit(contracts.ListAuditRequest{AccountID: a.AccountID}), contracts.AuditAccountTierChanged), "moved account synth to the normal tier — by owner")
}
