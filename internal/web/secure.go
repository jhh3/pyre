package web

import (
	"context"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// contentSecurityPolicy allows only the app's own files: no inline
// scripts, no third-party anything, no framing.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; media-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

type emailKey struct{}

// DevEmail is the identity of a loopback request in --dev mode.
const DevEmail = "dev@localhost"

// secure wraps the app: security headers, authentication (X-Pyre-Email
// from the ngrok policy, or loopback with --dev) and, for POSTs, CSRF
// checks (JSON content type and a same-origin Origin).
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		email, err := s.authenticate(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodPost:
			if err := s.checkCSRF(r); err != nil {
				writeErr(w, err)
				return
			}
		default:
			h.Set("Allow", "GET, HEAD, POST")
			writeJSON(w, http.StatusMethodNotAllowed, c.ErrorBody{Error: &c.Error{Code: c.ErrInvalid, Message: "Method not allowed."}})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), emailKey{}, email)))
	})
}

// authenticate returns the signed-in email.
func (s *Server) authenticate(r *http.Request) (string, error) {
	vals := r.Header.Values(HeaderEmail)
	if len(vals) > 1 {
		return "", c.Errorf(c.ErrForbidden, "More than one %s header; refusing the request.", HeaderEmail)
	}
	email := ""
	if len(vals) == 1 {
		email = strings.ToLower(strings.TrimSpace(vals[0]))
	}
	if email == "" {
		if s.opts.Dev && loopback(r.RemoteAddr) {
			return DevEmail, nil
		}
		return "", c.Errorf(c.ErrUnauthorized, "Not signed in. Open %s and sign in with your @%s Google account.", DefaultOrigin, EmailDomain)
	}
	if !allowedEmail(email) {
		return "", c.Errorf(c.ErrForbidden, "%s is not allowed here; sign in with an @%s account.", email, EmailDomain)
	}
	return email, nil
}

// allowedEmail: exactly one "@", a local part, and the domain EmailDomain
// (not a subdomain of it).
func allowedEmail(e string) bool {
	at := strings.IndexByte(e, '@')
	if at <= 0 || strings.Count(e, "@") != 1 || strings.ContainsAny(e, " \t\r\n,;<>") {
		return false
	}
	return e[at+1:] == EmailDomain
}

func loopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkCSRF: a state-changing request must be JSON (a cross-site form
// cannot send that without a preflight) from the app's own origin.
func (s *Server) checkCSRF(r *http.Request) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return c.Errorf(c.ErrInvalid, "Send the request as JSON (Content-Type: application/json).")
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return c.Errorf(c.ErrForbidden, "Cross-site request refused.")
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return c.Errorf(c.ErrForbidden, "Request refused: no Origin header.")
	}
	if !s.sameOrigin(origin, r) {
		return c.Errorf(c.ErrForbidden, "Request refused: origin %s is not this app.", origin)
	}
	return nil
}

func (s *Server) sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	norm := strings.ToLower(u.Scheme + "://" + u.Host)
	for _, o := range s.opts.Origins {
		if strings.ToLower(strings.TrimRight(o, "/")) == norm {
			return true
		}
	}
	return false
}

func emailOf(r *http.Request) string {
	e, _ := r.Context().Value(emailKey{}).(string)
	return e
}
