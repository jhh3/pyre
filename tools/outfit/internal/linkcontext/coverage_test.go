package linkcontext

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/builtin"
	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
)

func TestEveryBuiltinLinkPatternMatchesCanonicalAndTrackedURLs(t *testing.T) {
	packages, err := builtin.All()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, loaded := range packages {
		for _, resolver := range loaded.Package.Links {
			for patternIndex, pattern := range resolver.Patterns {
				for _, lookup := range resolver.LookupOptions() {
					environment := ""
					if len(pattern.Environments) > 0 {
						environment = pattern.Environments[0]
					}
					raw := representativeURL(pattern)
					if _, ok := matchResolverForEnvironment(raw, resolver, lookup, environment); !ok {
						t.Errorf("%s/%s pattern %d mode %s did not match %s", loaded.Package.Key, resolver.ID, patternIndex, lookup.Mode, raw)
					}
					tracked := raw
					separator := "?"
					if strings.Contains(raw, "?") {
						separator = "&"
					}
					tracked += separator + "utm_source=outfit-eval#detail"
					if _, ok := matchResolverForEnvironment(tracked, resolver, lookup, environment); !ok {
						t.Errorf("%s/%s rejected harmless tracking query or fragment: %s", loaded.Package.Key, resolver.ID, tracked)
					}
					count++
				}
			}
		}
	}
	if count < 60 {
		t.Fatalf("coverage unexpectedly small: %d pattern/mode combinations", count)
	}
}

func TestEveryBuiltinLinkPatternRejectsHostSuffixAndHTTP(t *testing.T) {
	packages, err := builtin.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, loaded := range packages {
		for _, resolver := range loaded.Package.Links {
			for _, pattern := range resolver.Patterns {
				if len(pattern.Hosts) == 0 {
					continue
				}
				lookup := resolver.LookupOptions()[0]
				environment := ""
				if len(pattern.Environments) > 0 {
					environment = pattern.Environments[0]
				}
				canonical := representativeURL(pattern)
				if _, ok := matchResolverForEnvironment(strings.Replace(canonical, "https://", "http://", 1), resolver, lookup, environment); ok {
					t.Errorf("%s/%s accepted HTTP", loaded.Package.Key, resolver.ID)
				}
				parsed, _ := url.Parse(canonical)
				parsed.Host = parsed.Host + ".attacker.test"
				if _, ok := matchResolverForEnvironment(parsed.String(), resolver, lookup, environment); ok {
					t.Errorf("%s/%s accepted host suffix attack", loaded.Package.Key, resolver.ID)
				}
			}
		}
	}
}

func TestStripePaymentURLUsesTypedIDPrefix(t *testing.T) {
	packages, _ := builtin.All()
	var stripe kit.Package
	for _, loaded := range packages {
		if loaded.Package.Key == "stripe" {
			stripe = loaded.Package
		}
	}
	matched := func(raw string) []string {
		var ids []string
		for _, resolver := range stripe.Links {
			for _, lookup := range resolver.LookupOptions() {
				if lookup.Mode != "rest" {
					continue
				}
				if _, ok := matchResolverForEnvironment(raw, resolver, lookup, "live"); ok {
					ids = append(ids, resolver.ID)
				}
			}
		}
		sort.Strings(ids)
		return ids
	}
	if got := matched("https://dashboard.stripe.com/payments/pi_123"); strings.Join(got, ",") != "payment_intent" {
		t.Fatalf("payment intent routes = %v", got)
	}
	if got := matched("https://dashboard.stripe.com/payments/ch_123"); strings.Join(got, ",") != "charge" {
		t.Fatalf("charge routes = %v", got)
	}
}

func TestMultipleAccountsRequireExplicitConnection(t *testing.T) {
	store := linkTestStore(t)
	pkg := linkTestPackage()
	if err := store.InstallPackage(context.Background(), pkg, "builtin"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"acme", "personal"} {
		if err := store.CreateConnection(context.Background(), state.Connection{ID: name, Name: name, ConnectorKey: "links", SelectedMode: "rest", CredentialMethod: "no-auth", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	service := Service{State: store, Connections: connection.Service{State: store}}
	resolution := service.ResolveDetailed(context.Background(), "https://links.example.test/items/123", ResolveOptions{})
	if len(resolution.Resources) != 0 || len(resolution.Diagnostics) != 1 || resolution.Diagnostics[0].Code != "link_ambiguous" || strings.Join(resolution.Diagnostics[0].Connections, ",") != "acme,personal" {
		t.Fatalf("ambiguous result = %#v", resolution)
	}
	_, diagnostics := service.candidates(context.Background(), []string{"https://links.example.test/items/123"}, ResolveOptions{Connection: "personal"})
	if len(diagnostics) != 0 {
		t.Fatalf("explicit connection remained ambiguous: %#v", diagnostics)
	}
}

func TestLinkFailureDiagnosticsDistinguishRecoveryStates(t *testing.T) {
	store := linkTestStore(t)
	pkg := linkTestPackage()
	if err := store.InstallPackage(context.Background(), pkg, "builtin"); err != nil {
		t.Fatal(err)
	}
	service := Service{State: store, Connections: connection.Service{State: store}}
	assertDiagnostic(t, service, "https://links.example.test/items/123", "link_connection_required")
	assertDiagnostic(t, service, "https://unknown.example.test/items/123", "link_unsupported")
	if err := store.CreateConnection(context.Background(), state.Connection{ID: "broken", Name: "broken", ConnectorKey: "links", SelectedMode: "rest", CredentialMethod: "no-auth", Status: "broken"}); err != nil {
		t.Fatal(err)
	}
	assertDiagnostic(t, service, "https://links.example.test/items/123", "link_connection_unhealthy")
}

func TestUnknownShortRedirectIsNotFollowed(t *testing.T) {
	service := Service{State: linkTestStore(t)}
	resolution := service.ResolveDetailed(context.Background(), "https://bit.ly/provider-resource", ResolveOptions{})
	if len(resolution.Resources) != 0 || len(resolution.Diagnostics) != 1 || resolution.Diagnostics[0].Code != "link_unsupported" {
		t.Fatalf("short redirect result = %#v", resolution)
	}
}

func TestSelectedModeAndPolicyFailuresHaveExactRecovery(t *testing.T) {
	store := linkTestStore(t)
	loaded := linkTestPackage()
	loaded.Package.Modes = append(loaded.Package.Modes, kit.Mode{
		ID: "other", Label: "Other", Description: "No link lookup",
		Recommendation:          kit.Recommendation{Rank: 2, Reason: "Alternative"},
		Executor:                kit.Executor{Kind: "http", BaseURL: "https://other.example.test/"},
		CredentialMethods:       []string{"no-auth"},
		DefaultCredentialMethod: "no-auth",
		Operations: []kit.Operation{{
			Name: "other_read", Description: "Read another resource.", Class: "read",
			HTTP: &kit.HTTPOperation{Method: "GET", Path: "items"},
		}},
	})
	raw, _ := json.Marshal(loaded.Package)
	loaded.Raw = raw
	if err := store.InstallPackage(context.Background(), loaded, "builtin"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(context.Background(), state.Connection{ID: "other", Name: "other", ConnectorKey: "links", SelectedMode: "other", CredentialMethod: "no-auth", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	assertDiagnostic(t, Service{State: store, Connections: connection.Service{State: store}}, "https://links.example.test/items/123", "link_mode_unsupported")
	assertDiagnostic(t, Service{State: store, Connections: connection.Service{State: store, AllowedLinks: func(string) bool { return false }}}, "https://links.example.test/items/123", "link_policy_blocked")
}

func assertDiagnostic(t *testing.T, service Service, raw, code string) {
	t.Helper()
	resolution := service.ResolveDetailed(context.Background(), raw, ResolveOptions{})
	if len(resolution.Diagnostics) != 1 || resolution.Diagnostics[0].Code != code || resolution.Diagnostics[0].Hint == "" {
		t.Fatalf("%s diagnostic = %#v", raw, resolution.Diagnostics)
	}
}

func linkTestStore(t *testing.T) *state.Store {
	t.Helper()
	root := t.TempDir()
	store, err := state.Open(context.Background(), state.Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func linkTestPackage() kit.Loaded {
	pkg := kit.Package{
		APIVersion: kit.APIVersion,
		Key:        "links",
		Display:    "Links",
		Version:    "1",
		CredentialMethods: []kit.CredentialMethod{{
			ID: "no-auth", Label: "None", Description: "Public", Custody: "none",
		}},
		Modes: []kit.Mode{{
			ID: "rest", Label: "REST", Description: "REST",
			Recommendation:          kit.Recommendation{Rank: 1, Reason: "Direct"},
			Executor:                kit.Executor{Kind: "http", BaseURL: "https://links.example.test/"},
			CredentialMethods:       []string{"no-auth"},
			DefaultCredentialMethod: "no-auth",
			Operations: []kit.Operation{{
				Name: "get_item", Title: "Get item", Description: "Get one item.", Class: "read",
				HTTP: &kit.HTTPOperation{Method: "GET", Path: "items/{{arg.id}}"},
			}},
		}},
		Links: []kit.LinkResolver{{
			ID: "item", ResourceType: "links.item",
			Patterns: []kit.LinkPattern{{Hosts: []string{"links.example.test"}, Path: "/items/:id", Segments: map[string]kit.SegmentRule{"id": {Kind: "integer"}}}},
			Lookup:   kit.LinkLookup{Mode: "rest", Operation: "get_item", Arguments: map[string]string{"id": "id"}},
		}},
	}
	raw, _ := json.Marshal(pkg)
	return kit.Loaded{Package: pkg, Digest: "links-digest", Source: "test", Raw: raw}
}

func representativeURL(pattern kit.LinkPattern) string {
	host := strings.TrimPrefix(pattern.Hosts[0], "*.")
	if strings.HasPrefix(pattern.Hosts[0], "*.") {
		host = "workspace." + host
	}
	path := pattern.Path
	for name, rule := range pattern.Segments {
		value := representativeSegment(rule)
		if rule.Kind != "path" {
			value = url.PathEscape(value)
		}
		path = strings.ReplaceAll(path, ":"+name, value)
	}
	query := url.Values{}
	for name, rule := range pattern.Query {
		query.Set(name, representativeSegment(rule))
	}
	raw := "https://" + host + path
	if encoded := query.Encode(); encoded != "" {
		raw += "?" + encoded
	}
	return raw
}

func representativeSegment(rule kit.SegmentRule) string {
	if len(rule.OneOf) > 0 {
		return rule.OneOf[0]
	}
	value := map[string]string{
		"slug":                "example-slug",
		"integer":             "123",
		"sha":                 "abcdef1",
		"linear-identifier":   "ENG-123",
		"slack-channel":       "C012ABC",
		"slack-ts":            "p1712345678123456",
		"opaque-token":        "token_123",
		"notion-page":         "a1b2c3d4e5f67890a1b2c3d4e5f67890",
		"google-calendar-eid": "ZXZlbnRfMTIzIGpvaG5AZXhhbXBsZS5jb20",
		"path":                "folder/document",
	}[rule.Kind]
	if value == "" {
		value = "value"
	}
	if rule.Prefix != "" {
		value = rule.Prefix + "123"
	}
	return value
}
