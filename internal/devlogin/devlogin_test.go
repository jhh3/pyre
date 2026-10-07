package devlogin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// realOutput is the layout of codex-cli 0.160.1 `codex login --device-auth`
// captured on the owner's Mac (code replaced with a synthetic one).
const realOutput = "\nWelcome to Codex [v\x1b[90m0.160.1\x1b[0m]\n\x1b[90mOpenAI's command-line coding agent\x1b[0m\n\n" +
	"Follow these steps to sign in with ChatGPT using device code authorization:\n\n" +
	"1. Open this link in your browser and sign in to your account\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\n\n" +
	"2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n   \x1b[94mSYNT-H3TIC\x1b[0m\n\n" +
	"\x1b[90mContinue only if you started this login in Codex. If a website or another person gave you this code, cancel.\x1b[0m\n"

func TestParserRealCodexOutput(t *testing.T) {
	var p Parser
	ready := false
	for _, l := range strings.Split(realOutput, "\n") {
		if p.Feed(l) {
			ready = true
		}
	}
	if !ready || p.URL != "https://auth.openai.com/codex/device" || p.Code != "SYNT-H3TIC" || p.ExpiresIn != 15*time.Minute {
		t.Fatalf("parsed %+v", p)
	}
}

func TestParserVariants(t *testing.T) {
	for _, tc := range []struct {
		lines     []string
		url, code string
	}{
		{[]string{"Go to https://auth.openai.com/codex/device.", "Enter code: ABCD-EFGH"}, "https://auth.openai.com/codex/device", "ABCD-EFGH"},
		{[]string{"Your code is", "WXYZ-12345", "then open https://example.invalid/device?x=1"}, "https://example.invalid/device?x=1", "WXYZ-12345"},
		{[]string{"Welcome to Codex [v0.160.1]", "no code here"}, "", ""},
	} {
		var p Parser
		for _, l := range tc.lines {
			p.Feed(l)
		}
		if p.URL != tc.url || p.Code != tc.code {
			t.Errorf("%q: got %q %q", tc.lines, p.URL, p.Code)
		}
	}
}

type fakeAPI struct {
	accounts []c.AccountView
	upserts  []c.UpsertAccountRequest
	actions  []c.NodeActionRequest
	regErr   *c.Error
}

func (f *fakeAPI) InvokeNodeAction(_ context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	f.actions = append(f.actions, req)
	if f.regErr != nil {
		return c.NodeActionResult{Outcome: c.OutcomeFailed, Error: f.regErr}, nil
	}
	b, _ := json.Marshal(map[string]any{"provider_ref": "synthetic-acct-new", "verified": true})
	return c.NodeActionResult{Outcome: c.OutcomeSucceeded, Result: b}, nil
}

func (f *fakeAPI) ListAccounts(context.Context, c.ListAccountsRequest) (c.Page[c.AccountView], error) {
	return c.Page[c.AccountView]{Items: f.accounts}, nil
}

func (f *fakeAPI) UpsertAccount(_ context.Context, req c.UpsertAccountRequest) (c.Account, error) {
	f.upserts = append(f.upserts, req)
	id := req.AccountID
	if id == "" {
		id = "acct_synthetic"
	}
	return c.Account{AccountID: id, Provider: req.Provider, Label: req.Label, RotaRef: req.RotaRef, ProviderRef: req.ProviderRef, PerMachineIndependent: req.PerMachineIndependent}, nil
}

func TestRegisterCompletedLogin(t *testing.T) {
	v := View{State: StateCompleted, Provider: c.ProviderOpenAI, NodeID: "node_a", Email: "person@synthetic.invalid", RotaName: "person@synthetic.invalid"}
	f := &fakeAPI{}
	r, err := Register(context.Background(), f, v)
	if err != nil {
		t.Fatal(err)
	}
	if r.Account.Label != "person" || !r.Account.PerMachineIndependent || r.Account.ProviderRef != "synthetic-acct-new" || !r.Verified {
		t.Fatalf("registration = %+v", r)
	}
	if len(f.actions) != 1 || f.actions[0].Action != c.ActionAccountRegister || f.actions[0].NodeID != "node_a" {
		t.Fatalf("actions = %+v", f.actions)
	}
	// An existing hub account (same rota name) is reused with its label.
	f = &fakeAPI{accounts: []c.AccountView{{Account: c.Account{AccountID: "acct_old", Provider: c.ProviderOpenAI, Label: "work", RotaRef: v.RotaName}}}}
	r, err = Register(context.Background(), f, v)
	if err != nil || r.Account.AccountID != "acct_old" || r.Account.Label != "work" {
		t.Fatalf("reuse = %+v, %v", r, err)
	}
}

func TestRegisterRefusesTheLiveLogin(t *testing.T) {
	v := View{State: StateCompleted, Provider: c.ProviderOpenAI, NodeID: "node_a", Email: "me@synthetic.invalid", RotaName: "me@synthetic.invalid", LiveLogin: true}
	f := &fakeAPI{}
	if _, err := Register(context.Background(), f, v); !c.IsCode(err, c.ErrForbidden) || !strings.Contains(err.Error(), "already logged in to that account") {
		t.Fatalf("live: %v", err)
	}
	if len(f.upserts)+len(f.actions) != 0 {
		t.Fatal("a refused registration touched the hub")
	}
	// The node's own refusal is translated into the same plain words.
	v.LiveLogin = false
	f = &fakeAPI{regErr: c.Errorf(c.ErrForbidden, "rota account is the node's live default login; it is never checked out")}
	if _, err := Register(context.Background(), f, v); !strings.Contains(err.Error(), "Pyre already uses it") {
		t.Fatalf("node refusal: %v", err)
	}
	if _, err := Register(context.Background(), &fakeAPI{}, View{State: StateWaiting}); !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("incomplete: %v", err)
	}
}
