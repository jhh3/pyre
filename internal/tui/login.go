package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/devlogin"
)

// Device login: L on the Accounts view picks a machine, then the machine
// runs `codex login --device-auth` in a private temporary home. The modal
// shows the link, the one-time code and a QR code, follows the status, and
// on success offers to register the login as a Pyre account there. The
// credential never leaves the machine.

// openLoginURL opens a link in this machine's browser (tests replace it).
var openLoginURL = func(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "linux":
		return exec.Command("xdg-open", u).Start()
	}
	return fmt.Errorf("no browser opener")
}

// loginPollWait is the status long-poll per round trip (seconds).
var loginPollWait = 8

type deviceLoginOverlay struct {
	nodeID, node string
	v            devlogin.View
	err          error
	reg          *devlogin.Registration
	regNote      string
	registering  bool
	opened       bool
}

func (m *Model) loginOnMachine() tea.Cmd {
	if m.view != viewAccounts {
		return nil
	}
	var items []pickItem
	now := m.now()
	for _, n := range m.sortedNodes() {
		it := pickItem{id: n.NodeID, label: n.Name, detail: n.Platform + "/" + n.Arch}
		if n.Connection.State != contracts.ConnConnected {
			seen := "never seen"
			if n.Connection.LastSeenAt != nil {
				seen = "last seen " + ago(now, *n.Connection.LastSeenAt)
			}
			it.reason = "offline (" + seen + ")"
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return m.setToast("no machines yet", toneMuted)
	}
	m.overlay = &pickerOverlay{title: "Log a machine into Codex", items: items, onPick: func(m *Model, it pickItem) tea.Cmd {
		o := &deviceLoginOverlay{nodeID: it.id, node: it.label, v: devlogin.View{State: devlogin.StateStarting}}
		m.overlay = o
		return o.call(m, func(ctx context.Context) (devlogin.View, error) {
			return devlogin.Start(ctx, m.api, o.nodeID, contracts.ProviderOpenAI)
		})
	}}
	return nil
}

// call runs one node round trip off the UI loop and feeds the view back.
func (o *deviceLoginOverlay) call(m *Model, fn func(ctx context.Context) (devlogin.View, error)) tea.Cmd {
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, time.Duration(loginPollWait+60)*time.Second)
		defer cancel()
		v, err := fn(ctx)
		return resultMsg{val: v, err: err, then: func(m *Model, val any, err error) tea.Cmd { return o.got(m, val, err) }}
	}
}

func (o *deviceLoginOverlay) got(m *Model, val any, err error) tea.Cmd {
	if m.overlay != o {
		return nil // closed; esc already cancelled it on the machine
	}
	if err != nil {
		if o.v.LoginID != "" && contracts.IsCode(err, contracts.ErrUnavailable) {
			return o.poll(m) // a slow round trip; the login keeps running
		}
		o.err = err
		return nil
	}
	o.v = val.(devlogin.View)
	if o.v.Done() {
		if o.v.State == devlogin.StateCompleted {
			return m.reload("accounts", nil)
		}
		return nil
	}
	return o.poll(m)
}

func (o *deviceLoginOverlay) poll(m *Model) tea.Cmd {
	id := o.v.LoginID
	return o.call(m, func(ctx context.Context) (devlogin.View, error) {
		start := time.Now()
		v, err := devlogin.Status(ctx, m.api, o.nodeID, id, loginPollWait)
		paceLoginPoll(ctx, start)
		return v, err
	})
}

func (o *deviceLoginOverlay) hints() []hint {
	switch {
	case o.v.State == devlogin.StateCompleted && o.reg == nil && !o.v.LiveLogin && o.regNote == "":
		return []hint{{"r", "add to Pyre"}, {"esc", "close"}}
	case o.v.Done() || o.err != nil:
		return []hint{{"esc", "close"}}
	case o.v.VerificationURL != "":
		return []hint{{"o", "open link here"}, {"esc", "cancel login"}}
	}
	return []hint{{"esc", "cancel"}}
}

func (o *deviceLoginOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc", "q":
		m.overlay = nil
		if o.v.LoginID != "" && !o.v.Done() {
			id, node := o.v.LoginID, o.nodeID
			return m.do(func(ctx context.Context) (any, error) {
				return devlogin.Cancel(ctx, m.api, node, id)
			}, func(m *Model, _ any, err error) tea.Cmd {
				if err != nil {
					return m.errToast("cancel login", err)
				}
				return m.setToast("login cancelled; nothing was saved", toneMuted)
			})
		}
		return nil
	case "o":
		if o.v.VerificationURL != "" && !o.v.Done() {
			if err := openLoginURL(o.v.VerificationURL); err == nil {
				o.opened = true
			}
		}
		return nil
	case "r":
		if o.v.State != devlogin.StateCompleted || o.reg != nil || o.registering || o.regNote != "" {
			return nil
		}
		if o.v.LiveLogin {
			o.regNote = devlogin.LiveLoginRefusal
			return nil
		}
		o.registering = true
		v := o.v
		return m.do(func(ctx context.Context) (any, error) {
			return devlogin.Register(ctx, m.api, v)
		}, func(m *Model, val any, err error) tea.Cmd {
			o.registering = false
			if err != nil {
				o.regNote = err.Error()
				if e, ok := err.(*contracts.Error); ok {
					o.regNote = e.Message
				}
				return nil
			}
			r := val.(devlogin.Registration)
			o.reg = &r
			return m.reload("accounts", nil)
		})
	}
	return nil
}

func (o *deviceLoginOverlay) view(m *Model, w, h int) []string {
	bw := min(72, w-2)
	inner := bw - 4
	title := "Log " + o.node + " into Codex"
	var lines []string
	add := func(s ...string) { lines = append(lines, s...) }
	switch {
	case o.err != nil:
		msg := o.err.Error()
		if e, ok := o.err.(*contracts.Error); ok {
			msg = e.Message
		}
		add(m.th.tone(toneWarn, trunc("Could not start: ", inner)))
		for _, l := range wrap(msg, inner) {
			add(m.th.dim.Render(l))
		}
	case o.v.State == devlogin.StateStarting:
		add(m.th.dim.Render("Starting the login on "+o.node+"…"), "")
	case !o.v.Done() || o.v.State == devlogin.StateWaiting:
		add(m.th.label.Render("1  Open    ")+m.th.accent.Render(trunc(o.v.VerificationURL, inner-10)),
			m.th.label.Render("2  Enter   ")+m.th.title.Render(o.v.UserCode), "")
		if qr := qrLines(qrEncode(o.v.VerificationURL), m.th.dark || m.th.plain); qr != nil && h >= len(qr)+14 && inner >= len([]rune(qr[0])) {
			pad := (inner - len([]rune(qr[0]))) / 2
			for _, l := range qr {
				add(fmt.Sprintf("%*s%s", pad, "", l))
			}
			add("")
		}
		st := devlogin.Describe(o.v)
		if o.opened {
			st = "opened here · " + st
		}
		add(m.th.dim.Render(trunc("● "+st, inner)))
		add(m.th.faint.Render(trunc("Approve from any device. The login stays on "+o.node+".", inner)))
	default:
		tn := toneWarn
		if o.v.State == devlogin.StateCompleted {
			tn = toneGood
		}
		for _, l := range wrap(devlogin.Describe(o.v), inner) {
			add(m.th.tone(tn, l))
		}
		switch {
		case o.v.State != devlogin.StateCompleted:
		case o.v.LiveLogin:
			add("")
			for _, l := range wrap(devlogin.LiveLoginRefusal, inner) {
				add(m.th.dim.Render(l))
			}
		case o.reg != nil:
			add("", m.th.tone(toneGood, trunc("Added to Pyre as “"+o.reg.Account.Label+"” on "+o.node+".", inner)))
		case o.registering:
			add("", m.th.dim.Render("Adding to Pyre…"))
		case o.regNote != "":
			add("")
			for _, l := range wrap(o.regNote, inner) {
				add(m.th.tone(toneWarn, l))
			}
		default:
			add("", m.th.dim.Render(trunc("Press r to use it from Pyre on "+o.node+" (as “"+devlogin.LabelFor(o.v.Email)+"”).", inner)))
		}
	}
	var hs string
	for i, x := range o.hints() {
		if i > 0 {
			hs += m.th.dim.Render("   ")
		}
		hs += m.th.key.Render(x.key) + m.th.dim.Render(" "+x.label)
	}
	add("", hs)
	return m.box(title, lines, bw, w, h)
}

// paceLoginPoll keeps status reads at most one per second when the node
// answers at once (an older node or the mock ignore wait_seconds).
func paceLoginPoll(ctx context.Context, start time.Time) {
	if d := time.Second - time.Since(start); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
}
