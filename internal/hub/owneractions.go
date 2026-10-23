package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Owner approvals for owner-only operations (review finding "owner-only
// friction": agents were refused and handed the owner shell commands to
// paste). When a session asks for an operation only the owner may perform
// (archiving a session it did not start, or itself when the owner started
// it; revoking a node; creating an enrollment ticket), the hub keeps the
// guard but files the exact request as an ask in the owner's inbox
// (options approve/deny, no default, ownerActionDeadline) and answers the
// caller approval_requested instead of forbidden. A repeat for the same
// operation and target merges into the open ask as an update.
//
// The ask is created hub-side on behalf of the requesting session, so a
// child session's request reaches the owner too (only these hub-made asks
// skip the "children ask their parent" rule); its carrying owner message
// comes from ownerActionSender. Approving moves the row to "approved" in
// the answer's transaction (decideAskTx); the decision send (sendDecisions)
// then runs the stored request as the approving owner with operation ID
// op_owneraction-<ask>, so a retry or the maintenance repair replays it
// instead of running it twice, and replies to the requester (and every
// session whose repeat merged) with the outcome. The audit entries carry
// the reason "approved request from <session>". Deny, expiry and
// withdrawal reach the requester as ordinary ask decisions; nothing runs.
//
// An enrollment ticket's one-time secret is never put in a message: the
// requester runs the same command again and the hub hands it the ticket by
// replaying the approved operation (which rotates the secret).

var ownerActionMigrations = []sqlitex.Migration{{Namespace: "hub_owner_actions", Version: 1, Name: "owner_actions", SQL: `
CREATE TABLE owner_actions (
	ask_id TEXT PRIMARY KEY,
	op TEXT NOT NULL,
	target TEXT NOT NULL,
	session_id TEXT NOT NULL, -- the requester
	request TEXT NOT NULL,    -- the exact request to run (operation ID set when run)
	state TEXT NOT NULL,      -- open | approved | done | failed | denied | closed | spent
	approved_by TEXT,         -- the approving owner principal (JSON)
	result TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX owner_actions_open ON owner_actions(op, target) WHERE state='open';
CREATE INDEX owner_actions_target ON owner_actions(op, target, state, updated_at);
`}}

// Owner-only operations a session may request.
const (
	ownerOpArchive = "sessions.archive"
	ownerOpRevoke  = contracts.OpRevokeNode
	ownerOpEnroll  = contracts.OpCreateEnrollment
)

const (
	ownerActionApprove = "approve"
	ownerActionDeny    = "deny"
	// ownerActionDeadline: an unanswered request expires (nothing runs).
	ownerActionDeadline = 7 * 24 * time.Hour
	// ownerActionDeniedHold: a denied request is not filed again this soon.
	ownerActionDeniedHold = 24 * time.Hour
)

// ownerActionSender sends the owner message carrying an owner-action ask
// (a child requester may not message the owner itself).
var ownerActionSender = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "approvals"}

// ownerAction is one request to file.
type ownerAction struct {
	op, target string
	req        any    // the exact request to run
	title      string // the ask title (a question)
	what       string // markdown: what is requested and what approving does
	reason     string // agent-supplied reason, if any
}

type ownerActionRow struct {
	askID, op, target, sessionID, request, state, result string
	approvedBy                                           contracts.Principal
}

func scanOwnerAction(sc interface{ Scan(...any) error }) (ownerActionRow, error) {
	var r ownerActionRow
	var by sql.NullString
	err := sc.Scan(&r.askID, &r.op, &r.target, &r.sessionID, &r.request, &r.state, &by, &r.result)
	if by.Valid {
		_ = persistence.FromJSON(by.String, &r.approvedBy)
	}
	return r, err
}

const ownerActionCols = `ask_id, op, target, session_id, request, state, approved_by, result`

// sessionLabel is "\"<title>\" (<id>)", or the bare ID when untitled.
func sessionLabel(s contracts.Session) string {
	if t := strings.TrimSpace(s.Title); t != "" {
		return fmt.Sprintf("%q (%s)", oneLine(t, 60), s.SessionID)
	}
	return s.SessionID
}

// requestOwnerAction files act for the owner's approval on behalf of the
// session principal p (or merges it into the open request for the same
// operation and target) and returns the approval_requested error the
// caller sees.
func (h *Hub) requestOwnerAction(ctx context.Context, p contracts.Principal, act ownerAction) error {
	requester, err := h.getSession(ctx, h.db, p.ID)
	if err != nil {
		return err
	}
	now := h.now()
	var created *contracts.Ask
	var askID, status string
	var count int
	err = h.tx(ctx, func(tx *sql.Tx) error {
		var open string
		switch err := tx.QueryRowContext(ctx, `SELECT ask_id FROM owner_actions WHERE op=? AND target=? AND state='open'`, act.op, act.target).Scan(&open); {
		case err == nil:
			askID, status = open, "merged"
			text := "Requested again by " + sessionLabel(requester) + "."
			if act.reason != "" {
				text += " Reason given: " + oneLine(act.reason, 1000)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO ask_updates(ask_id,seq,session_id,text,at) SELECT ?, COALESCE(MAX(seq),0)+1, ?, ?, ? FROM ask_updates WHERE ask_id=?`,
				open, requester.SessionID, text, persistence.Time(now), open); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE asks SET count=count+1, updated_at=? WHERE ask_id=?`, persistence.Time(now), open); err != nil {
				return err
			}
			a, err := h.getAskFull(ctx, tx, open)
			if err != nil {
				return err
			}
			count = a.Count
			return h.emitAsk(ctx, tx, a)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var denied, at string
		switch err := tx.QueryRowContext(ctx, `SELECT ask_id, updated_at FROM owner_actions WHERE op=? AND target=? AND state='denied' AND updated_at>=? ORDER BY updated_at DESC LIMIT 1`,
			act.op, act.target, persistence.Time(now.Add(-ownerActionDeniedHold))).Scan(&denied, &at); {
		case err == nil:
			return contracts.Errorf(contracts.ErrForbidden, "the owner declined this request %s (%s); it is not filed again within %s. If something changed, explain it with ask_owner (pyre ask new)",
				askClock(persistence.ParseTime(at)), denied, ownerActionDeniedHold).WithRef(contracts.KindAsk, denied)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		in := askInput{CreateAskRequest: contracts.CreateAskRequest{
			TopicKey: NormalizeTopicKey("owner action " + act.op + " " + act.target),
			Title:    oneLine(act.title, contracts.AskMaxTitle),
			Context:  ownerActionContext(requester, act),
			Options: []contracts.AskOption{
				{ID: ownerActionApprove, Label: "Approve", Detail: "Pyre runs it now with your authority, logs it, and tells the session the outcome"},
				{ID: ownerActionDeny, Label: "Deny", Detail: "nothing changes; the session is told you declined"},
			},
			ProjectID: requester.ProjectID,
			Reopen:    true, ConfirmNew: true,
		}, deadline: now.Add(ownerActionDeadline), rule: "custom"}
		resp, err := h.createAskTx(ctx, tx, requester.SessionID, in, now)
		if err != nil {
			return err
		}
		if resp.Ask == nil || resp.Status != contracts.AskCreated {
			return contracts.Errorf(contracts.ErrInternal, "filing the approval returned %s", resp.Status)
		}
		created, askID, status, count = resp.Ask, resp.Ask.AskID, "filed", 1
		_, err = tx.ExecContext(ctx, `INSERT INTO owner_actions(ask_id,op,target,session_id,request,state,created_at,updated_at) VALUES(?,?,?,?,?,'open',?,?)`,
			askID, act.op, act.target, requester.SessionID, persistence.JSON(act.req), persistence.Time(now), persistence.Time(now))
		return err
	})
	if err != nil {
		return err
	}
	if created != nil && created.MessageID != "" {
		h.sendAskMessage(ctx, *created)
	}
	msg := fmt.Sprintf("needs the owner's approval: filed as %s in the owner's inbox (%s). Pyre runs it with the owner's authority when he approves and messages you the outcome; do not ask him to run it himself, carry on with other work",
		askID, strings.TrimSuffix(act.title, "?"))
	if status == "merged" {
		msg = fmt.Sprintf("already waiting for the owner's approval as %s (requested %d times; your request was added to it). Pyre runs it when he approves and messages you the outcome; do not ask him to run it himself",
			askID, count)
	}
	return contracts.Errorf(contracts.ErrApprovalRequested, "%s", msg).WithRef(contracts.KindAsk, askID).
		WithDetail("ask_id", askID).WithDetail("status", status)
}

// ownerActionContext is the ask context: who asks for what, and what each
// answer does.
func ownerActionContext(requester contracts.Session, act ownerAction) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s asks for an operation only you may perform:\n\n%s\n\n", sessionLabel(requester), strings.TrimSpace(act.what))
	if act.reason != "" {
		fmt.Fprintf(&b, "Reason given: %s\n\n", oneLine(act.reason, 2000))
	}
	b.WriteString("- **Approve**: Pyre runs exactly this request now with your authority, records it in the audit log as approved by you, and messages the session the outcome.\n")
	b.WriteString("- **Deny**: nothing changes; the session is told you declined.\n\n")
	fmt.Fprintf(&b, "There is no default: with no answer within %d days the request expires and nothing runs. Repeat requests for the same operation are merged here.", int(ownerActionDeadline/(24*time.Hour)))
	s := b.String()
	if utf8.RuneCountInString(s) > contracts.AskMaxContext {
		s = string([]rune(s)[:contracts.AskMaxContext])
	}
	return s
}

// isOwnerActionAsk reports whether an ask carries an owner-action request.
func (h *Hub) isOwnerActionAsk(ctx context.Context, askID string) bool {
	var one int
	return h.db.QueryRowContext(ctx, `SELECT 1 FROM owner_actions WHERE ask_id=?`, askID).Scan(&one) == nil
}

// settleOwnerActionTx records an owner-action ask's decision in the same
// transaction (a no-op for other asks): an owner's approve becomes
// "approved" (run by finishOwnerActions), any other answer "denied",
// expiry or withdrawal "closed".
func (h *Hub) settleOwnerActionTx(ctx context.Context, tx *sql.Tx, a contracts.Ask) error {
	state, by := "closed", ""
	if a.State == contracts.AskAnswered {
		state = "denied"
		if a.Answer != nil && a.Answer.OptionID == ownerActionApprove && isOwner(a.Answer.By) {
			state, by = "approved", persistence.JSON(a.Answer.By)
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE owner_actions SET state=?, approved_by=?, updated_at=? WHERE ask_id=? AND state='open'`,
		state, persistence.NullStr(by), persistence.Time(h.now()), a.AskID)
	return err
}

// finishOwnerActions runs the approved owner-action asks among asks and
// tells their requesters the outcome (recording each message in sent); it
// returns the other asks for the ordinary decision message.
func (h *Hub) finishOwnerActions(ctx context.Context, asks []contracts.Ask, sent map[string]string) []contracts.Ask {
	rest := asks[:0:0]
	for _, a := range asks {
		if a.State != contracts.AskAnswered {
			rest = append(rest, a)
			continue
		}
		r, err := scanOwnerAction(h.db.QueryRowContext(ctx, `SELECT `+ownerActionCols+` FROM owner_actions WHERE ask_id=?`, a.AskID))
		if err != nil || (r.state != "approved" && r.state != "done" && r.state != "failed") {
			rest = append(rest, a) // not an owner action, or denied
			continue
		}
		if id := h.finishOwnerAction(ctx, a, r); id != "" {
			sent[a.AskID] = id
		}
	}
	return rest
}

// finishOwnerAction runs an approved request (once: a replay of its
// operation ID returns the stored outcome), stores the outcome and sends
// it to the requesters. It returns the requester's message ID ("" when the
// run or send must be retried by maintenance).
func (h *Hub) finishOwnerAction(ctx context.Context, a contracts.Ask, r ownerActionRow) string {
	if r.state == "approved" {
		result, err := h.runOwnerAction(ctx, r)
		state := "done"
		if err != nil {
			if contracts.CodeOf(err).DefaultRetryable() {
				return "" // decision_pending stays set: maintenance retries
			}
			state, result = "failed", "it failed: "+contracts.AsError(err).Message
		}
		if _, err := h.db.ExecContext(ctx, `UPDATE owner_actions SET state=?, result=?, updated_at=? WHERE ask_id=? AND state='approved'`,
			state, result, persistence.Time(h.now()), r.askID); err != nil {
			return ""
		}
		if cur, err := scanOwnerAction(h.db.QueryRowContext(ctx, `SELECT `+ownerActionCols+` FROM owner_actions WHERE ask_id=?`, r.askID)); err == nil {
			r = cur // a concurrent finisher may have stored it first
		}
	}
	outcome := "Done: " + r.result
	if r.state == "failed" {
		outcome = "But " + r.result
	}
	text := fmt.Sprintf("[pyre approval] The owner approved: %s\n%s\n(ask %s)", strings.TrimSuffix(a.Title, "?"), outcome, a.AskID)
	sender := r.approvedBy
	if sender.Kind == "" {
		sender = askSystem
	}
	recipients := []string{r.sessionID}
	if rows, err := h.db.QueryContext(ctx, `SELECT DISTINCT session_id FROM ask_updates WHERE ask_id=? AND session_id<>'' AND session_id<>?`, a.AskID, r.sessionID); err == nil {
		for rows.Next() {
			var sid string
			if rows.Scan(&sid) == nil {
				recipients = append(recipients, sid)
			}
		}
		rows.Close()
	}
	first := ""
	for i, sid := range recipients {
		s, err := h.getSession(ctx, h.db, sid)
		if err != nil || s.Archived {
			continue // nobody to tell
		}
		req := contracts.SendMessageRequest{
			Mutation:  contracts.Mutation{OperationID: "op_owneraction-msg-" + a.AskID + "-" + sid},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid},
			ProjectID: a.ProjectID,
			Mode:      contracts.ModeQueue,
			Body:      contracts.MessageBody{Text: text},
		}
		if i == 0 {
			req.InReplyTo = a.MessageID
		}
		mv, err := h.msgs.SendMessage(contracts.WithPrincipal(ctx, sender), req)
		if err != nil {
			if i == 0 && retryableSend(err) {
				return "" // maintenance retries
			}
			continue
		}
		if i == 0 {
			first = mv.Envelope.MessageID
		}
	}
	_, _ = h.db.ExecContext(ctx, `UPDATE asks SET decision_pending=0, decision_message_id=? WHERE ask_id=?`, first, a.AskID)
	return first
}

// ownerActionReasonKey carries the audit reason of an approved request.
type ownerActionReasonKey struct{}

// ownerActionReason is the audit reason carried by ctx ("" when none).
func ownerActionReason(ctx context.Context) string {
	s, _ := ctx.Value(ownerActionReasonKey{}).(string)
	return s
}

// runOwnerAction performs an approved request as the approving owner and
// describes the outcome in one line.
func (h *Hub) runOwnerAction(ctx context.Context, r ownerActionRow) (string, error) {
	who := r.sessionID
	if s, err := h.getSession(ctx, h.db, r.sessionID); err == nil {
		who = sessionLabel(s)
	}
	octx := context.WithValue(contracts.WithPrincipal(ctx, r.approvedBy), ownerActionReasonKey{}, "approved request from "+who+" ("+r.askID+")")
	opID := "op_owneraction-" + r.askID
	switch r.op {
	case ownerOpArchive:
		var req contracts.UpdateSessionRequest
		if err := persistence.FromJSON(r.request, &req); err != nil {
			return "", err
		}
		req.OperationID = opID
		v, err := h.UpdateSession(octx, req)
		if err != nil {
			return "", err
		}
		if v.Session.Archived {
			return "session " + sessionLabel(v.Session) + " is archived.", nil
		}
		return "session " + sessionLabel(v.Session) + " archives when its current turn ends (Pyre stops it gracefully first).", nil
	case ownerOpRevoke:
		var req contracts.RevokeRequest
		if err := persistence.FromJSON(r.request, &req); err != nil {
			return "", err
		}
		req.OperationID = opID
		n, err := h.RevokeNode(octx, req)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("machine %s (%s) is revoked.", orName(n.Name), n.NodeID), nil
	case ownerOpEnroll:
		var req contracts.EnrollmentRequest
		if err := persistence.FromJSON(r.request, &req); err != nil {
			return "", err
		}
		req.OperationID = opID
		t, err := h.CreateEnrollment(octx, req)
		if err != nil {
			return "", err
		}
		h.auditTx(octx, h.db, auditRec{Action: contracts.AuditNodeTicketCreated, Actor: actorOf(r.approvedBy), NodeID: t.NodeID, To: req.Name, Dedupe: "owneraction:" + r.askID})
		return fmt.Sprintf("enrollment ticket for %q created as %s, valid until %s. Its one-time secret is never sent in messages: run the same enroll-node command (same --name) again before then to receive it.",
			req.Name, t.NodeID, askClock(t.ExpiresAt)), nil
	}
	return "", contracts.Errorf(contracts.ErrInternal, "unknown owner action %q", r.op)
}

func orName(s string) string {
	if s == "" {
		return "(unnamed)"
	}
	return s
}

// ---- the guarded operations ----

// archiveApproval files a session's archive request for a session it may
// not archive itself (checkArchive). An approved archive of a live session
// happens when its current turn ends.
func (h *Hub) archiveApproval(ctx context.Context, p contracts.Principal, target contracts.Session) error {
	yes := true
	what := fmt.Sprintf("**Archive session %s.** ", sessionLabel(target))
	switch {
	case target.SessionID == p.ID:
		what += "It asks to archive itself; you started it, so only you may archive it."
	default:
		what += "The requester did not start it, so only you may archive it."
	}
	state := "it has no run"
	if r, err := h.getRun(ctx, h.db, target.CurrentRunID); err == nil {
		state = "its run is " + string(r.State)
	}
	what += fmt.Sprintf(" Right now %s. A live session finishes its current turn first, then Pyre stops it gracefully and archives it; archived sessions can be restored.", state)
	return h.requestOwnerAction(ctx, p, ownerAction{
		op: ownerOpArchive, target: target.SessionID,
		req:   contracts.UpdateSessionRequest{SessionID: target.SessionID, Archived: &yes, WhenDone: true},
		title: fmt.Sprintf("Archive session %q (%s)?", oneLine(orName(target.Title), 50), target.SessionID),
		what:  what,
	})
}

// revokeApproval files a session's request to revoke a node.
func (h *Hub) revokeApproval(ctx context.Context, p contracts.Principal, req contracts.RevokeRequest) error {
	n, err := h.cfg.Fleet.GetNode(ctx, contracts.GetRequest{ID: req.ID})
	if err != nil {
		return err
	}
	if n.Connection.State == contracts.ConnRevoked {
		return contracts.Errorf(contracts.ErrConflict, "machine %s (%s) is already revoked", orName(n.Name), n.NodeID).WithRef(contracts.KindNode, n.NodeID)
	}
	what := fmt.Sprintf("**Revoke machine %s (%s).** Its credential stops working at once and its connection closes; sessions on it can no longer be reached through Pyre. Re-adding it needs a new enrollment.", orName(n.Name), n.NodeID)
	return h.requestOwnerAction(ctx, p, ownerAction{
		op: ownerOpRevoke, target: n.NodeID,
		req:    contracts.RevokeRequest{ID: n.NodeID, Reason: req.Reason},
		title:  fmt.Sprintf("Revoke machine %s (%s)?", oneLine(orName(n.Name), 50), n.NodeID),
		what:   what,
		reason: req.Reason,
	})
}

// enrollApproval hands a session the ticket of its approved enrollment
// request (replaying the approved operation rotates the secret), or files
// the request.
func (h *Hub) enrollApproval(ctx context.Context, p contracts.Principal, req contracts.EnrollmentRequest) (contracts.EnrollmentTicket, error) {
	name := strings.Join(strings.Fields(req.Name), " ")
	if name == "" {
		return contracts.EnrollmentTicket{}, contracts.Errorf(contracts.ErrInvalid, "node name is required")
	}
	target := strings.ToLower(name)
	r, err := scanOwnerAction(h.db.QueryRowContext(ctx, `SELECT `+ownerActionCols+` FROM owner_actions WHERE op=? AND target=? AND state='done'
		AND (session_id=? OR ask_id IN (SELECT ask_id FROM ask_updates WHERE session_id=?)) ORDER BY updated_at DESC LIMIT 1`, ownerOpEnroll, target, p.ID, p.ID))
	switch {
	case err == nil:
		var stored contracts.EnrollmentRequest
		if err := persistence.FromJSON(r.request, &stored); err != nil {
			return contracts.EnrollmentTicket{}, err
		}
		stored.OperationID = "op_owneraction-" + r.askID
		t, err := h.cfg.Fleet.CreateEnrollment(contracts.WithPrincipal(ctx, r.approvedBy), stored)
		if err == nil || contracts.CodeOf(err).DefaultRetryable() {
			return t, err
		}
		// Redeemed, expired or gone: the approval is used up; file anew.
		_, _ = h.db.ExecContext(ctx, `UPDATE owner_actions SET state='spent', updated_at=? WHERE ask_id=?`, persistence.Time(h.now()), r.askID)
	case !errors.Is(err, sql.ErrNoRows):
		return contracts.EnrollmentTicket{}, err
	}
	what := fmt.Sprintf("**Create a one-time enrollment ticket for a machine named %q**", name)
	if req.SSHAlias != "" {
		what += fmt.Sprintf(" (ssh alias %q)", req.SSHAlias)
	}
	what += ". The ticket lets one machine join your fleet as a node. Approving creates it; the requesting session receives its secret by running the same command again (the secret never goes into a message) and then installs and enrolls the machine."
	return contracts.EnrollmentTicket{}, h.requestOwnerAction(ctx, p, ownerAction{
		op: ownerOpEnroll, target: target,
		req:   contracts.EnrollmentRequest{Name: name, SSHAlias: req.SSHAlias, TTL: req.TTL},
		title: fmt.Sprintf("Create an enrollment ticket for %q?", oneLine(name, 60)),
		what:  what,
	})
}
