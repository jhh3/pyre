package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/httpapi"
)

// localSocketToken stands in for a missing bearer on the 0600 socket when
// no owner credential is configured (local fallbacks only; the hub will
// reject it).
const localSocketToken = "pyre-local-socket"

// localAuth authenticates local socket callers:
//   - a session token of a run on this node -> that session (agent tools)
//   - the configured owner credential (or no bearer) -> owner
//   - any other token -> verified with the hub (cached), else unauthorized
type localAuth struct {
	d     *Daemon
	mu    sync.Mutex
	cache map[string]cachedPrincipal
}

type cachedPrincipal struct {
	p   contracts.Principal
	exp time.Time
}

func (a *localAuth) Authenticate(ctx context.Context, bearer string) (contracts.Principal, error) {
	d := a.d
	if bearer == localSocketToken {
		return contracts.Principal{Kind: contracts.PrincipalOwner, ID: "local", Name: "local socket"}, nil
	}
	if d.cfg.OwnerToken != "" && bearer == d.cfg.OwnerToken {
		return contracts.Principal{Kind: contracts.PrincipalOwner, ID: "local", Name: "owner credential"}, nil
	}
	if sid, ok, err := d.st.sessionForToken(ctx, bearer); err != nil {
		return contracts.Principal{}, contracts.Errorf(contracts.ErrUnavailable, "local auth store: %v", err)
	} else if ok {
		return contracts.Principal{Kind: contracts.PrincipalSession, ID: sid}, nil
	}
	key := hashToken(bearer)
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && d.now().Before(c.exp) {
		a.mu.Unlock()
		return c.p, nil
	}
	a.mu.Unlock()
	up, err := d.upstream(bearer)
	if err != nil {
		return contracts.Principal{}, contracts.Errorf(contracts.ErrUnauthorized, "unknown credential and no hub to verify it")
	}
	p, err := up.WhoAmI(ctx)
	if err != nil {
		if hubDown(err) {
			return contracts.Principal{}, contracts.Errorf(contracts.ErrUnauthorized, "cannot verify credential while the hub is unreachable")
		}
		return contracts.Principal{}, err
	}
	a.mu.Lock()
	if len(a.cache) > 256 {
		a.cache = map[string]cachedPrincipal{}
	}
	a.cache[key] = cachedPrincipal{p: p, exp: d.now().Add(5 * time.Minute)}
	a.mu.Unlock()
	return p, nil
}

// LocalHandler returns the HTTP handler for the local socket: the standard
// v1 binding (internal/httpapi) over LocalAPI, with the raw bearer carried
// in the context so proxied calls keep the caller's identity.
func (d *Daemon) LocalHandler() http.Handler {
	inner := httpapi.NewServer(d.LocalAPI(), &localAuth{d: d, cache: map[string]cachedPrincipal{}}, httpapi.Options{
		Logger: d.cfg.Logger.With("component", "local-api"),
		Extra:  map[string]http.Handler{"GET " + PathLocalStatus: d.localStatusHandler()},
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			tok = strings.TrimSpace(h[7:])
		}
		if tok == "" {
			tok = d.cfg.OwnerToken
			if tok == "" {
				tok = localSocketToken
			}
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		fwd := tok
		if tok == localSocketToken {
			fwd = ""
		}
		note := &freshnessNote{}
		ctx := context.WithValue(WithBearer(r.Context(), fwd), freshnessNoteKey{}, note)
		inner.ServeHTTP(&freshnessWriter{ResponseWriter: w, note: note, definitive: d.hubUnreachable}, r.WithContext(ctx))
	})
}

// ServeLocal serves the local API on the Unix socket (mode 0600) until ctx
// is cancelled. A stale socket file from a previous daemon is replaced; a
// socket with a live listener is an error (another pyred is running).
func (d *Daemon) ServeLocal(ctx context.Context) error {
	path := d.SocketPath()
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return errors.New("daemon: " + path + " exists and is not a socket")
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return errors.New("daemon: another pyred is serving " + path)
		}
		_ = os.Remove(path)
	}
	// The socket lives in the 0700 state directory, so it is never
	// reachable by other users even before the chmod below.
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	_ = fileperm.Restrict(path, 0o600)
	srv := &http.Server{Handler: d.LocalHandler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		_ = os.Remove(path)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
