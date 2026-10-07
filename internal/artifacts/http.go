package artifacts

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// PreviewPurpose is the TokenIssuer ticket purpose for preview links.
const PreviewPurpose = "preview"

// DefaultPreviewTTL is the lifetime of a minted preview link.
const DefaultPreviewTTL = 5 * time.Minute

// PreviewCSP is the Content-Security-Policy on every preview response: an
// opaque-origin sandbox (no scripts, forms, popups or same-origin access),
// no network fetches, inline styles and data:/same-origin images only.
const PreviewCSP = "sandbox; default-src 'none'; img-src data: 'self'; style-src 'unsafe-inline'"

// contentCSP applies to authenticated downloads: never render actively.
const contentCSP = "sandbox; default-src 'none'"

// MintPreviewURL issues a one-time preview ticket bound to versionID and
// returns base + "/preview/<version_id>/<ticket>". base is "" for a
// hub-relative URL, or the separate preview origin (recommended, e.g.
// "https://hub.example:8444") so previews never share an origin with the
// management API.
func MintPreviewURL(ctx context.Context, tickets contracts.TokenIssuer, base, versionID string, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = DefaultPreviewTTL
	}
	ticket, exp, err := tickets.IssueTicket(ctx, PreviewPurpose, contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: versionID}, ttl)
	if err != nil {
		return "", time.Time{}, err
	}
	return strings.TrimRight(base, "/") + "/preview/" + url.PathEscape(versionID) + "/" + url.PathEscape(ticket), exp, nil
}

// MaxPreviewTTL caps a requested preview link lifetime.
const MaxPreviewTTL = time.Hour

// MintPreviewURL implements contracts.ArtifactAPI: any authenticated
// principal that can read the version gets a one-time link under the
// configured preview base.
func (s *Store) MintPreviewURL(ctx context.Context, req contracts.MintPreviewRequest) (contracts.PreviewURL, error) {
	if p, ok := contracts.PrincipalFrom(ctx); !ok || p.Kind == "" || p.ID == "" {
		return contracts.PreviewURL{}, contracts.Errorf(contracts.ErrUnauthorized, "no authenticated principal")
	}
	if s.tickets == nil {
		return contracts.PreviewURL{}, contracts.Errorf(contracts.ErrUnsupported, "preview links are not configured on this hub")
	}
	if req.TTLSeconds < 0 {
		return contracts.PreviewURL{}, contracts.Errorf(contracts.ErrInvalid, "ttl_seconds must be >= 0")
	}
	if _, err := getVersionQ(ctx, s.db, req.VersionID); err != nil {
		return contracts.PreviewURL{}, err
	}
	ttl := min(time.Duration(req.TTLSeconds)*time.Second, MaxPreviewTTL)
	u, exp, err := MintPreviewURL(ctx, s.tickets, s.previewBase, req.VersionID, ttl)
	if err != nil {
		return contracts.PreviewURL{}, err
	}
	return contracts.PreviewURL{URL: u, ExpiresAt: exp}, nil
}

func setPreviewHeaders(h http.Header) {
	h.Set("Content-Security-Policy", PreviewCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Del("Set-Cookie")
}

// pathSegments splits the escaped request path after prefix into exactly n
// unescaped segments.
func pathSegments(r *http.Request, prefix string, n int) ([]string, bool) {
	p, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	if !ok {
		return nil, false
	}
	parts := strings.Split(p, "/")
	if len(parts) != n {
		return nil, false
	}
	for i, s := range parts {
		u, err := url.PathUnescape(s)
		if err != nil || u == "" {
			return nil, false
		}
		parts[i] = u
	}
	return parts, true
}

// PreviewHandler serves contracts.PathPreview ("/preview/{version_id}/{token}").
// The token is a one-time TokenIssuer ticket (purpose "preview") bound to
// that exact version; no bearer token or cookie is read or set, so active
// content (HTML/SVG) cannot call management APIs. Every response carries
// PreviewCSP (sandbox), nosniff, CORP same-origin and no-referrer.
//
// Deployment: mount it on a separate origin/port (e.g. a second listener)
// so even a sandbox escape would not be same-origin with the management API.
func PreviewHandler(store *Store, tickets contracts.TokenIssuer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setPreviewHeaders(w.Header())
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		seg, ok := pathSegments(r, "/preview/", 2)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		versionID, ticket := seg[0], seg[1]
		ref, err := tickets.RedeemTicket(r.Context(), ticket, PreviewPurpose)
		if err != nil || ref.Kind != contracts.KindArtifactVersion || ref.ID != versionID {
			http.Error(w, "preview link is invalid or expired", http.StatusForbidden)
			return
		}
		rc, v, err := store.OpenContent(r.Context(), versionID)
		if err != nil {
			status := contracts.CodeOf(err).HTTPStatus()
			http.Error(w, http.StatusText(status), status)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", v.MediaType)
		w.Header().Set("Content-Disposition", disposition("inline", v.Filename))
		serve(w, r, rc, v)
	})
}

// ContentHandler serves contracts.PathContent ("/v1/content/{version_id}")
// with verified bytes, Range support, the version's media type and an
// attachment disposition with the sanitized filename. It never renders
// active content (sandbox CSP + nosniff). The hub must authenticate the
// request first and put the principal in the request context
// (contracts.WithPrincipal); without one it answers 401.
func ContentHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", contentCSP)
		h.Set("Referrer-Policy", "no-referrer")
		if _, ok := contracts.PrincipalFrom(r.Context()); !ok {
			writeErr(w, contracts.Errorf(contracts.ErrUnauthorized, "authentication required"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, contracts.Errorf(contracts.ErrUnsupported, "method not allowed"))
			return
		}
		seg, ok := pathSegments(r, "/v1/content/", 1)
		if !ok {
			writeErr(w, contracts.Errorf(contracts.ErrNotFound, "not found"))
			return
		}
		rc, v, err := store.OpenContent(r.Context(), seg[0])
		if err != nil {
			writeErr(w, err)
			return
		}
		defer rc.Close()
		h.Set("Content-Type", v.MediaType)
		h.Set("Content-Disposition", disposition("attachment", v.Filename))
		h.Set("ETag", `"`+string(v.Digest)+`"`)
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
		serve(w, r, rc, v)
	})
}

func serve(w http.ResponseWriter, r *http.Request, rc io.ReadCloser, v contracts.ArtifactVersion) {
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc)
	}
}

func disposition(kind, filename string) string {
	if d := mime.FormatMediaType(kind, map[string]string{"filename": SafeFilename(filename)}); d != "" {
		return d
	}
	return kind
}

func writeErr(w http.ResponseWriter, err error) {
	e := contracts.AsError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(contracts.ErrorBody{Error: e})
}
