package tui

import "github.com/jhh3/pyre/internal/contracts"

func (t theme) tone(tn tone, s string) string { return t.tones[tn].Render(s) }

// status is a symbol + label + tone triple.
type status struct {
	sym   string
	label string
	tone  tone
}

func (s status) String() string { return s.sym + " " + s.label }

// sessionStatus classifies a session for display. Offline and stale take
// precedence over the last observed run state, which is never mutated into
// idle or failed by loss of contact.
func sessionStatus(sv contracts.SessionView, node *contracts.Node) status {
	if sv.Run == nil {
		return status{"·", "no run", toneMuted}
	}
	if !sv.Run.State.Terminal() {
		if node != nil && node.Connection.State != contracts.ConnConnected {
			return status{"◌", "offline", toneMuted}
		}
		if sv.Run.Freshness.Stale || sv.Freshness.Stale {
			return status{"◌", "stale", toneMuted}
		}
	}
	if sv.Run.State == contracts.RunRunning && sv.Session.Harness == contracts.HarnessShell {
		// A live shell is open, not "working": no agent is busy, and a
		// screen of green shells would drown the agents that are.
		return status{"●", "running", toneNone}
	}
	return runStatus(sv.Run.State)
}

func runStatus(s contracts.RunState) status {
	switch s {
	case contracts.RunRunning:
		return status{"●", "working", toneGood}
	case contracts.RunStarting:
		return status{"◔", "starting", toneGood}
	case contracts.RunWaitingApproval:
		return status{"!", "approval", toneWarn}
	case contracts.RunWaitingInput:
		return status{"○", "idle", toneNone}
	case contracts.RunWaitingBackground:
		return status{"◐", "background", toneNone}
	case contracts.RunExiting:
		return status{"◑", "exiting", toneMuted}
	case contracts.RunExited:
		return status{"–", "exited", toneMuted}
	case contracts.RunFailed:
		return status{"✗", "failed", toneBad}
	}
	return status{"?", "unknown", toneMuted}
}

func nodeStatus(n contracts.Node) status {
	switch n.Connection.State {
	case contracts.ConnConnected:
		return status{"●", "online", toneGood}
	case contracts.ConnDisconnected:
		return status{"◌", "offline", toneMuted}
	case contracts.ConnNever:
		return status{"·", "never seen", toneMuted}
	case contracts.ConnRevoked:
		return status{"✗", "revoked", toneBad}
	}
	return status{"?", string(n.Connection.State), toneMuted}
}

func attentionStatus(k contracts.AttentionKind) status {
	switch k {
	case contracts.AttnApproval:
		return status{"!", "approval", toneWarn}
	case contracts.AttnQuestion:
		return status{"?", "question", toneWarn}
	case contracts.AttnFailure:
		return status{"✗", "failure", toneBad}
	case contracts.AttnAccountPool:
		return status{"⊘", "accounts", toneBad}
	case contracts.AttnReviewReady:
		return status{"◇", "review", toneAccent}
	case contracts.AttnMessage:
		return status{"✉", "message", toneNone}
	case contracts.AttnIndeterminate:
		return status{"⁇", "uncertain", toneWarn}
	case contracts.AttnConflict:
		return status{"≠", "conflict", toneWarn}
	case contracts.AttnComment:
		return status{"✎", "comment", toneNone}
	case contracts.AttnUpdate:
		return status{"↑", "update", toneNone}
	case contracts.AttnNode:
		return status{"◌", "machine", toneMuted}
	}
	return status{"·", string(k), toneNone}
}

func deliveryTone(s contracts.RuntimeDeliveryState) tone {
	switch s {
	case contracts.DeliveryAccepted:
		return toneGood
	case contracts.DeliveryIndeterminate:
		return toneWarn
	case contracts.DeliveryRejected, contracts.DeliveryExpired:
		return toneBad
	}
	return toneNone
}

func (t theme) status(s status) string { return t.tone(s.tone, s.sym+" "+s.label) }
