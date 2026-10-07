package artifacts

import (
	"encoding/hex"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
)

// SafeFilename returns a display-only filename: the last path element of
// name (either separator), without control or bidi-override characters,
// leading/trailing dots or spaces, limited to 255 bytes. It is never used
// to build a storage path; blobs are addressed by digest.
func SafeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r),
			r == 0x200b, r == 0x2028, r == 0x2029, r == 0xfeff:
			continue
		case r == '"' || r == ':' || r == '*' || r == '?' || r == '<' || r == '>' || r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	out = truncate(out, 255)
	if out == "" {
		return "artifact"
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// activeTypes can execute script or load resources when rendered and are
// only ever rendered through the isolated preview endpoint.
var activeTypes = map[string]bool{
	"text/html":             true,
	"application/xhtml+xml": true,
	"image/svg+xml":         true,
	"text/xml":              true,
	"application/xml":       true,
	"text/xsl":              true,
	"application/xslt+xml":  true,
}

// IsActiveType reports whether a media type is active content that needs
// the sandboxed preview (and must never be rendered on the management
// origin).
func IsActiveType(mediaType string) bool {
	base, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		base = strings.ToLower(strings.TrimSpace(mediaType))
	}
	return activeTypes[base] || strings.HasSuffix(base, "+xml")
}

// Deterministic extension table (mime.TypeByExtension consults platform
// files, so common types are pinned here first).
var extTypes = map[string]string{
	".md": "text/markdown", ".markdown": "text/markdown", ".txt": "text/plain", ".log": "text/plain",
	".csv": "text/csv", ".tsv": "text/tab-separated-values", ".json": "application/json",
	".yaml": "application/yaml", ".yml": "application/yaml", ".toml": "application/toml",
	".html": "text/html", ".htm": "text/html", ".xhtml": "application/xhtml+xml",
	".svg": "image/svg+xml", ".xml": "application/xml", ".png": "image/png", ".jpg": "image/jpeg",
	".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp", ".pdf": "application/pdf",
	".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar",
	".mp4": "video/mp4", ".webm": "video/webm", ".mp3": "audio/mpeg", ".wav": "audio/wav",
	".diff": "text/x-diff", ".patch": "text/x-diff",
}

func typeByFilename(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return ""
	}
	if t, ok := extTypes[ext]; ok {
		return t
	}
	return mime.TypeByExtension(ext)
}

// normalizeMediaType validates a declared media type ("" stays "").
func normalizeMediaType(mt string) (string, error) {
	mt = strings.TrimSpace(mt)
	if mt == "" {
		return "", nil
	}
	base, params, err := mime.ParseMediaType(mt)
	if err != nil || !strings.Contains(base, "/") {
		return "", contracts.Errorf(contracts.ErrInvalid, "media_type %q is not a valid media type", mt)
	}
	return mime.FormatMediaType(base, params), nil
}

// sniffMediaType is the fallback when neither a declared type nor a known
// extension is available.
func sniffMediaType(head []byte) string {
	if len(head) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(head)
}

// parseDigest validates "sha256:<64 hex>" and returns it lowercased with
// the raw hex.
func parseDigest(d contracts.Digest) (contracts.Digest, string, error) {
	s := strings.ToLower(strings.TrimSpace(string(d)))
	hexPart, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(hexPart) != 64 {
		return "", "", contracts.Errorf(contracts.ErrInvalid, "digest must be sha256:<64 hex chars>")
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", "", contracts.Errorf(contracts.ErrInvalid, "digest must be sha256:<64 hex chars>")
	}
	return contracts.Digest(s), hexPart, nil
}
