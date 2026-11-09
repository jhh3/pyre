package ghwait

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPI is GitHub's REST API.
const DefaultAPI = "https://api.github.com"

// Client is a GitHub REST client that keeps each answer with its ETag in a
// directory and sends If-None-Match, so an unchanged answer is a 304 that
// GitHub does not count against the rate limit.
type Client struct {
	Base  string                                    // API base (DefaultAPI when empty)
	Token func(ctx context.Context) (string, error) // the node's gh token
	HTTP  *http.Client
	// CacheDir keeps answers by URL (one file each). Empty: no cache.
	CacheDir string
	// Guard (optional) is asked before each request that goes out: the gh
	// shim's per-session budget (ghguard). An error skips the request.
	Guard func(ctx context.Context) error

	mu        sync.Mutex
	remaining int // X-RateLimit-Remaining of the last answer (-1 unknown)
	limit     int
	// Stats counts requests (tests, logs).
	Requests, NotModified int
}

// Response is one GET's answer: from the network or (304) the cache.
type Response struct {
	Status   int
	Body     []byte
	Next     string // Link rel="next" URL
	Cached   bool   // a 304: Body came from the cache
	Fetched  time.Time
	ETag     string
	rateLeft int
}

// HTTPError is a non-2xx, non-304 answer.
type HTTPError struct {
	Status  int
	Message string
	URL     string
	// RateLimited: a 403/429 for the primary or secondary rate limit
	// (transient), unlike a 401/403/404 that needs a person.
	RateLimited bool
}

func (e *HTTPError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("GitHub %d for %s: %s", e.Status, e.URL, msg)
}

// Permanent reports whether err needs a person (bad auth, no access, not
// found) rather than a retry.
func Permanent(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return !he.RateLimited && (he.Status == 401 || he.Status == 403 || he.Status == 404 || he.Status == 410 || he.Status == 422)
	}
	return false
}

type cacheEntry struct {
	URL     string    `json:"url"`
	ETag    string    `json:"etag"`
	Body    []byte    `json:"body"`
	Next    string    `json:"next,omitempty"`
	Fetched time.Time `json:"fetched"`
}

func (c *Client) cachePath(u string) string {
	h := sha256.Sum256([]byte(u))
	return filepath.Join(c.CacheDir, fmt.Sprintf("%x.json", h[:12]))
}

func (c *Client) loadCache(u string) (cacheEntry, bool) {
	if c.CacheDir == "" {
		return cacheEntry{}, false
	}
	b, err := os.ReadFile(c.cachePath(u))
	if err != nil {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if json.Unmarshal(b, &e) != nil || e.URL != u || e.ETag == "" {
		return cacheEntry{}, false
	}
	return e, true
}

func (c *Client) saveCache(e cacheEntry) {
	if c.CacheDir == "" {
		return
	}
	b, err := json.Marshal(e)
	if err == nil {
		_ = writeAtomic(c.cachePath(e.URL), b)
	}
}

// RateLeft is the last seen X-RateLimit-Remaining and limit (-1, -1 when
// none seen yet).
func (c *Client) RateLeft() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit == 0 {
		return -1, -1
	}
	return c.remaining, c.limit
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// Get fetches path (relative to Base, or an absolute URL from a Link).
func (c *Client) Get(ctx context.Context, path string) (Response, error) {
	base := strings.TrimRight(c.Base, "/")
	if base == "" {
		base = DefaultAPI
	}
	u := path
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = base + "/" + strings.TrimLeft(path, "/")
	}
	if c.Guard != nil {
		if err := c.Guard(ctx); err != nil {
			return Response{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "pyre-wait")
	if c.Token != nil {
		tok, err := c.Token(ctx)
		if err != nil {
			return Response{}, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	cached, haveCache := c.loadCache(u)
	if haveCache {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Response{}, err
	}
	c.mu.Lock()
	c.Requests++
	if rem, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); err == nil {
		c.remaining = rem
		if l, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit")); err == nil {
			c.limit = l
		}
	}
	if resp.StatusCode == http.StatusNotModified {
		c.NotModified++
	}
	c.mu.Unlock()
	now := time.Now()
	switch {
	case resp.StatusCode == http.StatusNotModified && haveCache:
		cached.Fetched = now
		c.saveCache(cached)
		return Response{Status: 200, Body: cached.Body, Next: cached.Next, Cached: true, Fetched: now, ETag: cached.ETag}, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		r := Response{Status: resp.StatusCode, Body: body, Fetched: now, ETag: resp.Header.Get("ETag")}
		if m := linkNext.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			r.Next = m[1]
		}
		if r.ETag != "" {
			c.saveCache(cacheEntry{URL: u, ETag: r.ETag, Body: body, Next: r.Next, Fetched: now})
		}
		return r, nil
	}
	he := &HTTPError{Status: resp.StatusCode, URL: u}
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &msg) == nil {
		he.Message = msg.Message
	}
	low := strings.ToLower(he.Message)
	if resp.StatusCode == 429 || (resp.StatusCode == 403 && (resp.Header.Get("X-RateLimit-Remaining") == "0" || strings.Contains(low, "rate limit") || resp.Header.Get("Retry-After") != "")) {
		he.RateLimited = true
	}
	return Response{}, he
}

// GetAll fetches path and follows Link rel="next" (at most maxPages),
// decoding each page's JSON with add.
func (c *Client) GetAll(ctx context.Context, path string, maxPages int, add func([]byte) error) error {
	next := path
	for i := 0; next != "" && i < maxPages; i++ {
		r, err := c.Get(ctx, next)
		if err != nil {
			return err
		}
		if err := add(r.Body); err != nil {
			return fmt.Errorf("decode %s: %w", next, err)
		}
		next = r.Next
	}
	return nil
}

// writeAtomic writes b to path through a temp file and a rename.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}
