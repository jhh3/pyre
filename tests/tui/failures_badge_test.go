package tui_test

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// Header counts (owner request 2026-10-12): 84 stale "Session run failed"
// items flagged their ended sessions "needs you" (Sessions 44 with nothing
// live to answer). The Sessions count is sessions that need you now; a
// failure on an ended session still shows in its row.

const failedSess = "ses_synth-fbx" // "api work (limited)": exited in the fixture

// failedLaunchAPI is the mock with one ended session flagged by a launch
// failure, as the hub flags it (sessionflag: failure → needs you/failed).
type failedLaunchAPI struct {
	*mock.Backend
	reason string // "failed" or "reauth"
}

func (f failedLaunchAPI) item() contracts.AttentionItem {
	kind := contracts.AttnFailure
	if f.reason == "reauth" {
		kind = contracts.AttnAccountPool
	}
	at := mock.FixtureEpoch
	return contracts.AttentionItem{AttentionID: "att_synth-launchfail", Kind: kind, Severity: "normal", State: contracts.AttnOpen,
		Object: contracts.ObjectRef{Kind: contracts.KindSession, ID: failedSess}, SessionID: failedSess,
		Title: "Session run failed", Summary: "Synthetic: no account with capacity right now", CreatedAt: at, UpdatedAt: at, Count: 1}
}

func (f failedLaunchAPI) flag(s *contracts.SessionView) {
	if s.Session.SessionID == failedSess {
		s.Flag = &contracts.SessionFlag{Level: contracts.FlagNeedsYou, Reason: f.reason, Text: "Session run failed",
			AttentionID: "att_synth-launchfail", Kind: string(f.item().Kind), Count: 1, Since: mock.FixtureEpoch}
		s.Attention = 1
	}
}

func (f failedLaunchAPI) ListSessions(ctx context.Context, req contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	p, err := f.Backend.ListSessions(ctx, req)
	for i := range p.Items {
		f.flag(&p.Items[i])
	}
	return p, err
}

func (f failedLaunchAPI) GetSession(ctx context.Context, req contracts.GetRequest) (contracts.SessionView, error) {
	s, err := f.Backend.GetSession(ctx, req)
	f.flag(&s)
	return s, err
}

func (f failedLaunchAPI) ListAttention(ctx context.Context, req contracts.ListAttentionRequest) (contracts.Page[contracts.AttentionItem], error) {
	p, err := f.Backend.ListAttention(ctx, req)
	it := f.item()
	if err == nil && (len(req.States) == 0 || slices.Contains(req.States, it.State)) &&
		(len(req.Kinds) == 0 || slices.Contains(req.Kinds, it.Kind)) && (req.SessionID == "" || req.SessionID == failedSess) && req.PageRequest.Cursor == "" {
		p.Items = append(p.Items, it)
	}
	return p, err
}

var tabCount = regexp.MustCompile(`(S) (Sessions)(?: (\d+))?`)

// headerCount reads a header tab's count ("S Sessions 3" → 3; none → 0).
func headerCount(t *testing.T, h *tuitest.Harness, tab string) int {
	t.Helper()
	head := strings.Split(h.View(), "\n")[0]
	for _, m := range tabCount.FindAllStringSubmatch(head, -1) {
		if m[2] == tab {
			n, _ := strconv.Atoi(m[3])
			return n
		}
	}
	t.Fatalf("no %s tab in the header: %s", tab, head)
	return 0
}

// stripLine is the "Needs you" strip under the Sessions panes.
func stripLine(h *tuitest.Harness) string {
	for _, l := range strings.Split(h.View(), "\n") {
		if strings.Contains(l, " Needs you ") || strings.Contains(l, "Nothing needs you") {
			return l
		}
	}
	return ""
}

func TestEndedLaunchFailureIsNotNeedsYou(t *testing.T) {
	for _, reason := range []string{"failed", "reauth"} {
		t.Run(reason, func(t *testing.T) {
			h, _ := start(t, 160, 34, func(api contracts.API) contracts.API {
				return failedLaunchAPI{Backend: api.(*mock.Backend), reason: reason}
			}, nil)
			// The fixture's three live sessions that need you (deploy prep
			// waiting on approval, worker, scratch) count; the ended one
			// flagged by its failure does not.
			if n := headerCount(t, h, "Sessions"); n != 3 {
				t.Fatalf("Sessions badge %d, want 3 (live/actionable only):\n%s", n, h.View())
			}
			h.Key(".") // show ended sessions
			h.WaitFor("ended shown", 5*sec, func() bool { return rowOf(h.View(), "api work (limited)") != "" })
			row := rowOf(h.View(), "api work (limited)")
			if strings.Contains(row, "needs you") || !(strings.Contains(row, "✗ failed") || strings.Contains(row, "⊘ re-auth")) {
				t.Fatalf("the failure shows in its row, not as needs you: %q", row)
			}
			strip := stripLine(h)
			if !strings.Contains(strip, "deploy prep") || strings.Contains(strip, "api work") {
				t.Fatalf("strip lists the live approval, not the ended failure: %q", strip)
			}
			if n := headerCount(t, h, "Sessions"); n != 3 {
				t.Fatalf("showing ended sessions changes nothing: %d", n)
			}
		})
	}
}

func TestLiveApprovalCountsAsNeedsYou(t *testing.T) {
	h, _ := start(t, 160, 34, nil, nil)
	if !strings.Contains(rowOf(h.View(), "deploy prep"), "approval") {
		t.Fatalf("deploy prep waits on approval: %q", rowOf(h.View(), "deploy prep"))
	}
	if n := headerCount(t, h, "Sessions"); n != 3 {
		t.Fatalf("Sessions badge %d, want 3", n)
	}
	if strip := stripLine(h); !strings.Contains(strip, "deploy prep") {
		t.Fatalf("strip: %q", strip)
	}
}
