package mock

import (
	"encoding/json"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/devlogin"
)

// Simulated account.device_login* (internal/devlogin): a login shows the
// real verification URL and a synthetic code, then "approves" itself after
// DeviceLoginApproveAfter of mock time. Nothing is saved anywhere.
var (
	// DeviceLoginApproveAfter is when a simulated login completes.
	DeviceLoginApproveAfter = 8 * time.Second
	// DeviceLoginEmail is the simulated account.
	DeviceLoginEmail = "you@synthetic.invalid"
	// DeviceLoginLive makes the simulated login match the machine's own
	// default login (registration is then refused).
	DeviceLoginLive = false
)

var mockDeviceLogins sync.Map // *Backend -> *sync.Map(login id -> devlogin.View)

func (b *Backend) deviceLoginAction(n *c.Node, action string, params c.RawJSON) (any, bool, error) {
	switch action {
	case devlogin.ActionStart, devlogin.ActionStatus, devlogin.ActionCancel:
	default:
		return nil, false, nil
	}
	m, _ := mockDeviceLogins.LoadOrStore(b, &sync.Map{})
	logins := m.(*sync.Map)
	now := b.now()
	if action == devlogin.ActionStart {
		var p devlogin.Params
		_ = json.Unmarshal(params, &p)
		if p.Provider == c.ProviderAnthropic {
			return nil, true, c.Errorf(c.ErrUnsupported, "Claude Code has no device-code login (mock)")
		}
		exp := now.Add(15 * time.Minute)
		v := devlogin.View{LoginID: c.NewID("dlogin"), Provider: c.ProviderOpenAI, NodeID: n.NodeID, State: devlogin.StateWaiting,
			VerificationURL: "https://auth.openai.com/codex/device", UserCode: "SYNT-H3TIC", CodeExpiresAt: &exp, StartedAt: now, Version: 2}
		logins.Store(v.LoginID, v)
		return v, true, nil
	}
	var p devlogin.StatusParams
	_ = json.Unmarshal(params, &p)
	raw, ok := logins.Load(p.LoginID)
	if !ok {
		return nil, true, c.Errorf(c.ErrNotFound, "no such login on this machine")
	}
	v := raw.(devlogin.View)
	if !v.Done() {
		switch {
		case action == devlogin.ActionCancel:
			v.State, v.UserCode, v.FinishedAt = devlogin.StateCancelled, "", &now
			v.Version++
		case now.Sub(v.StartedAt) >= DeviceLoginApproveAfter:
			v.State, v.UserCode, v.FinishedAt = devlogin.StateCompleted, "", &now
			v.Email, v.RotaName, v.LiveLogin = DeviceLoginEmail, DeviceLoginEmail, DeviceLoginLive
			if v.LiveLogin {
				v.Detail = devlogin.LiveLoginRefusal
			}
			v.Version++
		}
		logins.Store(v.LoginID, v)
	}
	return v, true, nil
}
