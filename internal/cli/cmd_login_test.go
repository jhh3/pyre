package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/devlogin"
)

// fakeLoginNode answers the account.device_login* actions like a node
// (waiting, then completed after a few status reads); every other call goes
// to the mock hub.
type fakeLoginNode struct {
	c.API
	mu       sync.Mutex
	live     bool
	actions  []string
	statuses int
}

func (f *fakeLoginNode) InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, req.Action)
	view := devlogin.View{LoginID: "dlogin_synth", Provider: c.ProviderOpenAI, NodeID: req.NodeID, State: devlogin.StateWaiting,
		VerificationURL: "https://auth.openai.com/codex/device", UserCode: "SYNT-H3TIC"}
	reply := func(v any) (c.NodeActionResult, error) {
		b, _ := json.Marshal(v)
		return c.NodeActionResult{CommandID: "cmd_synth", Outcome: c.OutcomeSucceeded, Result: b}, nil
	}
	switch req.Action {
	case devlogin.ActionStart:
		var p devlogin.Params
		_ = json.Unmarshal(req.Params, &p)
		if p.Provider == c.ProviderAnthropic {
			return c.NodeActionResult{CommandID: "cmd_synth", Outcome: c.OutcomeFailed, Error: c.AsError(c.Errorf(c.ErrUnsupported, "Claude Code has no device-code login"))}, nil
		}
		return reply(view)
	case devlogin.ActionStatus:
		f.statuses++
		if f.statuses >= 2 {
			view.State, view.UserCode = devlogin.StateCompleted, ""
			view.Email, view.RotaName, view.LiveLogin = "person@synthetic.invalid", "person@synthetic.invalid", f.live
		}
		return reply(view)
	case devlogin.ActionCancel:
		view.State = devlogin.StateCancelled
		return reply(view)
	}
	f.mu.Unlock()
	defer f.mu.Lock()
	return f.API.InvokeNodeAction(ctx, req)
}

func withOpener(t *testing.T) *[]string {
	var opened []string
	old := openURL
	openURL = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openURL = old })
	return &opened
}

func TestAccountsLoginRegisters(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	opened := withOpener(t)
	node := &fakeLoginNode{API: h.b}
	r := run(t, e, &App{API: node}, "accounts", "login", "--node", "node_synth-mac", "--register", "--poll", "0s")
	expectCode(t, r, 0)
	for _, want := range []string{"https://auth.openai.com/codex/device", "SYNT-H3TIC", "logged in as person@synthetic.invalid", "registered as Pyre account person"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("missing %q:\n%s", want, r)
		}
	}
	if len(*opened) != 1 || (*opened)[0] != "https://auth.openai.com/codex/device" {
		t.Fatalf("opened %v", *opened)
	}
	ctx := c.WithPrincipal(context.Background(), testOwner)
	page, _ := h.b.ListAccounts(ctx, c.ListAccountsRequest{Provider: c.ProviderOpenAI})
	found := false
	for _, av := range page.Items {
		if av.Account.RotaRef == "person@synthetic.invalid" {
			found = av.Account.Label == "person" && av.Account.PerMachineIndependent
		}
	}
	if !found {
		t.Fatalf("hub account not created as expected: %+v", page.Items)
	}
}

func TestAccountsLoginLiveLoginNotRegistered(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	withOpener(t)
	node := &fakeLoginNode{API: h.b, live: true}
	r := run(t, e, &App{API: node}, "--json", "accounts", "login", "--node", "node_synth-mac", "--register", "--no-open", "--poll", "0s")
	expectCode(t, r, 0)
	out := mustJSON[DeviceLoginResult](t, r)
	if out.Registration != nil || out.NotRegistered != devlogin.LiveLoginRefusal || !out.Login.LiveLogin {
		t.Fatalf("live: %+v", out)
	}
	if !strings.Contains(r.stderr, "SYNT-H3TIC") {
		t.Fatalf("code not shown on stderr in --json mode: %s", r)
	}
	for _, a := range node.actions {
		if a == c.ActionAccountRegister {
			t.Fatal("registration attempted for the live login")
		}
	}
}

func TestAccountsLoginRefusals(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	withOpener(t)
	node := &fakeLoginNode{API: h.b}
	r := run(t, e, &App{API: node}, "accounts", "login", "--node", "node_synth-mac", "--provider", "anthropic")
	expectCode(t, r, ExitUnsupported)
	if !strings.Contains(r.stderr, "device-code") {
		t.Fatalf("anthropic: %s", r)
	}
	expectCode(t, run(t, e, &App{API: node}, "accounts", "login"), ExitUsage)
	expectCode(t, run(t, e, &App{API: node}, "accounts", "login", "--node", "node_synth-mac", "--provider", "gemini"), ExitUsage)
}
