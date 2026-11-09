package httpapi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Conditional reads and compression of RPC answers (contracts/conditional.go).
// Both are negotiated by request headers that older peers never send, so a
// hub with them serves old clients unchanged, and a new client reading an
// old hub just gets 200s with plain bodies.

// gzipMinBytes is the smallest answer worth compressing.
const gzipMinBytes = 4 << 10

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
	return w
}}

// snapshotter is implemented by contracts.Page.
type snapshotter interface{ SnapshotTime() time.Time }

// writeRPC writes a successful RPC answer: 304 when a conditional read's
// content is unchanged, else the JSON body, gzipped when worthwhile and
// accepted.
func (s *server) writeRPC(w http.ResponseWriter, r *http.Request, op string, resp any) {
	b, err := json.Marshal(resp)
	if err != nil {
		writeJSON(w, http.StatusOK, resp) // reports the marshal failure
		return
	}
	h := w.Header()
	if s.opts.ETags && contracts.ConditionalOps[op] {
		var snap time.Time
		if sn, ok := resp.(snapshotter); ok {
			snap = sn.SnapshotTime()
		}
		tag := rpcETag(b, snap)
		h.Set("ETag", tag)
		if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, tag) {
			if !snap.IsZero() {
				h.Set(contracts.SnapshotAtHeader, snap.UTC().Format(time.RFC3339Nano))
			}
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	if s.opts.Compress && len(b) >= gzipMinBytes {
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			var buf bytes.Buffer
			zw := gzipPool.Get().(*gzip.Writer)
			zw.Reset(&buf)
			_, werr := zw.Write(b)
			cerr := zw.Close()
			gzipPool.Put(zw)
			if werr == nil && cerr == nil {
				h.Set("Content-Encoding", "gzip")
				if info, _ := r.Context().Value(logInfoKey).(*logInfo); info != nil {
					info.rawBytes = int64(len(b))
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(buf.Bytes())
				return
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// rpcETag is a strong tag over body with its top-level snapshot_at
// (the read time, which changes on every read) left out.
func rpcETag(body []byte, snap time.Time) string {
	hashed := body
	if !snap.IsZero() {
		if v, err := json.Marshal(snap); err == nil {
			field := append([]byte(`"snapshot_at":`), v...)
			if i := bytes.LastIndex(body, field); i >= 0 {
				hashed = make([]byte, 0, len(body)-len(field))
				hashed = append(append(hashed, body[:i]...), body[i+len(field):]...)
			}
		}
	}
	sum := sha256.Sum256(hashed)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagMatches reports whether an If-None-Match value names tag (weak
// comparison, as RFC 9110 prescribes for If-None-Match).
func etagMatches(inm, tag string) bool {
	for _, t := range strings.Split(inm, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == tag {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether an Accept-Encoding value allows gzip.
func acceptsGzip(v string) bool {
	for _, part := range strings.Split(v, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		params = strings.ReplaceAll(params, " ", "")
		return params != "q=0" && params != "q=0.0" && params != "q=0.00" && params != "q=0.000"
	}
	return false
}
