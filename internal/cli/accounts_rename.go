package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// RenameAccount changes an account's label on the hub (accounts.update).
// The account is any reference `pyre accounts` accepts (ID, label, rota
// name or a unique prefix).
func (o *Ops) RenameAccount(ctx context.Context, ref, label, opID string) (c.Account, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return c.Account{}, usagef("a new label is required")
	}
	u, ok := o.API.(c.AccountUpdateAPI)
	if !ok {
		return c.Account{}, c.Errorf(c.ErrUnsupported, "renaming accounts needs %s, which this connection does not offer", c.OpUpdateAccount)
	}
	acct, err := o.ResolveAccount(ctx, ref)
	if err != nil {
		return c.Account{}, err
	}
	return u.UpdateAccount(ctx, c.UpdateAccountRequest{Mutation: mut(opID), AccountID: acct.AccountID, Label: &label})
}

// DefaultAccountLabel is the label a registration uses when none is given:
// the rota name's local part ("dev@example.com" -> "dev"). Never a machine
// name: one account serves every machine it is registered on.
func DefaultAccountLabel(rota string) string {
	rota = strings.TrimSpace(rota)
	if i := strings.Index(rota, "@"); i > 0 {
		return rota[:i]
	}
	return rota
}

func (a *App) addAccountRename(accounts *cobra.Command) {
	accounts.AddCommand(&cobra.Command{
		Use:   "rename <account> <label>",
		Short: "Rename an account (its label everywhere: CLI, TUI, Android)",
		Long: `Changes the account's label on the hub. <account> is an account ID, label,
rota name or a unique prefix. The label is one account's name on every
machine it is registered on, so do not put a machine name in it.`,
		Args: cobra.ExactArgs(2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			acct, err := ops.RenameAccount(cmd.Context(), args[0], args[1], a.opID)
			if err != nil {
				return err
			}
			return a.emit(acct, func(w io.Writer) {
				fmt.Fprintf(w, "%s is now %q\n", acct.AccountID, acct.Label)
			})
		}),
	})
}
