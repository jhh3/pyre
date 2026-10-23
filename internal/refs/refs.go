// Package refs resolves the names a user types (--account, --project) to
// IDs through the hub's read API. The CLI and pyred (pyre run without a hub
// login) share it, so the same reference resolves the same way on both.
package refs

import (
	"context"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// maxPages bounds a listing.
const maxPages = 100

func listAll[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) (c.Page[T], error)) ([]T, error) {
	var out []T
	cur := ""
	for range maxPages {
		page, err := fetch(ctx, cur)
		if err != nil {
			return out, err
		}
		out = append(out, page.Items...)
		if cur = page.NextCursor; cur == "" {
			break
		}
	}
	return out, nil
}

// NotFound is the error for a reference that matches nothing.
func NotFound(kind c.ObjectKind, ref string) error {
	return c.Errorf(c.ErrNotFound, "no %s matches %q", kind, ref)
}

// Ambiguous is the error for a reference that matches several objects.
func Ambiguous(kind c.ObjectKind, ref string, ids []string) error {
	return c.Errorf(c.ErrConflict, "%q matches several %ss (%s); use an ID", ref, kind, strings.Join(ids, ", "))
}

// Account resolves an account ID, label or rota name of provider (exact,
// case-insensitive, else a unique prefix). A token account wins over
// another login of the same name (accounts_token.go); the other is still
// reachable by ID or rota name.
func Account(ctx context.Context, api c.API, ref string, provider c.Provider) (c.Account, error) {
	ref = strings.TrimSpace(ref)
	items, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return api.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}, Provider: provider})
	})
	if err != nil {
		return c.Account{}, err
	}
	var hits, prefix []c.Account
	lower := strings.ToLower(ref)
	tokenKind := map[string]bool{}
	for _, av := range items {
		a := av.Account
		for _, g := range av.Grants {
			tokenKind[a.AccountID] = tokenKind[a.AccountID] || g.Kind == c.CredOAuthToken
		}
		if a.AccountID == ref {
			return a, nil
		}
		switch {
		case a.Label == ref || a.RotaRef == ref || strings.EqualFold(a.Label, ref) || strings.EqualFold(a.RotaRef, ref):
			hits = append(hits, a)
		case strings.HasPrefix(strings.ToLower(a.Label), lower) || strings.HasPrefix(strings.ToLower(a.RotaRef), lower):
			prefix = append(prefix, a)
		}
	}
	if len(hits) == 0 {
		hits = prefix
	}
	if len(hits) > 1 {
		var tok []c.Account
		for _, a := range hits {
			if tokenKind[a.AccountID] {
				tok = append(tok, a)
			}
		}
		if len(tok) == 1 {
			hits = tok
		}
	}
	switch len(hits) {
	case 0:
		return c.Account{}, NotFound(c.KindAccount, ref)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, a := range hits {
		ids[i] = a.AccountID
	}
	return c.Account{}, Ambiguous(c.KindAccount, ref, ids)
}

// AccountID turns an --account reference into an account ID ("" stays
// ""; a full account ID is passed through without a lookup).
func AccountID(ctx context.Context, api c.API, ref string, provider c.Provider) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, c.PrefixAccount+"_") {
		return ref, nil
	}
	a, err := Account(ctx, api, ref, provider)
	if err != nil {
		return "", err
	}
	return a.AccountID, nil
}

// Project resolves a project ID or name (exact, else case-insensitive).
func Project(ctx context.Context, api c.API, ref string) (c.Project, error) {
	ref = strings.TrimSpace(ref)
	if c.ValidID(ref, c.PrefixProject) {
		return api.GetProject(ctx, c.GetRequest{ID: ref})
	}
	items, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Project], error) {
		return api.ListProjects(ctx, c.ListProjectsRequest{PageRequest: c.PageRequest{Cursor: cur}, IncludeArchived: true})
	})
	if err != nil {
		return c.Project{}, err
	}
	var hits []c.Project
	for _, eq := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		for _, p := range items {
			if eq(p.Name, ref) {
				hits = append(hits, p)
			}
		}
		if len(hits) > 0 {
			break
		}
	}
	switch len(hits) {
	case 0:
		return c.Project{}, NotFound(c.KindProject, ref)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, p := range hits {
		ids[i] = p.ProjectID
	}
	return c.Project{}, Ambiguous(c.KindProject, ref, ids)
}

// ProjectPtr resolves an optional project reference: "" is none given
// (nil), "none" is explicitly no project (""), an ID passes through.
func ProjectPtr(ctx context.Context, api c.API, ref string) (*string, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return nil, nil
	case ref == "none":
		empty := ""
		return &empty, nil
	case c.ValidID(ref, c.PrefixProject):
		return &ref, nil
	}
	p, err := Project(ctx, api, ref)
	if err != nil {
		return nil, err
	}
	return &p.ProjectID, nil
}
