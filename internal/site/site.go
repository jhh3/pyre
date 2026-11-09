// Package site is `pyre site`: a small review site. Agents put one folder
// per page under a root directory (page.json + a self-contained
// index.html + assets); the server renders an index of status cards and
// each page with a shared stylesheet, a theme toggle and a notes widget.
// People leave notes on any element marked data-note="<id>"; a new note is
// delivered as a Pyre message to the session the page names. The server
// listens on loopback only and trusts an identity header set by a
// signing-in proxy (ngrok Google OAuth). docs/site.md describes it.
package site

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PageFile and HTMLFile are the two files a page folder must hold.
const (
	PageFile = "page.json"
	HTMLFile = "index.html"
)

// PageItem is the item ID of a note on the page as a whole (its index card).
const PageItem = "_page"

var (
	slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	itemRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)
	// noteAttrRE finds data-note attributes in page HTML.
	noteAttrRE = regexp.MustCompile(`\sdata-note\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	headRE     = regexp.MustCompile(`(?i)<head(\s[^>]*)?>`)
	bodyRE     = regexp.MustCompile(`(?i)<body(\s[^>]*)?>`)
	bodyEndRE  = regexp.MustCompile(`(?i)</body\s*>`)
)

// ValidSlug reports whether s can name a page (lowercase, digits, hyphens;
// it is part of the URL and of every note's key, so never rename a page).
func ValidSlug(s string) bool { return slugRE.MatchString(s) }

// ValidItem reports whether s can be a data-note ID.
func ValidItem(s string) bool { return itemRE.MatchString(s) }

// Meta is page.json.
type Meta struct {
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	// Status: draft, ready (for review), decided, archived (or any short
	// word, shown as-is). Archived pages sit in a collapsed section.
	Status string `json:"status,omitempty"`
	// Updated is RFC 3339 or a date (YYYY-MM-DD); default: page.json's
	// modification time.
	Updated string `json:"updated,omitempty"`
	// Owner is who to ask about the page (free text).
	Owner string `json:"owner,omitempty"`
	// Session is the Pyre session that owns the page: every new note is
	// delivered to it as a message.
	Session string `json:"session,omitempty"`
	// Order sorts pages (lower first); pages without it sort newest first.
	Order *int `json:"order,omitempty"`
	// Archived forces the archived section on or off.
	Archived *bool `json:"archived,omitempty"`
}

// Page is a loaded page folder.
type Page struct {
	Slug    string
	Dir     string
	Meta    Meta
	Updated time.Time
}

// IsArchived: Meta.Archived wins; else status archived.
func (p Page) IsArchived() bool {
	if p.Meta.Archived != nil {
		return *p.Meta.Archived
	}
	return strings.EqualFold(p.Meta.Status, "archived")
}

// Problem is something wrong with a page folder (Check).
type Problem struct {
	Page    string `json:"page"`
	Message string `json:"message"`
}

func (p Problem) String() string { return p.Page + ": " + p.Message }

// LoadPage reads <root>/<slug>/page.json.
func LoadPage(root, slug string) (Page, error) {
	if !ValidSlug(slug) {
		return Page{}, fmt.Errorf("%q is not a page name (lowercase letters, digits and hyphens)", slug)
	}
	dir := filepath.Join(root, slug)
	path := filepath.Join(dir, PageFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return Page{}, err
	}
	var m Meta
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&m); err != nil {
		return Page{}, fmt.Errorf("%s: %v", PageFile, err)
	}
	if strings.TrimSpace(m.Title) == "" {
		return Page{}, fmt.Errorf("%s has no title", PageFile)
	}
	p := Page{Slug: slug, Dir: dir, Meta: m}
	if m.Updated != "" {
		t, err := parseWhen(m.Updated)
		if err != nil {
			return Page{}, fmt.Errorf("%s: updated %q is not RFC 3339 or YYYY-MM-DD", PageFile, m.Updated)
		}
		p.Updated = t
	} else if fi, err := os.Stat(path); err == nil {
		p.Updated = fi.ModTime()
	}
	return p, nil
}

func parseWhen(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02", s, Pacific)
}

// LoadPages loads every page folder under root, sorted for the index
// (Order first, then newest first). Folders that fail to load come back
// as problems; folders without page.json (and dot folders) are ignored.
func LoadPages(root string) ([]Page, []Problem, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var pages []Page
	var probs []Problem
	for _, e := range ents {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, name, PageFile)); err != nil {
			continue
		}
		p, err := LoadPage(root, name)
		if err != nil {
			probs = append(probs, Problem{Page: name, Message: err.Error()})
			continue
		}
		pages = append(pages, p)
	}
	sort.SliceStable(pages, func(i, j int) bool {
		a, b := pages[i], pages[j]
		ao, bo := 1<<30, 1<<30
		if a.Meta.Order != nil {
			ao = *a.Meta.Order
		}
		if b.Meta.Order != nil {
			bo = *b.Meta.Order
		}
		if ao != bo {
			return ao < bo
		}
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		return a.Slug < b.Slug
	})
	return pages, probs, nil
}

// Check validates every page under root: page.json, index.html, and that
// each data-note ID is well formed and unique within its page.
func Check(root string) ([]Page, []Problem, error) {
	pages, probs, err := LoadPages(root)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range pages {
		b, err := os.ReadFile(filepath.Join(p.Dir, HTMLFile))
		if err != nil {
			probs = append(probs, Problem{Page: p.Slug, Message: "no " + HTMLFile})
			continue
		}
		if !bodyRE.Match(b) {
			probs = append(probs, Problem{Page: p.Slug, Message: HTMLFile + " has no <body> tag"})
		}
		seen := map[string]bool{}
		for _, id := range NoteIDs(b) {
			switch {
			case !ValidItem(id) || id == PageItem:
				probs = append(probs, Problem{Page: p.Slug, Message: fmt.Sprintf("data-note=%q: use letters, digits, '.', '_' or '-' (at most 80), not %q", id, PageItem)})
			case seen[id]:
				probs = append(probs, Problem{Page: p.Slug, Message: fmt.Sprintf("data-note=%q is used twice; each item needs its own ID", id)})
			}
			seen[id] = true
		}
	}
	return pages, probs, nil
}

// NoteIDs returns the data-note IDs in page HTML, in order.
func NoteIDs(b []byte) []string {
	var ids []string
	for _, m := range noteAttrRE.FindAllSubmatch(b, -1) {
		id := string(m[1])
		if len(m[2]) > 0 {
			id = string(m[2])
		}
		ids = append(ids, html.UnescapeString(id))
	}
	return ids
}

// Pacific is where times are shown (America/Los_Angeles); a fixed -08:00
// zone when the system has no tz database.
var Pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.FixedZone("PT", -8*3600)
}()

// FormatTime is "Nov 9, 2026, 10:42 AM PT".
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(Pacific).Format("Jan 2, 2006, 3:04 PM") + " PT"
}

// Scaffold creates <root>/<slug>/ with page.json and a starter index.html.
func Scaffold(root, slug string, m Meta, now time.Time) (string, error) {
	if !ValidSlug(slug) {
		return "", fmt.Errorf("%q is not a page name: use lowercase letters, digits and hyphens (it becomes the URL)", slug)
	}
	if strings.TrimSpace(m.Title) == "" {
		m.Title = slug
	}
	if m.Status == "" {
		m.Status = "draft"
	}
	if m.Updated == "" {
		m.Updated = now.In(Pacific).Format(time.RFC3339)
	}
	dir := filepath.Join(root, slug)
	if _, err := os.Stat(dir); err == nil {
		return "", fmt.Errorf("%s already exists", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, PageFile), append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	page := strings.ReplaceAll(starterHTML, "{{TITLE}}", html.EscapeString(m.Title))
	page = strings.ReplaceAll(page, "{{SUMMARY}}", html.EscapeString(firstNonEmpty(m.Summary, "One paragraph: what is being decided and why now.")))
	if err := os.WriteFile(filepath.Join(dir, HTMLFile), []byte(page), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
