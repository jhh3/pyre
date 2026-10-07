// Package client implements contracts.API over the v1 HTTP binding.
//
// Behavior shared by every call:
//
//   - Mutations carry an operation_id. If the caller leaves it empty, the
//     client generates one ONCE before the first attempt, so every retry of
//     that logical operation uses the same id and the hub returns the stored
//     result instead of repeating the effect.
//   - Only retryable failures are retried (transport errors, and API errors
//     with Retryable=true such as unavailable/capacity_exhausted), with
//     bounded exponential backoff plus jitter, honoring Retry-After.
//     Requests that are not idempotent and carry no operation id are never
//     retried (every mutation in the v1 API carries one, and reads and upload
//     chunks are idempotent by contract).
//   - API errors are returned as *contracts.Error. Transport failures are
//     *contracts.Error{Code: unavailable, Retryable: true}. Context
//     cancellation returns the context's error.
//   - A self-signed hub certificate is accepted iff its SHA-256 fingerprint
//     matches Options.CertSHA256.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Defaults.
const (
	DefaultTimeout       = 30 * time.Second
	DefaultMaxAttempts   = 5
	DefaultRetryBase     = 200 * time.Millisecond
	DefaultRetryMax      = 5 * time.Second
	DefaultChunkSize     = 4 << 20
	DefaultMaxChunkBytes = 8 << 20
	maxResponseBytes     = 64 << 20
)

// Options configures a Client. The zero value is usable.
type Options struct {
	// CertSHA256 pins the hub's TLS leaf certificate by the SHA-256 of its
	// DER bytes (hex; optional "sha256:" prefix and ':' separators). When
	// set, the certificate chain is NOT validated against system roots —
	// the pin alone authenticates the hub (self-signed hubs). When empty,
	// normal system verification applies.
	CertSHA256 string
	// Timeout bounds one attempt (default 30s). Long-poll calls add their
	// WaitSeconds on top.
	Timeout time.Duration
	// UserAgent is sent on every request (default "pyre-client").
	UserAgent string
	// Clock is used for Retry-After dates (default system clock).
	Clock contracts.Clock
	// HTTPClient overrides the transport entirely (tests). CertSHA256 is
	// ignored when set.
	HTTPClient *http.Client
	// MaxAttempts bounds attempts per call including the first (default 5).
	MaxAttempts int
	// RetryBase / RetryMax shape exponential backoff (defaults 200ms / 5s).
	RetryBase time.Duration
	RetryMax  time.Duration
	// MaxChunkBytes bounds one UploadChunk body (default 8 MiB).
	MaxChunkBytes int64
}

// Client is an HTTP implementation of contracts.API. It is safe for
// concurrent use.
type Client struct {
	base   *url.URL
	token  string
	opts   Options
	hc     *http.Client
	cfgErr error
	sleep  func(ctx context.Context, d time.Duration) error
}

var _ contracts.API = (*Client)(nil)

// New returns a client for the hub at baseURL (e.g. "https://hub:7788")
// authenticating with token. Configuration errors (bad URL or pin) are
// reported by Err and returned from every call.
func New(baseURL, token string, opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "pyre-client"
	}
	if opts.Clock == nil {
		opts.Clock = contracts.SystemClock{}
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = DefaultRetryBase
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = DefaultRetryMax
	}
	if opts.MaxChunkBytes <= 0 {
		opts.MaxChunkBytes = DefaultMaxChunkBytes
	}
	c := &Client{token: token, opts: opts, sleep: sleepCtx}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		c.cfgErr = contracts.Errorf(contracts.ErrInvalid, "client: invalid hub URL %q", baseURL)
		return c
	}
	c.base = u
	switch {
	case opts.HTTPClient != nil:
		c.hc = opts.HTTPClient
	case opts.CertSHA256 != "":
		if u.Scheme != "https" {
			c.cfgErr = contracts.Errorf(contracts.ErrInvalid, "client: a certificate pin requires an https hub URL")
			return c
		}
		tc, err := PinnedTLSConfig(opts.CertSHA256)
		if err != nil {
			c.cfgErr = err
			return c
		}
		c.hc = &http.Client{Transport: newTransport(tc)}
	default:
		c.hc = &http.Client{Transport: newTransport(nil)}
	}
	return c
}

func newTransport(tc *tls.Config) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = tc
	t.MaxIdleConnsPerHost = 8
	t.ResponseHeaderTimeout = 0 // per-call contexts bound time (long-polls)
	return t
}

// Err reports a configuration error, if any.
func (c *Client) Err() error { return c.cfgErr }

// BaseURL returns the hub URL.
func (c *Client) BaseURL() string {
	if c.base == nil {
		return ""
	}
	return c.base.String()
}

// ---- TLS pinning ----

// PinMismatchError reports a hub certificate whose fingerprint does not
// match the configured pin. It is never retried.
type PinMismatchError struct{ Got string }

func (e *PinMismatchError) Error() string {
	return "hub TLS certificate fingerprint sha256:" + e.Got + " does not match the pinned fingerprint"
}

// ParsePin decodes a SHA-256 fingerprint in hex, tolerating a "sha256:"
// prefix, ':' or space separators, and either case.
func ParsePin(pin string) ([]byte, error) {
	p := strings.TrimSpace(strings.ToLower(pin))
	p = strings.TrimPrefix(p, "sha256:")
	p = strings.NewReplacer(":", "", " ", "", "-", "").Replace(p)
	b, err := hex.DecodeString(p)
	if err != nil || len(b) != sha256.Size {
		return nil, contracts.Errorf(contracts.ErrInvalid, "client: certificate pin must be a hex SHA-256 fingerprint")
	}
	return b, nil
}

// CertFingerprint returns the lowercase hex SHA-256 of a DER certificate,
// the format accepted by ParsePin and printed by hubs.
func CertFingerprint(der []byte) string {
	s := sha256.Sum256(der)
	return hex.EncodeToString(s[:])
}

// PinnedTLSConfig accepts exactly the leaf certificate with the given
// SHA-256 fingerprint, regardless of issuer or hostname.
func PinnedTLSConfig(pin string) (*tls.Config, error) {
	want, err := ParsePin(pin)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by VerifyConnection pin check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return &PinMismatchError{Got: "none"}
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return &PinMismatchError{Got: hex.EncodeToString(got[:])}
			}
			return nil
		},
	}, nil
}

// ---- request plumbing ----

type request struct {
	method      string
	path        string
	query       url.Values
	body        []byte
	contentType string
	extraWait   time.Duration
	// idempotent: safe to retry (reads, upload chunks, or mutations with an
	// operation id).
	idempotent bool
	// readAll buffers the whole success body inside the retry loop, so a
	// connection dropped mid-response is retried like any transport error.
	readAll bool
}

// call performs an RPC op.
func (c *Client) call(ctx context.Context, op string, in any, out any, idempotent bool, extraWait time.Duration) error {
	body := []byte("{}")
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return contracts.Errorf(contracts.ErrInvalid, "client: encode request: %v", err)
		}
		body = b
	}
	resp, err := c.do(ctx, request{method: http.MethodPost, path: contracts.RPCPath(op), body: body, contentType: "application/json", idempotent: idempotent, extraWait: extraWait, readAll: true})
	if err != nil {
		return err
	}
	reportFreshness(ctx, op, resp.Header)
	return decodeBytes(resp.data, out)
}

// mutate ensures an operation id (generated once, before any attempt) and
// performs the call; retries reuse the same id.
func (c *Client) mutate(ctx context.Context, op string, m *contracts.Mutation, in any, out any) error {
	if m.OperationID == "" {
		m.OperationID = contracts.NewID(contracts.PrefixOperation)
	}
	return c.call(ctx, op, in, out, m.OperationID != "", 0)
}

func (c *Client) read(ctx context.Context, op string, in any, out any) error {
	return c.call(ctx, op, in, out, true, 0)
}

func decodeBytes(b []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &contracts.Error{Code: contracts.ErrInternal, Message: "client: decode hub response: " + err.Error()}
	}
	return nil
}

type response struct {
	*http.Response
	cancel context.CancelFunc
	data   []byte // set when request.readAll
}

// do runs req with retries. On success without readAll the caller must
// close Body and call cancel.
func (c *Client) do(ctx context.Context, req request) (*response, error) {
	if c.cfgErr != nil {
		return nil, c.cfgErr
	}
	maxAttempts := c.opts.MaxAttempts
	if n, ok := ctx.Value(maxAttemptsKey{}).(int); ok && n > 0 && n < maxAttempts {
		maxAttempts = n
	}
	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.once(ctx, req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var final noRetryError
		if errors.As(err, &final) {
			return nil, final.err
		}
		if !req.idempotent || !Retryable(err) || attempt+1 >= maxAttempts {
			return nil, err
		}
		wait := c.backoff(attempt)
		if retryAfter > wait {
			wait = retryAfter
		}
		if wait > c.opts.RetryMax {
			wait = c.opts.RetryMax
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// backoff returns full-jitter exponential backoff in [d/2, d].
func (c *Client) backoff(attempt int) time.Duration {
	d := c.opts.RetryBase << uint(min(attempt, 16))
	if d > c.opts.RetryMax || d <= 0 {
		d = c.opts.RetryMax
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Retryable reports whether err may be retried with the same operation id.
func Retryable(err error) bool {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.Retryable
	}
	return false
}

func (c *Client) newHTTPRequest(ctx context.Context, req request) (*http.Request, error) {
	u := *c.base
	u.Path = c.base.Path + req.path
	if req.query != nil {
		u.RawQuery = req.query.Encode()
	}
	var body io.Reader
	if req.body != nil {
		body = bytes.NewReader(req.body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.method, u.String(), body)
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "client: build request: %v", err)
	}
	c.setHeaders(hr.Header)
	if req.contentType != "" {
		hr.Header.Set("Content-Type", req.contentType)
	}
	return hr, nil
}

func (c *Client) setHeaders(h http.Header) {
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	h.Set(contracts.ProtocolHeader, strconv.Itoa(contracts.ProtocolVersion))
	h.Set("User-Agent", c.opts.UserAgent)
	h.Set("Accept", "application/json")
}

func (c *Client) once(ctx context.Context, req request) (*response, time.Duration, error) {
	actx, cancel := context.WithTimeout(ctx, c.opts.Timeout+req.extraWait)
	hr, err := c.newHTTPRequest(actx, req)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	resp, err := c.hc.Do(hr)
	if err != nil {
		cancel()
		return nil, 0, transportError(ctx, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if !req.readAll {
			return &response{Response: resp, cancel: cancel}, 0, nil
		}
		defer cancel()
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if err != nil {
			return nil, 0, transportError(ctx, err)
		}
		if len(data) > maxResponseBytes {
			return nil, 0, contracts.Errorf(contracts.ErrPayloadTooLarge, "client: hub response exceeds %d bytes", maxResponseBytes)
		}
		return &response{Response: resp, cancel: func() {}, data: data}, 0, nil
	}
	defer cancel()
	defer resp.Body.Close()
	err = errorFromResponse(resp)
	if resp.Header.Get(NoRetryHeader) != "" {
		err = noRetryError{err}
	}
	return nil, c.retryAfter(resp.Header.Get("Retry-After")), err
}

func (c *Client) retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.opts.Clock.Now()); d > 0 {
			return d
		}
	}
	return 0
}

// errorFromResponse decodes an ErrorBody, or synthesizes an error from the
// status for non-Pyre responses (proxies, load balancers).
func errorFromResponse(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var eb contracts.ErrorBody
	if json.Unmarshal(b, &eb) == nil && eb.Error != nil && eb.Error.Code != "" {
		return eb.Error
	}
	return statusError(resp.StatusCode)
}

func statusError(status int) *contracts.Error {
	code := contracts.ErrInternal
	switch status {
	case http.StatusBadRequest:
		code = contracts.ErrInvalid
	case http.StatusUnauthorized:
		code = contracts.ErrUnauthorized
	case http.StatusForbidden:
		code = contracts.ErrForbidden
	case http.StatusNotFound:
		code = contracts.ErrNotFound
	case http.StatusConflict:
		code = contracts.ErrConflict
	case http.StatusGone:
		code = contracts.ErrExpired
	case http.StatusRequestEntityTooLarge:
		code = contracts.ErrPayloadTooLarge
	case http.StatusUpgradeRequired:
		code = contracts.ErrProtocolMismatch
	case http.StatusTooManyRequests:
		code = contracts.ErrCapacityExhausted
	case http.StatusNotImplemented:
		code = contracts.ErrUnsupported
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		code = contracts.ErrUnavailable
	}
	e := contracts.Errorf(code, "hub returned HTTP %d", status)
	return e
}

// transportError classifies a failed round trip. TLS verification failures
// (including pin mismatches) are not retryable; network failures are.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var pin *PinMismatchError
	if errors.As(err, &pin) {
		return &contracts.Error{Code: contracts.ErrUnauthorized, Message: pin.Error(), Retryable: false}
	}
	var unk x509.UnknownAuthorityError
	var host x509.HostnameError
	var inv x509.CertificateInvalidError
	var cve *tls.CertificateVerificationError
	if errors.As(err, &unk) || errors.As(err, &host) || errors.As(err, &inv) || errors.As(err, &cve) {
		return &contracts.Error{Code: contracts.ErrUnauthorized, Message: "hub TLS verification failed: " + err.Error(), Retryable: false}
	}
	var se *SocketDialError
	if errors.As(err, &se) {
		return se.apiError()
	}
	var ue *url.Error
	msg := err.Error()
	if errors.As(err, &ue) {
		msg = ue.Op + " " + redactURL(ue.URL) + ": " + ue.Err.Error()
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return (&contracts.Error{Code: contracts.ErrUnavailable, Message: "hub request timed out: " + msg, Retryable: true}).WithDetail(DetailUnreachable, "hub")
	}
	return (&contracts.Error{Code: contracts.ErrUnavailable, Message: "hub unreachable: " + msg, Retryable: true}).WithDetail(DetailUnreachable, "hub")
}

// DetailUnreachable is the error detail naming the endpoint a failed round
// trip could not reach: "hub" or "local_daemon". Clients use it to say what
// was tried and what to do.
const DetailUnreachable = "unreachable"

// SocketDialError is a failed connection to the local daemon's unix socket
// (a client in socket mode, e.g. clientconn with PYRE_SOCKET). It is
// reported as the local daemon, not the hub, being unreachable.
type SocketDialError struct {
	Path string
	Err  error
}

func (e *SocketDialError) Error() string { return "dial unix " + e.Path + ": " + e.Err.Error() }
func (e *SocketDialError) Unwrap() error { return e.Err }

// maxSocketPath is the conservative unix socket path limit (sun_path is
// 104 bytes on macOS, 108 on Linux, including the terminator).
const maxSocketPath = 103

func (e *SocketDialError) apiError() *contracts.Error {
	const todo = "start pyred on this machine, point PYRE_SOCKET at its socket (<state dir>/pyred.sock), or unset PYRE_SOCKET to use the hub (PYRE_HUB or `pyre pair`)"
	var msg string
	retry := true
	switch {
	case len(e.Path) > maxSocketPath:
		msg = fmt.Sprintf("local daemon socket path %s is %d bytes, longer than unix sockets allow (%d): use a shorter PYRE_SOCKET / pyred --state directory", e.Path, len(e.Path), maxSocketPath)
		retry = false
	case errors.Is(e.Err, fs.ErrNotExist):
		msg = fmt.Sprintf("local daemon unreachable: no socket at %s (pyred is not running there): %s", e.Path, todo)
	case errors.Is(e.Err, syscall.ECONNREFUSED):
		msg = fmt.Sprintf("local daemon unreachable: nothing is listening on %s (stale socket; pyred stopped?): %s", e.Path, todo)
	case errors.Is(e.Err, fs.ErrPermission):
		msg = fmt.Sprintf("local daemon socket %s: permission denied (owned by another user?): use your own pyred's socket or unset PYRE_SOCKET to use the hub", e.Path)
		retry = false
	default:
		msg = fmt.Sprintf("local daemon unreachable at socket %s: %v: %s", e.Path, e.Err, todo)
	}
	return (&contracts.Error{Code: contracts.ErrUnavailable, Message: msg, Retryable: retry}).
		WithDetail(DetailUnreachable, "local_daemon").WithDetail("socket", e.Path)
}

func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "<url>"
	}
	u.User = nil
	u.RawQuery = ""
	return u.String()
}
