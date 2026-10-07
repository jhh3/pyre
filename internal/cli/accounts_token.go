package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Token accounts: `pyre accounts add-token|remove-token|tokens` and
// `pyre accounts --refresh`. A token is read from stdin or the macOS
// keychain and sent once, in the body of accounts.set_token; it is never
// an argument, printed, logged or written anywhere by the CLI.

// AddTokenParams configures `pyre accounts add-token`.
type AddTokenParams struct {
	Label    string
	Provider string
	Nodes    []string // empty: every node with the harness
	// Token is the value (from stdin or the keychain); never printed.
	Token string `json:"-"`
}

// AddTokenNode is one node's registration outcome.
type AddTokenNode struct {
	NodeID   string `json:"node_id"`
	Name     string `json:"name,omitempty"`
	Verified bool   `json:"verified"`
	Detail   string `json:"detail,omitempty"`
	Error    string `json:"error,omitempty"`
}

// AddTokenResult is the outcome of add-token (no token).
type AddTokenResult struct {
	Account c.Account `json:"account"`
	Created bool      `json:"created"`
	// Attached: the token was added to an existing account that had none
	// (e.g. the person's rota or per-machine login account); that login
	// stays as the fallback when the token is removed or rejected.
	Attached bool                 `json:"attached,omitempty"`
	Status   c.AccountTokenStatus `json:"status"`
	Nodes    []AddTokenNode       `json:"nodes"`
}

func (o *Ops) tokenAPI() (c.AccountTokenAPI, error) {
	t, ok := o.API.(c.AccountTokenAPI)
	if !ok {
		return nil, c.Errorf(c.ErrUnsupported, "this connection does not offer account tokens; upgrade pyre-hub")
	}
	return t, nil
}

// tokenAccounts returns the IDs of accounts whose token is set.
func (o *Ops) tokenAccounts(ctx context.Context) (map[string]c.AccountTokenStatus, error) {
	t, err := o.tokenAPI()
	if err != nil {
		return nil, err
	}
	sts, err := t.ListAccountTokens(ctx, c.ListAccountTokensRequest{})
	if err != nil {
		return nil, err
	}
	out := map[string]c.AccountTokenStatus{}
	for _, s := range sts {
		out[s.AccountID] = s
	}
	return out, nil
}

// AddToken attaches the token to the account with this provider and label
// (whatever its kind: one account per identity), or creates a token
// account when there is none; it stores the token on the hub and registers
// it on the chosen nodes (default: every node with the provider's harness).
// Several accounts with the label are refused, naming them.
func (o *Ops) AddToken(ctx context.Context, p AddTokenParams) (AddTokenResult, error) {
	var out AddTokenResult
	prov := c.Provider(strings.ToLower(strings.TrimSpace(p.Provider)))
	h, err := harnessForProvider(prov)
	if err != nil {
		return out, err
	}
	label := strings.TrimSpace(p.Label)
	if label == "" {
		return out, usagef("a label is required")
	}
	if p.Token == "" {
		return out, usagef("no token: pipe it on stdin or use --keychain-service")
	}
	t, err := o.tokenAPI()
	if err != nil {
		return out, err
	}
	toks, err := o.tokenAccounts(ctx)
	if err != nil {
		return out, err
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return o.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}, Provider: prov})
	})
	if err != nil {
		return out, err
	}
	acct, err := tokenTarget(page.Items, prov, label)
	if err != nil {
		return out, err
	}
	if acct != nil {
		_, had := toks[acct.AccountID]
		out.Attached = !had
	}
	if acct == nil {
		a, err := o.API.UpsertAccount(ctx, c.UpsertAccountRequest{Mutation: mut(""), Provider: prov, Label: label, PerMachineIndependent: true})
		if err != nil {
			return out, err
		}
		acct, out.Created = &a, true
	}
	var nodeIDs []string
	for _, n := range p.Nodes {
		id, err := o.NodeID(ctx, n)
		if err != nil {
			return out, err
		}
		nodeIDs = append(nodeIDs, id)
	}
	st, err := t.SetAccountToken(ctx, c.SetAccountTokenRequest{Mutation: mut(""), AccountID: acct.AccountID, Token: p.Token, Nodes: nodeIDs})
	if err != nil {
		return out, err
	}
	out.Status = st
	if st.AccountID != "" && st.AccountID != acct.AccountID {
		// The hub folded the account into the same-label one (hub
		// accounts_merge.go): register that one.
		acct = &c.Account{AccountID: st.AccountID, Provider: prov, Label: label}
		out.Created, out.Attached = false, true
	}
	if v, err := o.accountView(ctx, acct.AccountID, prov); err == nil {
		acct = &v.Account
	}
	out.Account = *acct
	targets, names, err := o.tokenTargets(ctx, h, nodeIDs)
	if err != nil {
		return out, err
	}
	for _, id := range targets {
		var nr struct {
			Verified bool   `json:"verified"`
			Detail   string `json:"detail"`
		}
		r := AddTokenNode{NodeID: id, Name: names[id]}
		if _, err := o.NodeAction(ctx, id, c.ActionAccountRegister, c.AccountRegisterParams{Account: out.Account, Harness: h, Source: c.SourceToken}, &nr, ""); err != nil {
			r.Error = c.AsError(err).Message
		} else {
			r.Verified, r.Detail = nr.Verified, nr.Detail
		}
		out.Nodes = append(out.Nodes, r)
	}
	if s, err := t.ListAccountTokens(ctx, c.ListAccountTokensRequest{AccountID: acct.AccountID}); err == nil && len(s) == 1 {
		out.Status = s[0]
	}
	return out, nil
}

// tokenTarget picks the account add-token attaches to: the one account of
// the provider whose label matches (case-insensitive), nil when there is
// none, and an error naming them when there are several.
func tokenTarget(views []c.AccountView, prov c.Provider, label string) (*c.Account, error) {
	var hits []c.Account
	for _, v := range views {
		a := v.Account
		if a.Provider == prov && strings.EqualFold(strings.TrimSpace(a.Label), label) {
			hits = append(hits, a)
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return &hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, a := range hits {
		ids[i] = a.AccountID
	}
	return nil, c.Errorf(c.ErrConflict, "%d %s accounts are labelled %q (%s); one account per person: rename or remove the extra one (`pyre accounts rename <id> <label>`), then add the token again",
		len(hits), prov, label, strings.Join(ids, ", ")).WithDetail("accounts", ids)
}

// tokenTargets is the explicit node list, or every non-revoked node that
// reports the harness available.
func (o *Ops) tokenTargets(ctx context.Context, h c.Harness, explicit []string) ([]string, map[string]string, error) {
	names := map[string]string{}
	np, err := o.ListNodes(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	var all []string
	for _, n := range np.Items {
		names[n.NodeID] = n.Name
		for _, hi := range n.Harnesses {
			if hi.Harness == h && hi.Available {
				all = append(all, n.NodeID)
				break
			}
		}
	}
	if len(explicit) > 0 {
		return explicit, names, nil
	}
	return all, names, nil
}

// RemoveToken deletes an account's token on the hub. Nodes revoke their
// grant at their next fetch; running sessions keep working until they end.
func (o *Ops) RemoveToken(ctx context.Context, ref string) (c.AccountTokenStatus, error) {
	t, err := o.tokenAPI()
	if err != nil {
		return c.AccountTokenStatus{}, err
	}
	a, err := o.ResolveAccount(ctx, ref)
	if err != nil {
		return c.AccountTokenStatus{}, err
	}
	st, err := t.RemoveAccountToken(ctx, c.RemoveAccountTokenRequest{Mutation: mut(""), AccountID: a.AccountID})
	if err != nil {
		return st, err
	}
	// Tell nodes now (best effort): a forced observe finds the token gone.
	if v, err := o.accountView(ctx, a.AccountID, a.Provider); err == nil {
		seen := map[string]bool{}
		for _, g := range v.Grants {
			if g.Kind == c.CredOAuthToken && !seen[g.NodeID] {
				seen[g.NodeID] = true
				_, _ = o.NodeAction(ctx, g.NodeID, c.ActionAccountObserve, map[string]any{"provider": a.Provider, "force": true}, nil, "")
			}
		}
	}
	return st, nil
}

// accountView finds one account's view.
func (o *Ops) accountView(ctx context.Context, id string, prov c.Provider) (c.AccountView, error) {
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return o.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}, Provider: prov})
	})
	if err != nil {
		return c.AccountView{}, err
	}
	for _, v := range page.Items {
		if v.Account.AccountID == id {
			return v, nil
		}
	}
	return c.AccountView{}, notFoundf(c.KindAccount, id)
}

// RefreshAccounts asks every node holding a token grant to probe now.
func (o *Ops) RefreshAccounts(ctx context.Context, provider c.Provider, warn func(string, ...any)) {
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return o.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}, Provider: provider})
	})
	if err != nil {
		warn("refresh: %v", err)
		return
	}
	nodes := map[string]bool{}
	var order []string
	for _, v := range page.Items {
		for _, g := range v.Grants {
			if g.Kind == c.CredOAuthToken && !nodes[g.NodeID] {
				nodes[g.NodeID] = true
				order = append(order, g.NodeID)
			}
		}
	}
	for _, id := range order {
		if _, err := o.NodeAction(ctx, id, c.ActionAccountObserve, map[string]any{"provider": provider, "force": true}, nil, ""); err != nil {
			warn("refresh on %s: %v", id, err)
		}
	}
}

// readTokenFrom returns the single-line token from r (trailing newline and
// surrounding spaces removed).
func readTokenFrom(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, c.MaxAccountToken+2))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if strings.ContainsAny(tok, "\r\n") {
		return "", usagef("the token must be a single line")
	}
	return tok, nil
}

// keychainToken reads a generic password with the macOS security tool. The
// value comes back on its stdout; nothing is printed.
func keychainToken(ctx context.Context, account, service string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", usagef("--keychain-service works on macOS only; pipe the token on stdin")
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w")
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", c.Errorf(c.ErrNotFound, "keychain item (account %q, service %q) not found or not readable", account, service)
	}
	return readTokenFrom(&out)
}

func (a *App) addTokenCommands(accounts *cobra.Command) {
	var p AddTokenParams
	var kcService, kcAccount string
	add := &cobra.Command{
		Use:   "add-token <label> --provider anthropic|openai [--node N]... [--keychain-service S]",
		Short: "Add (or replace) a long-lived token for an account, usable on every machine at once",
		Long: `Adds the token to the account labelled <label> (one account per person:
an existing rota or per-machine login account of that label gets the token
and keeps its login as the fallback; several accounts with the label are
refused), or creates the token account <label> when there is none. The
token is stored on the hub encrypted at rest and the account is registered
on every node with the provider's harness (or only --node); nodes added
later pick it up on their own. The token is read from stdin, or on macOS from the keychain with
--keychain-service (account --keychain-account, default "pyre"); it is
never an argument and never printed.

anthropic: the token from ` + "`claude setup-token`" + ` (sk-ant-oat01-…). Runs get it as
CLAUDE_CODE_OAUTH_TOKEN with an isolated profile. Usage comes from a 1-token
Haiku probe (at most every 10 minutes while in use).
openai: a Codex access token (CODEX_ACCESS_TOKEN); registered and shown,
not yet selected for runs.

  pyre accounts add-token personal --provider anthropic --keychain-service pyre-claude-token-test
  claude setup-token | tail -1 | pyre accounts add-token work --provider anthropic`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			q := p
			q.Label = args[0]
			var err error
			if kcService != "" {
				q.Token, err = keychainToken(ctx, kcAccount, kcService)
			} else {
				if a.IsTerminal != nil && a.IsTerminal() {
					return usagef("pipe the token on stdin (it is never an argument), or use --keychain-service on macOS")
				}
				q.Token, err = readTokenFrom(a.Stdin)
			}
			if err != nil {
				return err
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			res, err := ops.AddToken(ctx, q)
			q.Token = ""
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				switch {
				case res.Created:
					fmt.Fprintf(w, "token account %s (%s) created · %s\n", res.Account.Label, res.Account.AccountID, tokenStatusLine(res.Status, time.Now()))
				case res.Attached:
					fmt.Fprintf(w, "token added to account %s (%s) · %s\n", res.Account.Label, res.Account.AccountID, tokenStatusLine(res.Status, time.Now()))
					if res.Account.RotaRef != "" {
						fmt.Fprintln(w, "  runs use the token on every machine; the rota login stays as the fallback")
					}
				default:
					fmt.Fprintf(w, "token of account %s (%s) updated · %s\n", res.Account.Label, res.Account.AccountID, tokenStatusLine(res.Status, time.Now()))
				}
				for _, n := range res.Nodes {
					name := orDash(n.Name)
					switch {
					case n.Error != "":
						fmt.Fprintf(w, "  %s: not registered: %s\n", name, n.Error)
					case n.Verified:
						fmt.Fprintf(w, "  %s: ready · %s\n", name, n.Detail)
					default:
						fmt.Fprintf(w, "  %s: registered · %s\n", name, orDash(n.Detail))
					}
				}
				if len(res.Nodes) == 0 {
					fmt.Fprintln(w, "  no node has the harness; register later with --node")
				}
			})
		}),
	}
	f := add.Flags()
	f.StringVar(&p.Provider, "provider", "", "anthropic or openai")
	f.StringSliceVar(&p.Nodes, "node", nil, "node ID or name (repeatable; default: every node with the harness)")
	f.StringVar(&kcService, "keychain-service", "", "macOS: read the token from this keychain item's service")
	f.StringVar(&kcAccount, "keychain-account", "pyre", "macOS: the keychain item's account")
	accounts.AddCommand(add)

	accounts.AddCommand(&cobra.Command{
		Use:   "remove-token <account>",
		Short: "Delete a token account's token on the hub (nodes revoke it at their next use)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			st, err := ops.RemoveToken(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.emit(st, func(w io.Writer) {
				fmt.Fprintf(w, "token removed from %s; new runs will not use it (running sessions keep going)\n", st.AccountID)
			})
		}),
	})

	accounts.AddCommand(&cobra.Command{
		Use:   "tokens",
		Short: "Token accounts: whether a token is set, when it was added and last verified (never the token)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			toks, err := ops.tokenAccounts(cmd.Context())
			if err != nil {
				return err
			}
			page, err := listAll(cmd.Context(), func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
				return ops.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}})
			})
			if err != nil {
				return err
			}
			labels := map[string]string{}
			for _, v := range page.Items {
				labels[v.Account.AccountID] = v.Account.Label
			}
			list := make([]c.AccountTokenStatus, 0, len(toks))
			for _, v := range page.Items {
				if st, ok := toks[v.Account.AccountID]; ok {
					list = append(list, st)
				}
			}
			return a.emit(list, func(w io.Writer) {
				if len(list) == 0 {
					fmt.Fprintln(w, "no token accounts (pyre accounts add-token)")
					return
				}
				var rows [][]string
				for _, st := range list {
					rows = append(rows, []string{orDash(labels[st.AccountID]), string(st.Provider), tokenStatusLine(st, time.Now())})
				}
				table(w, "ACCOUNT\tPROVIDER\tTOKEN", rows)
			})
		}),
	})
}

// tokenStatusLine: "token set · added Oct 6 · last verified 3m ago".
func tokenStatusLine(st c.AccountTokenStatus, now time.Time) string {
	if !st.Set {
		return "no token"
	}
	parts := []string{"token set", "added " + st.AddedAt.In(now.Location()).Format("Jan 2")}
	switch {
	case st.Rejected:
		parts = append(parts, "rejected: "+orDash(st.Detail))
	case st.VerifiedAt != nil:
		parts = append(parts, "last verified "+ago(*st.VerifiedAt))
	default:
		parts = append(parts, "not verified yet")
	}
	return strings.Join(parts, " · ")
}
