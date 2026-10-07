package cli

import (
	"context"
	"path"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// ResolveArtifact accepts an artifact ID or a title (exact, then
// case-insensitive). Several artifacts with that title are a conflict
// listing their IDs.
func (o *Ops) ResolveArtifact(ctx context.Context, ref string) (c.Artifact, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return c.Artifact{}, usagef("an artifact is required (see `pyre artifact ls`)")
	}
	if c.ValidID(ref, c.PrefixArtifact) {
		return o.API.GetArtifact(ctx, c.GetRequest{ID: ref})
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Artifact], error) {
		return o.API.ListArtifacts(ctx, c.ListArtifactsRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return c.Artifact{}, err
	}
	var hits []c.Artifact
	for _, pass := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		for _, a := range page.Items {
			if pass(a.Title, ref) {
				hits = append(hits, a)
			}
		}
		if len(hits) > 0 {
			break
		}
	}
	switch len(hits) {
	case 0:
		return c.Artifact{}, notFoundf(c.KindArtifact, ref)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, a := range hits {
		ids[i] = a.ArtifactID
	}
	return c.Artifact{}, ambiguous(c.KindArtifact, ref, ids)
}

// artifactID returns an artifact ID for an ID or title ("" stays "").
func (o *Ops) artifactID(ctx context.Context, ref string) (string, error) {
	if ref == "" || c.ValidID(ref, c.PrefixArtifact) {
		return ref, nil
	}
	a, err := o.ResolveArtifact(ctx, ref)
	return a.ArtifactID, err
}

// ResolveVersionID accepts a version ID (returned as is, so offline cache
// reads keep working), or an artifact ID or title (its latest version).
func (o *Ops) ResolveVersionID(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if c.ValidID(ref, c.PrefixVersion) {
		return ref, nil
	}
	a, err := o.ResolveArtifact(ctx, ref)
	if err != nil {
		return "", err
	}
	if a.LatestVersionID == "" {
		return "", c.Errorf(c.ErrNotFound, "artifact %s has no published version", a.ArtifactID).WithRef(c.KindArtifact, a.ArtifactID)
	}
	return a.LatestVersionID, nil
}

// ResolveConnection accepts a connection ID or name.
func (o *Ops) ResolveConnection(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", usagef("a connection is required (see `pyre connections`)")
	}
	if c.ValidID(ref, c.PrefixConnection) {
		return ref, nil
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Connection], error) {
		return o.API.ListConnections(ctx, c.ListConnectionsRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return "", err
	}
	var ids []string
	for _, cn := range page.Items {
		if cn.Name == ref || strings.EqualFold(cn.Name, ref) {
			ids = append(ids, cn.ConnectionID)
		}
	}
	switch len(ids) {
	case 0:
		return "", notFoundf(c.KindConnection, ref)
	case 1:
		return ids[0], nil
	}
	return "", ambiguous(c.KindConnection, ref, ids)
}

// parseCheckout parses node:path:repo:mode[:branch] (the node may be a name).
func (o *Ops) parseCheckout(ctx context.Context, s string) (c.CheckoutBinding, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 && len(parts) != 5 {
		return c.CheckoutBinding{}, usagef("--checkout %q: want node:path:repo:mode[:branch]", s)
	}
	b := c.CheckoutBinding{Path: parts[1], RepoID: parts[2], Mode: c.CheckoutMode(parts[3])}
	if len(parts) == 5 {
		b.Branch = parts[4]
	}
	switch b.Mode {
	case c.CheckoutShared, c.CheckoutWorktree, c.CheckoutPlain:
	default:
		return b, usagef("--checkout %q: mode must be shared, worktree or plain", s)
	}
	if !path.IsAbs(b.Path) {
		return b, usagef("--checkout %q: the path must be absolute on that node", s)
	}
	if parts[0] == "" || b.RepoID == "" {
		return b, usagef("--checkout %q: node and repo are required", s)
	}
	b.NodeID = parts[0]
	if !c.ValidID(b.NodeID, c.PrefixNode) {
		n, err := o.ResolveNode(ctx, b.NodeID)
		if err != nil {
			return b, err
		}
		b.NodeID = n.NodeID
	}
	return b, nil
}
