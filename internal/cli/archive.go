package cli

import (
	"context"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Archiving hides a session from listings (sessions.update archived=true);
// it does not stop anything. A live session must be stopped first unless
// the caller insists (CLI --force), so a running session never disappears
// from view by accident.

// ArchiveParams archives or restores one session (MCP archive_session
// input). Force is CLI-only.
type ArchiveParams struct {
	Session     string `json:"session"`
	OperationID string `json:"operation_id,omitempty"`
	Force       bool   `json:"-"`
}

// sessionEnded reports that nothing of the session runs: its run exited or
// failed, or it has none. Offline/stale runs are not ended.
func sessionEnded(s c.SessionView) bool { return s.Run == nil || s.Run.State.Terminal() }

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
	// Right after `pyre stop` the node's exit report can trail the stop's
	// receipt by a moment: give a just-stopped run a few seconds to land.
	for deadline := time.Now().Add(3 * time.Second); !sessionEnded(sv) && !p.Force && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return c.SessionView{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if again, err := o.ResolveSession(ctx, sv.Session.SessionID); err == nil {
			sv = again
		}
	}
	if !sessionEnded(sv) && !p.Force {
		return c.SessionView{}, c.Errorf(c.ErrConflict, "session %s (%s) is still %s; stop it first (pyre stop %s), then archive",
			sv.Session.SessionID, orDash(sv.Session.Title), sv.Run.State, sv.Session.SessionID)
	}
	yes := true
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
