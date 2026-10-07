package conformance_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/conformance"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// mockSim adapts mock.Backend's simulation controls to the conformance
// capability interfaces.
type mockSim struct {
	b   *mock.Backend
	clk *mock.FakeClock
}

func (m mockSim) SettleDelivery(context.Context) error { m.b.Settle(0); return nil }
func (m mockSim) ReportDelivery(_ context.Context, id string, st c.RuntimeDeliveryState) error {
	return m.b.ReportDelivery(id, st, "")
}
func (m mockSim) SetRunState(_ context.Context, runID string, st c.RunState) error {
	return m.b.SetRunState(runID, st, "")
}
func (m mockSim) SetNodeConnected(_ context.Context, nodeID string, up bool) error {
	return m.b.SetNodeConnected(nodeID, up)
}
func (m mockSim) RequestApproval(_ context.Context, runID string, ttl time.Duration) (c.Approval, error) {
	return m.b.RequestApproval(runID, mock.ApprovalSpec{TTL: ttl})
}
func (m mockSim) ResolveApprovalNatively(_ context.Context, id, opt string) error {
	_, err := m.b.ResolveApprovalNatively(id, opt)
	return err
}
func (m mockSim) TrimEvents(_ context.Context, keep int) error { m.b.TrimEvents(keep); return nil }
func (m mockSim) AdvanceClock(d time.Duration)                 { m.clk.Advance(d) }
func (m mockSim) ReportConnection(_ context.Context, cn c.Connection) error {
	m.b.ReportConnection(cn)
	return nil
}

func newMock(t *testing.T) (*mock.Backend, *mock.FakeClock) {
	clk := mock.NewFakeClock(mock.FixtureEpoch)
	return mock.New(mock.Options{Clock: clk}), clk
}

// TestConformanceMock runs the suite against the in-process mock.
func TestConformanceMock(t *testing.T) {
	conformance.Run(t, conformance.Harness{Name: "mock", New: func(t *testing.T) *conformance.Target {
		b, clk := newMock(t)
		return &conformance.Target{
			API:    b,
			As:     func(ctx context.Context, p c.Principal) context.Context { return c.WithPrincipal(ctx, p) },
			NodeID: "node_synth-mac",
			Sim:    mockSim{b: b, clk: clk},
		}
	}})
}

// TestConformanceMockOverHTTP runs the suite through httpapi + client, so
// wire encoding, strict decoding and error mapping are exercised too.
func TestConformanceMockOverHTTP(t *testing.T) {
	conformance.Run(t, conformance.Harness{Name: "mock-http", New: func(t *testing.T) *conformance.Target {
		b, clk := newMock(t)
		auth := httpapi.NewStaticTokens(nil)
		// Token "p:<kind>:<id>" authenticates as that principal (test only).
		auth.Fallback = func(tok string) (c.Principal, bool) {
			parts := strings.SplitN(tok, ":", 3)
			if len(parts) != 3 || parts[0] != "p" {
				return c.Principal{}, false
			}
			return c.Principal{Kind: c.PrincipalKind(parts[1]), ID: parts[2]}, true
		}
		srv := httptest.NewServer(httpapi.NewServer(b, auth, httpapi.Options{StreamWait: time.Second}))
		t.Cleanup(srv.Close)
		var mu sync.Mutex
		clients := map[string]*client.Client{}
		api := conformance.ByPrincipal(func(ctx context.Context) c.API {
			p, ok := c.PrincipalFrom(ctx)
			tok := "anonymous"
			if ok {
				tok = "p:" + string(p.Kind) + ":" + p.ID
			}
			mu.Lock()
			defer mu.Unlock()
			if cl, ok := clients[tok]; ok {
				return cl
			}
			cl := client.New(srv.URL, tok, client.Options{RetryBase: time.Millisecond, MaxAttempts: 2})
			clients[tok] = cl
			return cl
		})
		return &conformance.Target{
			API:    api,
			As:     func(ctx context.Context, p c.Principal) context.Context { return c.WithPrincipal(ctx, p) },
			NodeID: "node_synth-mac",
			Sim:    mockSim{b: b, clk: clk},
		}
	}})
}
