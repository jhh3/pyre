package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// SetAccountTier puts an account in the normal or reserve tier on the hub
// (accounts.update). The account is any reference `pyre accounts` accepts
// (ID, label, rota name or a unique prefix) of provider; with no provider
// a label that names accounts of several providers is refused.
func (o *Ops) SetAccountTier(ctx context.Context, ref, tier string, provider c.Provider, opID string) (c.Account, error) {
	t, ok := c.ParseAccountTier(tier)
	if !ok || strings.TrimSpace(tier) == "" {
		return c.Account{}, usagef("tier must be normal or reserve, not %q", tier)
	}
	switch provider {
	case "", c.ProviderAnthropic, c.ProviderOpenAI, c.ProviderGoogle:
	default:
		return c.Account{}, usagef("--provider must be anthropic, openai or google, not %q", provider)
	}
	u, ok := o.API.(c.AccountUpdateAPI)
	if !ok {
		return c.Account{}, c.Errorf(c.ErrUnsupported, "account tiers need %s, which this connection does not offer", c.OpUpdateAccount)
	}
	acct, err := o.tierAccount(ctx, ref, provider)
	if err != nil {
		return c.Account{}, err
	}
	out, err := u.UpdateAccount(ctx, c.UpdateAccountRequest{Mutation: mut(opID), AccountID: acct.AccountID, Tier: &t})
	if e := c.AsError(err); e != nil && e.Code == c.ErrInvalid && strings.Contains(e.Message, "nothing to update") {
		// A hub from before tiers ignores the field.
		return c.Account{}, c.Errorf(c.ErrUnsupported, "this hub predates account tiers; upgrade pyre-hub")
	}
	if err == nil && out.Tier != t {
		return c.Account{}, c.Errorf(c.ErrUnsupported, "this hub did not store the tier (it predates account tiers); upgrade pyre-hub")
	}
	return out, err
}

// tierAccount resolves ref within provider, or, with none, within each
// provider: one match is the account, matches under several providers are
// ambiguous (a tier set on the wrong provider's account would go unseen).
func (o *Ops) tierAccount(ctx context.Context, ref string, provider c.Provider) (c.Account, error) {
	if provider != "" || c.ValidID(strings.TrimSpace(ref), c.PrefixAccount) {
		return o.resolveAccount(ctx, ref, provider)
	}
	var hits []c.Account
	for _, p := range []c.Provider{c.ProviderAnthropic, c.ProviderOpenAI, c.ProviderGoogle} {
		if a, err := o.resolveAccount(ctx, ref, p); err == nil {
			hits = append(hits, a)
		} else if !c.IsCode(err, c.ErrNotFound) {
			return c.Account{}, err
		}
	}
	switch len(hits) {
	case 0:
		return o.resolveAccount(ctx, ref, "")
	case 1:
		return hits[0], nil
	}
	var names []string
	for _, a := range hits {
		names = append(names, string(a.Provider)+" "+a.AccountID)
	}
	return c.Account{}, c.Errorf(c.ErrConflict, "%q names accounts of several providers (%s); pass --provider or an account ID", ref, strings.Join(names, ", "))
}

func (a *App) addAccountTier(accounts *cobra.Command) {
	var provider string
	cmd := &cobra.Command{
		Use:   "tier <account> normal|reserve [--provider anthropic|openai]",
		Short: "Use an account only after the others (reserve) or as usual (normal)",
		Long: fmt.Sprintf(`Sets the account's selection tier on the hub. <account> is an account ID,
label, rota name or a unique prefix; when a label names accounts of
several providers, pass --provider (or the ID).

A reserve account takes new work only when every normal account of its
provider is at its limit, unavailable, or near its limit (5-hour or weekly
window at %.0f%%, what pyre accounts shows as "near limit"). An explicit
--account and a preference list still choose it. Idle sessions that were
put on a reserve account move back to a normal account between turns once
one has room again (never sessions started with that account named).`, usage.NearPercent),
		Args: cobra.ExactArgs(2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			acct, err := ops.SetAccountTier(cmd.Context(), args[0], args[1], c.Provider(provider), a.opID)
			if err != nil {
				return err
			}
			return a.emit(acct, func(w io.Writer) {
				fmt.Fprintf(w, "%s account %s (%s) is now %s\n", acct.Provider, orDash(acct.Label), acct.AccountID, acct.TierName())
			})
		}),
	}
	cmd.Flags().StringVar(&provider, "provider", "", "the account's provider (anthropic, openai); needed when the label names accounts of several")
	accounts.AddCommand(cmd)
}
