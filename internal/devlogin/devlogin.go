// Package devlogin is the shared, nonsecret vocabulary of a remote device
// login: a node runs the provider's own device-code login (`codex login
// --device-auth`) into a private temporary home and saves the result as a
// rota account on that node; requesters only ever see the verification URL,
// the one-time code, the state and the account email. The credential never
// leaves the node.
//
// The node side lives in internal/accounts (devlogin.go); this package holds
// the action names and payloads, the CLI output parser and the client helpers
// the CLI and TUI share.
package devlogin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Node actions (additive; owner principals only — they are not session actions).
const (
	// ActionStart starts a device login: Params -> View (returns once the
	// URL and code are known, or the login failed to start).
	ActionStart = "account.device_login"
	// ActionStatus reads a login: StatusParams -> View (waits up to
	// WaitSeconds for a change).
	ActionStatus = "account.device_login_status"
	// ActionCancel stops a login: StatusParams -> View.
	ActionCancel = "account.device_login_cancel"
)

// Login states.
const (
	StateStarting  = "starting"  // the provider CLI is running; no code yet
	StateWaiting   = "waiting"   // URL and code shown; waiting for approval
	StateCompleted = "completed" // saved as a rota account on the node
	StateFailed    = "failed"
	StateExpired   = "expired" // the code timed out before approval
	StateCancelled = "cancelled"
)

// DefaultTimeout is how long a node waits for approval (Codex codes expire
// after 15 minutes).
const DefaultTimeout = 15 * time.Minute

// MaxWaitSeconds bounds one status long-poll (it holds a node action slot).
const MaxWaitSeconds = 10

// Params starts a login.
type Params struct {
	Provider       c.Provider `json:"provider"`
	TimeoutSeconds int        `json:"timeout_seconds,omitempty"`
}

// StatusParams names a login.
type StatusParams struct {
	LoginID     string `json:"login_id"`
	WaitSeconds int    `json:"wait_seconds,omitempty"`
}

// View is everything a requester learns about a login. It never carries a
// token.
type View struct {
	LoginID         string     `json:"login_id"`
	Provider        c.Provider `json:"provider"`
	NodeID          string     `json:"node_id,omitempty"`
	State           string     `json:"state"`
	VerificationURL string     `json:"verification_url,omitempty"`
	UserCode        string     `json:"user_code,omitempty"`
	CodeExpiresAt   *time.Time `json:"code_expires_at,omitempty"`
	// Email is the signed-in account (completed only); it is also the rota
	// account name the login was saved under.
	Email    string `json:"email,omitempty"`
	RotaName string `json:"rota_name,omitempty"`
	// Replaced: an earlier saved copy of this account was kept as a backup
	// next to the new one.
	Replaced bool `json:"replaced,omitempty"`
	// LiveLogin: the new login is the same account as this machine's own
	// default login, so it must not be registered as a Pyre account.
	LiveLogin  bool       `json:"live_login,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Version    int        `json:"version"` // bumps on every change
}

// Done reports a final state.
func (v View) Done() bool {
	switch v.State {
	case StateCompleted, StateFailed, StateExpired, StateCancelled:
		return true
	}
	return false
}

// LiveLoginRefusal is the plain-words reason a login that matches the
// machine's own default login is not registered.
const LiveLoginRefusal = "this machine is already logged in to that account; Pyre already uses it (not registered again)"

// LabelFor is the account label for an email: its local part.
func LabelFor(email string) string {
	email = strings.TrimSpace(email)
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return email
}

// ---- provider CLI output parsing ----

var (
	ansiRE    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)
	urlRE     = regexp.MustCompile(`https://[^\s"'<>]+`)
	codeRE    = regexp.MustCompile(`^[A-Z0-9]{4,8}(?:-[A-Z0-9]{4,8})+$`)
	expiresRE = regexp.MustCompile(`expires in (\d+) (minute|minutes|second|seconds)`)
)

// Parser extracts the verification URL, the one-time code and its lifetime
// from `codex login --device-auth` output, line by line. The real output
// (codex-cli 0.160) is:
//
//  1. Open this link in your browser and sign in to your account
//     https://auth.openai.com/codex/device
//  2. Enter this one-time code (expires in 15 minutes)
//     ABCD-12345
//
// with ANSI colours, which are stripped.
type Parser struct {
	URL       string
	Code      string
	ExpiresIn time.Duration
	wantCode  bool
}

// StripANSI removes terminal escape sequences.
func StripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// Feed consumes one output line and reports whether the URL and code are
// both known.
func (p *Parser) Feed(line string) bool {
	l := strings.TrimSpace(StripANSI(line))
	if l == "" {
		return p.Ready()
	}
	low := strings.ToLower(l)
	if m := expiresRE.FindStringSubmatch(low); m != nil {
		n, _ := strconv.Atoi(m[1])
		if strings.HasPrefix(m[2], "minute") {
			p.ExpiresIn = time.Duration(n) * time.Minute
		} else {
			p.ExpiresIn = time.Duration(n) * time.Second
		}
	}
	if p.URL == "" {
		if u := urlRE.FindString(l); u != "" {
			p.URL = strings.TrimRight(u, ".,;:)")
		}
	}
	if strings.Contains(low, "code") {
		p.wantCode = true
		// "Enter this one-time code: ABCD-12345" on one line.
		if i := strings.LastIndex(l, ":"); i >= 0 && !strings.Contains(l[i:], "//") {
			if f := strings.TrimSpace(l[i+1:]); codeRE.MatchString(f) && p.Code == "" {
				p.Code = f
			}
		}
	}
	if p.Code == "" && codeRE.MatchString(l) {
		p.Code = l
	}
	return p.Ready()
}

// Ready reports whether both the URL and the code are known.
func (p *Parser) Ready() bool { return p.URL != "" && p.Code != "" }

// ---- client helpers (CLI, TUI) ----

// ActionAPI is the hub surface needed to drive a login.
type ActionAPI interface {
	InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error)
}

func invoke(ctx context.Context, api ActionAPI, nodeID, action string, params any, timeout int) (View, error) {
	var v View
	b, err := json.Marshal(params)
	if err != nil {
		return v, err
	}
	res, err := api.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		NodeID: nodeID, Action: action, Params: b, TimeoutSeconds: timeout})
	if err != nil {
		return v, err
	}
	if res.Error != nil {
		return v, res.Error
	}
	switch res.Outcome {
	case c.OutcomeSucceeded:
	case c.OutcomeUnsupported:
		return v, c.Errorf(c.ErrUnsupported, "this machine's Pyre does not support device login yet (update pyred)")
	default:
		return v, c.Errorf(c.ErrUnavailable, "the machine did not answer in time (outcome %s, command %s)", res.Outcome, res.CommandID).WithRef(c.KindCommand, res.CommandID)
	}
	if err := json.Unmarshal(res.Result, &v); err != nil {
		return v, c.Errorf(c.ErrInternal, "malformed device login result: %v", err)
	}
	if v.NodeID == "" {
		v.NodeID = nodeID
	}
	return v, nil
}

// Start asks the node to begin a device login for provider.
func Start(ctx context.Context, api ActionAPI, nodeID string, provider c.Provider) (View, error) {
	return invoke(ctx, api, nodeID, ActionStart, Params{Provider: provider}, 60)
}

// Status reads a login, waiting up to wait seconds for a change.
func Status(ctx context.Context, api ActionAPI, nodeID, loginID string, wait int) (View, error) {
	return invoke(ctx, api, nodeID, ActionStatus, StatusParams{LoginID: loginID, WaitSeconds: wait}, wait+20)
}

// Cancel stops a login (its temporary home is removed on the node).
func Cancel(ctx context.Context, api ActionAPI, nodeID, loginID string) (View, error) {
	return invoke(ctx, api, nodeID, ActionCancel, StatusParams{LoginID: loginID}, 20)
}

// RegisterAPI is the hub surface Register needs (contracts.API has it).
type RegisterAPI interface {
	ActionAPI
	ListAccounts(ctx context.Context, req c.ListAccountsRequest) (c.Page[c.AccountView], error)
	UpsertAccount(ctx context.Context, req c.UpsertAccountRequest) (c.Account, error)
}

// Registration is the outcome of Register.
type Registration struct {
	Account  c.Account `json:"account"`
	NodeID   string    `json:"node_id"`
	Verified bool      `json:"verified"`
	Detail   string    `json:"detail,omitempty"`
}

// Register makes a completed login a Pyre account on its node: the hub
// account (reused by provider + rota name; label = the email's local part,
// per-machine independent) and the node's rota-backed profile
// (account.register). A login that matches the machine's own default login
// is refused with LiveLoginRefusal.
func Register(ctx context.Context, api RegisterAPI, v View) (Registration, error) {
	out := Registration{NodeID: v.NodeID}
	if v.State != StateCompleted || v.RotaName == "" {
		return out, c.Errorf(c.ErrConflict, "the login has not completed")
	}
	if v.LiveLogin {
		return out, c.Errorf(c.ErrForbidden, "%s", LiveLoginRefusal)
	}
	h := c.HarnessCodex
	if v.Provider == c.ProviderAnthropic {
		h = c.HarnessClaude
	}
	var acct *c.Account
	cur := ""
	for {
		page, err := api.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}, Provider: v.Provider})
		if err != nil {
			return out, err
		}
		for i := range page.Items {
			if a := page.Items[i].Account; a.Provider == v.Provider && a.RotaRef == v.RotaName {
				acct = &a
			}
		}
		if page.NextCursor == "" || acct != nil {
			break
		}
		cur = page.NextCursor
	}
	req := c.UpsertAccountRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Provider: v.Provider,
		Label: LabelFor(v.RotaName), RotaRef: v.RotaName, PerMachineIndependent: true}
	if acct != nil {
		req.AccountID, req.Disabled, req.ProviderRef = acct.AccountID, acct.Disabled, acct.ProviderRef
		if acct.Label != "" {
			req.Label = acct.Label
		}
	}
	a, err := api.UpsertAccount(ctx, req)
	if err != nil {
		return out, err
	}
	out.Account = a
	b, _ := json.Marshal(c.AccountRegisterParams{Account: a, Harness: h, Source: "rota", BindRotaIdentity: true})
	res, err := api.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		NodeID: v.NodeID, Action: c.ActionAccountRegister, Params: b, TimeoutSeconds: 30})
	if err != nil {
		return out, err
	}
	if res.Error != nil {
		if strings.Contains(res.Error.Message, "live default login") {
			return out, c.Errorf(c.ErrForbidden, "%s", LiveLoginRefusal)
		}
		return out, res.Error
	}
	if res.Outcome != c.OutcomeSucceeded {
		return out, c.Errorf(c.ErrUnavailable, "registering on %s: outcome %s (command %s)", v.NodeID, res.Outcome, res.CommandID)
	}
	var nr struct {
		ProviderRef string `json:"provider_ref"`
		Verified    bool   `json:"verified"`
		Detail      string `json:"detail"`
	}
	_ = json.Unmarshal(res.Result, &nr)
	out.Verified, out.Detail = nr.Verified, nr.Detail
	if nr.ProviderRef != "" && a.ProviderRef != nr.ProviderRef {
		upd := c.UpsertAccountRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, AccountID: a.AccountID, Provider: a.Provider,
			Label: a.Label, Disabled: a.Disabled, RotaRef: a.RotaRef, PerMachineIndependent: a.PerMachineIndependent, ProviderRef: nr.ProviderRef}
		if out.Account, err = api.UpsertAccount(ctx, upd); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Describe is a one-line, plain-words status for a view.
func Describe(v View) string {
	switch v.State {
	case StateStarting:
		return "starting the login on the machine…"
	case StateWaiting:
		s := "waiting for you to approve the code"
		if v.CodeExpiresAt != nil {
			if left := time.Until(*v.CodeExpiresAt).Round(time.Minute); left > 0 {
				s += fmt.Sprintf(" (expires in %d min)", int(left.Minutes()))
			}
		}
		return s
	case StateCompleted:
		s := "logged in as " + v.Email + "; saved on the machine as rota account " + v.RotaName
		if v.Replaced {
			s += " (the earlier copy was kept as a backup)"
		}
		return s
	case StateExpired:
		return "the code expired before it was approved; start again"
	case StateCancelled:
		return "cancelled; nothing was saved"
	case StateFailed:
		if v.Detail != "" {
			return "login failed: " + v.Detail
		}
		return "login failed"
	}
	return v.State
}
