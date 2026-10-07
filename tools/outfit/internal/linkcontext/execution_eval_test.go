package linkcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/outfit/internal/builtin"
	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/invoke"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/state"
)

func TestMain(m *testing.M) {
	if os.Getenv("OUTFIT_GWS_EXECUTION_FIXTURE") == "1" {
		joined := strings.Join(os.Args, " ")
		payload := map[string]string{
			" drive files get ":                `{"id":"drive-file-123","name":"Customer rollout plan","description":"Launch and migration notes","owners":[{"displayName":"Ava"}]}`,
			" docs documents get ":             `{"documentId":"doc-123","title":"Incident response playbook","body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"Escalate within ten minutes"}}]}}]}}`,
			" sheets spreadsheets values get ": `{"range":"A1:Z200","values":[["Owner","Status"],["Ava","Ready for launch"]]}`,
			" sheets spreadsheets get ":        `{"spreadsheetId":"sheet-123","properties":{"title":"Launch readiness tracker"},"sheets":[{"properties":{"title":"Milestones"}}]}`,
			" slides presentations get ":       `{"presentationId":"slides-123","title":"Board launch review","slides":[{"objectId":"slide-1","pageElements":[{"shape":{"text":{"textElements":[{"textRun":{"content":"Revenue readiness"}}]}}}]}]}`,
			" calendar events get ":            `{"id":"event_123","htmlLink":"https://calendar.google.com/calendar/event?eid=fixture","summary":"Launch go-no-go","description":"Review production readiness","status":"confirmed","creator":{"email":"ava@example.com"}}`,
		}
		for signature, raw := range payload {
			if strings.Contains(" "+joined+" ", signature) {
				_, _ = os.Stdout.WriteString(raw)
				os.Exit(0)
			}
		}
		fmt.Fprintln(os.Stderr, "unknown GWS execution fixture:", joined)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

type executionCase struct {
	name, resolver, raw, connection, want string
}

var httpExecutionCases = []executionCase{
	{"slack-message", "slack/message", "https://app.slack.com/archives/C012ABC/p1712345678123456", "slack-work", "First investigation update"},
	{"notion-page", "notion/page", "https://www.notion.so/a1b2c3d4e5f67890a1b2c3d4e5f67890", "notion-work", "Nested rollback and verification steps"},
	{"sentry-issue", "sentry/issue", "https://sentry.io/organizations/acme/issues/42", "sentry-work", "web@abc123"},
	{"github-pull", "github/pull_request", "https://github.com/acme/widget/pull/42", "github-work", "24 simultaneous first-run processes"},
	{"github-issue", "github/issue", "https://github.com/acme/widget/issues/73", "github-work", "Reconnect without losing completed work"},
	{"github-commit", "github/commit", "https://github.com/acme/widget/commit/abcdef1234567", "github-work", "Fence candidate activation"},
	{"github-action", "github/action_run", "https://github.com/acme/widget/actions/runs/9001", "github-work", "Linux integration shard"},
	{"github-release", "github/release", "https://github.com/acme/widget/releases/tag/v0.10.7", "github-work", "Agent evaluation release"},
	{"linear-issue", "linear/issue", "https://linear.app/acme/issue/ENG-123/make-connector-evals-resumable", "linear-work", "Preserve completed trials after interruption"},
	{"intercom-conversation", "intercom/conversation", "https://app.intercom.com/a/inbox/workspace-1/inbox/inbox-1/conversation/cnv_123", "intercom-work", "Please keep my entered values"},
	{"stripe-customer", "stripe/customer", "https://dashboard.stripe.com/test/customers/cus_123", "stripe-work", "Ada Lovelace"},
	{"stripe-payment", "stripe/payment_intent", "https://dashboard.stripe.com/test/payments/pi_123", "stripe-work", "insufficient funds"},
	{"stripe-subscription", "stripe/subscription", "https://dashboard.stripe.com/test/subscriptions/sub_123", "stripe-work", "Enterprise annual plan"},
	{"stripe-invoice", "stripe/invoice", "https://dashboard.stripe.com/test/invoices/in_123", "stripe-work", "INV-2026-0042"},
	{"stripe-charge", "stripe/charge", "https://dashboard.stripe.com/test/payments/ch_123", "stripe-work", "duplicate transaction"},
	{"stripe-event", "stripe/event", "https://dashboard.stripe.com/test/events/evt_123", "stripe-work", "invoice.payment_failed"},
	{"stripe-webhook", "stripe/webhook_endpoint", "https://dashboard.stripe.com/test/workbench/webhooks/we_123", "stripe-work", "https://hooks.example.com/stripe"},
}

var gwsExecutionCases = []executionCase{
	{"gws-drive", "google-workspace/drive_file", "https://drive.google.com/file/d/drive-file-123/view", "workspace", "Launch and migration notes"},
	{"gws-docs", "google-workspace/docs_document", "https://docs.google.com/document/d/doc-123/edit", "workspace", "Escalate within ten minutes"},
	{"gws-sheets", "google-workspace/sheets_spreadsheet", "https://docs.google.com/spreadsheets/d/sheet-123/edit", "workspace", "Ready for launch"},
	{"gws-slides", "google-workspace/slides_presentation", "https://docs.google.com/presentation/d/slides-123/edit", "workspace", "Revenue readiness"},
	{"gws-calendar", "google-workspace/calendar_event", "https://calendar.google.com/calendar/event?eid=ZXZlbnRfMTIzIGF2YUBleGFtcGxlLmNvbQ", "workspace", "Review production readiness"},
}

type evalVault struct{ values map[string]map[string]string }

func (vault *evalVault) Put(_ context.Context, value map[string]string) (string, error) {
	ref := fmt.Sprintf("ref-%d", len(vault.values)+1)
	vault.values[ref] = value
	return ref, nil
}
func (vault *evalVault) Get(_ context.Context, ref string) (map[string]string, error) {
	value, ok := vault.values[ref]
	if !ok {
		return nil, fmt.Errorf("missing fixture secret %s", ref)
	}
	return value, nil
}
func (vault *evalVault) Delete(_ context.Context, ref string) error {
	delete(vault.values, ref)
	return nil
}

func TestBlessedExecutionEvalReadsProviderShapedResources(t *testing.T) {
	server := executionFixtureServer(t)
	store := linkTestStore(t)
	vault := &evalVault{values: map[string]map[string]string{}}
	executor := runtime.Executor{HTTPClient: server.Client()}
	connections := connection.Service{State: store, Vault: vault, Executor: executor}
	service := Service{State: store, Connections: connections}

	installEvalConnection(t, store, vault, "slack", "rest", "managed-token", "slack-work", server.URL+"/api/", map[string]string{"TOKEN": "slack-token"}, nil)
	installEvalConnection(t, store, vault, "notion", "rest", "managed-token", "notion-work", server.URL+"/v1/", map[string]string{"TOKEN": "notion-token"}, nil)
	installEvalConnection(t, store, vault, "sentry", "rest", "managed-token", "sentry-work", server.URL+"/api/0/", map[string]string{"TOKEN": "sentry-token"}, nil)
	installEvalConnection(t, store, vault, "github", "rest", "managed-token", "github-work", server.URL+"/", map[string]string{"TOKEN": "github-token"}, nil)
	installEvalConnection(t, store, vault, "linear", "graphql", "managed-key", "linear-work", server.URL+"/", map[string]string{"TOKEN": "linear-token"}, nil)
	installEvalConnection(t, store, vault, "intercom", "rest", "managed-token", "intercom-work", server.URL+"/", map[string]string{"TOKEN": "intercom-token"}, nil)
	installEvalConnection(t, store, vault, "stripe", "rest", "managed-restricted-key", "stripe-work", server.URL+"/v1/", map[string]string{"STRIPE_API_KEY": "rk_test_fixture"}, map[string]string{"environment": "test"})

	for _, test := range httpExecutionCases {
		t.Run(test.name, func(t *testing.T) {
			resolution := service.ResolveDetailed(context.Background(), test.raw, ResolveOptions{Mode: ModeHeavy, Connection: test.connection})
			if len(resolution.Diagnostics) != 0 || len(resolution.Resources) != 1 {
				t.Fatalf("resolution = %#v", resolution)
			}
			resource := resolution.Resources[0]
			if resource.Connection != test.connection || resource.Mode != ModeHeavy || !strings.Contains(resource.Content, test.want) {
				t.Fatalf("resource = %#v", resource)
			}
			rendered := Render(resolution.Resources)
			if !strings.Contains(rendered, `connection="`+test.connection+`"`) || !strings.Contains(rendered, test.want) || !strings.Contains(rendered, `trust="untrusted-external"`) {
				t.Fatalf("rendered context = %s", rendered)
			}
		})
	}
}

func TestBlessedExecutionEvalPaginatesSlackWithoutLosingPages(t *testing.T) {
	server := executionFixtureServer(t)
	store := linkTestStore(t)
	vault := &evalVault{values: map[string]map[string]string{}}
	connections := connection.Service{State: store, Vault: vault, Executor: runtime.Executor{HTTPClient: server.Client()}}
	pkg, mode := installEvalConnection(t, store, vault, "slack", "rest", "managed-token", "slack-work", server.URL+"/api/", map[string]string{"TOKEN": "slack-token"}, nil)
	operation, _ := runtime.FindOperation(mode, "thread")
	envelope, err := invoke.Run(context.Background(), invoke.Metadata{Connection: "slack-work", Connector: pkg.Key, Mode: mode.ID}, operation, map[string]string{"channel": "C012ABC", "ts": "1712345678.123456"}, invoke.Options{All: true, MaxPages: 5, MaxItems: 100}, func(ctx context.Context, arguments map[string]string) (runtime.Result, error) {
		return connections.Run(ctx, "slack-work", "thread", arguments, false)
	})
	if err != nil || !envelope.OK || envelope.Meta.Pages != 2 || envelope.Meta.Items != 3 || envelope.Meta.Truncated {
		t.Fatalf("pagination envelope = %#v err=%v", envelope, err)
	}
	pages, ok := envelope.Data.([]any)
	if !ok || len(pages) != 2 || !strings.Contains(fmt.Sprintf("%v", pages[1]), "Resolved after the database failover") {
		t.Fatalf("pagination data = %#v", envelope.Data)
	}
}

func TestBlessedExecutionEvalReadsDriveThroughPinnedCLI(t *testing.T) {
	t.Setenv("OUTFIT_GWS_EXECUTION_FIXTURE", "1")
	store := linkTestStore(t)
	vault := &evalVault{values: map[string]map[string]string{}}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	connections := connection.Service{State: store, Vault: vault, Executor: runtime.Executor{}, ResolveTool: func(kit.ToolRequirement) (string, error) { return executable, nil }}
	installEvalConnection(t, store, vault, "google-workspace", "cli", "native-login", "workspace", "", nil, map[string]string{"capabilities": "drive,docs,sheets,slides,calendar"})
	service := Service{State: store, Connections: connections}
	for _, test := range gwsExecutionCases {
		t.Run(test.name, func(t *testing.T) {
			resolution := service.ResolveDetailed(context.Background(), test.raw, ResolveOptions{Mode: ModeHeavy, Connection: test.connection})
			if len(resolution.Diagnostics) != 0 || len(resolution.Resources) != 1 || !strings.Contains(resolution.Resources[0].Content, test.want) {
				t.Fatalf("GWS execution resolution = %#v", resolution)
			}
		})
	}
}

func TestBlessedExecutionEvalCoversEveryDeclaredResolver(t *testing.T) {
	covered := map[string]bool{}
	for _, test := range append(append([]executionCase{}, httpExecutionCases...), gwsExecutionCases...) {
		if covered[test.resolver] {
			t.Fatalf("duplicate execution case for %s", test.resolver)
		}
		covered[test.resolver] = true
	}
	packages, err := builtin.All()
	if err != nil {
		t.Fatal(err)
	}
	var missing, unknown []string
	declared := map[string]bool{}
	for _, loaded := range packages {
		for _, resolver := range loaded.Package.Links {
			key := loaded.Package.Key + "/" + resolver.ID
			declared[key] = true
			if !covered[key] {
				missing = append(missing, key)
			}
		}
	}
	for key := range covered {
		if !declared[key] {
			unknown = append(unknown, key)
		}
	}
	if len(missing) > 0 || len(unknown) > 0 {
		t.Fatalf("execution fixture coverage missing=%v unknown=%v", missing, unknown)
	}
}

func TestFailureEvalExplainsExpiredOAuthRecovery(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`))
	}))
	defer tokenServer.Close()
	store := linkTestStore(t)
	vault := &evalVault{values: map[string]map[string]string{"oauth-ref": {
		"OAUTH_CLIENT_ID": "client", "OAUTH_ACCESS_TOKEN": "expired", "OAUTH_REFRESH_TOKEN": "revoked", "OAUTH_TOKEN_EXPIRY": time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
	}}}
	pkg := kit.Package{
		APIVersion: kit.APIVersion, Key: "oauth-links", Display: "OAuth links", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{
			ID: "oauth", Label: "OAuth", Description: "OAuth", Custody: "oauth",
			Fields: []kit.CredentialField{
				{Key: "OAUTH_CLIENT_ID", Label: "Client"},
				{Key: "OAUTH_ACCESS_TOKEN", Label: "Access", Secret: true},
				{Key: "OAUTH_REFRESH_TOKEN", Label: "Refresh", Secret: true},
				{Key: "OAUTH_TOKEN_EXPIRY", Label: "Expiry"},
			},
			OAuth:    &kit.OAuthConfig{AuthorizationURL: "https://auth.example.test/authorize", TokenURL: tokenServer.URL},
			Bindings: []kit.CredentialBinding{{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.OAUTH_ACCESS_TOKEN}}"}},
		}},
		Modes: []kit.Mode{{
			ID: "rest", Label: "REST", Description: "REST",
			Recommendation:          kit.Recommendation{Rank: 1, Reason: "Direct"},
			Executor:                kit.Executor{Kind: "http", BaseURL: "https://api.example.test/"},
			CredentialMethods:       []string{"oauth"},
			DefaultCredentialMethod: "oauth",
			Operations: []kit.Operation{{
				Name: "get", Description: "Read item.", Class: "read",
				HTTP: &kit.HTTPOperation{Method: "GET", Path: "items/{{arg.id}}"},
			}},
		}},
		Links: []kit.LinkResolver{{
			ID: "item", ResourceType: "oauth.item",
			Patterns: []kit.LinkPattern{{Hosts: []string{"app.example.test"}, Path: "/items/:id", Segments: map[string]kit.SegmentRule{"id": {Kind: "integer"}}}},
			Lookup:   kit.LinkLookup{Mode: "rest", Operation: "get", Arguments: map[string]string{"id": "id"}},
		}},
	}
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(context.Background(), kit.Loaded{Package: pkg, Raw: raw, Digest: "oauth-eval", Source: "test"}, "builtin"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(context.Background(), state.Connection{ID: "oauth", Name: "oauth", ConnectorKey: pkg.Key, SelectedMode: "rest", CredentialMethod: "oauth", Status: "active", CredentialRef: "oauth-ref"}); err != nil {
		t.Fatal(err)
	}
	service := Service{State: store, Connections: connection.Service{State: store, Vault: vault}}
	resolution := service.ResolveDetailed(context.Background(), "https://app.example.test/items/123", ResolveOptions{})
	if len(resolution.Diagnostics) != 1 || resolution.Diagnostics[0].Code != "link_credential_expired" || !strings.Contains(resolution.Diagnostics[0].Hint, "outfit credential replace oauth") {
		t.Fatalf("expired OAuth diagnostic = %#v", resolution)
	}
}

func executionFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", "execution", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		var fixture, inline string
		switch {
		case request.URL.Path == "/api/conversations.replies":
			if request.Header.Get("Authorization") != "Bearer slack-token" {
				http.Error(response, `{"ok":false,"error":"not_authed"}`, http.StatusUnauthorized)
				return
			}
			fixture = "slack-thread-page-1.json"
			if request.URL.Query().Get("cursor") == "cursor-2" {
				fixture = "slack-thread-page-2.json"
			}
		case strings.HasPrefix(request.URL.Path, "/v1/pages/"):
			fixture = "notion-page.json"
		case request.URL.Path == "/v1/blocks/a1b2c3d4-e5f6-7890-a1b2-c3d4e5f67890/children":
			fixture = "notion-blocks-root.json"
		case request.URL.Path == "/v1/blocks/child-block/children":
			fixture = "notion-blocks-child.json"
		case request.URL.Path == "/api/0/issues/42/":
			fixture = "sentry-issue.json"
		case request.URL.Path == "/api/0/issues/42/events/":
			fixture = "sentry-events.json"
		case request.URL.Path == "/repos/acme/widget/pulls/42":
			fixture = "github-pull-request.json"
		case request.URL.Path == "/repos/acme/widget/pulls/42/files":
			fixture = "github-pull-request-files.json"
		case request.URL.Path == "/repos/acme/widget/issues/42/comments":
			fixture = "github-issue-comments.json"
		case request.URL.Path == "/repos/acme/widget/pulls/42/comments":
			fixture = "github-review-comments.json"
		case request.URL.Path == "/repos/acme/widget/issues/73":
			inline = `{"html_url":"https://github.com/acme/widget/issues/73","title":"Preserve eval checkpoints","body":"Reconnect without losing completed work","state":"open","user":{"login":"ava"}}`
		case request.URL.Path == "/repos/acme/widget/issues/73/comments":
			inline = `[{"body":"The checkpoint must remain atomic."}]`
		case request.URL.Path == "/repos/acme/widget/commits/abcdef1234567":
			inline = `{"html_url":"https://github.com/acme/widget/commit/abcdef1234567","commit":{"message":"Fence candidate activation","author":{"name":"Ava","date":"2026-08-25T10:00:00Z"}}}`
		case request.URL.Path == "/repos/acme/widget/actions/runs/9001":
			inline = `{"html_url":"https://github.com/acme/widget/actions/runs/9001","name":"Agent eval","display_title":"Every-mode routing","conclusion":"failure","actor":{"login":"ci"}}`
		case request.URL.Path == "/repos/acme/widget/actions/runs/9001/jobs":
			inline = `{"jobs":[{"name":"Linux integration shard","conclusion":"failure","steps":[{"name":"resource fixtures","conclusion":"failure"}]}]}`
		case request.URL.Path == "/repos/acme/widget/releases/tags/v0.10.7":
			inline = `{"html_url":"https://github.com/acme/widget/releases/tag/v0.10.7","name":"Outfit v0.10.7","body":"Agent evaluation release","draft":false,"author":{"login":"ava"}}`
		case request.URL.Path == "/graphql":
			if request.Method != http.MethodPost {
				http.Error(response, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Variables["id"] != "ENG-123" {
				http.Error(response, `{"errors":[{"message":"wrong issue id"}]}`, http.StatusBadRequest)
				return
			}
			fixture = "linear-issue.json"
		case request.URL.Path == "/conversations/cnv_123":
			fixture = "intercom-conversation.json"
		case request.URL.Path == "/v1/payment_intents/pi_123":
			fixture = "stripe-payment-intent.json"
		case request.URL.Path == "/v1/customers/cus_123":
			inline = `{"id":"cus_123","name":"Ada Lovelace","email":"ada@example.com","description":"Design partner"}`
		case request.URL.Path == "/v1/subscriptions/sub_123":
			inline = `{"id":"sub_123","description":"Enterprise annual plan","status":"active","customer":"cus_123","items":{"data":[{"price":{"nickname":"Enterprise"}}]}}`
		case request.URL.Path == "/v1/invoices/in_123":
			inline = `{"id":"in_123","number":"INV-2026-0042","description":"Annual renewal","status":"open","customer_email":"ada@example.com"}`
		case request.URL.Path == "/v1/charges/ch_123":
			inline = `{"id":"ch_123","description":"Order 42","failure_message":"duplicate transaction","status":"failed","customer":"cus_123"}`
		case request.URL.Path == "/v1/events/evt_123":
			inline = `{"id":"evt_123","type":"invoice.payment_failed","pending_webhooks":2,"data":{"object":{"id":"in_123"}}}`
		case request.URL.Path == "/v1/webhook_endpoints/we_123":
			inline = `{"id":"we_123","url":"https://hooks.example.com/stripe","enabled_events":["invoice.payment_failed"],"status":"enabled"}`
		default:
			http.Error(response, `{"error":"fixture_not_found"}`, http.StatusNotFound)
			return
		}
		if (strings.HasPrefix(request.URL.Path, "/v1/pages/") || strings.HasPrefix(request.URL.Path, "/v1/blocks/")) && request.Header.Get("Authorization") != "Bearer notion-token" {
			http.Error(response, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/0/") && request.Header.Get("Authorization") != "Bearer sentry-token" {
			http.Error(response, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/repos/") && request.Header.Get("Authorization") != "Bearer github-token" {
			http.Error(response, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if request.URL.Path == "/graphql" && request.Header.Get("Authorization") != "linear-token" {
			http.Error(response, `{"errors":[{"message":"unauthorized"}]}`, http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/conversations/") && request.Header.Get("Authorization") != "Bearer intercom-token" {
			http.Error(response, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/v1/") && request.Header.Get("Authorization") != "Bearer rk_test_fixture" && !strings.HasPrefix(request.URL.Path, "/v1/pages/") && !strings.HasPrefix(request.URL.Path, "/v1/blocks/") {
			http.Error(response, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if inline != "" {
			_, _ = response.Write([]byte(inline))
			return
		}
		_, _ = response.Write(read(fixture))
	}))
	t.Cleanup(server.Close)
	return server
}

func installEvalConnection(t *testing.T, store *state.Store, vault *evalVault, connectorKey, modeID, methodID, connectionName, baseURL string, secrets, identity map[string]string) (kit.Package, kit.Mode) {
	t.Helper()
	packages, err := builtin.All()
	if err != nil {
		t.Fatal(err)
	}
	var loaded kit.Loaded
	for _, candidate := range packages {
		if candidate.Package.Key == connectorKey {
			loaded = candidate
			break
		}
	}
	if loaded.Package.Key == "" {
		t.Fatalf("missing builtin %s", connectorKey)
	}
	mode, ok := kit.FindMode(loaded.Package, modeID)
	if !ok {
		t.Fatalf("missing mode %s/%s", connectorKey, modeID)
	}
	if baseURL != "" {
		for index := range loaded.Package.Modes {
			if loaded.Package.Modes[index].ID == modeID {
				loaded.Package.Modes[index].Executor.BaseURL = baseURL
				mode = loaded.Package.Modes[index]
			}
		}
	}
	raw, _ := json.Marshal(loaded.Package)
	loaded.Raw = raw
	loaded.Digest = connectorKey + "-eval"
	if err := store.InstallPackage(context.Background(), loaded, "builtin"); err != nil {
		t.Fatal(err)
	}
	ref := ""
	if secrets != nil {
		ref = connectorKey + "-ref"
		vault.values[ref] = secrets
	}
	if identity == nil {
		identity = map[string]string{}
	}
	if err := store.CreateConnection(context.Background(), state.Connection{ID: connectionName, Name: connectionName, ConnectorKey: connectorKey, SelectedMode: modeID, CredentialMethod: methodID, Status: "active", CredentialRef: ref, SafeIdentity: identity}); err != nil {
		t.Fatal(err)
	}
	return loaded.Package, mode
}
