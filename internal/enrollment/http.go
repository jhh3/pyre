package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/transport"
)

const maxExchangeBody = 16 << 10

// RateLimitConfig bounds unauthenticated exchange attempts. Zero fields
// take defaults: 10 attempts burst per remote IP refilled one per 6s, and a
// global burst of 60 refilled 2 per second.
type RateLimitConfig struct {
	PerIPBurst  int
	PerIPEvery  time.Duration
	GlobalBurst int
	GlobalEvery time.Duration
	Disabled    bool
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	cfg    RateLimitConfig
	mu     sync.Mutex
	global bucket
	perIP  map[string]*bucket
	now    func() time.Time
}

func newLimiter(cfg RateLimitConfig) *limiter {
	if cfg.PerIPBurst <= 0 {
		cfg.PerIPBurst = 10
	}
	if cfg.PerIPEvery <= 0 {
		cfg.PerIPEvery = 6 * time.Second
	}
	if cfg.GlobalBurst <= 0 {
		cfg.GlobalBurst = 60
	}
	if cfg.GlobalEvery <= 0 {
		cfg.GlobalEvery = 500 * time.Millisecond
	}
	return &limiter{cfg: cfg, perIP: map[string]*bucket{}, now: time.Now,
		global: bucket{tokens: float64(cfg.GlobalBurst)}}
}

func take(b *bucket, burst int, every time.Duration, now time.Time) (bool, time.Duration) {
	if !b.last.IsZero() {
		b.tokens += float64(now.Sub(b.last)) / float64(every)
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) * float64(every))
}

// allow reports whether an attempt from ip may proceed, else how long to wait.
func (l *limiter) allow(ip string) (bool, time.Duration) {
	if l.cfg.Disabled {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.perIP) > 10000 { // bound memory: forget idle addresses
		for k, b := range l.perIP {
			if now.Sub(b.last) > time.Duration(l.cfg.PerIPBurst)*l.cfg.PerIPEvery {
				delete(l.perIP, k)
			}
		}
	}
	b := l.perIP[ip]
	if b == nil {
		b = &bucket{tokens: float64(l.cfg.PerIPBurst)}
		l.perIP[ip] = b
	}
	if ok, wait := take(b, l.cfg.PerIPBurst, l.cfg.PerIPEvery, now); !ok {
		return false, wait
	}
	return take(&l.global, l.cfg.GlobalBurst, l.cfg.GlobalEvery, now)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *contracts.Error) {
	writeJSON(w, e.Code.HTTPStatus(), contracts.ErrorBody{Error: e})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// HandlerOptions configure the exchange handlers.
type HandlerOptions struct {
	Logger *slog.Logger // wrapped with redaction
}

func (s *Store) exchange(w http.ResponseWriter, r *http.Request, log *slog.Logger, what string, body any, fn func(context.Context) (any, error)) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, contracts.Errorf(contracts.ErrInvalid, "use POST"))
		return
	}
	ip := remoteIP(r)
	if ok, wait := s.limit.allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		log.Warn(what+" rate limited", "remote", ip)
		writeErr(w, contracts.Errorf(contracts.ErrCapacityExhausted, "too many attempts; retry later"))
		return
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxExchangeBody))
	if err := dec.Decode(body); err != nil {
		writeErr(w, contracts.Errorf(contracts.ErrInvalid, "malformed request body"))
		return
	}
	out, err := fn(r.Context())
	if err != nil {
		e := contracts.AsError(err)
		if e.Code == contracts.ErrInternal {
			log.Error(what+" failed", "remote", ip, "err", err)
			e = contracts.Errorf(contracts.ErrInternal, "internal error")
		} else {
			log.Warn(what+" refused", "remote", ip, "code", e.Code)
		}
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// EnrollExchangeHandler serves POST contracts.PathEnrollExchange
// (unauthenticated; the one-time secret is the credential).
func (s *Store) EnrollExchangeHandler(opts HandlerOptions) http.Handler {
	log := transport.NewRedactingLogger(handlerOf(opts.Logger))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ex contracts.NodeEnrollExchange
		s.exchange(w, r, log, "node enrollment", &ex, func(ctx context.Context) (any, error) {
			cred, err := s.RedeemNode(ctx, ex)
			if err == nil {
				log.Info("node enrolled", "node", cred.NodeID, "remote", remoteIP(r))
			}
			return cred, err
		})
	})
}

// PairExchangeHandler serves POST contracts.PathPairExchange.
func (s *Store) PairExchangeHandler(opts HandlerOptions) http.Handler {
	log := transport.NewRedactingLogger(handlerOf(opts.Logger))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ex contracts.PairingExchange
		s.exchange(w, r, log, "client pairing", &ex, func(ctx context.Context) (any, error) {
			cred, err := s.RedeemPairing(ctx, ex)
			if err == nil {
				log.Info("client paired", "client", cred.ClientID, "remote", remoteIP(r))
			}
			return cred, err
		})
	})
}

// Register mounts both exchange handlers on mux.
func (s *Store) Register(mux *http.ServeMux, opts HandlerOptions) {
	mux.Handle(contracts.PathEnrollExchange, s.EnrollExchangeHandler(opts))
	mux.Handle(contracts.PathPairExchange, s.PairExchangeHandler(opts))
}

func handlerOf(l *slog.Logger) slog.Handler {
	if l == nil {
		return nil
	}
	return l.Handler()
}

// ---- pairing URI ----

// PairingURI is the decoded pyre://pair deep link / QR payload.
type PairingURI struct {
	HubURL    string
	PinSHA256 string
	PairingID string
	Code      string
}

// String formats pyre://pair?hub=<url>&pin=<sha256>&id=<pairing_id>&code=<code>.
// pin is omitted for CA-issued hub certificates.
func (p PairingURI) String() string {
	var b strings.Builder
	b.WriteString("pyre://pair?hub=")
	b.WriteString(url.QueryEscape(p.HubURL))
	if p.PinSHA256 != "" {
		b.WriteString("&pin=")
		b.WriteString(url.QueryEscape(p.PinSHA256))
	}
	b.WriteString("&id=")
	b.WriteString(url.QueryEscape(p.PairingID))
	b.WriteString("&code=")
	b.WriteString(url.QueryEscape(p.Code))
	return b.String()
}

// ParsePairingURI decodes and validates a pyre://pair link.
func ParsePairingURI(s string) (PairingURI, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "pyre" || u.Host != "pair" {
		return PairingURI{}, contracts.Errorf(contracts.ErrInvalid, "not a pyre://pair link")
	}
	q := u.Query()
	p := PairingURI{HubURL: q.Get("hub"), PinSHA256: q.Get("pin"), PairingID: q.Get("id"), Code: q.Get("code")}
	hu, err := url.Parse(p.HubURL)
	if err != nil || hu.Scheme != "https" || hu.Host == "" {
		return PairingURI{}, contracts.Errorf(contracts.ErrInvalid, "pairing link hub must be an https:// URL")
	}
	if p.PinSHA256 != "" {
		if p.PinSHA256, err = transport.NormalizeFingerprint(p.PinSHA256); err != nil {
			return PairingURI{}, contracts.Errorf(contracts.ErrInvalid, "pairing link pin: %v", err)
		}
	}
	if !contracts.ValidID(p.PairingID, contracts.PrefixPairing) || !strings.HasPrefix(p.Code, transport.PrefixPairingCode) {
		return PairingURI{}, contracts.Errorf(contracts.ErrInvalid, "pairing link is missing its id or code")
	}
	return p, nil
}

// ---- client-side exchange helpers ----

func postExchange(ctx context.Context, hubURL, pin, path string, in, out any) error {
	hu, err := url.Parse(hubURL)
	if err != nil || hu.Scheme != "https" || hu.Host == "" {
		return contracts.Errorf(contracts.ErrInvalid, "hub URL must be https://host[:port]")
	}
	tlsCfg, err := transport.ClientTLSConfig(pin)
	if err != nil {
		return contracts.Errorf(contracts.ErrInvalid, "%v", err)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(hubURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(contracts.ProtocolHeader, strconv.Itoa(contracts.ProtocolVersion))
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	resp, err := client.Do(req)
	if err != nil {
		return contracts.Errorf(contracts.ErrUnavailable, "contact hub: %s", transport.Redact(err.Error()))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		var eb contracts.ErrorBody
		if json.Unmarshal(b, &eb) == nil && eb.Error != nil {
			return eb.Error
		}
		return contracts.Errorf(contracts.ErrUnavailable, "hub returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("enrollment: decode hub response: %w", err)
	}
	return nil
}

// ExchangeEnrollment is the node side of enrollment: it redeems the ticket
// secret over pinned TLS and returns the node credential.
func ExchangeEnrollment(ctx context.Context, hubURL, pin string, ex contracts.NodeEnrollExchange) (contracts.NodeCredential, error) {
	var cred contracts.NodeCredential
	err := postExchange(ctx, hubURL, pin, contracts.PathEnrollExchange, ex, &cred)
	return cred, err
}

// ExchangePairing is the client side of pairing.
func ExchangePairing(ctx context.Context, p PairingURI, clientName, kind string) (contracts.ClientCredential, error) {
	var cred contracts.ClientCredential
	err := postExchange(ctx, p.HubURL, p.PinSHA256, contracts.PathPairExchange,
		contracts.PairingExchange{PairingID: p.PairingID, Code: p.Code, ClientName: clientName, Kind: kind}, &cred)
	return cred, err
}
