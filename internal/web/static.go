package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The single-page app: the build output of web/ (React + Vite; see
// web/README.md), committed so Go builds need no Node.
//
//go:embed static
var staticFiles embed.FS

func staticFS() fs.FS {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// staticHandler serves the app's files; any other path that is not a file
// gets index.html (client-side routes such as /inbox/<thread>).
func (s *Server) staticHandler() http.Handler {
	files := http.FileServerFS(s.opts.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Files are embedded in the binary: revalidate rather than cache
		// across upgrades, except Vite's content-hashed assets, which never
		// change under their name.
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(s.opts.Static, name); err == nil && !st.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
			if path.Ext(name) != "" {
				http.NotFound(w, r) // a missing asset is a 404, not the app
				return
			}
		}
		b, err := fs.ReadFile(s.opts.Static, "index.html")
		if err != nil {
			http.Error(w, "the web app is not built into this binary", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
}
