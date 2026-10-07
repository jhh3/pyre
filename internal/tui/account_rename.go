package tui

import (
	"context"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Account rename: R on an Accounts card opens a one-line label editor
// prefilled with the current label. Enter saves through accounts.update
// (the hub's label is the account's name on every machine); Esc cancels.

// selectedAccount is the account of the selected Accounts row.
func (m *Model) selectedAccount() (contracts.Account, bool) {
	rows := accountRows(m.usageCards()) // usage_view.go
	if i := m.mgmtList.idx; i >= 0 && i < len(rows) {
		return *rows[i].best.Account, true
	}
	return contracts.Account{}, false
}

// renameAccountSelected opens the label editor for the selected account.
func (m *Model) renameAccountSelected() tea.Cmd {
	if m.view != viewAccounts {
		return nil
	}
	a, ok := m.selectedAccount()
	if !ok {
		return m.setToast("select an account to rename", toneMuted)
	}
	if _, ok := m.api.(contracts.AccountUpdateAPI); !ok {
		return m.setToast("renaming accounts needs a newer hub", toneWarn)
	}
	in := textinput.New()
	in.CharLimit = contracts.MaxAccountLabel
	in.Prompt = "> "
	in.Placeholder = "e.g. the rota name before @"
	in.SetValue(a.Label)
	in.CursorEnd()
	r := &accountRenameOverlay{id: a.AccountID, old: a.Label, in: in}
	m.overlay = r
	return r.in.Focus()
}

type accountRenameOverlay struct {
	id, old string
	in      textinput.Model
	note    string
}

func (r *accountRenameOverlay) hints() []hint { return []hint{{"enter", "save"}, {"esc", "cancel"}} }

func (r *accountRenameOverlay) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	r.in, cmd = r.in.Update(msg)
	return cmd
}

func (r *accountRenameOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.overlay = nil
		return nil
	case "enter":
		v := strings.TrimSpace(r.in.Value())
		if v == "" {
			r.note = "An account needs a name: type one, or esc to keep “" + r.old + "”."
			return nil
		}
		m.overlay = nil
		if v == r.old {
			return nil
		}
		return m.renameAccount(r.id, v)
	}
	r.note = ""
	var cmd tea.Cmd
	r.in, cmd = r.in.Update(k)
	return cmd
}

func (r *accountRenameOverlay) view(m *Model, w, h int) []string {
	bw := min(64, w-2)
	r.in.SetWidth(bw - 8)
	lines := []string{r.in.View(), ""}
	if r.note != "" {
		lines = append(lines, m.th.tone(toneWarn, trunc(r.note, bw-4)))
	} else {
		lines = append(lines, m.th.faint.Render(trunc("One name on every machine: leave the machine out of it.", bw-4)))
	}
	lines = append(lines, "", m.th.key.Render("enter")+m.th.dim.Render(" save   ")+m.th.key.Render("esc")+m.th.dim.Render(" cancel"))
	return m.box("Rename account", lines, bw, w, h)
}

// renameAccount sets the label through accounts.update. The operation id
// is stable per (account, label), so a retry is deduplicated by the hub.
func (m *Model) renameAccount(id, label string) tea.Cmd {
	u, ok := m.api.(contracts.AccountUpdateAPI)
	if !ok {
		return m.setToast("renaming accounts needs a newer hub", toneWarn)
	}
	key := "acctrename:" + id + ":" + label
	req := contracts.UpdateAccountRequest{Mutation: contracts.Mutation{OperationID: m.opID(key)}, AccountID: id, Label: &label}
	return m.do(func(ctx context.Context) (any, error) {
		return u.UpdateAccount(ctx, req)
	}, func(m *Model, v any, err error) tea.Cmd {
		if err != nil {
			if contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) {
				return m.setToast("rename: outcome unknown; repeating it is safe (same operation id)", toneWarn)
			}
			delete(m.opIDs, key)
			return m.errToast("rename failed", err)
		}
		delete(m.opIDs, key)
		return tea.Batch(m.setToast("account renamed to "+strconv.Quote(label), toneMuted), m.reload("accounts", nil))
	})
}
