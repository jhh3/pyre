package repoindex

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Score rates how well query matches a repo: every whitespace-separated
// term must match its name, remote URL or path. Exact and prefix name
// matches rank far above subsequence matches; remote and path matches
// rank below name matches. ok is false when a term does not match.
func Score(query string, r contracts.Repo) (int, bool) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return 0, true
	}
	name := strings.ToLower(r.Name)
	remote := strings.ToLower(strings.TrimSuffix(r.RemoteURL, ".git"))
	path := strings.ToLower(r.Path)
	total := 0
	for _, t := range terms {
		best := 0
		switch {
		case name == t:
			best = 1000
		case strings.HasPrefix(name, t):
			best = 600 - min(len(name)-len(t), 100)
		case strings.Contains(name, t):
			best = 400 - min(len(name)-len(t), 100)
		case wordPrefix(name, t):
			best = 300
		case subseq(name, t):
			best = 150 - min(len(name)-len(t), 100)
		}
		if best == 0 {
			switch {
			case strings.HasSuffix(remote, "/"+t) || strings.HasSuffix(remote, ":"+t):
				best = 500
			case strings.Contains(remote, t):
				best = 120
			case strings.Contains(path, t):
				best = 80
			}
		}
		if best == 0 {
			return 0, false
		}
		total += best
	}
	return total, true
}

// wordPrefix: t matches the initials of name's words ("vw" ~ "vortex-web").
func wordPrefix(name, t string) bool {
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == ' ' })
	if len(words) < 2 {
		return false
	}
	var ini strings.Builder
	for _, w := range words {
		ini.WriteByte(w[0])
	}
	return strings.HasPrefix(ini.String(), t)
}

func subseq(s, t string) bool {
	i := 0
	for j := 0; j < len(s) && i < len(t); j++ {
		if s[j] == t[i] {
			i++
		}
	}
	return i == len(t)
}

// recent is when a repo was last used: a session ran there, else git activity.
func recent(e contracts.RepoEntry) time.Time {
	if e.LastUsedAt != nil && e.LastUsedAt.After(e.LastActivity) {
		return *e.LastUsedAt
	}
	return e.LastActivity
}

// Rank filters and orders entries for query: best score first, then most
// recently used (sessions) or active (git), then name. An empty query
// lists every entry, most recent first.
func Rank(query string, entries []contracts.RepoEntry) []contracts.RepoEntry {
	out := make([]contracts.RepoEntry, 0, len(entries))
	for _, e := range entries {
		s, ok := Score(query, e.Repo)
		if !ok {
			continue
		}
		e.Score = s
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		ua, ub := a.LastUsedAt != nil, b.LastUsedAt != nil
		if ua != ub {
			return ua
		}
		if ra, rb := recent(a), recent(b); !ra.Equal(rb) {
			return ra.After(rb)
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.NodeName < b.NodeName
	})
	return out
}

// Group collapses entries of the same repository on several machines
// (same remote URL, else same name) into one choice, keeping rank order;
// the first entry of each group is its preferred machine.
func Group(ranked []contracts.RepoEntry) [][]contracts.RepoEntry {
	var groups [][]contracts.RepoEntry
	at := map[string]int{}
	for _, e := range ranked {
		k := RepoKey(e.Repo)
		if i, ok := at[k]; ok {
			groups[i] = append(groups[i], e)
			continue
		}
		at[k] = len(groups)
		groups = append(groups, []contracts.RepoEntry{e})
	}
	return groups
}

// RepoKey identifies a repository across machines: its normalized remote
// ("github.com/owner/name"), else its directory name.
func RepoKey(r contracts.Repo) string {
	u := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.RemoteURL), ".git"))
	if u == "" {
		return "name:" + strings.ToLower(r.Name)
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexByte(u, '@'); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[:i] + "/" + strings.TrimPrefix(u[i+1:], "/") // host:o/n and host:/o/n alike
	}
	if rest, ok := strings.CutPrefix(u, "github/"); ok {
		u = "github.com/" + rest // an unresolved "github:o/n" ssh alias (older indexes)
	}
	return "remote:" + filepath.ToSlash(u)
}
