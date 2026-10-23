package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/refs"
)

// RegisterAccountParams onboards a rota account on a node.
type RegisterAccountParams struct {
	Node        string `json:"node"`
	Provider    string `json:"provider"` // anthropic | openai
	Rota        string `json:"rota"`     // rota account name on the node
	Label       string `json:"label,omitempty"`
	Independent bool   `json:"independent,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// AccountRegistration is the outcome of `pyre accounts register`.
type AccountRegistration struct {
	Account   c.Account       `json:"account"`
	NodeID    string          `json:"node_id"`
	Harness   c.Harness       `json:"harness"`
	Verified  bool            `json:"verified"`
	Detail    string          `json:"detail,omitempty"`
	Grant     *c.DeviceGrant  `json:"grant,omitempty"`
	CommandID string          `json:"command_id"`
	Raw       json.RawMessage `json:"node_result,omitempty"`
}

func harnessForProvider(p c.Provider) (c.Harness, error) {
	switch p {
	case c.ProviderAnthropic:
		return c.HarnessClaude, nil
	case c.ProviderOpenAI:
		return c.HarnessCodex, nil
	}
	return "", usagef("--provider must be anthropic or openai")
}

// RegisterAccount creates (or reuses, by provider + rota name) the hub
// account, registers it on the node (account.register, source rota, binding
// the snapshot's nonsecret identity) and records the provider identity on
// the hub account.
func (o *Ops) RegisterAccount(ctx context.Context, p RegisterAccountParams) (AccountRegistration, error) {
	var out AccountRegistration
	prov := c.Provider(strings.ToLower(strings.TrimSpace(p.Provider)))
	h, err := harnessForProvider(prov)
	if err != nil {
		return out, err
	}
	if p.Node == "" || strings.TrimSpace(p.Rota) == "" {
		return out, usagef("--node and --rota are required")
	}
	nodeID, err := o.NodeID(ctx, p.Node)
	if err != nil {
		return out, err
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return o.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}, Provider: prov})
	})
	if err != nil {
		return out, err
	}
	var acct *c.Account
	for i := range page.Items {
		if a := page.Items[i].Account; a.Provider == prov && a.RotaRef == p.Rota {
			acct = &a
			break
		}
	}
	req := c.UpsertAccountRequest{Mutation: mut(p.OperationID), Provider: prov, Label: p.Label, RotaRef: p.Rota, PerMachineIndependent: p.Independent}
	if acct != nil {
		req.AccountID, req.Disabled = acct.AccountID, acct.Disabled
		req.PerMachineIndependent = p.Independent || acct.PerMachineIndependent
		if req.Label == "" {
			req.Label = acct.Label
		}
	}
	if req.Label == "" {
		req.Label = DefaultAccountLabel(p.Rota)
	}
	a, err := o.API.UpsertAccount(ctx, req)
	if err != nil {
		return out, err
	}
	var nr struct {
		ProviderRef string         `json:"provider_ref"`
		Grant       *c.DeviceGrant `json:"grant"`
		Verified    bool           `json:"verified"`
		Detail      string         `json:"detail"`
	}
	res, err := o.NodeAction(ctx, nodeID, c.ActionAccountRegister, c.AccountRegisterParams{Account: a, Harness: h, Source: "rota", BindRotaIdentity: true}, &nr, "")
	out = AccountRegistration{Account: a, NodeID: nodeID, Harness: h, CommandID: res.CommandID, Raw: json.RawMessage(res.Result)}
	if err != nil {
		return out, err
	}
	out.Verified, out.Detail, out.Grant = nr.Verified, nr.Detail, nr.Grant
	if nr.ProviderRef != "" && a.ProviderRef != nr.ProviderRef {
		upd := c.UpsertAccountRequest{Mutation: mut(""), AccountID: a.AccountID, Provider: a.Provider, Label: a.Label, Disabled: a.Disabled,
			RotaRef: a.RotaRef, PerMachineIndependent: a.PerMachineIndependent, ProviderRef: nr.ProviderRef}
		if out.Account, err = o.API.UpsertAccount(ctx, upd); err != nil {
			return out, err
		}
	}
	return out, nil
}

// ResolveAccount accepts an account ID, label or rota name (exact), or a
// unique prefix of a label or rota name (e.g. the local part of a rota
// email).
func (o *Ops) ResolveAccount(ctx context.Context, ref string) (c.Account, error) {
	return o.resolveAccount(ctx, ref, "")
}

// accountRef turns an --account reference into an account ID ("" stays "").
// A full account ID is passed through without a lookup (internal/refs).
func (o *Ops) accountRef(ctx context.Context, ref string, provider c.Provider) (string, error) {
	return refs.AccountID(ctx, o.API, ref, provider)
}

func (o *Ops) resolveAccount(ctx context.Context, ref string, provider c.Provider) (c.Account, error) {
	if strings.TrimSpace(ref) == "" {
		return c.Account{}, usagef("an account is required (see `pyre accounts`)")
	}
	return refs.Account(ctx, o.API, ref, provider)
}

func (a *App) addAccountOnboarding(accounts *cobra.Command) {
	var rp RegisterAccountParams
	register := &cobra.Command{
		Use:   "register --node N --provider anthropic|openai --rota NAME [--label L] [--independent]",
		Short: "Register a rota account: create the hub account and prepare it on a node (rota checkout)",
		Long: `Creates the hub account for a rota account (or reuses the one with the same
provider and rota name), asks the node to register it (account.register:
an isolated profile backed by the rota snapshot, which must not be the
node's live default login) and records the snapshot's nonsecret identity on
the hub account. Repeat with another --node to use the account there.
--independent marks the login as per-machine (never copied between machines
with rota export/import), so nodes check it out without a fleet lease.
The node must run pyred --rota-checkout.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := rp
			p.OperationID = a.opID
			res, err := ops.RegisterAccount(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "account %s (%s, rota %s) registered on %s for %s; verified=%v\n", res.Account.AccountID, res.Account.Label, res.Account.RotaRef, res.NodeID, res.Harness, res.Verified)
				if res.Detail != "" {
					fmt.Fprintf(w, "  %s\n", res.Detail)
				}
				if res.Account.PerMachineIndependent {
					fmt.Fprintln(w, "  per-machine independent: no fleet lease needed")
				}
			})
		}),
	}
	f := register.Flags()
	f.StringVar(&rp.Node, "node", "", "node ID or name")
	f.StringVar(&rp.Provider, "provider", "", "anthropic or openai")
	f.StringVar(&rp.Rota, "rota", "", "rota account name on that node")
	f.StringVar(&rp.Label, "label", "", "account label (default: the rota name before @; never a machine name)")
	f.BoolVar(&rp.Independent, "independent", false, "per-machine independent login (no fleet lease)")
	accounts.AddCommand(register)

	var coNode string
	checkouts := &cobra.Command{
		Use:   "checkouts --node N",
		Short: "List rota checkouts on a node",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if coNode == "" {
				return usagef("--node is required")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			var out []map[string]any
			if _, err := ops.NodeAction(cmd.Context(), coNode, c.ActionAccountCheckouts, nil, &out, ""); err != nil {
				return err
			}
			if out == nil {
				out = []map[string]any{}
			}
			return a.emit(out, func(w io.Writer) {
				var rows [][]string
				for _, m := range out {
					rows = append(rows, []string{fmt.Sprint(m["account_id"]), fmt.Sprint(m["harness"]), fmt.Sprint(m["rota_name"]), fmt.Sprint(m["state"]), fmt.Sprint(m["checked_out_at"])})
				}
				table(w, "ACCOUNT\tHARNESS\tROTA\tSTATE\tSINCE", rows)
			})
		}),
	}
	checkouts.Flags().StringVar(&coNode, "node", "", "node ID or name")
	accounts.AddCommand(checkouts)

	var ciNode string
	checkin := &cobra.Command{
		Use:   "checkin <account> --node N",
		Short: "Check a rota account back in on a node (refused while runs use it)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if ciNode == "" {
				return usagef("--node is required")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			acct, err := ops.ResolveAccount(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			var out map[string]any
			if _, err := ops.NodeAction(cmd.Context(), ciNode, c.ActionAccountCheckin, map[string]string{"account_id": acct.AccountID}, &out, a.opID); err != nil {
				return err
			}
			return a.emit(out, func(w io.Writer) {
				fmt.Fprintf(w, "%s on %s: %v %v\n", acct.AccountID, ciNode, out["state"], out["detail"])
			})
		}),
	}
	checkin.Flags().StringVar(&ciNode, "node", "", "node ID or name")
	accounts.AddCommand(checkin)
}
