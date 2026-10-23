package contracts

import (
	"encoding/hex"
	"slices"
	"strings"
)

// Node-started sessions (pyre run without a hub login, W-pyrerun-node).
// A node's credential is readable by every process of the user its pyred
// runs as, so sessions.start by a node principal is refused unless it is a
// headless pyre run record on the node itself: claude or codex, exactly the exec
// claim argument (ArgPyreExec, optionally ArgPyreExecResume), no parent,
// report, prompt, checkout, env profile or launch context; tags forced to
// NodeStartTags. The hub and the mock backend share this check; the hub
// also mints no session token for it, and a node may not resume sessions.

// NodeStartTags are the tags of every node-started session.
var NodeStartTags = []string{TagPyreRun, TagReportNone, TagArchiveWhenDone}

// NodeStarter reports whether p starts sessions under the node rules: a
// node credential not acting for the owner.
func NodeStarter(p Principal) bool {
	return p.Kind == PrincipalNode && (p.OnBehalfOf == nil || p.OnBehalfOf.Kind != PrincipalOwner)
}

// CheckNodeStart refuses a node principal's start request that is anything
// but a headless claude or codex run on the node itself, and normalizes the rest.
func CheckNodeStart(p Principal, req *StartSessionRequest) error {
	deny := func(format string, a ...any) error {
		return Errorf(ErrForbidden, "a node credential starts only headless pyre run sessions on its own node: "+format, a...).
			WithRef(KindNode, p.ID)
	}
	switch {
	case req.NodeSelector != nil || req.NodeID != p.ID:
		return deny("node_id must be %s", p.ID)
	case req.Harness != HarnessClaude && req.Harness != HarnessCodex:
		return deny("harness must be claude or codex (got %s)", req.Harness)
	case req.ParentSessionID != "":
		return deny("no parent session")
	case req.ReportToParent != nil && *req.ReportToParent:
		return deny("no report to a parent")
	case req.InitialPrompt != "":
		return deny("no initial prompt")
	case req.Checkout != nil:
		return deny("no checkout")
	case req.EnvProfile != "":
		return deny("no environment profile")
	case req.IncludeContext:
		return deny("no launch context")
	}
	hash, resume := 0, 0
	for _, a := range req.Args {
		name, val, _ := strings.Cut(a, "=")
		switch name {
		case ArgPyreExec:
			if b, err := hex.DecodeString(val); err != nil || len(b) != 32 {
				return deny("%s needs the claim's hex SHA-256", ArgPyreExec)
			}
			hash++
		case ArgPyreExecResume:
			if strings.TrimSpace(val) == "" || strings.HasPrefix(val, "-") {
				return deny("%s needs a conversation id", ArgPyreExecResume)
			}
			resume++
		default:
			return deny("no harness arguments other than %s and %s (got %q)", ArgPyreExec, ArgPyreExecResume, a)
		}
	}
	if hash != 1 || resume > 1 {
		return deny("exactly one %s=<hash> (and at most one %s=<id>)", ArgPyreExec, ArgPyreExecResume)
	}
	no := false
	req.ReportToParent = &no
	req.Tags = slices.Clone(NodeStartTags)
	return nil
}
