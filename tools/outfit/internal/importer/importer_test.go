package importer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
)

func TestImportOpenAPIGeneratesFixedOriginOperationsAndLinks(t *testing.T) {
	source := writeFixture(t, `openapi: 3.1.0
info: {title: Acme, version: 1}
servers: [{url: "https://api.acme.test/v1"}]
components:
  parameters:
    IssueId: {name: id, in: path, required: true, schema: {type: string}}
paths:
  /v1/issues/{id}:
    get:
      operationId: getIssue
      summary: Get issue
      parameters:
        - {$ref: '#/components/parameters/IssueId'}
    delete:
      operationId: deleteIssue
      summary: Delete issue
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
`)
	result, err := Import(context.Background(), Options{Kind: "openapi", Source: source, Key: "acme-api", LinkHost: "app.acme.test", AuthField: "TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	mode := result.Package.Modes[0]
	if mode.Executor.BaseURL != "https://api.acme.test/v1/" || len(mode.Operations) != 2 || len(result.Package.Links) != 1 {
		t.Fatalf("unexpected package: %#v", result.Package)
	}
	for _, operation := range mode.Operations {
		if operation.Name == "delete_issue" && operation.Class != "destructive" {
			t.Fatalf("delete was not destructive: %#v", operation)
		}
		if operation.HTTP.Path != "/issues/:id" {
			t.Fatalf("path was not relative and parameterized: %s", operation.HTTP.Path)
		}
	}
	assertWritesValid(t, result)
}

func TestImportPrivateNetworkCapabilityIsExplicitAndPersisted(t *testing.T) {
	source := writeFixture(t, `openapi: 3.1.0
info: {title: Internal, version: 1}
servers: [{url: "https://service.internal/"}]
paths:
  /health:
    get: {operationId: health, summary: Health}
`)
	result, err := Import(context.Background(), Options{Kind: "openapi", Source: source, Key: "internal-api", PrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Package.Modes[0].Executor.PrivateNet || len(result.Warnings) < 2 {
		t.Fatalf("private-network review boundary was lost: %#v", result)
	}
	assertWritesValid(t, result)
}

func TestOpenAPIImportIsByteDeterministic(t *testing.T) {
	source := writeFixture(t, `openapi: 3.1.0
info: {title: Acme, version: 1}
servers: [{url: "https://api.acme.test/v1/"}]
paths:
  /issues/{id}:
    patch:
      operationId: updateIssue
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [state, title]
              properties:
                title: {type: string}
                labels: {type: array}
                state: {type: string}
  /projects/{project}/issues/{id}:
    get:
      operationId: getIssue
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
        - {name: project, in: path, required: true, schema: {type: string}}
  /archived-issues/{id}:
    get:
      operationId: getIssue
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
`)
	first, err := Import(context.Background(), Options{Kind: "openapi", Source: source, Key: "acme-api", LinkHost: "app.acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Import(context.Background(), Options{Kind: "openapi", Source: source, Key: "acme-api", LinkHost: "app.acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("imports differ:\nfirst=%#v\nsecond=%#v", first, second)
	}
	firstDir, err := Write(t.TempDir(), first)
	if err != nil {
		t.Fatal(err)
	}
	secondDir, err := Write(t.TempDir(), second)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"connector.yaml", "README.md"} {
		firstBytes, err := os.ReadFile(filepath.Join(firstDir, name))
		if err != nil {
			t.Fatal(err)
		}
		secondBytes, err := os.ReadFile(filepath.Join(secondDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(firstBytes) != string(secondBytes) {
			t.Fatalf("%s differs across deterministic imports", name)
		}
	}
}

func TestImportPostmanKeepsOneOriginAndModelsJSON(t *testing.T) {
	source := writeFixture(t, `{
  "info":{"name":"Acme"},
  "variable":[{"key":"base","value":"https://api.acme.test"}],
  "item":[
    {"name":"List Things","request":{"method":"GET","url":"{{base}}/things?limit=10"}},
    {"name":"Create Thing","request":{"method":"POST","url":"{{base}}/things","body":{"mode":"raw","raw":"{\"name\":\"demo\"}"}}},
    {"name":"Foreign","request":{"method":"GET","url":"https://evil.test/data"}}
  ]
}`)
	result, err := Import(context.Background(), Options{Kind: "postman", Source: source, Key: "acme-api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Package.Modes[0].Operations) != 2 || len(result.Warnings) < 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	assertWritesValid(t, result)
}

func TestImportPostmanStructuredURLAndPathVariable(t *testing.T) {
	source := writeFixture(t, `{
  "item":[{"name":"Get Thing","request":{"method":"GET","url":{"protocol":"https","host":["api","acme","test"],"path":["things","{{id}}"],"query":[{"key":"expand","value":"yes"}]}}}]
}`)
	result, err := Import(context.Background(), Options{Kind: "postman", Source: source, Key: "acme-api"})
	if err != nil {
		t.Fatal(err)
	}
	op := result.Package.Modes[0].Operations[0]
	if op.HTTP.Path != "/things/:id" || len(op.HTTP.Query) != 1 || op.HTTP.Query[0] != "expand" {
		t.Fatalf("unexpected structured Postman URL: %#v", op.HTTP)
	}
}

func TestImportCLIParsesCommandsAndConservativelyClassifies(t *testing.T) {
	source := writeFixture(t, `Commands:
  list       List issues
  create     Create an issue
  delete     Delete an issue permanently
`)
	result, err := Import(context.Background(), Options{Kind: "cli", Source: source, Binary: "acme", Key: "acme-cli"})
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]string{}
	for _, operation := range result.Package.Modes[0].Operations {
		classes[operation.Name] = operation.Class
	}
	if classes["list"] != "read" || classes["create"] != "write" || classes["delete"] != "destructive" {
		t.Fatalf("unexpected classifications: %#v", classes)
	}
	assertWritesValid(t, result)
}

func TestImportCLICompletionSchemaModelsNestedRequiredAndOptionalArgs(t *testing.T) {
	source := writeFixture(t, `{
  "name":"acme",
  "subcommands":[{"name":"issue","subcommands":[{"name":"get","description":"Get issue","args":{"name":"id"},"options":[{"name":["-r","--repo"],"description":"Repository","args":{"name":"repo"}},{"name":"--json","description":"Print JSON"}]}]}]
}`)
	result, err := Import(context.Background(), Options{Kind: "cli", Source: source, Binary: "acme", Key: "acme-cli"})
	if err != nil {
		t.Fatal(err)
	}
	op := result.Package.Modes[0].Operations[0]
	if op.Name != "issue_get" || len(op.CLI.Args) != 3 || len(op.CLI.OptionalArgs) != 2 || op.CLI.OptionalArgs[0].Flag != "--repo" || !op.CLI.OptionalArgs[1].Boolean {
		t.Fatalf("unexpected completion import: %#v", op)
	}
	assertWritesValid(t, result)
}

func TestImportMCPUsesLiveAnnotationsAndRetainsUpstreamToolNames(t *testing.T) {
	discover := func(_ context.Context, _ kit.Mode, secrets map[string]string) ([]runtime.DiscoveredTool, error) {
		if secrets["TOKEN"] != "test" {
			return nil, fmt.Errorf("missing discovery token")
		}
		return []runtime.DiscoveredTool{{Name: "issues.get", Title: "Get issue", Description: "Read an issue", Class: "read", InputSchema: map[string]any{"type": "object"}}, {Name: "issues.delete", Description: "Delete an issue", Class: "destructive"}}, nil
	}
	result, err := Import(context.Background(), Options{Kind: "mcp", Source: "https://mcp.acme.test/mcp", Key: "acme-mcp", AuthField: "TOKEN", Secrets: map[string]string{"TOKEN": "test"}, MCPDiscover: discover})
	if err != nil {
		t.Fatal(err)
	}
	operations := result.Package.Modes[0].Operations
	if len(operations) != 2 || operations[0].MCPTool == operations[0].Name {
		t.Fatalf("sanitized id should retain exact upstream tool: %#v", operations)
	}
	assertWritesValid(t, result)
}

func TestImportMCPRejectsInsecureEndpointBeforeCredentialedDiscovery(t *testing.T) {
	called := false
	_, err := Import(context.Background(), Options{Kind: "mcp", Source: "http://mcp.example.test/mcp", Key: "acme-mcp", AuthField: "TOKEN", Secrets: map[string]string{"TOKEN": "do-not-send"}, MCPDiscover: func(context.Context, kit.Mode, map[string]string) ([]runtime.DiscoveredTool, error) {
		called = true
		return nil, nil
	}})
	if err == nil || called {
		t.Fatalf("insecure MCP discovery was attempted: called=%t err=%v", called, err)
	}
}

func TestImportGraphQLIntrospectsQueriesAndMutations(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"__schema":{"queryType":{"name":"Query"},"mutationType":{"name":"Mutation"},"types":[{"kind":"OBJECT","name":"Query","fields":[{"name":"issue","description":"Get issue","args":[{"name":"id","type":{"kind":"NON_NULL","ofType":{"kind":"SCALAR","name":"ID"}}}],"type":{"kind":"OBJECT","name":"Issue"}}]},{"kind":"OBJECT","name":"Mutation","fields":[{"name":"deleteIssue","description":"Delete issue","args":[{"name":"id","type":{"kind":"NON_NULL","ofType":{"kind":"SCALAR","name":"ID"}}}],"type":{"kind":"SCALAR","name":"Boolean"}}]}]}}}`)
	}))
	defer server.Close()
	result, err := Import(context.Background(), Options{Kind: "graphql", Source: server.URL + "/graphql", Key: "acme-graphql", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Package.Modes[0].Operations) != 3 || result.Package.Modes[0].Verify != "schema_info" {
		t.Fatalf("unexpected GraphQL package: %#v", result.Package)
	}
	assertWritesValid(t, result)
}

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertWritesValid(t *testing.T, result Result) {
	t.Helper()
	directory, err := Write(t.TempDir(), result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kit.Load(directory); err != nil {
		t.Fatal(err)
	}
}
