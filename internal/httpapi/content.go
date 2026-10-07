package httpapi

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- uploads ----

// limitTracker wraps a request body and remembers if the limit was hit, so
// an implementation's read error can be reported as payload_too_large.
type limitTracker struct {
	r       io.Reader
	n       int64 // remaining
	tripped bool
	readErr error // reading the request body failed (client gone or cut off)
}

func (l *limitTracker) Read(p []byte) (int, error) {
	if l.n <= 0 {
		// Probe one byte to distinguish "exactly at limit" from "over".
		var one [1]byte
		k, err := l.r.Read(one[:])
		if k > 0 {
			l.tripped = true
			return 0, contracts.Errorf(contracts.ErrPayloadTooLarge, "upload chunk exceeds limit")
		}
		return 0, err
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	k, err := l.r.Read(p)
	l.n -= int64(k)
	if err != nil && err != io.EOF {
		l.readErr = err
	}
	return k, err
}

func (s *server) serveUpload(w http.ResponseWriter, r *http.Request) {
	uploadID := r.PathValue("upload_id")
	if !contracts.ValidID(uploadID, contracts.PrefixUpload) {
		WriteError(w, contracts.Errorf(contracts.ErrInvalid, "invalid upload id"))
		return
	}
	offStr := r.URL.Query().Get("offset")
	offset, err := strconv.ParseInt(offStr, 10, 64)
	if offStr == "" || err != nil || offset < 0 {
		WriteError(w, contracts.Errorf(contracts.ErrInvalid, "offset query parameter must be a non-negative integer"))
		return
	}
	if r.ContentLength > s.opts.MaxChunkBytes {
		WriteError(w, contracts.Errorf(contracts.ErrPayloadTooLarge, "upload chunk exceeds %d bytes", s.opts.MaxChunkBytes))
		return
	}
	body := &limitTracker{r: r.Body, n: s.opts.MaxChunkBytes}
	up, err := s.api.UploadChunk(r.Context(), uploadID, offset, body)
	if err != nil {
		if body.tripped {
			WriteError(w, contracts.Errorf(contracts.ErrPayloadTooLarge, "upload chunk exceeds %d bytes", s.opts.MaxChunkBytes))
			return
		}
		if body.readErr != nil {
			// The client went away (or its connection was cut) mid-chunk:
			// not a server fault. The upload stays open and resumable.
			noteCancelled(r, body.readErr)
			WriteError(w, contracts.Errorf(contracts.ErrInvalid, "upload chunk interrupted: the request body ended early"))
			return
		}
		noteInternal(r, err)
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, up)
}

// ---- content ----

func (s *server) serveContent(w http.ResponseWriter, r *http.Request) {
	versionID := r.PathValue("version_id")
	if !contracts.ValidID(versionID, contracts.PrefixVersion) {
		WriteError(w, contracts.Errorf(contracts.ErrInvalid, "invalid version id"))
		return
	}
	rc, ver, err := s.api.OpenContent(r.Context(), versionID)
	if err != nil {
		noteInternal(r, err)
		WriteError(w, err)
		return
	}
	defer rc.Close()

	h := w.Header()
	h.Set("Content-Type", SafeMediaType(ver.MediaType))
	h.Set("Content-Disposition", ContentDisposition("attachment", ver.Filename))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	if ver.Digest != "" {
		h.Set("ETag", `"`+strings.ReplaceAll(string(ver.Digest), `"`, "")+`"`)
	}
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", ver.CreatedAt, rs)
		return
	}
	serveRangeStream(w, r, rc, ver.Size)
}

// serveRangeStream serves a forward-only reader honoring a single byte range.
// Multi-range requests are answered with the full body (permitted by RFC 9110).
func serveRangeStream(w http.ResponseWriter, r *http.Request, rc io.Reader, size int64) {
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == h.Get("ETag") {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rng := r.Header.Get("Range")
	if ir := r.Header.Get("If-Range"); ir != "" && ir != h.Get("ETag") {
		rng = ""
	}
	start, length, partial, ok := parseSingleRange(rng, size)
	if !ok {
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writeErrorStatus(w, http.StatusRequestedRangeNotSatisfiable, contracts.Errorf(contracts.ErrInvalid, "requested range not satisfiable"))
		return
	}
	if !partial {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.CopyN(w, rc, size)
		}
		return
	}
	if _, err := io.CopyN(io.Discard, rc, start); err != nil {
		WriteError(w, contracts.Errorf(contracts.ErrInternal, "content shorter than recorded size"))
		return
	}
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, size))
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	if r.Method != http.MethodHead {
		_, _ = io.CopyN(w, rc, length)
	}
}

// parseSingleRange parses "bytes=a-b", "bytes=a-" or "bytes=-n".
// partial=false means serve the whole body; ok=false means 416.
func parseSingleRange(v string, size int64) (start, length int64, partial, ok bool) {
	if v == "" {
		return 0, size, false, true
	}
	const prefix = "bytes="
	if !strings.HasPrefix(v, prefix) {
		return 0, size, false, true // unknown unit: ignore
	}
	spec := strings.TrimSpace(v[len(prefix):])
	if strings.Contains(spec, ",") {
		return 0, size, false, true // multi-range: serve full
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false, false
	}
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		if size == 0 {
			return 0, 0, false, false
		}
		return size - n, n, true, true
	}
	s, err := strconv.ParseInt(a, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false, false
	}
	e := size - 1
	if b != "" {
		e, err = strconv.ParseInt(b, 10, 64)
		if err != nil || e < s {
			return 0, 0, false, false
		}
		if e >= size {
			e = size - 1
		}
	}
	return s, e - s + 1, true, true
}

// SafeMediaType returns a well-formed media type or application/octet-stream.
func SafeMediaType(mt string) string {
	if mt == "" {
		return "application/octet-stream"
	}
	parsed, params, err := mime.ParseMediaType(mt)
	if err != nil || !strings.Contains(parsed, "/") {
		return "application/octet-stream"
	}
	return mime.FormatMediaType(parsed, params)
}

// SanitizeFilename reduces a display filename to a safe base name: no path
// components, control characters, quotes, or leading dots; at most 180
// bytes. Empty results become "download".
func SanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), r == '"', r == '\\', r == '/', r == ';', r == '%':
			b.WriteRune('_')
		case unicode.Is(unicode.Cf, r): // bidi overrides, zero-width chars
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	for len(out) > 180 {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	if out == "" {
		return "download"
	}
	return out
}

// ContentDisposition builds a header value with an ASCII fallback filename
// and an RFC 5987 UTF-8 filename* parameter.
func ContentDisposition(disposition, filename string) string {
	name := SanitizeFilename(filename)
	var ascii strings.Builder
	needsExt := false
	for _, r := range name {
		if r < 0x20 || r > 0x7e {
			ascii.WriteByte('_')
			needsExt = true
			continue
		}
		ascii.WriteRune(r)
	}
	v := disposition + `; filename="` + ascii.String() + `"`
	if needsExt {
		v += "; filename*=UTF-8''" + rfc5987Escape(name)
	}
	return v
}

func rfc5987Escape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
