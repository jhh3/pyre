package hub

import (
	"context"
	"slices"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
)

// Node-started sessions (pyre run without a hub login, W-pyrerun-node).
//
// A node's credential lives on that machine, readable by every process of
// the user pyred runs as, so the hub trusts a node principal no further
// than that user: it may start exactly one kind of session, a headless
// `pyre run` record on itself. The node's exec adapter starts no process
// (the caller runs claude itself), so such a start can bind one of the
// node's own accounts and record the run, nothing more. It never names a
// parent, never reports to one, never carries a prompt, checkout, launch
// context or environment profile, and its tags are forced: pyre run (owner
// attention ignores it), report:none, archive:when-done.

// nodeStartTags are the tags of every node-started session.
var nodeStartTags = contracts.NodeStartTags

// nodeStarter reports whether p starts sessions under the node rules.
func nodeStarter(p contracts.Principal) bool { return contracts.NodeStarter(p) }

// checkNodeStart: contracts.CheckNodeStart (shared with the mock backend).
func checkNodeStart(p contracts.Principal, req *contracts.StartSessionRequest) error {
	return contracts.CheckNodeStart(p, req)
}

// forceNodeStartTags gives a node-started session exactly nodeStartTags
// (and title:auto when the hub added it to an untitled one).
func forceNodeStartTags(tags []string) []string {
	out := slices.Clone(nodeStartTags)
	if slices.Contains(tags, autotitle.TagAuto) {
		out = append(out, autotitle.TagAuto)
	}
	return out
}

// checkNodeRetag refuses a node principal's tag change on a node-started
// pyre run session: the forced tags (owner attention ignores pyre run;
// report:none) stay as the hub set them. A node also cannot resume
// sessions (ResumeSession) or reparent them (checkReparent).
func (h *Hub) checkNodeRetag(ctx context.Context, p contracts.Principal, req contracts.UpdateSessionRequest) error {
	if !nodeStarter(p) || req.Tags == nil {
		return nil
	}
	s, err := h.getSession(ctx, h.db, req.SessionID)
	if err != nil {
		return err
	}
	if s.CreatedBy.Kind == contracts.PrincipalNode && slices.Contains(s.Tags, contracts.TagPyreRun) {
		return contracts.Errorf(contracts.ErrForbidden, "a node credential cannot change the tags of a pyre run session it started").
			WithRef(contracts.KindSession, s.SessionID)
	}
	return nil
}
