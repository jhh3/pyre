package cli

// Held (batched) answers (W-web-inbox): the owner holds answers instead of
// sending them; the hub sends the queue once contracts.AskBatchQuiet passes
// with no new hold (or at once with `pyre ask send-held`). Owner only; the
// ops are optional (contracts.HeldAnswersAPI).

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func (o *Ops) heldAnswers() (c.HeldAnswersAPI, error) {
	if a, ok := o.API.(c.HeldAnswersAPI); ok {
		return a, nil
	}
	return nil, c.Errorf(c.ErrUnsupported, "this connection does not offer held answers (%s); upgrade pyre-hub", c.OpAsksHold)
}

func printHeld(w io.Writer, h c.HeldAnswers, now time.Time) {
	if len(h.Items) == 0 {
		fmt.Fprintln(w, "no held answers")
		return
	}
	for _, it := range h.Items {
		ans := it.OptionID
		if it.Text != "" {
			if ans != "" {
				ans += ": "
			}
			ans += clip(it.Text, 80)
		}
		fmt.Fprintf(w, "%s  %s  held %s\n", it.AskID, ans, agoFrom(now, it.HeldAt))
	}
	if h.SendsAt != nil {
		fmt.Fprintf(w, "%d held; sent together at %s unless you hold another (pyre ask send-held sends now)\n", len(h.Items), askWhen(h.SendsAt, now))
	}
}

func (a *App) addAskHoldCommands(ask *cobra.Command) {
	held := func(run func(cmd *cobra.Command, args []string, api c.HeldAnswersAPI) error) func(cmd *cobra.Command, args []string) error {
		return func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			api, err := ops.heldAnswers()
			if err != nil {
				return err
			}
			return run(cmd, args, api)
		}
	}
	var text string
	hold := &cobra.Command{
		Use:   "hold <ask_id> [<option>] [--text TEXT]",
		Short: "Hold an answer: it is sent with the others once 5 minutes pass with no new hold",
		Args:  cobra.RangeArgs(1, 2),
		RunE: a.run(held(func(cmd *cobra.Command, args []string, api c.HeldAnswersAPI) error {
			item := c.AnswerAskItem{AskID: args[0], Text: text}
			if len(args) == 2 {
				item.OptionID = args[1]
			}
			if item.OptionID == "" && item.Text == "" {
				return usagef("give an option (pyre ask show %s lists them) or --text", item.AskID)
			}
			h, err := api.HoldAnswer(cmd.Context(), c.HoldAnswerRequest{Mutation: mut(a.opID), AnswerAskItem: item})
			if err != nil {
				return err
			}
			return a.emit(h, func(w io.Writer) { printHeld(w, h, time.Now()) })
		})),
	}
	hold.Flags().StringVar(&text, "text", "", "free-text answer (with or instead of an option)")
	ask.AddCommand(hold)
	ask.AddCommand(&cobra.Command{
		Use:   "held",
		Short: "List held answers and when they will be sent",
		Args:  cobra.NoArgs,
		RunE: a.run(held(func(cmd *cobra.Command, args []string, api c.HeldAnswersAPI) error {
			h, err := api.ListHeldAnswers(cmd.Context(), struct{}{})
			if err != nil {
				return err
			}
			return a.emit(h, func(w io.Writer) { printHeld(w, h, time.Now()) })
		})),
	})
	ask.AddCommand(&cobra.Command{
		Use:   "unhold <ask_id>",
		Short: "Take back a held answer (the ask stays open)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(held(func(cmd *cobra.Command, args []string, api c.HeldAnswersAPI) error {
			h, err := api.UnholdAnswer(cmd.Context(), c.UnholdAnswerRequest{Mutation: mut(a.opID), AskID: args[0]})
			if err != nil {
				return err
			}
			return a.emit(h, func(w io.Writer) { printHeld(w, h, time.Now()) })
		})),
	})
	ask.AddCommand(&cobra.Command{
		Use:   "send-held",
		Short: "Send every held answer now",
		Args:  cobra.NoArgs,
		RunE: a.run(held(func(cmd *cobra.Command, args []string, api c.HeldAnswersAPI) error {
			resp, err := api.FlushHeldAnswers(cmd.Context(), c.FlushHeldAnswersRequest{Mutation: mut(a.opID)})
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) {
				if len(resp.Results) == 0 {
					fmt.Fprintln(w, "no held answers")
				}
				for _, r := range resp.Results {
					switch {
					case r.Error != nil:
						fmt.Fprintf(w, "%s: not sent: %s\n", r.AskID, r.Error.Message)
					case r.Ask != nil:
						fmt.Fprintf(w, "%s: %s\n", r.AskID, askOutcome(*r.Ask))
					}
				}
			})
		})),
	})
}
