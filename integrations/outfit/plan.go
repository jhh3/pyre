package outfit

import (
	"fmt"
	"sort"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
)

// LocalFacts is what the executing node knows about a connection locally.
type LocalFacts struct {
	// OutfitHasIt is true when Outfit on this node holds the connection.
	OutfitHasIt bool
	// Provisions are Pyre-provisioned credentials on this node (names/refs).
	Provisions []credentials.Provision
}

// SessionPlan says how a session will use one granted connection. It holds
// references and node IDs only, so it may be recorded with the run.
type SessionPlan struct {
	ConnectionID string `json:"connection_id"`
	Name         string `json:"name"`
	Mode         string `json:"mode"`    // ModeLocalCredential | ModeRemoteInvoke
	NodeID       string `json:"node_id"` // executing node (local) or node that runs Outfit (remote)
	// Grants are the launch-time provisioning references (local mode only):
	// environment variable NAME plus where the launch wrapper reads the value.
	Grants []credentials.LaunchGrant `json:"grants,omitempty"`
	// Guide is one secret-free line for the session's agent tool guide.
	Guide string `json:"guide"`
}

// PlanSession chooses how a session on executingNode uses connection c:
//
//  1. local_credential when the connection declares that mode and this node
//     has a Pyre-provisioned credential (the launch wrapper reads it from the
//     node's store into the run environment; no record holds the value);
//  2. otherwise remote_invoke through Outfit, preferring Outfit on this node,
//     then the most recently verified node;
//  3. otherwise ErrUnavailable — availability must be verified before use.
//
// Grant authorization is checked separately (and again at every use).
func PlanSession(c contracts.Connection, local LocalFacts, executingNode string) (SessionPlan, error) {
	plan := SessionPlan{ConnectionID: c.ConnectionID, Name: c.Name}
	if hasMode(c.Modes, ModeLocalCredential) && len(local.Provisions) > 0 {
		plan.Mode, plan.NodeID = ModeLocalCredential, executingNode
		var names []string
		for _, p := range local.Provisions {
			ref, err := p.ParsedRef()
			if err != nil {
				return SessionPlan{}, contracts.Errorf(contracts.ErrInvalid, "connection %s has an invalid provisioning reference: %v", c.ConnectionID, err)
			}
			plan.Grants = append(plan.Grants, credentials.LaunchGrant{ConnectionID: c.ConnectionID, EnvVar: p.EnvVar, Ref: ref})
			names = append(names, "$"+p.EnvVar)
		}
		sort.Strings(names)
		plan.Guide = fmt.Sprintf("connection %s (%s): credential provided in %v for this session", c.Name, c.Service, names)
		return plan, nil
	}
	if !hasMode(c.Modes, ModeRemoteInvoke) {
		return SessionPlan{}, contracts.Errorf(contracts.ErrUnavailable, "connection %s has no local provision on %s and does not support remote invocation", c.ConnectionID, executingNode).WithRef(contracts.KindConnection, c.ConnectionID)
	}
	var best *contracts.ConnectionAvailability
	for i := range c.Availability {
		av := &c.Availability[i]
		if av.Mode != ModeRemoteInvoke || av.State != "verified" {
			continue
		}
		switch {
		case best == nil,
			av.NodeID == executingNode && local.OutfitHasIt && best.NodeID != executingNode,
			best.NodeID != executingNode && av.VerifiedAt.After(best.VerifiedAt):
			best = av
		}
	}
	if best == nil {
		return SessionPlan{}, contracts.Errorf(contracts.ErrUnavailable, "connection %s has no verified availability on any node; run connection.check on the node that holds it", c.ConnectionID).WithRef(contracts.KindConnection, c.ConnectionID)
	}
	plan.Mode, plan.NodeID = ModeRemoteInvoke, best.NodeID
	plan.Guide = fmt.Sprintf("connection %s (%s): call connection.invoke with connection_id=%s; Outfit on %s executes it and the credential stays there", c.Name, c.Service, c.ConnectionID, best.NodeID)
	return plan, nil
}

func hasMode(modes []string, m string) bool {
	for _, x := range modes {
		if x == m {
			return true
		}
	}
	return false
}
