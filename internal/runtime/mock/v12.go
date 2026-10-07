package mock

import (
	"context"
	"net/url"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Contract v1.2 operations.

// GetAttention returns one attention item in any state (owner only).
func (b *Backend) GetAttention(ctx context.Context, req c.GetRequest) (c.AttentionItem, error) {
	return query(b, ctx, func(p c.Principal) (c.AttentionItem, error) {
		if err := requireOwner(p); err != nil {
			return c.AttentionItem{}, err
		}
		b.expireApprovalsLocked()
		a, ok := b.attention[req.ID]
		if !ok {
			return c.AttentionItem{}, notFound(c.KindAttention, req.ID)
		}
		item := *a
		if item.State == c.AttnSnoozed && item.SnoozedUntil != nil && !b.now().Before(*item.SnoozedUntil) {
			item.State = c.AttnOpen // snooze elapsed
		}
		return item, nil
	})
}

// MintPreviewURL returns a preview link under the mock's hub URL. The mock
// preview handler does not validate tokens; the token is random so links
// look like the hub's.
func (b *Backend) MintPreviewURL(ctx context.Context, req c.MintPreviewRequest) (c.PreviewURL, error) {
	return query(b, ctx, func(p c.Principal) (c.PreviewURL, error) {
		if req.TTLSeconds < 0 {
			return c.PreviewURL{}, c.Errorf(c.ErrInvalid, "ttl_seconds must be >= 0")
		}
		if _, ok := b.versions[req.VersionID]; !ok {
			return c.PreviewURL{}, notFound(c.KindArtifactVersion, req.VersionID)
		}
		ttl := time.Duration(req.TTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		ttl = min(ttl, time.Hour)
		u := strings.TrimRight(b.opts.HubURL, "/") + "/preview/" + url.PathEscape(req.VersionID) + "/" + randomSecret()
		return c.PreviewURL{URL: u, ExpiresAt: b.now().Add(ttl)}, nil
	})
}

// CheckConnectionGrant decides from the mock catalog's grant scopes. Node
// principals may only ask about sessions whose current run is on them.
func (b *Backend) CheckConnectionGrant(ctx context.Context, req c.CheckGrantRequest) (c.GrantDecision, error) {
	return query(b, ctx, func(p c.Principal) (c.GrantDecision, error) {
		if req.ConnectionID == "" || req.SessionID == "" {
			return c.GrantDecision{}, c.Errorf(c.ErrInvalid, "connection_id and session_id are required")
		}
		if p.Kind == c.PrincipalSession && p.ID != req.SessionID {
			return c.GrantDecision{}, c.Errorf(c.ErrForbidden, "a session may only check its own grants")
		}
		cn, ok := b.connections[req.ConnectionID]
		if !ok {
			return c.GrantDecision{Reason: "connection is not in the catalog"}, nil
		}
		d := c.GrantDecision{Generation: cn.Generation}
		s, ok := b.sessions[req.SessionID]
		if !ok || s.Archived {
			d.Reason = "session is unknown or archived"
			return d, nil
		}
		if p.Kind == c.PrincipalNode {
			if r, ok := b.runs[s.CurrentRunID]; !ok || r.NodeID != p.ID {
				return c.GrantDecision{}, c.Errorf(c.ErrForbidden, "node %s may only check grants for sessions running on it", p.ID).WithRef(c.KindSession, req.SessionID)
			}
		}
		if !b.grantAllows(cn, req.SessionID) {
			d.Reason = "no active grant matches this session (node, project, session)"
			return d, nil
		}
		d.Allowed, d.Reason = true, "granted (mock catalog scope)"
		return d, nil
	})
}

// ReportConnection adds or replaces a catalog connection as a node report
// would (simulation control). A newly reported connection has no grants
// (scope access "none") until connection.grant.
func (b *Backend) ReportConnection(cn c.Connection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cn = clone(cn)
	if old, ok := b.connections[cn.ConnectionID]; ok {
		cn.Scope, cn.Generation = old.Scope, old.Generation
	} else {
		cn.Scope = c.ConnectionScope{Access: "none"}
		if cn.Generation == 0 {
			cn.Generation = 1
		}
	}
	b.connections[cn.ConnectionID] = &cn
}

// cmdResult is a recorded node action result (mock actions complete
// synchronously, so the result is final at once).
type cmdResult struct {
	res    c.NodeActionResult
	actor  c.Principal
	nodeID string
}

// GetCommandResult returns a recorded node action result (v1.4).
func (b *Backend) GetCommandResult(ctx context.Context, req c.GetRequest) (c.NodeActionResult, error) {
	return query(b, ctx, func(p c.Principal) (c.NodeActionResult, error) {
		r, ok := b.cmdResults[req.ID]
		if !ok {
			return c.NodeActionResult{}, notFound(c.KindCommand, req.ID)
		}
		if p.Kind != c.PrincipalOwner && p.Kind != c.PrincipalSystem && !(p.Kind == r.actor.Kind && p.ID == r.actor.ID) && !(p.Kind == c.PrincipalNode && p.ID == r.nodeID) {
			return c.NodeActionResult{}, c.Errorf(c.ErrForbidden, "command %s belongs to another principal", req.ID).WithRef(c.KindCommand, req.ID)
		}
		return r.res, nil
	})
}
