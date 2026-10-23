package refs

import (
	"context"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

type fakeAPI struct {
	c.API
	accounts []c.AccountView
	projects []c.Project
}

func (f *fakeAPI) ListAccounts(_ context.Context, r c.ListAccountsRequest) (c.Page[c.AccountView], error) {
	// Two pages: resolution reads them all.
	if r.Cursor == "" {
		return c.Page[c.AccountView]{Items: f.accounts[:1], NextCursor: "p2"}, nil
	}
	return c.Page[c.AccountView]{Items: f.accounts[1:]}, nil
}

func (f *fakeAPI) ListProjects(context.Context, c.ListProjectsRequest) (c.Page[c.Project], error) {
	return c.Page[c.Project]{Items: f.projects}, nil
}

func (f *fakeAPI) GetProject(_ context.Context, r c.GetRequest) (c.Project, error) {
	for _, p := range f.projects {
		if p.ProjectID == r.ID {
			return p, nil
		}
	}
	return c.Project{}, NotFound(c.KindProject, r.ID)
}

func TestAccountAndProject(t *testing.T) {
	tok := []c.DeviceGrant{{Kind: c.CredOAuthToken}}
	api := &fakeAPI{
		accounts: []c.AccountView{
			{Account: c.Account{AccountID: "acct_max", Label: "work-max", RotaRef: "work-max"}},
			{Account: c.Account{AccountID: "acct_login", Label: "Personal", RotaRef: "personal"}},
			{Account: c.Account{AccountID: "acct_tok", Label: "Personal"}, Grants: tok},
			{Account: c.Account{AccountID: "acct_a", Label: "team-a"}},
			{Account: c.Account{AccountID: "acct_b", Label: "team-b"}},
		},
		projects: []c.Project{{ProjectID: "prj_s", Name: "sentry"}, {ProjectID: "prj_S", Name: "Sentry"}, {ProjectID: "prj_d1", Name: "dup"}, {ProjectID: "prj_d2", Name: "Dup"}},
	}
	ctx := context.Background()
	for ref, want := range map[string]string{"work": "acct_max", "WORK-MAX": "acct_max", "personal": "acct_tok", "acct_x": "acct_x", "": ""} {
		if got, err := AccountID(ctx, api, ref, c.ProviderAnthropic); err != nil || got != want {
			t.Errorf("account %q = %q, %v (want %q)", ref, got, err, want)
		}
	}
	if _, err := AccountID(ctx, api, "team", c.ProviderAnthropic); !c.IsCode(err, c.ErrConflict) {
		t.Errorf("ambiguous prefix: %v", err)
	}
	if _, err := AccountID(ctx, api, "nobody", c.ProviderAnthropic); !c.IsCode(err, c.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	for ref, want := range map[string]string{"sentry": "prj_s", "Sentry": "prj_S", "dup": "prj_d1"} {
		if p, err := Project(ctx, api, ref); err != nil || p.ProjectID != want {
			t.Errorf("project %q = %+v, %v", ref, p, err)
		}
	}
	if _, err := Project(ctx, api, "DUP"); !c.IsCode(err, c.ErrConflict) {
		t.Errorf("ambiguous project: %v", err)
	}
	if p, err := ProjectPtr(ctx, api, "none"); err != nil || p == nil || *p != "" {
		t.Errorf("none = %v %v", p, err)
	}
	if p, err := ProjectPtr(ctx, api, ""); err != nil || p != nil {
		t.Errorf("empty = %v %v", p, err)
	}
}
