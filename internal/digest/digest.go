// Package digest builds the "while you were away" digest: a concise,
// per-agent summary of what happened since the owner last looked — what
// each agent did, what changed (files, documents, artifacts, messages),
// what is blocked and what needs the owner — with a deep link per line to
// the exact transcript moment or object.
//
// It is computed client-side from existing hub operations (events.list,
// sessions.list, attention.list, sessions.read); there is no digest hub
// operation. The summary line is extracted deterministically (the first
// sentence of the agent's last message, counts of turns and tool calls,
// files touched from the transcript's tool entries). No model is called.
// An optional summarizer (see Summarize) can rewrite the digest with a
// cheap model through the owner's own harness; it is off by default.
//
// Build is a pure function of its Input, so every client (CLI, TUI, MCP,
// and the Android port in android/.../digest) produces the same ranking
// from the same data; tests/fixtures/digest holds the shared fixture.
package digest

import (
	"encoding/json"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Rank orders agents in the digest: needs you first, then completed work,
// then still working. Quiet sessions (only observations, nothing done) are
// collapsed into a count.
type Rank string

const (
	RankNeedsYou Rank = "needs_you"
	RankDone     Rank = "done"
	RankWorking  Rank = "working"
	RankQuiet    Rank = "quiet"
)

func (r Rank) order() int {
	switch r {
	case RankNeedsYou:
		return 0
	case RankDone:
		return 1
	case RankWorking:
		return 2
	}
	return 3
}

// Ref is one object a line points at, with its deep link.
type Ref struct {
	Kind  string    `json:"kind"` // attention | artifact_version | document_revision | message | note | comment | approval
	ID    string    `json:"id"`
	Title string    `json:"title"`
	Link  string    `json:"link"`
	At    time.Time `json:"at"`
	// Direction for messages: "sent" (by this agent) or "received".
	Direction string `json:"direction,omitempty"`
}

// Agent is one session's line in the digest.
type Agent struct {
	SessionID string     `json:"session_id"`
	Title     string     `json:"title"`
	Harness   c.Harness  `json:"harness,omitempty"`
	Node      string     `json:"node,omitempty"`
	ProjectID *string    `json:"project_id,omitempty"`
	RunID     string     `json:"run_id,omitempty"`
	State     c.RunState `json:"state,omitempty"`
	Archived  bool       `json:"archived,omitempty"`
	Rank      Rank       `json:"rank"`
	// Summary is the one-line summary; SummarySource says where it came
	// from: "attention", "blocked", "assistant", "activity", "state" or
	// "model" (only with the optional summarizer).
	Summary       string `json:"summary"`
	SummarySource string `json:"summary_source"`
	// Blocked is set when the agent cannot continue without the owner
	// (failed run, accounts exhausted, waiting on a prompt).
	Blocked string `json:"blocked,omitempty"`
	// Link is the deep link for the line: the attention item that needs
	// the owner, else the transcript moment of the summary
	// (pyre://session/<id>?run=<run>&seq=<n>), else the session.
	Link string `json:"link"`
	Seq  int64  `json:"seq,omitempty"` // transcript entry behind Summary, when it came from the transcript

	Turns int `json:"turns"` // user turns since the cutoff
	// Replies counts the agent's messages since the cutoff.
	Replies int `json:"replies"`
	Tools   int `json:"tools"`
	// Partial: the transcript window did not reach back to the cutoff, so
	// the counts are lower bounds.
	Partial bool     `json:"partial,omitempty"`
	Files   []string `json:"files,omitempty"`

	NeedsYou  []Ref `json:"needs_you,omitempty"`
	Artifacts []Ref `json:"artifacts,omitempty"`
	Docs      []Ref `json:"docs,omitempty"`
	Messages  []Ref `json:"messages,omitempty"`
	Notes     []Ref `json:"notes,omitempty"`

	Events       int       `json:"events"`
	LastActivity time.Time `json:"last_activity"`
}

// Digest is the whole "while you were away" summary.
type Digest struct {
	Since       time.Time `json:"since"`
	SinceCursor int64     `json:"since_cursor"`
	HeadCursor  int64     `json:"head_cursor"`
	GeneratedAt time.Time `json:"generated_at"`
	// SinceLabel says how the cutoff was chosen ("last seen", "2h").
	SinceLabel string `json:"since_label,omitempty"`
	// Truncated: more events than the bound, or history older than the
	// hub's retention; the digest covers what was read.
	Truncated bool    `json:"truncated,omitempty"`
	Agents    []Agent `json:"agents"`
	// Quiet counts sessions with only noise (observations, lease changes,
	// archived without results) collapsed out of Agents.
	Quiet int `json:"quiet"`
	// Other holds items needing the owner that belong to no session
	// (machines, accounts, updates).
	Other []Ref `json:"other,omitempty"`
	// Warnings are non-fatal read failures (a transcript that could not be
	// read from an offline machine, ...).
	Warnings []string `json:"warnings,omitempty"`
	// Summary is set only by the optional model summarizer.
	Summary string `json:"summary,omitempty"`
}

// Empty reports whether nothing happened worth showing.
func (d Digest) Empty() bool { return len(d.Agents) == 0 && len(d.Other) == 0 }

// Count returns the number of agents with rank r.
func (d Digest) Count(r Rank) int {
	n := 0
	for _, a := range d.Agents {
		if a.Rank == r {
			n++
		}
	}
	return n
}

// Input is everything Build needs; Fetch fills it from the hub.
type Input struct {
	Since       time.Time
	SinceCursor int64
	HeadCursor  int64
	Now         time.Time
	SinceLabel  string
	Truncated   bool
	Events      []c.Event
	Sessions    []c.SessionView
	// Attention: open and read items (any age; Build keeps those updated
	// after Since).
	Attention []c.AttentionItem
	// Transcripts: tail of each candidate session's current run, by
	// session ID. Entries at or before Since are ignored.
	Transcripts map[string]c.ReadSessionResponse
	Warnings    []string
}

// EventKinds are the event kinds the digest reads; observational noise
// (node status, leases, account observations, delivery receipts) is left
// out at the source.
var EventKinds = []c.EventKind{
	c.EvSessionCreated, c.EvSessionUpdated, c.EvRunStarted, c.EvRunObserved, c.EvRunEnded,
	c.EvMessageStored, c.EvApprovalRequested, c.EvArtifactPublished, c.EvDocumentRevised,
	c.EvDocumentConflict, c.EvCommentCreated, c.EvNoteCreated, c.EvAttentionChanged, c.EvAccountExhausted,
}

// SessionLink is the deep link to a transcript moment. Seq 0 links to the
// session itself.
func SessionLink(sessionID, runID string, seq int64) string {
	l := c.DeepLink(c.ObjectRef{Kind: c.KindSession, ID: sessionID})
	if seq <= 0 {
		return l
	}
	q := url.Values{}
	if runID != "" {
		q.Set("run", runID)
	}
	q.Set("seq", strconv.FormatInt(seq, 10))
	return l + "?" + q.Encode()
}

// Link is a parsed pyre:// deep link.
type Link struct {
	Kind  c.ObjectKind
	ID    string
	RunID string
	Seq   int64
}

// ParseLink parses pyre://<kind>/<id>[?run=..&seq=..].
func ParseLink(s string) (Link, bool) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "pyre" || u.Host == "" {
		return Link{}, false
	}
	id := strings.Trim(u.Path, "/")
	if id == "" || strings.Contains(id, "/") {
		return Link{}, false
	}
	l := Link{Kind: c.ObjectKind(u.Host), ID: id, RunID: u.Query().Get("run")}
	l.Seq, _ = strconv.ParseInt(u.Query().Get("seq"), 10, 64)
	return l, true
}

type acc struct {
	a          Agent
	sv         *c.SessionView
	startedNew bool // a run started after the cutoff
	ended      *c.Run
	exhausted  string
	failed     string
	files      map[string]bool
	lastAsst   *c.TranscriptEntry
	activity   bool // something meaningful beyond observations
}

// Build computes the digest. It never calls the network.
func Build(in Input) Digest {
	d := Digest{Since: in.Since, SinceCursor: in.SinceCursor, HeadCursor: in.HeadCursor, GeneratedAt: in.Now,
		SinceLabel: in.SinceLabel, Truncated: in.Truncated, Agents: []Agent{}, Warnings: in.Warnings}
	byID := map[string]*acc{}
	svs := map[string]*c.SessionView{}
	for i := range in.Sessions {
		svs[in.Sessions[i].Session.SessionID] = &in.Sessions[i]
	}
	get := func(sid string) *acc {
		if sid == "" {
			return nil
		}
		if x := byID[sid]; x != nil {
			return x
		}
		x := &acc{files: map[string]bool{}, a: Agent{SessionID: sid, Title: sid}}
		if sv := svs[sid]; sv != nil {
			x.sv = sv
			x.a.Title = sv.Session.Title
			x.a.Harness = sv.Session.Harness
			x.a.Node = sv.NodeName
			x.a.ProjectID = sv.Session.ProjectID
			x.a.Archived = sv.Session.Archived
			if sv.Run != nil {
				x.a.RunID = sv.Run.RunID
				x.a.State = sv.Run.State
			}
		}
		byID[sid] = x
		return x
	}
	touch := func(x *acc, t time.Time) {
		if t.After(x.a.LastActivity) {
			x.a.LastActivity = t
		}
	}

	// 1. Events after the cutoff.
	for _, ev := range in.Events {
		if in.SinceCursor > 0 && ev.Cursor <= in.SinceCursor {
			continue
		}
		at := ev.OccurredAt
		if at.IsZero() {
			at = ev.IngestedAt
		}
		switch ev.Kind {
		case c.EvRunStarted, c.EvRunObserved, c.EvRunEnded:
			var r c.Run
			if json.Unmarshal(ev.Payload, &r) != nil {
				continue
			}
			x := get(firstNonEmpty(r.SessionID, ev.SessionID))
			if x == nil {
				continue
			}
			x.a.Events++
			switch {
			case ev.Kind == c.EvRunStarted:
				x.startedNew, x.activity = true, true
				touch(x, at)
			case r.State.Terminal():
				rr := r
				x.ended, x.activity = &rr, true
				if r.State == c.RunFailed {
					x.failed = failReason(r)
				}
				touch(x, at)
			}
		case c.EvSessionCreated, c.EvSessionUpdated:
			if x := get(ev.SessionID); x != nil {
				x.a.Events++
			}
		case c.EvMessageStored:
			var mv c.MessageView
			if json.Unmarshal(ev.Payload, &mv) != nil {
				continue
			}
			e := mv.Envelope
			ref := Ref{Kind: string(c.KindMessage), ID: e.MessageID, Title: firstSentence(e.Body.Text, 90),
				Link: c.DeepLink(c.ObjectRef{Kind: c.KindMessage, ID: e.MessageID}), At: at}
			if e.Sender.Kind == c.PrincipalSession {
				if x := get(e.Sender.ID); x != nil {
					r := ref
					r.Direction = "sent"
					x.a.Messages = append(x.a.Messages, r)
					x.a.Events++
					x.activity = true
					touch(x, at)
				}
			}
			if e.Recipient.Kind == c.AddrSession && e.Recipient.ID != e.Sender.ID {
				if x := get(e.Recipient.ID); x != nil {
					r := ref
					r.Direction = "received"
					x.a.Messages = append(x.a.Messages, r)
					x.a.Events++
				}
			}
		case c.EvArtifactPublished:
			var pr c.PublishReceipt
			if json.Unmarshal(ev.Payload, &pr) != nil {
				continue
			}
			sid := firstNonEmpty(pr.Version.Provenance.SessionID, pr.Artifact.SessionID, ev.SessionID, sessionOf(pr.Version.Author))
			x := get(sid)
			if x == nil {
				continue
			}
			title := pr.Artifact.Title
			if pr.Version.Seq > 1 {
				title += " v" + strconv.FormatInt(pr.Version.Seq, 10)
			}
			x.a.Artifacts = append(x.a.Artifacts, Ref{Kind: string(c.KindArtifactVersion), ID: pr.Version.VersionID, Title: title,
				Link: c.DeepLink(c.ObjectRef{Kind: c.KindArtifactVersion, ID: pr.Version.VersionID}), At: at})
			x.a.Events++
			x.activity = true
			touch(x, at)
		case c.EvDocumentRevised, c.EvDocumentConflict:
			var rv struct {
				RevisionID string      `json:"revision_id"`
				ProposalID string      `json:"proposal_id"`
				DocumentID string      `json:"document_id"`
				Author     c.Principal `json:"author"`
				Summary    string      `json:"summary"`
				Provenance *c.Provenance
			}
			if json.Unmarshal(ev.Payload, &rv) != nil {
				continue
			}
			sid := sessionOf(rv.Author)
			if rv.Provenance != nil && rv.Provenance.SessionID != "" {
				sid = rv.Provenance.SessionID
			}
			x := get(firstNonEmpty(sid, ev.SessionID))
			if x == nil {
				continue
			}
			ref := Ref{Kind: string(c.KindDocumentRevision), ID: rv.RevisionID, Title: firstNonEmpty(rv.Summary, rv.DocumentID),
				Link: c.DeepLink(c.ObjectRef{Kind: c.KindDocumentRevision, ID: rv.RevisionID}), At: at}
			if ev.Kind == c.EvDocumentConflict {
				ref.Kind, ref.ID = string(c.KindProposal), rv.ProposalID
				ref.Title = "conflicting edit to " + rv.DocumentID
				ref.Link = c.DeepLink(c.ObjectRef{Kind: c.KindProposal, ID: rv.ProposalID})
			}
			x.a.Docs = append(x.a.Docs, ref)
			x.a.Events++
			x.activity = true
			touch(x, at)
		case c.EvNoteCreated:
			var n c.Note
			if json.Unmarshal(ev.Payload, &n) != nil {
				continue
			}
			x := get(firstNonEmpty(n.SessionID, n.Provenance.SessionID, ev.SessionID, sessionOf(n.Author)))
			if x == nil {
				continue
			}
			x.a.Notes = append(x.a.Notes, Ref{Kind: string(c.KindNote), ID: n.NoteID, Title: string(n.Kind) + ": " + n.Title,
				Link: c.DeepLink(c.ObjectRef{Kind: c.KindNote, ID: n.NoteID}), At: at})
			x.a.Events++
			x.activity = true
			touch(x, at)
		case c.EvCommentCreated:
			var cm c.ReviewComment
			if json.Unmarshal(ev.Payload, &cm) != nil {
				continue
			}
			if x := get(sessionOf(cm.Author)); x != nil {
				x.a.Events++
				x.activity = true
				touch(x, at)
			}
		case c.EvAccountExhausted:
			var p struct {
				SessionID string `json:"session_id"`
				Provider  string `json:"provider"`
				Outcome   string `json:"outcome"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			if x := get(firstNonEmpty(p.SessionID, ev.SessionID)); x != nil {
				x.a.Events++
				x.exhausted = "accounts exhausted"
				if p.Provider != "" {
					x.exhausted = "all " + titleWord(p.Provider) + " accounts exhausted"
				}
				x.activity = true
				touch(x, at)
			}
		default:
			if x := get(ev.SessionID); x != nil {
				x.a.Events++
			}
		}
	}

	// 2. Attention items updated after the cutoff that still need the owner.
	for _, it := range in.Attention {
		if it.State != c.AttnOpen || !it.UpdatedAt.After(in.Since) {
			continue
		}
		link := it.DeepLink
		if link == "" {
			link = c.DeepLink(c.ObjectRef{Kind: c.KindAttention, ID: it.AttentionID})
		}
		ref := Ref{Kind: string(c.KindAttention), ID: it.AttentionID, Title: it.Title, Link: link, At: it.UpdatedAt}
		x := get(it.SessionID)
		if x == nil {
			d.Other = append(d.Other, ref)
			continue
		}
		x.a.NeedsYou = append(x.a.NeedsYou, ref)
		x.activity = true
		touch(x, it.UpdatedAt)
	}

	// 3. Transcripts of sessions with activity (and of live sessions the
	// caller chose to read): turns, tools, files, the last agent message.
	for sid, tr := range in.Transcripts {
		x := byID[sid]
		if x == nil {
			if !hasEntriesAfter(tr, in.Since) {
				continue
			}
			x = get(sid)
		}
		before := false
		for i := range tr.Entries {
			e := tr.Entries[i]
			if !e.At.After(in.Since) {
				before = true
				continue
			}
			switch e.Kind {
			case "user":
				x.a.Turns++
			case "pyre_message":
				x.a.Turns++
			case "assistant":
				x.a.Replies++
				ee := e
				x.lastAsst = &ee
			case "tool":
				if e.Text == "tool result" {
					continue
				}
				n, files := toolCalls(e.Text)
				x.a.Tools += n
				for _, f := range files {
					x.files[relTo(f, cwdOf(x.sv))] = true
				}
			default:
				continue
			}
			x.activity = true
			touch(x, e.At)
		}
		if (!before && len(tr.Entries) > 0) || tr.Gap {
			x.a.Partial = true
		}
	}

	// 4. Rank, summarize, link.
	for _, x := range byID {
		finish(x, in)
		if x.a.Rank == RankQuiet {
			d.Quiet++
			continue
		}
		d.Agents = append(d.Agents, x.a)
	}
	sort.SliceStable(d.Agents, func(i, j int) bool {
		a, b := d.Agents[i], d.Agents[j]
		if a.Rank.order() != b.Rank.order() {
			return a.Rank.order() < b.Rank.order()
		}
		if a.Rank == RankNeedsYou && (len(a.NeedsYou) > 0) != (len(b.NeedsYou) > 0) {
			return len(a.NeedsYou) > 0 // actionable requests before blocked runs
		}
		if !a.LastActivity.Equal(b.LastActivity) {
			return a.LastActivity.After(b.LastActivity)
		}
		return a.SessionID < b.SessionID
	})
	sort.SliceStable(d.Other, func(i, j int) bool { return d.Other[i].At.After(d.Other[j].At) })
	return d
}

func finish(x *acc, in Input) {
	a := &x.a
	for f := range x.files {
		a.Files = append(a.Files, f)
	}
	sort.Strings(a.Files)
	sort.SliceStable(a.NeedsYou, func(i, j int) bool { return a.NeedsYou[i].At.After(a.NeedsYou[j].At) })

	// Blocked: the agent cannot continue without the owner.
	state := a.State
	switch {
	case x.exhausted != "" && (state == "" || state.Terminal() || state == c.RunWaitingInput):
		a.Blocked = x.exhausted
	case state == c.RunFailed:
		a.Blocked = firstNonEmpty(x.failed, failReasonView(x.sv), "run failed")
	case state == c.RunWaitingApproval && len(a.NeedsYou) == 0:
		a.Blocked = "waiting on a prompt in its terminal"
	}

	meaningful := x.activity || a.Blocked != ""
	switch {
	case len(a.NeedsYou) > 0 || a.Blocked != "":
		a.Rank = RankNeedsYou
	case !meaningful:
		a.Rank = RankQuiet
	case a.Archived:
		// The owner archived it after it ended: they have seen it.
		a.Rank = RankQuiet
	case state == c.RunRunning || state == c.RunStarting || state == c.RunWaitingBackground || state == c.RunExiting:
		a.Rank = RankWorking
	case state == c.RunUnknown:
		a.Rank = RankWorking // observer lost contact; the run may still be working
	default: // waiting_input, exited, or no run
		a.Rank = RankDone
	}
	if a.Rank == RankQuiet {
		return
	}

	// Summary and link, most actionable first.
	a.Link = SessionLink(a.SessionID, "", 0)
	switch {
	case len(a.NeedsYou) > 0:
		a.Summary, a.SummarySource = a.NeedsYou[0].Title, "attention"
		a.Link = a.NeedsYou[0].Link
	case a.Blocked != "":
		a.Summary, a.SummarySource = upperFirst(a.Blocked), "blocked"
	}
	if x.lastAsst != nil {
		if a.Summary == "" {
			a.Summary, a.SummarySource = firstSentence(x.lastAsst.Text, 160), "assistant"
		}
		a.Seq = x.lastAsst.Seq
		if a.SummarySource != "attention" {
			a.Link = SessionLink(a.SessionID, firstNonEmpty(x.lastAsst.RunID, a.RunID), x.lastAsst.Seq)
		}
	}
	if a.Summary == "" {
		if s := activitySummary(a); s != "" {
			a.Summary, a.SummarySource = s, "activity"
		}
	}
	if a.Summary == "" {
		a.Summary, a.SummarySource = stateSummary(x), "state"
	}
}

func activitySummary(a *Agent) string {
	switch {
	case len(a.Artifacts) > 0:
		s := "Published " + a.Artifacts[len(a.Artifacts)-1].Title
		if n := len(a.Artifacts) - 1; n > 0 {
			s += " and " + plural(n, "more artifact", "more artifacts")
		}
		return s
	case len(a.Docs) > 0:
		return "Revised " + a.Docs[len(a.Docs)-1].Title
	case len(a.Notes) > 0:
		return "Recorded " + a.Notes[len(a.Notes)-1].Title
	}
	sent := 0
	for _, m := range a.Messages {
		if m.Direction == "sent" {
			sent++
		}
	}
	if sent > 0 {
		return "Sent " + plural(sent, "message", "messages")
	}
	return ""
}

func stateSummary(x *acc) string {
	switch {
	case x.ended != nil && x.ended.State == c.RunExited:
		if x.ended.ExitCode != nil && *x.ended.ExitCode != 0 {
			return "Exited with code " + strconv.Itoa(*x.ended.ExitCode)
		}
		return "Finished"
	case x.a.State == c.RunWaitingInput:
		return "Idle at the prompt"
	case x.startedNew:
		return "Started"
	case x.a.State == c.RunRunning:
		return "Working"
	}
	return "Activity"
}

func failReason(r c.Run) string {
	if reason := strings.TrimRight(strings.TrimSpace(r.StateReason), ":;,. "); reason != "" {
		return "failed: " + reason
	}
	if r.ExitCode != nil {
		return "failed with exit code " + strconv.Itoa(*r.ExitCode)
	}
	return "run failed"
}

func failReasonView(sv *c.SessionView) string {
	if sv == nil || sv.Run == nil || sv.Run.State != c.RunFailed {
		return ""
	}
	return failReason(*sv.Run)
}

func cwdOf(sv *c.SessionView) string {
	if sv == nil || sv.Run == nil {
		return ""
	}
	if sv.Run.Checkout != nil && sv.Run.Checkout.Path != "" {
		return sv.Run.Checkout.Path
	}
	return sv.Run.Cwd
}

func relTo(p, base string) string {
	if base == "" || !path.IsAbs(p) {
		return p
	}
	base = strings.TrimSuffix(base, "/")
	if strings.HasPrefix(p, base+"/") {
		return strings.TrimPrefix(p, base+"/")
	}
	return p
}

func hasEntriesAfter(tr c.ReadSessionResponse, since time.Time) bool {
	for _, e := range tr.Entries {
		if e.At.After(since) && (e.Kind == "assistant" || e.Kind == "user" || e.Kind == "tool") {
			return true
		}
	}
	return false
}

func sessionOf(p c.Principal) string {
	if p.Kind == c.PrincipalSession {
		return p.ID
	}
	if p.OnBehalfOf != nil && p.OnBehalfOf.Kind == c.PrincipalSession {
		return p.OnBehalfOf.ID
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func upperFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
