package repoindex

import (
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// The global repo catalog (W-newsession): every machine's repo index and
// the owner's GitHub repositories as one list, deduplicated by normalized
// remote and ranked used in Pyre > contributed on GitHub > present on a
// machine > the rest.

// NormalizeRemote is a remote's host/path, lower case, without scheme,
// user, port-less ssh colon or .git: git@github.com:o/n.git,
// https://github.com/o/n and ssh://git@github.com/o/n.git are all
// "github.com/o/n".
func NormalizeRemote(u string) string {
	k := RepoKey(contracts.Repo{RemoteURL: strings.TrimRight(strings.TrimSpace(u), "/")})
	k, _ = strings.CutPrefix(k, "remote:")
	return strings.TrimSuffix(k, "/")
}

// CatalogKey is RepoKey with a trailing slash removed.
func CatalogKey(r contracts.Repo) string {
	if strings.TrimSpace(r.RemoteURL) != "" {
		return "remote:" + NormalizeRemote(r.RemoteURL)
	}
	return RepoKey(r)
}

// FullNameOf returns "owner/name" for a hosted remote (host/owner/name).
func FullNameOf(remote string) string {
	n := NormalizeRemote(remote)
	parts := strings.Split(n, "/")
	if len(parts) < 3 || n == "" {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// Catalog merges node repo entries (as repos.list returns them) and GitHub
// snapshots, filters by query and ranks. Present entries keep the order
// of entries (pass them ranked for "best machine first").
func Catalog(entries []contracts.RepoEntry, gh []contracts.GitHubRepoIndex, query string, now time.Time) []contracts.CatalogRepo {
	byKey := map[string]*contracts.CatalogRepo{}
	var order []string
	get := func(key string) *contracts.CatalogRepo {
		if c, ok := byKey[key]; ok {
			return c
		}
		c := &contracts.CatalogRepo{Key: key, Present: []contracts.RepoEntry{}}
		byKey[key] = c
		order = append(order, key)
		return c
	}
	for _, e := range entries {
		c := get(CatalogKey(e.Repo))
		c.Present = append(c.Present, e)
		if c.Name == "" {
			c.Name = e.Name
		}
		if c.RemoteURL == "" && e.RemoteURL != "" {
			c.RemoteURL = e.RemoteURL
			c.FullName = FullNameOf(e.RemoteURL)
		}
		if e.LastUsedAt != nil && (c.LastUsedAt == nil || e.LastUsedAt.After(*c.LastUsedAt)) {
			t := *e.LastUsedAt
			c.LastUsedAt = &t
		}
		if e.LastActivity.After(c.LastActivity) {
			c.LastActivity = e.LastActivity
		}
	}
	for _, idx := range gh {
		for _, g := range idx.Repos {
			if g.FullName == "" || g.CloneURL == "" {
				continue
			}
			c := get(CatalogKey(contracts.Repo{RemoteURL: g.CloneURL}))
			c.GitHub = true
			if c.Name == "" {
				_, c.Name, _ = strings.Cut(g.FullName, "/")
			}
			if c.FullName == "" {
				c.FullName = g.FullName
			}
			if c.RemoteURL == "" {
				c.RemoteURL = g.CloneURL
			}
			if c.Description == "" {
				c.Description = g.Description
			}
			c.Private = c.Private || g.Private
			c.Archived = c.Archived || g.Archived
			if !g.PushedAt.IsZero() && (c.PushedAt == nil || g.PushedAt.After(*c.PushedAt)) {
				t := g.PushedAt
				c.PushedAt = &t
			}
			if g.ContributedAt != nil && (c.ContributedAt == nil || g.ContributedAt.After(*c.ContributedAt)) {
				t := *g.ContributedAt
				c.ContributedAt = &t
			}
		}
	}
	merged := make([]contracts.CatalogRepo, 0, len(order))
	for _, k := range order {
		c := byKey[k]
		if c.Archived && len(c.Present) == 0 && c.LastUsedAt == nil {
			continue // archived and nowhere checked out: noise
		}
		c.Rank = catalogRank(c, now)
		merged = append(merged, *c)
	}
	return RankCatalog(merged, query)
}

// RankCatalog filters catalog items by query and orders them: clearly
// better matches first, then rank (used > contributed > local > other),
// then the rank's own recency.
func RankCatalog(items []contracts.CatalogRepo, query string) []contracts.CatalogRepo {
	out := make([]contracts.CatalogRepo, 0, len(items))
	for _, c := range items {
		s, ok := catalogScore(query, &c)
		if !ok {
			continue
		}
		c.Score = s
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if query != "" {
			if ba, bb := a.Score/150, b.Score/150; ba != bb {
				return ba > bb
			}
		}
		if a.Rank != b.Rank {
			return a.Rank > b.Rank
		}
		if ta, tb := rankTime(a), rankTime(b); !ta.Equal(tb) {
			return ta.After(tb)
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out
}

// contributedWindow: older contributions do not lift a repository.
const contributedWindow = 180 * 24 * time.Hour

func catalogRank(c *contracts.CatalogRepo, now time.Time) int {
	switch {
	case c.LastUsedAt != nil:
		return contracts.CatalogRankUsed
	case c.ContributedAt != nil && now.Sub(*c.ContributedAt) < contributedWindow:
		return contracts.CatalogRankContributed
	case len(c.Present) > 0:
		return contracts.CatalogRankLocal
	}
	return contracts.CatalogRankOther
}

// rankTime is the signal that orders a repository within its rank.
func rankTime(c contracts.CatalogRepo) time.Time {
	switch c.Rank {
	case contracts.CatalogRankUsed:
		return *c.LastUsedAt
	case contracts.CatalogRankContributed:
		return *c.ContributedAt
	case contracts.CatalogRankLocal:
		return c.LastActivity
	}
	if c.PushedAt != nil {
		return *c.PushedAt
	}
	return time.Time{}
}

// CatalogScore rates how well query matches a catalog repository (ok is
// false when it does not match).
func CatalogScore(query string, c contracts.CatalogRepo) (int, bool) { return catalogScore(query, &c) }

// catalogScore matches query against the name, owner/name, remote and the
// checkout paths.
func catalogScore(query string, c *contracts.CatalogRepo) (int, bool) {
	if strings.TrimSpace(query) == "" {
		return 0, true
	}
	r := contracts.Repo{Name: c.Name, RemoteURL: c.RemoteURL}
	if len(c.Present) > 0 {
		r.Path = c.Present[0].Path
	}
	best, ok := Score(query, r)
	if c.FullName != "" && strings.EqualFold(strings.TrimSpace(query), c.FullName) {
		return max(best, 1100), true
	}
	for _, e := range c.Present[min(1, len(c.Present)):] {
		if s, ok2 := Score(query, e.Repo); ok2 && s > best {
			best, ok = s, true
		}
	}
	return best, ok
}

// FindCatalog resolves a spec ("owner/name", a clone URL or a name)
// against a catalog: an exact owner/name or remote first, else the best
// unique name match. ok is false when nothing (or several) match.
func FindCatalog(spec string, items []contracts.CatalogRepo) (contracts.CatalogRepo, []contracts.CatalogRepo, bool) {
	spec = strings.TrimSpace(spec)
	norm := NormalizeRemote(spec)
	for _, c := range items {
		if (c.FullName != "" && strings.EqualFold(c.FullName, spec)) || (strings.Contains(spec, ":") || strings.Contains(spec, "://")) && NormalizeRemote(c.RemoteURL) == norm {
			return c, nil, true
		}
	}
	var exact []contracts.CatalogRepo
	for _, c := range items {
		if strings.EqualFold(c.Name, spec) {
			exact = append(exact, c)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil, true
	}
	if len(exact) > 1 {
		return contracts.CatalogRepo{}, exact, false
	}
	return contracts.CatalogRepo{}, nil, false
}

// IsGitHubSpec reports whether s looks like GitHub "owner/name" (not a
// path, URL or ssh remote).
func IsGitHubSpec(s string) bool {
	if IsPathSpec(s) || strings.ContainsAny(s, ":@ \t") {
		return false
	}
	o, n, ok := strings.Cut(s, "/")
	return ok && o != "" && n != "" && !strings.Contains(n, "/")
}
