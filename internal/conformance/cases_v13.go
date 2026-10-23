package conformance

import (
	"slices"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// v1.3: accounts.lease.* — one node holds an account's refresh family at a
// time (unless the account is per-machine independent).
func testAccountLease(t *testing.T, s *suite) {
	nodeA := s.tg.As(s.ctx, c.Principal{Kind: c.PrincipalNode, ID: s.tg.NodeID})
	otherID := "node_conformance-other"
	nodeB := s.tg.As(s.ctx, c.Principal{Kind: c.PrincipalNode, ID: otherID})
	acct, err := s.api.UpsertAccount(s.owner(), c.UpsertAccountRequest{Mutation: newOp(), Provider: c.ProviderAnthropic, Label: "conformance-lease"})
	s.must(err, "create account")

	l1, err := s.api.AcquireAccountLease(nodeA, c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: acct.AccountID, Holder: "rota-checkout:grant_a"})
	s.must(err, "acquire")
	if l1.LeaseID == "" || l1.NodeID != s.tg.NodeID || l1.AccountID != acct.AccountID || !l1.ExpiresAt.After(l1.AcquiredAt) || l1.ReleasedAt != nil {
		t.Fatalf("lease %+v", l1)
	}
	again, err := s.api.AcquireAccountLease(nodeA, c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: acct.AccountID, Holder: "rota-checkout:grant_a"})
	s.must(err, "re-acquire by the same holder")
	if again.LeaseID != l1.LeaseID {
		t.Fatalf("same node+holder got a new lease %s (had %s)", again.LeaseID, l1.LeaseID)
	}
	_, err = s.api.AcquireAccountLease(nodeB, c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: acct.AccountID, Holder: "rota-checkout:grant_b"})
	e := s.wantCode(err, c.ErrConflict, "acquire while another node holds")
	if !strings.Contains(e.Message, s.tg.NodeID) && e.Details["holder_node"] != s.tg.NodeID {
		t.Fatalf("conflict does not name the holder node: %+v", e)
	}
	_, err = s.api.AcquireAccountLease(nodeA, c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: acct.AccountID, Holder: "rota-checkout:grant_other"})
	s.wantCode(err, c.ErrConflict, "second holder on the same node")
	_, err = s.api.AcquireAccountLease(s.owner(), c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: acct.AccountID, Holder: "x"})
	s.wantCode(err, c.ErrForbidden, "owner acquires")
	_, err = s.api.AcquireAccountLease(nodeA, c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: c.NewID(c.PrefixAccount), Holder: "x"})
	s.wantCode(err, c.ErrNotFound, "unknown account")

	_, err = s.api.RenewAccountLease(nodeB, c.RenewAccountLeaseRequest{Mutation: newOp(), LeaseID: l1.LeaseID})
	s.wantCode(err, c.ErrForbidden, "renew another node's lease")
	r, err := s.api.RenewAccountLease(nodeA, c.RenewAccountLeaseRequest{Mutation: newOp(), LeaseID: l1.LeaseID, TTLSeconds: 600})
	s.must(err, "renew")
	if r.LeaseID != l1.LeaseID || r.ExpiresAt.Before(r.AcquiredAt) {
		t.Fatalf("renewed %+v", r)
	}
	_, err = s.api.ReleaseAccountLease(nodeB, c.ReleaseAccountLeaseRequest{Mutation: newOp(), LeaseID: l1.LeaseID})
	s.wantCode(err, c.ErrForbidden, "release another node's lease")
	rel, err := s.api.ReleaseAccountLease(nodeA, c.ReleaseAccountLeaseRequest{Mutation: newOp(), LeaseID: l1.LeaseID})
	s.must(err, "release")
	if rel.ReleasedAt == nil {
		t.Fatalf("released lease %+v", rel)
	}
	if rel2, err := s.api.ReleaseAccountLease(nodeA, c.ReleaseAccountLeaseRequest{Mutation: newOp(), LeaseID: l1.LeaseID}); err != nil || rel2.ReleasedAt == nil {
		t.Fatalf("second release: %+v %v", rel2, err)
	}
	_, err = s.api.RenewAccountLease(nodeA, c.RenewAccountLeaseRequest{Mutation: newOp(), LeaseID: l1.LeaseID})
	s.wantCode(err, c.ErrConflict, "renew a released lease")
	lb, err := s.api.AcquireAccountLease(nodeB, c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: acct.AccountID, Holder: "rota-checkout:grant_b"})
	s.must(err, "acquire after release")
	if lb.NodeID != otherID {
		t.Fatalf("lease after release %+v", lb)
	}

	// Per-machine independent accounts: nodes do not exclude each other.
	ind, err := s.api.UpsertAccount(s.owner(), c.UpsertAccountRequest{Mutation: newOp(), Provider: c.ProviderOpenAI, Label: "conformance-independent", PerMachineIndependent: true})
	s.must(err, "create independent account")
	if !ind.PerMachineIndependent {
		t.Fatalf("account %+v", ind)
	}
	if _, err := s.api.AcquireAccountLease(nodeA, c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: ind.AccountID, Holder: "rota-checkout:grant_ia"}); err != nil {
		t.Fatalf("independent A: %v", err)
	}
	if _, err := s.api.AcquireAccountLease(nodeB, c.AcquireAccountLeaseRequest{Mutation: newOp(), AccountID: ind.AccountID, Holder: "rota-checkout:grant_ib"}); err != nil {
		t.Fatalf("independent B: %v", err)
	}
}

// Node-started sessions: a node credential starts only a headless pyre run
// on itself (claude or codex, the exec claim argument, nothing else; tags forced)
// and resumes nothing (contracts.CheckNodeStart).
func testNodeStart(t *testing.T, s *suite) {
	node := s.tg.As(s.ctx, c.Principal{Kind: c.PrincipalNode, ID: s.tg.NodeID})
	exec := c.ArgPyreExec + "=" + strings.Repeat("ab", 32)
	cwd := s.tg.Cwd
	ok := func() c.StartSessionRequest {
		return c.StartSessionRequest{Mutation: newOp(), NodeID: s.tg.NodeID, Harness: c.HarnessClaude, Title: "node start (conformance)",
			Cwd: cwd, Args: []string{exec}, Tags: []string{c.TagReportParent}}
	}
	for name, mod := range map[string]func(r *c.StartSessionRequest){
		"other node":  func(r *c.StartSessionRequest) { r.NodeID = "node_conformance-other" },
		"harness":     func(r *c.StartSessionRequest) { r.Harness = c.HarnessShell },
		"no exec arg": func(r *c.StartSessionRequest) { r.Args = nil },
		"extra arg":   func(r *c.StartSessionRequest) { r.Args = append(r.Args, "--dangerously-skip-permissions") },
		"prompt":      func(r *c.StartSessionRequest) { r.InitialPrompt = "synthetic" },
	} {
		r := ok()
		mod(&r)
		_, err := s.api.StartSession(node, r)
		s.wantCode(err, c.ErrForbidden, "node start: "+name)
	}
	resp, err := s.api.StartSession(node, ok())
	s.must(err, "node start, allowed shape")
	got := resp.Session.Session
	if !slices.Equal(got.Tags, c.NodeStartTags) || got.Parent() != "" || got.CreatedBy.Kind != c.PrincipalNode {
		t.Fatalf("node-started session %+v", got)
	}
	_, err = s.api.ResumeSession(node, c.ResumeRequest{Mutation: newOp(), SessionID: got.SessionID})
	s.wantCode(err, c.ErrForbidden, "node resume")
}
