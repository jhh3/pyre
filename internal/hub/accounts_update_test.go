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
