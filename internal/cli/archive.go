package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Archiving hides a session from listings (sessions.update archived=true).
// A live session is never archived out of sight by accident: it is
// archived "when done" (W-archive2: the hub stops it gracefully after its
// current turn, then archives it) when the caller is the session itself or
// the session that started it, or asks for it (--when-done); otherwise it
// must be stopped first unless the caller insists (CLI --force).

// ArchiveParams archives or restores one session (MCP archive_session
// input; no session means the calling session). Force and WhenDone are
// CLI-only.
type ArchiveParams struct {
	Session     string `json:"session,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Force       bool   `json:"-"`
	WhenDone    bool   `json:"-"`
}

// ArchiveResult is archive_session's answer: archived now, or scheduled
// to archive when the session's current turn ends.
type ArchiveResult struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Archived  bool   `json:"archived"`
	WhenDone  bool   `json:"when_done"`
	Note      string `json:"note"`
}

// archiveResult describes an archive outcome for people and agents.
func archiveResult(sv c.SessionView, self bool) ArchiveResult {
	r := ArchiveResult{SessionID: sv.Session.SessionID, Title: sv.Session.Title, Archived: sv.Session.Archived,
		WhenDone: !sv.Session.Archived && slices.Contains(sv.Session.Tags, c.TagArchiveWhenDone)}
	switch {
	case r.Archived:
		r.Note = "archived"
	case r.WhenDone && self:
		r.Note = "will archive when this turn ends: finish your reply; Pyre then stops this session gracefully and archives it"
	case r.WhenDone:
		r.Note = "will archive when its current turn ends (Pyre stops it gracefully first)"
	default:
		r.Note = "not archived"
	}
	return r
}

// archivesWhenDone: a live session is archived when done if the caller
// asked, or the caller is the session itself or the session that started it.
func (o *Ops) archivesWhenDone(p ArchiveParams, sv c.SessionView) bool {
	if p.WhenDone {
		return true
	}
	self := o.Self.SessionID
	return self != "" && (sv.Session.SessionID == self || sv.Session.StartedBy(self))
}

// sessionEnded reports that nothing of the session runs: its run exited or
// failed, or it has none. Offline/stale runs are not ended.
func sessionEnded(s c.SessionView) bool { return s.Run == nil || s.Run.State.Terminal() }

// adapterDataNoHandle is the Run.AdapterData key pyred sets on a launch
// interrupted before a handle was recorded (internal/daemon launchrecover.go).
// A provably never-started run is failed, so sessionEnded already covers it.
const adapterDataNoHandle = "pyre.no_handle"

// runUnstoppable: the run's launch was interrupted before pyred recorded a
// handle, so Pyre cannot stop it (state unknown, nothing to signal).
// Archiving it needs no stop step (W-resilience).
func runUnstoppable(s c.SessionView) bool {
	if s.Run == nil || s.Run.State != c.RunUnknown {
		return false
	}
	return s.Run.AdapterData[adapterDataNoHandle] == "true" || strings.Contains(s.Run.StateReason, "before a run handle was recorded")
}

// endedAt is when a session's run ended (best known), for prune ages.
func endedAt(s c.SessionView) time.Time {
	if s.Run == nil {
		return s.Session.CreatedAt
	}
	if s.Run.EndedAt != nil {
		return *s.Run.EndedAt
	}
	if !s.Run.Freshness.ObservedAt.IsZero() {
		return s.Run.Freshness.ObservedAt
	}
	return s.Run.StartedAt
}

// Archive archives a session.
func (o *Ops) Archive(ctx context.Context, p ArchiveParams) (c.SessionView, error) {
	sv, err := o.ResolveSession(ctx, p.Session)
	if err != nil {
		return c.SessionView{}, err
	}
	yes := true
	if !p.Force && !sessionEnded(sv) && o.archivesWhenDone(p, sv) {
		// W-archive2: the hub archives an ended session at once and
		// schedules a live one for when its turn ends.
		return o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(p.OperationID), SessionID: sv.Session.SessionID, Archived: &yes, WhenDone: true})
	}
	// Right after `pyre stop` the node's exit report can trail the stop's
	// receipt by a moment: give a just-stopped run a few seconds to land.
	force := p.Force || runUnstoppable(sv) // nothing Pyre could stop first
	for deadline := time.Now().Add(3 * time.Second); !sessionEnded(sv) && !force && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return c.SessionView{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if again, err := o.ResolveSession(ctx, sv.Session.SessionID); err == nil {
			sv = again
		}
	}
	if !sessionEnded(sv) && !force {
		return c.SessionView{}, c.Errorf(c.ErrConflict, "session %s (%s) is still %s; archive it when its turn ends (pyre sessions archive --when-done %s) or stop it first (pyre stop %s)",
			sv.Session.SessionID, orDash(sv.Session.Title), sv.Run.State, sv.Session.SessionID, sv.Session.SessionID)
	}
	return o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(p.OperationID), SessionID: sv.Session.SessionID, Archived: &yes})
}

// Unarchive restores an archived session to listings.
func (o *Ops) Unarchive(ctx context.Context, p ArchiveParams) (c.SessionView, error) {
	id, err := o.archivedSessionID(ctx, p.Session)
	if err != nil {
		return c.SessionView{}, err
	}
	no := false
	return o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(p.OperationID), SessionID: id, Archived: &no})
}

// archivedSessionID resolves a session ID or the exact title of an
// archived session (ResolveSession does not see archived ones).
func (o *Ops) archivedSessionID(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", usagef("a session is required (see `pyre sessions ls --all`)")
	}
	if c.ValidID(ref, c.PrefixSession) {
		return ref, nil
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		return o.API.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur}, Query: ref, IncludeArchived: true})
	})
	if err != nil {
		return "", err
	}
	var hits []string
	for _, pass := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		for _, s := range page.Items {
			if s.Session.Archived && pass(s.Session.Title, ref) {
				hits = append(hits, s.Session.SessionID)
			}
		}
		if len(hits) > 0 {
			break
		}
	}
	switch len(hits) {
	case 0:
		return "", c.Errorf(c.ErrNotFound, "no archived session titled %q (pyre sessions ls --all)", ref)
	case 1:
		return hits[0], nil
	}
	return "", c.Errorf(c.ErrConflict, "%d archived sessions are titled %q; use an ID: %s", len(hits), ref, strings.Join(hits, ", "))
}

// PruneParams selects ended sessions to archive.
type PruneParams struct {
	OlderThan time.Duration // ended at least this long ago (0: any)
	Node      string
	DryRun    bool
}

// PruneResult lists what prune archived (or would archive).
type PruneResult struct {
	DryRun   bool            `json:"dry_run"`
	Archived []c.SessionView `json:"archived"`
	Failed   []PruneFailure  `json:"failed,omitempty"`
}

// PruneFailure is one session prune could not archive.
type PruneFailure struct {
	SessionID string `json:"session_id"`
	Error     string `json:"error"`
}

// Prune archives ended sessions (exited, failed or without a run),
// optionally only those ended before now-OlderThan or on one node.
func (o *Ops) Prune(ctx context.Context, p PruneParams, now time.Time) (PruneResult, error) {
	page, err := o.ListSessions(ctx, SessionFilter{Node: p.Node})
	if err != nil {
		return PruneResult{}, err
	}
	res := PruneResult{DryRun: p.DryRun, Archived: []c.SessionView{}}
	for _, s := range page.Items {
		if s.Session.Archived || !sessionEnded(s) {
			continue
		}
		if p.OlderThan > 0 && now.Sub(endedAt(s)) < p.OlderThan {
			continue
		}
		if p.DryRun {
			res.Archived = append(res.Archived, s)
			continue
		}
		yes := true
		sv, err := o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(""), SessionID: s.Session.SessionID, Archived: &yes})
		if err != nil {
			res.Failed = append(res.Failed, PruneFailure{SessionID: s.Session.SessionID, Error: err.Error()})
			continue
		}
		res.Archived = append(res.Archived, sv)
	}
	if len(res.Failed) > 0 {
		return res, c.Errorf(c.ErrUnavailable, "prune archived %d session(s); %d failed (first: %s)", len(res.Archived), len(res.Failed), res.Failed[0].Error)
	}
	return res, nil
}

// ArchiveProjectParams archives a project (MCP archive_project input).
type ArchiveProjectParams struct {
	Project      string `json:"project"`
	WithSessions bool   `json:"with_sessions,omitempty"`
	OperationID  string `json:"operation_id,omitempty"`
}

// ArchivedSessionRef names one session archive_project acted on.
type ArchivedSessionRef struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

// ArchiveProjectResult is archive_project's answer.
type ArchiveProjectResult struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Archived  bool   `json:"archived"`
	// ArchivedSessions were archived now (ended); WhenDone archive when
	// their current turn ends; EndedLeft are ended sessions left listed
	// (without with_sessions).
	ArchivedSessions []ArchivedSessionRef `json:"archived_sessions"`
	WhenDone         []ArchivedSessionRef `json:"when_done_sessions"`
	EndedLeft        []ArchivedSessionRef `json:"ended_sessions_left"`
	Note             string               `json:"note"`
}

func sessionRef(s c.SessionView) ArchivedSessionRef {
	return ArchivedSessionRef{SessionID: s.Session.SessionID, Title: s.Session.Title}
}

// ArchiveProject archives a project. Live sessions in it refuse the
// archive unless WithSessions, which archives its ended sessions now and
// its live ones when their turns end (W-archive2).
func (o *Ops) ArchiveProject(ctx context.Context, p ArchiveProjectParams) (ArchiveProjectResult, error) {
	pr, err := o.ResolveProject(ctx, p.Project)
	if err != nil {
		return ArchiveProjectResult{}, err
	}
	res := ArchiveProjectResult{ProjectID: pr.ProjectID, Name: pr.Name,
		ArchivedSessions: []ArchivedSessionRef{}, WhenDone: []ArchivedSessionRef{}, EndedLeft: []ArchivedSessionRef{}}
	pid := pr.ProjectID
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		return o.API.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur}, ProjectID: &pid})
	})
	if err != nil {
		return res, err
	}
	var live, ended []c.SessionView
	for _, s := range page.Items {
		switch {
		case s.Session.Archived:
		case sessionEnded(s) || runUnstoppable(s):
			ended = append(ended, s)
		default:
			live = append(live, s)
		}
	}
	if len(live) > 0 && !p.WithSessions {
		var names []string
		for i, s := range live {
			if i == 5 {
				names = append(names, fmt.Sprintf("… %d more", len(live)-i))
				break
			}
			names = append(names, s.Session.SessionID+" "+strconvQuote(s.Session.Title))
		}
		return res, c.Errorf(c.ErrConflict, "project %s has %d live session(s) (%s); with_sessions: true (pyre project archive --with-sessions) archives them when their turns end",
			pr.Name, len(live), strings.Join(names, ", "))
	}
	if p.WithSessions {
		var failed []string
		yes := true
		for _, s := range append(ended, live...) {
			sv, err := o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(""), SessionID: s.Session.SessionID, Archived: &yes, WhenDone: true})
			switch {
			case err != nil:
				failed = append(failed, s.Session.SessionID+": "+err.Error())
			case sv.Session.Archived:
				res.ArchivedSessions = append(res.ArchivedSessions, sessionRef(s))
			default:
				res.WhenDone = append(res.WhenDone, sessionRef(s))
			}
		}
		if len(failed) > 0 {
			return res, c.Errorf(c.ErrConflict, "project %s not archived: %d session(s) could not be archived (first: %s)", pr.Name, len(failed), failed[0])
		}
	} else {
		for _, s := range ended {
			res.EndedLeft = append(res.EndedLeft, sessionRef(s))
		}
	}
	if !pr.Archived {
		yes := true
		out, err := o.API.UpdateProject(ctx, c.UpdateProjectRequest{Mutation: mut(p.OperationID), ProjectID: pr.ProjectID, Version: pr.Version, Archived: &yes})
		if err != nil {
			return res, err
		}
		pr = out
	}
	res.Archived = pr.Archived
	var note []string
	note = append(note, "project archived")
	if n := len(res.ArchivedSessions); n > 0 {
		note = append(note, fmt.Sprintf("%d ended session(s) archived", n))
	}
	if n := len(res.WhenDone); n > 0 {
		note = append(note, fmt.Sprintf("%d live session(s) archive when their turns end", n))
	}
	if n := len(res.EndedLeft); n > 0 {
		note = append(note, fmt.Sprintf("%d ended session(s) are still listed: archive them too with archive_project with_sessions: true (pyre project archive %s --with-sessions)", n, pr.ProjectID))
	}
	res.Note = strings.Join(note, "; ")
	return res, nil
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }
