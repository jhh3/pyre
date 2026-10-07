package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// stallingAuth answers only after the request context ends, the way the
// credential store failed for requests that sat in a stalled network path
// (A20 soak: an ssh tunnel SIGSTOPped for 2 min, then resumed).
type stallingAuth struct{}

func (stallingAuth) Authenticate(ctx context.Context, _ string) (contracts.Principal, error) {
	<-ctx.Done()
	return contracts.Principal{}, contracts.Errorf(contracts.ErrUnavailable, "credential store unavailable")
}

// A request whose client went away while it was being authenticated is a
// client cancellation, not a server error (the soak logged 12 "internal
// error" lines for exactly that).
func TestClientGoneDuringAuthIsNotAnInternalError(t *testing.T) {
	lb := &logBuf{}
	// HTTP/2 (like the real TLS clients): a cancelled request resets its
	// stream, which cancels the handler's context.
	srv := httptest.NewUnstartedServer(NewServer(&fakeAPI{}, stallingAuth{}, Options{Logger: slog.New(slog.NewTextHandler(lb, nil))}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/system.whoami", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-token-123")
	if resp, err := srv.Client().Do(req); err == nil {
		resp.Body.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(lb.String(), "system.whoami") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	logs := lb.String()
	if !strings.Contains(logs, "system.whoami") {
		t.Fatalf("no access log:\n%s", logs)
	}
	if strings.Contains(logs, "level=ERROR") || strings.Contains(logs, "internal error") {
		t.Fatalf("client disconnect during auth logged as a server error:\n%s", logs)
	}
	if !strings.Contains(logs, "client cancelled") {
		t.Fatalf("want a client-cancelled note:\n%s", logs)
	}
}
