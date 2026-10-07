package tui_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/devlogin"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// loginRec records node actions (with their results) and account upserts.
type loginRec struct {
	contracts.API
	mu      sync.Mutex
	actions []string
	results []string
	upserts []contracts.UpsertAccountRequest
}

func (r *loginRec) InvokeNodeAction(ctx context.Context, req contracts.NodeActionRequest) (contracts.NodeActionResult, error) {
	res, err := r.API.InvokeNodeAction(ctx, req)
	r.mu.Lock()
	r.actions = append(r.actions, req.Action)
	b, _ := json.Marshal(res)
	r.results = append(r.results, string(b))
	r.mu.Unlock()
	return res, err
}

func (r *loginRec) UpsertAccount(ctx context.Context, req contracts.UpsertAccountRequest) (contracts.Account, error) {
	r.mu.Lock()
	r.upserts = append(r.upserts, req)
	r.mu.Unlock()
	return r.API.UpsertAccount(ctx, req)
}

func (r *loginRec) has(action string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.actions {
		if a == action {
			return true
		}
	}
	return false
}

func startLoginMock(t *testing.T) (*tuitest.Harness, *loginRec, *mock.FakeClock) {
	b, clk := tuitest.NewMock(t)
	rec := &loginRec{API: b}
	opts := tuitest.Options()
	opts.Clock = clk
	h := tuitest.New(t, rec, opts, 160, 48)
	h.WaitFor("initial load", 5*sec, func() bool { return h.M.Loaded("sessions") && h.M.Loaded("nodes") })
	h.Settle()
	return h, rec, clk
}

func setMockLogin(t *testing.T, after time.Duration, live bool) {
	oldA, oldL := mock.DeviceLoginApproveAfter, mock.DeviceLoginLive
	mock.DeviceLoginApproveAfter, mock.DeviceLoginLive = after, live
	t.Cleanup(func() { mock.DeviceLoginApproveAfter, mock.DeviceLoginLive = oldA, oldL })
}

func openAccounts(h *tuitest.Harness) {
	h.Key(":")
	h.Type("accounts")
	h.Key("enter")
	h.Settle()
}

// L on Accounts picks a machine, shows the link, code and a QR code while
// waiting, then offers to register the completed login.
func TestDeviceLoginFromAccounts(t *testing.T) {
	setMockLogin(t, 3*time.Second, false)
	h, rec, clk := startLoginMock(t)
	openAccounts(h)
	h.Key("L")
	if !h.Contains("Log a machine into Codex") {
		t.Fatalf("L did not open the machine picker:\n%s", h.View())
	}
	h.Key("enter")
	h.WaitFor("code shown", 3*sec, func() bool { return h.Contains("SYNT-H3TIC") })
	if !h.Contains("https://auth.openai.com/codex/device") || !h.Contains("▀") || !h.Contains("waiting for you to approve") {
		t.Fatalf("waiting modal incomplete:\n%s", h.View())
	}
	clk.Advance(5 * time.Second) // the simulated owner approves
	h.WaitFor("completed", 8*sec, func() bool { return h.Contains("logged in as you@synthetic.invalid") })
	if !h.Contains("Press r to use it from Pyre") {
		t.Fatalf("no register offer:\n%s", h.View())
	}
	h.Key("r")
	h.WaitFor("registered", 3*sec, func() bool { return h.Contains("Added to Pyre as") })
	rec.mu.Lock()
	ups, results := rec.upserts, rec.results
	rec.mu.Unlock()
	if len(ups) == 0 {
		t.Fatal("no hub account upsert")
	}
	if req := ups[0]; req.RotaRef != "you@synthetic.invalid" || req.Label != "you" || !req.PerMachineIndependent {
		t.Fatalf("upsert %+v", req)
	}
	if !rec.has(contracts.ActionAccountRegister) {
		t.Fatal("account.register not sent to the machine")
	}
	for _, r := range results {
		if strings.Contains(r, "refresh_token") || strings.Contains(r, "access_token") {
			t.Fatalf("a result carries a token: %s", r)
		}
	}
	h.Key("esc")
	if h.Contains("Added to Pyre") {
		t.Fatalf("esc did not close:\n%s", h.View())
	}
}

func TestDeviceLoginLiveAndCancel(t *testing.T) {
	setMockLogin(t, 200*time.Millisecond, true)
	h, rec, clk := startLoginMock(t)
	openAccounts(h)
	h.Key("L")
	h.Key("enter")
	// The mock stamps the login's start with the mock clock when the start
	// action arrives; advancing before that (a slower machine: server) would
	// leave it waiting forever.
	h.WaitFor("code shown", 3*sec, func() bool { return h.Contains("SYNT-H3TIC") })
	clk.Advance(time.Second)
	h.WaitFor("completed", 8*sec, func() bool { return h.Contains("already logged in to that account") })
	h.Key("r")
	h.Settle()
	if len(rec.upserts) != 0 {
		t.Fatal("the live login was registered")
	}
	h.Key("esc")

	setMockLogin(t, time.Hour, false)
	h.Key("L")
	h.Key("enter")
	h.WaitFor("code shown", 3*sec, func() bool { return h.Contains("SYNT-H3TIC") })
	h.Key("esc")
	h.WaitFor("cancelled on the machine", 3*sec, func() bool { return rec.has(devlogin.ActionCancel) })
}
