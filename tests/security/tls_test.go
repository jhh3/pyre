//go:build !windows

package security

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

func wrongPin(pin string) string {
	b := []byte(pin)
	if b[0] == 'a' {
		b[0] = 'b'
	} else {
		b[0] = 'a'
	}
	return string(b)
}

// countingHandler wraps the fixture server to count requests that reached
// the application (i.e. survived the TLS handshake).
type requestCounter struct {
	n    atomic.Int64
	auth atomic.Int64
	next http.Handler
}

func (c *requestCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.n.Add(1)
	if r.Header.Get("Authorization") != "" {
		c.auth.Add(1)
	}
	c.next.ServeHTTP(w, r)
}

// TestTLSPinMismatchRejected: with the wrong pin, the HTTP client, the node
// link and the enrollment exchange all abort in the TLS handshake — no
// request (and so no bearer token or one-time secret) reaches the server,
// the failure is not retried, and the enrollment ticket is not consumed.
func TestTLSPinMismatchRejected(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctr := &requestCounter{next: f.srv.Config.Handler}
	f.srv.Config.Handler = ctr
	bad := wrongPin(f.pin)

	// HTTP client.
	cl := client.New(f.srv.URL, f.owner.Token, client.Options{CertSHA256: bad, MaxAttempts: 5, RetryBase: time.Millisecond})
	_, err := cl.WhoAmI(context.Background())
	if !contracts.IsCode(err, contracts.ErrUnauthorized) || client.Retryable(err) {
		t.Fatalf("client with wrong pin = %v (retryable=%v), want non-retryable unauthorized", err, client.Retryable(err))
	}
	if strings.Contains(err.Error(), f.owner.Token) {
		t.Fatal("pin error mentions the bearer token")
	}
	good := client.New(f.srv.URL, f.owner.Token, client.Options{CertSHA256: f.pin})
	if _, err := good.WhoAmI(context.Background()); err != nil {
		t.Fatalf("client with the right pin: %v", err)
	}
	if ctr.n.Load() != 1 {
		t.Fatalf("server saw %d requests; only the correctly pinned one may arrive", ctr.n.Load())
	}

	// Node link.
	nodeID, nodeTok := f.enrollNode("pinned-node")
	tlsCfg, err := transport.PinnedTLSConfig(bad)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	link, err := transport.NewNodeLink(transport.NodeLinkConfig{
		HubURL: f.srv.URL, NodeID: nodeID, Token: nodeTok, TLS: tlsCfg, DB: db,
		Handler: transport.CommandHandlerFunc(func(context.Context, contracts.NodeCommand) (contracts.CommandResult, error) {
			return contracts.CommandResult{}, nil
		}),
		Backoff: transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = link.Run(ctx) }()
	time.Sleep(600 * time.Millisecond)
	st := link.Status()
	cancel()
	<-done
	if st.State == transport.LinkConnected || f.ns.Connected(nodeID) {
		t.Fatal("node link connected despite a pin mismatch")
	}
	if st.LastError == nil || !strings.Contains(st.LastError.Message, "fingerprint") {
		t.Fatalf("link error does not explain the pin mismatch: %+v", st.LastError)
	}
	if ctr.auth.Load() != 1 { // only the good client's request carried a bearer
		t.Fatalf("%d authenticated requests reached the server; the node token leaked past TLS", ctr.auth.Load())
	}

	// Enrollment exchange.
	tk, err := f.fleet.CreateEnrollment(f.ownerAPICtx(), contracts.EnrollmentRequest{Mutation: op(), Name: "pin-enroll"})
	if err != nil {
		t.Fatal(err)
	}
	f.remember(tk.Secret)
	if _, err := enrollment.ExchangeEnrollment(context.Background(), f.srv.URL, bad, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret}); err == nil {
		t.Fatal("enrollment exchange succeeded with the wrong pin")
	}
	cred, err := enrollment.ExchangeEnrollment(context.Background(), f.srv.URL, f.pin, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "h"})
	if err != nil {
		t.Fatalf("ticket consumed or broken by the mismatched attempt: %v", err)
	}
	f.remember(cred.Token)
	f.assertNoLeaks(nil)
}
