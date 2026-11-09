package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// In-turn mail doorbell (contracts.CapInputDoorbell, Options.MailNotice).
//
// Queued Pyre mail waits for the thread to go idle: a busy thread takes no
// turn/start, and steering a peer's message into the turn would hand the
// agent input it did not ask for. An agent in a long tool-heavy turn
// therefore never learned it had mail until the turn ended
// (iss_…: 10 messages, among them its integrator's
// instructions, waited up to 9 minutes). Claude runs hear of mail through
// their doorbell monitor; Codex runs get the same through one short
// turn/steer notice with the active turn id: the count and the oldest
// message's age, never a body or a sender, so the trust boundary stays
// where it was. The agent reads the mail with the pyre inbox tool (that
// read settles it: it is never pasted afterwards); unread mail is still
// delivered at idle as before.
//
// Bounds: the engine reports only mail that waited delivery.MailNoticeAge;
// at most one notice per turn, and a further one in the same turn only for
// mail no notice counted yet, mailNoticeEvery after the last; none while an
// approval or question is pending, while a delivery, wake or capacity
// continue is sending, or in a run without the pyre MCP server. A steer
// Codex refused (the turn ended or changed) is dropped, never retried: the
// mail is still delivered at idle. A notice is recorded before it is sent,
// in the run's state dir (survives a pyred restart), so no outcome, known
// or unknown, ever sends it twice.

const (
	// mailNoticeIDPrefix starts a notice's clientUserMessageId (how the
	// transcript tells it from typed input).
	mailNoticeIDPrefix = "pyre-doorbell-"
	// mailNoticeEvery spaces notices within one turn.
	mailNoticeEvery = 5 * time.Minute
	// mailNoticedMax bounds the message ids remembered as announced.
	mailNoticedMax = 256
	// mailNoticeFile (0600, in the run's private state dir) is the record.
	mailNoticeFile = "mail-notice.json"
)

// mailNoticeRecord is the run's doorbell record (mailNoticeFile).
type mailNoticeRecord struct {
	Turn    string    `json:"turn"`              // turn the last notice went to
	At      time.Time `json:"at"`                // when it was sent
	ID      string    `json:"client_id"`         // its clientUserMessageId
	Outcome string    `json:"outcome,omitempty"` // steered | refused: … | unknown: …
	IDs     []string  `json:"message_ids"`       // mail announced so far, oldest first
}

func loadMailNotice(stateDir string) mailNoticeRecord {
	var r mailNoticeRecord
	if b, err := os.ReadFile(filepath.Join(stateDir, mailNoticeFile)); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	return r
}

func saveMailNotice(stateDir string, r mailNoticeRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(stateDir, mailNoticeFile), b)
}

// NotifyQueuedMail implements delivery.MailNotifier (the node's delivery
// engine calls it while the run is busy): it steers one notice
// about mail (the session's queued mail that waited long enough) into the
// thread's active turn when the bounds above allow it.
func (a *Adapter) NotifyQueuedMail(ctx context.Context, h contracts.RunHandle, mail []contracts.QueuedMail) {
	if !a.o.MailNotice || len(mail) == 0 || stopped(h.StateDir) {
		return
	}
	rr, err := a.run(h)
	if err != nil {
		return
	}
	if !rr.deliverMu.TryLock() {
		return // a delivery, wake or capacity continue is sending: the next pass
	}
	defer rr.deliverMu.Unlock()
	if rr.retrying.Load() {
		return
	}
	// Never start an app-server for a notice: no app-server, no turn.
	c, err := a.client(ctx, rr, false)
	if err != nil {
		return
	}
	now := a.o.Now()
	ts := rr.ts
	ts.mu.Lock()
	turn := ts.mailNoticeTurn(rr.stateDir, mail, now)
	if turn == "" {
		ts.mu.Unlock()
		return
	}
	id := fmt.Sprintf("%s%d", mailNoticeIDPrefix, now.UnixNano())
	rec := ts.notice
	rec.Turn, rec.At, rec.ID, rec.Outcome = turn, now, id, "sending"
	for _, m := range mail {
		if !slices.Contains(rec.IDs, m.MessageID) {
			rec.IDs = append(rec.IDs, m.MessageID)
		}
	}
	if n := len(rec.IDs); n > mailNoticedMax {
		rec.IDs = slices.Clone(rec.IDs[n-mailNoticedMax:])
	}
	ts.notice = rec
	ts.mu.Unlock()
	// Recorded before the send: whatever happens next, this mail is never
	// announced again (an unknown outcome is never resent).
	if err := saveMailNotice(rr.stateDir, rec); err != nil {
		return
	}
	var sr turnSteerResponse
	err = c.call(ctx, "turn/steer", turnSteerParams{ThreadID: rr.meta.ThreadID, ExpectedTurnID: turn,
		Input: []userInput{{Type: "text", Text: mailNoticeText(mail, now)}}, ClientUserMessageID: id}, &sr)
	var re *rpcError
	switch {
	case err == nil:
		rec.Outcome = "steered"
	case errors.As(err, &re):
		rec.Outcome = "refused: " + oneLine(re.Message, 200) // e.g. the turn ended; dropped
	default:
		rec.Outcome = "unknown: " + oneLine(err.Error(), 200) // may have landed; never resent
	}
	ts.mu.Lock()
	if ts.notice.ID == id {
		ts.notice.Outcome = rec.Outcome
	}
	ts.mu.Unlock()
	_ = saveMailNotice(rr.stateDir, rec)
}

// mailNoticeTurn returns the active turn a notice about mail may go to now
// ("" for none). Called with ts.mu held.
func (ts *threadState) mailNoticeTurn(stateDir string, mail []contracts.QueuedMail, now time.Time) string {
	if !ts.noticeLoaded {
		ts.notice, ts.noticeLoaded = loadMailNotice(stateDir), true
	}
	if ts.closing || ts.status.Type != "active" || ts.activeTurn == "" {
		return ""
	}
	if slices.Contains(ts.status.ActiveFlags, "waitingOnApproval") || slices.Contains(ts.status.ActiveFlags, "waitingOnUserInput") {
		return ""
	}
	for _, pa := range ts.approvals {
		if pa.approval.State == contracts.ApprovalPending {
			return ""
		}
	}
	if ts.config["mcp_servers.pyre"] == nil {
		return "" // no pyre MCP server: the agent has no inbox tool to read it with
	}
	fresh := false
	for _, m := range mail {
		fresh = fresh || !slices.Contains(ts.notice.IDs, m.MessageID)
	}
	if !fresh {
		return ""
	}
	if ts.notice.Turn == ts.activeTurn && now.Sub(ts.notice.At) < mailNoticeEvery {
		return ""
	}
	return ts.activeTurn
}

// mailNoticeText is the notice: how much mail waits and for how long,
// never what it says or who sent it.
func mailNoticeText(mail []contracts.QueuedMail, now time.Time) string {
	oldest := now
	for _, m := range mail {
		if m.ReceivedAt.Before(oldest) {
			oldest = m.ReceivedAt
		}
	}
	noun, them := "message", "it"
	if len(mail) != 1 {
		noun, them = "messages", "them"
	}
	return fmt.Sprintf("[pyre] %d new %s for this session (oldest %s). Read %s with mcp__pyre__inbox (unacked: true) when you reach a stopping point; unread mail is delivered when this turn ends.",
		len(mail), noun, now.Sub(oldest).Round(time.Second), them)
}

// isMailNotice: the text of a mail notice (mailNoticeText).
func isMailNotice(t string) bool {
	first, _, _ := strings.Cut(t, "\n")
	return strings.HasPrefix(first, "[pyre] ") && strings.Contains(first, " new message") && strings.Contains(first, " for this session (oldest ")
}
