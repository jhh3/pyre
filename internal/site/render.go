package site

import (
	"embed"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed assets/site.css assets/notes.js
var assetFS embed.FS

//go:embed assets/starter.html
var starterHTML string

// Assets is the shared files served under /_site/.
func Assets() fs.FS {
	sub, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

var statusLabels = map[string][2]string{
	"draft":    {"Draft", "draft"},
	"ready":    {"Ready for review", "ready"},
	"review":   {"Ready for review", "ready"},
	"decided":  {"Decided", "decided"},
	"archived": {"Archived", "archived"},
}

// statusOf returns the label and CSS modifier for a status word.
func statusOf(s string) (string, string) {
	k := strings.ToLower(strings.TrimSpace(s))
	k = strings.TrimSuffix(k, " for review")
	if l, ok := statusLabels[k]; ok {
		return l[0], l[1]
	}
	if s == "" {
		return "Draft", "draft"
	}
	return s, "other"
}

func e(s string) string { return html.EscapeString(s) }

// RenderIndex is the site's front page: one card per page (archived pages
// in a collapsed section), each card taking notes on the page as a whole.
func RenderIndex(title string, pages []Page, now time.Time) []byte {
	var active, old []string
	for _, p := range pages {
		label, cls := statusOf(p.Meta.Status)
		var meta strings.Builder
		fmt.Fprintf(&meta, `<span class="pst pst-%s">%s</span>`, cls, e(label))
		if !p.Updated.IsZero() {
			fmt.Fprintf(&meta, `<span>Updated %s</span>`, e(FormatTime(p.Updated)))
		}
		if p.Meta.Owner != "" {
			fmt.Fprintf(&meta, `<span>· %s</span>`, e(p.Meta.Owner))
		}
		card := fmt.Sprintf(`<article class="card" id="%s" data-note="%s" data-note-page="%s"><div class="card-head"><div>`+
			`<div class="card-title"><a href="p/%s/">%s</a></div><p class="summary">%s</p><div class="meta">%s</div></div></div></article>`,
			e(p.Slug), PageItem, e(p.Slug), e(p.Slug), e(p.Meta.Title), e(p.Meta.Summary), meta.String())
		if p.IsArchived() {
			old = append(old, card)
		} else {
			active = append(active, card)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<link rel="stylesheet" href="_site/site.css"></head>
<body><div class="wrap"><nav class="ps-bar"><a href="./" aria-current="page">%s</a><button class="chip ps-theme" type="button" aria-label="Toggle light or dark theme">Theme</button></nav>
<header><h1>%s</h1><p class="sub">Pages for review. Open one to leave notes on its items; a note on a card below is about the whole page.</p></header></div>
<main class="wrap">`, e(title), e(title), e(title))
	if len(active) == 0 && len(old) == 0 {
		b.WriteString(`<p class="empty">No pages yet. Create one with <code>pyre site new &lt;name&gt;</code>.</p>`)
	}
	b.WriteString(strings.Join(active, "\n"))
	if len(old) > 0 {
		fmt.Fprintf(&b, `<details class="archived"><summary>Archived (%d)</summary>%s</details>`, len(old), strings.Join(old, "\n"))
	}
	total := len(active) + len(old)
	noun := "pages"
	if total == 1 {
		noun = "page"
	}
	fmt.Fprintf(&b, `</main><footer class="wrap">%d %s (%d active, %d archived) · %s</footer>
<script src="_site/notes.js"></script></body></html>
`, total, noun, len(active), len(old), e(FormatTime(now)))
	return []byte(b.String())
}

// RenderPage returns the page's index.html with the site's chrome added:
// the shared stylesheet first in <head> (the page's own styles win), a
// meta naming the page for the notes widget, a top bar (back link,
// status, theme toggle) after <body>, and the notes script before </body>.
func RenderPage(p Page, src []byte, siteTitle string) []byte {
	s := string(src)
	head := fmt.Sprintf(`<meta name="pyre-site-page" content="%s"><link rel="stylesheet" href="../../_site/site.css">`, e(p.Slug))
	if loc := headRE.FindStringIndex(s); loc != nil {
		s = s[:loc[1]] + head + s[loc[1]:]
	} else {
		s = head + s
	}
	label, cls := statusOf(p.Meta.Status)
	bar := fmt.Sprintf(`<div class="wrap ps-chrome"><nav class="ps-bar"><a href="../../">%s</a><span class="ps-sep">›</span><span class="ps-here">%s</span>`+
		`<span class="pst pst-%s">%s</span><button class="chip ps-theme" type="button" aria-label="Toggle light or dark theme">Theme</button></nav></div>`,
		e(siteTitle), e(p.Meta.Title), cls, e(label))
	if loc := bodyRE.FindStringIndex(s); loc != nil {
		s = s[:loc[1]] + bar + s[loc[1]:]
	} else {
		s = bar + s
	}
	const script = `<script src="../../_site/notes.js"></script>`
	if locs := bodyEndRE.FindAllStringIndex(s, -1); len(locs) > 0 {
		at := locs[len(locs)-1][0]
		s = s[:at] + script + s[at:]
	} else {
		s += script
	}
	return []byte(s)
}

// Export writes a static copy of the site to out (index, pages and their
// files, the shared assets). Notes need the server; the export is for
// previewing the rendering or archiving.
func Export(root, out, title string, now time.Time) ([]Page, []Problem, error) {
	pages, probs, err := Check(root)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Join(out, "_site"), 0o755); err != nil {
		return nil, nil, err
	}
	for _, name := range []string{"site.css", "notes.js"} {
		b, _ := fs.ReadFile(Assets(), name)
		if err := os.WriteFile(filepath.Join(out, "_site", name), b, 0o644); err != nil {
			return nil, nil, err
		}
	}
	for _, p := range pages {
		dst := filepath.Join(out, "p", p.Slug)
		if err := os.RemoveAll(dst); err != nil {
			return nil, nil, err
		}
		if err := copyTree(p.Dir, dst); err != nil {
			return nil, nil, err
		}
		src, err := os.ReadFile(filepath.Join(p.Dir, HTMLFile))
		if err != nil {
			continue // reported by Check
		}
		if err := os.WriteFile(filepath.Join(dst, HTMLFile), RenderPage(p, src, title), 0o644); err != nil {
			return nil, nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), RenderIndex(title, pages, now), 0o644); err != nil {
		return nil, nil, err
	}
	return pages, probs, nil
}

// copyTree copies regular files (no dot files, no symlinks) from src to dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}
