package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// DefaultListen is where `pyre site serve` listens (loopback only).
const DefaultListen = "127.0.0.1:7393"

// DefaultTitle names the site in its header.
const DefaultTitle = "Review"

// Identity headers the signing-in proxy sets (the ngrok policy strips any
// copies the client sent first).
const (
	HeaderEmail = "X-Pyre-Email"
	HeaderName  = "X-Pyre-Name"
)

// DevEmail is the identity of a loopback request in --dev mode.
const DevEmail = "dev@localhost"

// contentSecurityPolicy: pages are self-contained and may use inline
// script and style; nothing loads from other hosts except https images,
// and nothing may frame the site.
const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: https:; font-src 'self' data:; media-src 'self' data: blob:; connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// Deliver sends text to a Pyre session and returns the message ID.
type Deliver func(ctx context.Context, session, text string) (string, error)

// Options configures a Server.
type Options struct {
	// Root holds the page folders.
	Root string
	// StateDir holds notes.json. Empty keeps notes in memory.
	StateDir string
	// Listen is the loopback address (default DefaultListen).
	Listen string
	// Title names the site (default DefaultTitle).
	Title string
	// AllowDomains and AllowEmails decide who may use the site: an email
	// is admitted when it is listed or its domain is (exactly; not a
	// subdomain). At least one is required unless Dev.
	AllowDomains []string
	AllowEmails  []string
	// Origins are the public origins accepted on writes besides the
	// request's own host. The first one is also the base of the links in
	// delivered notes.
	Origins []string
	// Dev admits loopback requests without the identity header (local
	// testing only; never behind the tunnel).
	Dev bool
	// Deliver, when set, sends each new note to the session its page names.
	Deliver Deliver
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Logf logs operational problems (default log.Printf).
	Logf func(format string, args ...any)
}

// Server is the review site.
type Server struct {
	opts  Options
	notes *Store
	mux   *http.ServeMux
}

// CheckListen refuses a non-loopback address.
func CheckListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %v", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--listen %q: the review site listens on loopback only (127.0.0.1 or ::1); a signing-in proxy such as ngrok exposes it", addr)
	}
	return nil
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	if opts.Root == "" {
		return nil, errors.New("site: no page root")
	}
	if opts.Listen == "" {
		opts.Listen = DefaultListen
	}
	if err := CheckListen(opts.Listen); err != nil {
		return nil, err
	}
	if opts.Title == "" {
		opts.Title = DefaultTitle
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	for i, d := range opts.AllowDomains {
		opts.AllowDomains[i] = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
	}
	for i, m := range opts.AllowEmails {
		opts.AllowEmails[i] = strings.ToLower(strings.TrimSpace(m))
	}
	if !opts.Dev && len(opts.AllowDomains) == 0 && len(opts.AllowEmails) == 0 {
		return nil, errors.New("say who may use the site: --allow-domain example.org and/or --allow-email someone@example.org (or --dev for local testing)")
	}
	store, err := OpenStore(opts.StateDir)
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, notes: store, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

// Handler is the site with its security wrapper.
func (s *Server) Handler() http.Handler { return s.secure(s.mux) }

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.opts.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /{$}", s.index)
	m.Handle("GET /_site/", http.StripPrefix("/_site/", http.FileServerFS(Assets())))
	m.HandleFunc("GET /p/{slug}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/p/"+r.PathValue("slug")+"/", http.StatusMovedPermanently)
	})
	m.HandleFunc("GET /p/{slug}/{file...}", s.page)
	m.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"email": identityOf(r).email})
	})
	m.HandleFunc("GET /api/notes", s.listNotes)
	m.HandleFunc("POST /api/notes", s.addNote)
	m.HandleFunc("PATCH /api/notes/{id}", s.editNote)
	m.HandleFunc("DELETE /api/notes/{id}", s.deleteNote)
}

// ---- pages ----

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	pages, probs, err := LoadPages(s.opts.Root)
	if err != nil {
		s.opts.Logf("site: reading %s: %v", s.opts.Root, err)
		http.Error(w, "The page folder could not be read.", http.StatusInternalServerError)
		return
	}
	for _, p := range probs {
		s.opts.Logf("site: skipped page %s", p)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(RenderIndex(s.opts.Title, pages, s.opts.Now()))
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	slug, file := r.PathValue("slug"), r.PathValue("file")
	p, err := LoadPage(s.opts.Root, slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if file == "" || file == HTMLFile {
		src, err := os.ReadFile(filepath.Join(p.Dir, HTMLFile))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(RenderPage(p, src, s.opts.Title))
		return
	}
	// The page's own files. os.Root keeps symlinks and ".." inside the
	// folder; dot files and page.json stay private.
	clean := path.Clean("/" + file)[1:]
	for _, part := range strings.Split(clean, "/") {
		if strings.HasPrefix(part, ".") {
			http.NotFound(w, r)
			return
		}
	}
	if clean == PageFile {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(p.Dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	fi, err := fs.Stat(root.FS(), clean)
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, root.FS(), clean)
}

// ---- notes API ----

type noteView struct {
	Note
	Mine bool `json:"mine"`
}

func (s *Server) listNotes(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	page := r.URL.Query().Get("page")
	if page != "" && !ValidSlug(page) {
		writeErr(w, http.StatusBadRequest, "Unknown page.")
		return
	}
	out := []noteView{}
	for _, n := range s.notes.List(page) {
		out = append(out, noteView{Note: n, Mine: n.Author == id.email})
	}
	writeJSON(w, http.StatusOK, map[string]any{"me": id.email, "notes": out})
}

type noteReq struct {
	Page string `json:"page"`
	Item string `json:"item"`
	Text string `json:"text"`
}

func (s *Server) addNote(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	var req noteReq
	if !readBody(w, r, &req) {
		return
	}
	text, msg := cleanText(req.Text)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !ValidSlug(req.Page) || !(ValidItem(req.Item) || req.Item == PageItem) {
		writeErr(w, http.StatusBadRequest, "Unknown item.")
		return
	}
	p, err := LoadPage(s.opts.Root, req.Page)
	if err != nil {
		writeErr(w, http.StatusNotFound, "That page no longer exists.")
		return
	}
	n, err := s.notes.Add(Note{Page: req.Page, Item: req.Item, Author: id.email, Name: id.name, Text: text, Created: s.opts.Now().UTC()})
	if err != nil {
		s.opts.Logf("site: saving a note: %v", err)
		writeErr(w, http.StatusInternalServerError, "The note could not be saved. Try again.")
		return
	}
	if sess := strings.TrimSpace(p.Meta.Session); sess != "" && s.opts.Deliver != nil {
		n = s.deliver(r.Context(), p, n, sess)
	}
	writeJSON(w, http.StatusCreated, noteView{Note: n, Mine: true})
}

// deliver sends a new note to the page's session and records the outcome
// on the note (the note is already saved; a failed send loses nothing).
func (s *Server) deliver(ctx context.Context, p Page, n Note, sess string) Note {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	d := Delivery{Session: sess}
	mid, err := s.opts.Deliver(ctx, sess, s.noteMessage(p, n))
	if err != nil {
		d.Error = err.Error()
		s.opts.Logf("site: delivering note %s to %s: %v", n.ID, sess, err)
	} else {
		d.MessageID = mid
	}
	d.At = s.opts.Now().UTC()
	if upd, err := s.notes.Update(n.ID, "", func(x *Note) { x.Delivery = &d }); err == nil {
		return upd
	}
	n.Delivery = &d
	return n
}

// noteMessage is the Pyre message that carries a note to the page's agent.
func (s *Server) noteMessage(p Page, n Note) string {
	who := n.Author
	if n.Name != "" && n.Name != n.Author {
		who = n.Name + " <" + n.Author + ">"
	}
	where := "the page as a whole"
	if n.Item != PageItem {
		where = "item " + n.Item
	}
	link := "/p/" + p.Slug + "/"
	if len(s.opts.Origins) > 0 {
		link = strings.TrimRight(s.opts.Origins[0], "/") + link
	}
	return fmt.Sprintf("Review note on your page %q (%s, %s) from %s, %s:\n\n%s\n\nPage: %s\nAll notes on it: pyre site notes %s",
		p.Meta.Title, p.Slug, where, who, FormatTime(n.Created), n.Text, link, p.Slug)
}

func (s *Server) editNote(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	var req noteReq
	if !readBody(w, r, &req) {
		return
	}
	text, msg := cleanText(req.Text)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	now := s.opts.Now().UTC()
	n, err := s.notes.Update(r.PathValue("id"), id.email, func(x *Note) { x.Text, x.Edited, x.Name = text, &now, id.name })
	if err != nil {
		writeStoreErr(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, noteView{Note: n, Mine: true})
}

func (s *Server) deleteNote(w http.ResponseWriter, r *http.Request) {
	if err := s.notes.Delete(r.PathValue("id"), identityOf(r).email); err != nil {
		writeStoreErr(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeStoreErr(w http.ResponseWriter, s *Server, err error) {
	switch {
	case errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errNotAuthor):
		writeErr(w, http.StatusForbidden, err.Error())
	default:
		s.opts.Logf("site: saving notes: %v", err)
		writeErr(w, http.StatusInternalServerError, "The change could not be saved. Try again.")
	}
}

func cleanText(t string) (string, string) {
	t = strings.TrimSpace(t)
	switch {
	case t == "":
		return "", "Write something first."
	case len([]rune(t)) > MaxNoteText:
		return "", fmt.Sprintf("Notes are limited to %d characters.", MaxNoteText)
	}
	return t, ""
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "Request too large.")
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		writeErr(w, http.StatusBadRequest, "Send a JSON object.")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---- security ----

type identity struct{ email, name string }

type identityKey struct{}

func identityOf(r *http.Request) identity {
	id, _ := r.Context().Value(identityKey{}).(identity)
	return id
}

// secure: security headers on everything, then authentication (the
// identity header, or loopback with Dev) on everything, then CSRF checks
// on writes (JSON body, same-origin Origin, no cross-site fetch).
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
		id, status, msg := s.authenticate(r)
		if status != 0 {
			writeErr(w, status, msg)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodPost, http.MethodPatch, http.MethodDelete:
			if status, msg := s.checkCSRF(r); status != 0 {
				writeErr(w, status, msg)
				return
			}
		default:
			h.Set("Allow", "GET, HEAD, POST, PATCH, DELETE")
			writeErr(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func (s *Server) authenticate(r *http.Request) (identity, int, string) {
	vals := r.Header.Values(HeaderEmail)
	if len(vals) > 1 {
		return identity{}, http.StatusForbidden, "More than one " + HeaderEmail + " header; refusing the request."
	}
	email := ""
	if len(vals) == 1 {
		email = strings.ToLower(strings.TrimSpace(vals[0]))
	}
	if email == "" {
		if s.opts.Dev && loopback(r.RemoteAddr) {
			return identity{email: DevEmail, name: "dev"}, 0, ""
		}
		return identity{}, http.StatusUnauthorized, "Not signed in. Open the site through its sign-in link."
	}
	if !s.allowed(email) {
		return identity{}, http.StatusForbidden, email + " is not allowed on this site."
	}
	name := strings.TrimSpace(r.Header.Get(HeaderName))
	if len(name) > 80 || strings.ContainsAny(name, "\r\n<>") {
		name = ""
	}
	return identity{email: email, name: name}, 0, ""
}

// allowed: one "@", a local part, and a listed email or domain (exact
// match; a subdomain is not its parent).
func (s *Server) allowed(e string) bool {
	at := strings.IndexByte(e, '@')
	if at <= 0 || strings.Count(e, "@") != 1 || strings.ContainsAny(e, " \t\r\n,;<>\"'") {
		return false
	}
	for _, m := range s.opts.AllowEmails {
		if e == m {
			return true
		}
	}
	dom := e[at+1:]
	for _, d := range s.opts.AllowDomains {
		if dom == d {
			return true
		}
	}
	return false
}

func loopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) checkCSRF(r *http.Request) (int, string) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Method != http.MethodDelete && (err != nil || mt != "application/json") {
		return http.StatusBadRequest, "Send the request as JSON (Content-Type: application/json)."
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return http.StatusForbidden, "Cross-site request refused."
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return http.StatusForbidden, "Request refused: no Origin header."
	}
	if !s.sameOrigin(origin, r) {
		return http.StatusForbidden, "Request refused: origin " + origin + " is not this site."
	}
	return 0, ""
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
