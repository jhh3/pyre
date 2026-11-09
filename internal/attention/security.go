package attention

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

// deriveSecurity raises an urgent "security" item for a high-severity
// security.alert (decision I79): a session's tool call looked like a
// credential being sent out, or used a planted decoy. One item per session;
// repeats count. A "low" alert (a secret sent to its own provider, a decoy
// only read) is audited by the hub and raises nothing. The summary is the
// node's fixed-phrase reason (names of variables and hosts, never values);
// the redacted command stays in the audit log.
func deriveSecurity(ctx context.Context, ev contracts.Event, lk Lookup) []change {
	var a contracts.SecurityAlert
	if !unmarshal(ev, &a) || a.Severity != "high" {
		return nil
	}
	session := firstNonEmpty(a.SessionID, ev.SessionID)
	if session == "" {
		return nil
	}
	title := "Possible credential leak in " + quoteTitle(sessionTitle(ctx, lk, session))
	if a.Rule == "decoy-value" || a.Rule == "decoy+network" {
		title = "Decoy credential used in " + quoteTitle(sessionTitle(ctx, lk, session))
	}
	summary := a.Reason
	if a.NodeName != "" {
		summary += " (on " + a.NodeName + ")"
	}
	return []change{{
		op:        opRaise,
		groupKey:  "security:" + session,
		kind:      contracts.AttnSecurity,
		severity:  "urgent",
		object:    contracts.ObjectRef{Kind: contracts.KindSession, ID: session},
		sessionID: session,
		projectID: ev.ProjectID,
		title:     short(title, maxTitle),
		summary:   short(summary, maxSummary),
	}}
}
