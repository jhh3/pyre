package client

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Conditional list reads (contracts/conditional.go). The client keeps the
// last answer to each polled list read (contracts.ConditionalOps) with the
// hub's ETag and sends the tag back in If-None-Match; an unchanged list
// then comes back as a bodiless 304 and the kept body is decoded instead.
// A hub that predates ETags never tags its answers, so nothing is kept and
// no If-None-Match is ever sent to it.
//
// The cache is process-wide (pyred holds one client per proxied bearer) and
// bounded by bytes. Entries are keyed by hub URL, token, op and the exact
// request body, so one principal's answer is never offered for another's
// request; the hub's 304 vouches for the content itself.

const (
	condCacheMaxBytes = 16 << 20
	condEntryMaxBytes = 4 << 20
)

type condEntry struct {
	key  string
	etag string
	body []byte
}

type condCache struct {
	mu    sync.Mutex
	ll    *list.List // front = most recently used
	items map[string]*list.Element
	bytes int
	max   int
}

var sharedCond = newCondCache(condCacheMaxBytes)

func newCondCache(max int) *condCache {
	return &condCache{ll: list.New(), items: map[string]*list.Element{}, max: max}
}

func condKey(base, token, op string, body []byte) string {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(base), []byte(token), []byte(op), body} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{0})
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *condCache) get(key string) (condEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return condEntry{}, false
	}
	c.ll.MoveToFront(el)
	return *el.Value.(*condEntry), true
}

func (c *condCache) put(key, etag string, body []byte) {
	if etag == "" || len(body) > condEntryMaxBytes {
		c.drop(key)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*condEntry)
		c.bytes += len(body) - len(e.body)
		e.etag, e.body = etag, body
		c.ll.MoveToFront(el)
	} else {
		c.items[key] = c.ll.PushFront(&condEntry{key: key, etag: etag, body: body})
		c.bytes += len(body)
	}
	for c.bytes > c.max && c.ll.Len() > 1 {
		old := c.ll.Back()
		e := old.Value.(*condEntry)
		c.ll.Remove(old)
		delete(c.items, e.key)
		c.bytes -= len(e.body)
	}
}

func (c *condCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.bytes -= len(el.Value.(*condEntry).body)
		c.ll.Remove(el)
		delete(c.items, key)
	}
}

// notModifiedSnapshot is the JSON fragment that moves a reused page's
// snapshot_at to the hub's read time (nil when the 304 carries none).
func notModifiedSnapshot(h http.Header) []byte {
	v := h.Get(contracts.SnapshotAtHeader)
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil
	}
	return []byte(`{"snapshot_at":"` + t.UTC().Format(time.RFC3339Nano) + `"}`)
}
