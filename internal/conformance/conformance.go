// Package conformance is a reusable test suite for contracts.API
// implementations (the deterministic mock, the SQLite hub, the HTTP client
// against either, the daemon proxy). It checks contract semantics, not
// implementation details:
//
//	conformance.Run(t, conformance.Harness{Name: "hub", New: newHubTarget})
//
// Each subtest gets a fresh Target from Harness.New. Behavior that needs a
// runtime or node (delivery outcomes, approvals, node disconnects) is driven
// through optional simulator interfaces implemented by Target.Sim; when the
// target does not implement one, the dependent subtest is skipped with the
// missing capability named. Skips are not passes: report them.
package conformance

import (
	"context"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Harness creates fresh, isolated targets.
type Harness struct {
	Name string
	New  func(t *testing.T) *Target
}

// Target is one isolated implementation under test.
type Target struct {
	API c.API
	// As returns a context authenticated as p. The suite uses owner
	// principals (several client IDs) and session principals for sessions
	// it starts itself.
	As func(ctx context.Context, p c.Principal) context.Context
	// NodeID is a connected node on which Harness can be launched.
	NodeID string
	// Harness to launch (default mock).
	Harness c.Harness
	// Cwd for launches (default /tmp/pyre-conformance).
	Cwd string
	// Sim optionally implements the simulator interfaces below.
	Sim any
}

// Optional capabilities. A target implements those it can simulate.
type (
	// DeliverySimulator advances queued messages until quiescent, as a
	// connected node with an idle runtime would.
	DeliverySimulator interface {
		SettleDelivery(ctx context.Context) error
	}
	// DeliveryReporter records a node receipt for a message, binding it to
	// the recipient session's current run if not yet bound.
	DeliveryReporter interface {
		ReportDelivery(ctx context.Context, messageID string, state c.RuntimeDeliveryState) error
	}
	// RunStateSimulator records a node observation of a run.
	RunStateSimulator interface {
		SetRunState(ctx context.Context, runID string, state c.RunState) error
	}
	// NodeSimulator drops or restores a node's control connection.
	NodeSimulator interface {
		SetNodeConnected(ctx context.Context, nodeID string, connected bool) error
	}
	// ApprovalSimulator raises runtime approvals and native resolutions.
	ApprovalSimulator interface {
		RequestApproval(ctx context.Context, runID string, ttl time.Duration) (c.Approval, error)
		ResolveApprovalNatively(ctx context.Context, approvalID, optionID string) error
	}
	// EventTrimmer applies retention so only the newest keep events remain.
	EventTrimmer interface {
		TrimEvents(ctx context.Context, keep int) error
	}
	// ClockAdvancer moves the target's clock forward.
	ClockAdvancer interface {
		AdvanceClock(d time.Duration)
	}
)

func need[T any](t *testing.T, tg *Target, what string) T {
	t.Helper()
	s, ok := tg.Sim.(T)
	if !ok {
		t.Skipf("target lacks capability %s", what)
	}
	return s
}

// Run executes the whole suite.
func Run(t *testing.T, h Harness) {
	cases := []struct {
		name string
		fn   func(t *testing.T, s *suite)
	}{
		{"Unauthenticated", testUnauthenticated},
		{"StandaloneSession", testStandalone},
		{"IdempotencyReplayAndConflict", testIdempotency},
		{"MessageDedupByID", testMessageDedup},
		{"DeliveryFactsSeparate", testDeliveryFacts},
		{"AckAndReply", testAckReply},
		{"CancelAndReissueRules", testCancelReissue},
		{"IndeterminateResolveAndNoRebind", testIndeterminate},
		{"LeaseConflictTakeover", testLease},
		{"ApprovalSingleResolution", testApprovals},
		{"ApprovalExpiredAndWithdrawn", testApprovalExpiry},
		{"DocumentCASPreservesProposal", testDocumentCAS},
		{"ArtifactVerifyImmutableVersions", testArtifacts},
		{"CommentStaysOnVersion", testCommentVersion},
		{"EventsCursorAndReset", testEvents},
		{"AttentionWithdraw", testAttention},
		{"PaginationCursors", testPagination},
		{"AttentionGet", testAttentionGet},
		{"PreviewURLMint", testPreviewURL},
		{"ConnectionGrantCheck", testConnectionGrantCheck},
		{"AccountLease", testAccountLease},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(h.Name+"/"+tc.name, func(t *testing.T) {
			tg := h.New(t)
			if tg.Harness == "" {
				tg.Harness = c.HarnessMock
			}
			if tg.Cwd == "" {
				tg.Cwd = "/tmp/pyre-conformance"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			tc.fn(t, &suite{t: t, tg: tg, ctx: ctx, api: tg.API})
		})
	}
}
