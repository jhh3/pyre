package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/outfit/internal/kit"
)

func TestStreamingHelper(t *testing.T) {
	if os.Getenv("OUTFIT_TEST_STREAM") != "1" {
		return
	}
	fmt.Println("first streamed line")
	time.Sleep(30 * time.Second)
}

func TestCommandTemplateHelper(t *testing.T) {
	for index, argument := range os.Args {
		if argument == "--" && index+1 < len(os.Args) {
			fmt.Printf(`{"project":%q}`, os.Args[index+1])
			os.Exit(0)
		}
	}
}

func TestBrowserContinuationHelper(t *testing.T) {
	if os.Getenv("OUTFIT_TEST_BROWSER_CONTINUATION") != "1" {
		return
	}
	mode := ""
	for index, argument := range os.Args {
		if argument == "--" && index+1 < len(os.Args) {
			mode = os.Args[index+1]
			break
		}
	}
	switch mode {
	case "setup":
		nextStep := fmt.Sprintf("%s -test.run=TestBrowserContinuationHelper -- complete 'opaque value'", filepath.Base(os.Args[0]))
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
			"browser_url":       "https://connect.example.test/authorize",
			"verification_code": "blue-cat",
			"next_step":         nextStep,
		})
	case "complete":
		fmt.Print(`{"authenticated":true}`)
	case "fail":
		fmt.Printf(`{"authenticated":false,"token":%q}`, os.Getenv("OUTFIT_TEST_SECRET"))
		os.Exit(4)
	}
	os.Exit(0)
}

func TestIsolatedConfigHelper(t *testing.T) {
	if os.Getenv("OUTFIT_TEST_ISOLATED_CONFIG") != "1" {
		return
	}
	directory := os.Getenv("GOOGLE_WORKSPACE_CLI_CONFIG_DIR")
	_, sentinelErr := os.Stat(directory + "/host-profile")
	fmt.Printf(`{"directory":%q,"host_profile_visible":%t,"inherited_token_visible":%t}`, directory, sentinelErr == nil, os.Getenv("GOOGLE_WORKSPACE_CLI_TOKEN") != "")
	os.Exit(0)
}

func TestRunCommandTemplateUsesManagedBinaryAndExpandsSettings(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Executor{}).RunCommandTemplate(context.Background(), kit.CommandTemplate{Program: "ignored-system-binary", Args: []string{"-test.run=TestCommandTemplateHelper", "--", "{{setting.PROJECT}}"}}, map[string]string{"PROJECT": "acme"}, CredentialContext{Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	object, ok := result.Data.(map[string]any)
	if !ok || object["project"] != "acme" {
		t.Fatalf("unexpected command result: %#v", result)
	}
}

func TestRunCommandTemplateUsesEphemeralIsolatedConfigDirectory(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	hostConfig := t.TempDir()
	if err := os.WriteFile(hostConfig+"/host-profile", []byte("must not be visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_WORKSPACE_CLI_CONFIG_DIR", hostConfig)
	t.Setenv("GOOGLE_WORKSPACE_CLI_TOKEN", "must-not-leak")
	credentials := CredentialContext{
		Binary: binary,
		Bindings: []kit.CredentialBinding{
			{Target: "process-env", Name: "OUTFIT_TEST_ISOLATED_CONFIG", Value: "1"},
			{Target: "unset-env", Name: "GOOGLE_WORKSPACE_CLI_TOKEN"},
			{Target: "temporary-directory", Name: "GOOGLE_WORKSPACE_CLI_CONFIG_DIR"},
		},
	}
	result, err := (Executor{}).RunCommandTemplate(context.Background(), kit.CommandTemplate{Args: []string{"-test.run=TestIsolatedConfigHelper"}}, nil, credentials)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := result.Data.(map[string]any)
	if !ok || object["host_profile_visible"] != false || object["inherited_token_visible"] != false {
		t.Fatalf("host profile leaked into isolated command: %#v", result)
	}
	directory, _ := object["directory"].(string)
	if directory == "" || directory == hostConfig {
		t.Fatalf("expected a distinct temporary config directory, got %q", directory)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("isolated config directory survived command cleanup: %v", err)
	}
}

func TestRunCommandTemplateCompletesDeclarativeBrowserLogin(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opened := ""
	promptedURL, promptedCode := "", ""
	executor := Executor{
		OpenBrowser: func(target string) error {
			opened = target
			return errors.New("headless")
		},
		BrowserPrompt: func(target, code string) {
			promptedURL, promptedCode = target, code
		},
	}
	credentials := CredentialContext{Binary: binary, Bindings: []kit.CredentialBinding{{Target: "process-env", Name: "OUTFIT_TEST_BROWSER_CONTINUATION", Value: "1"}}}
	result, err := executor.RunCommandTemplate(context.Background(), kit.CommandTemplate{
		Program: "trusted-cli",
		Args:    []string{"-test.run=TestBrowserContinuationHelper", "--", "setup"},
		BrowserContinuation: &kit.BrowserContinuation{
			URLField: "browser_url", VerificationCodeField: "verification_code", NextStepField: "next_step", TimeoutSeconds: 5,
		},
	}, nil, credentials)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := result.Data.(map[string]any)
	if opened != "https://connect.example.test/authorize" || promptedURL != opened || promptedCode != "blue-cat" || !ok || object["authenticated"] != true {
		t.Fatalf("browser continuation did not complete: opened=%q prompted=%q/%q result=%#v", opened, promptedURL, promptedCode, result)
	}
}

func TestRunCommandTemplateRejectsUnsafeBrowserContinuation(t *testing.T) {
	for _, nextStep := range []string{"other-program complete", "trusted-cli 'unterminated"} {
		t.Run(nextStep, func(t *testing.T) {
			opened := false
			executor := Executor{OpenBrowser: func(string) error { opened = true; return nil }}
			_, err := executor.completeBrowserContinuation(context.Background(), kit.CommandTemplate{
				Program: "trusted-cli", BrowserContinuation: &kit.BrowserContinuation{URLField: "url", NextStepField: "next"},
			}, nil, CredentialContext{Binary: "/managed/trusted-cli"}, Result{Data: map[string]any{"url": "https://connect.example.test", "next": nextStep}})
			if err == nil || opened {
				t.Fatalf("unsafe continuation was accepted: %v", err)
			}
		})
	}
	_, err := (Executor{}).completeBrowserContinuation(context.Background(), kit.CommandTemplate{
		Program: "trusted-cli", BrowserContinuation: &kit.BrowserContinuation{URLField: "url", NextStepField: "next"},
	}, nil, CredentialContext{}, Result{Data: map[string]any{"url": "http://connect.example.test", "next": "trusted-cli complete"}})
	if err == nil || !strings.Contains(err.Error(), "unsafe browser URL") {
		t.Fatalf("insecure browser URL was accepted: %v", err)
	}
}

func TestRunCommandTemplateReportsAndRedactsFailureStdout(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	secret := "must-not-leak"
	credentials := CredentialContext{
		Binary: binary,
		Method: kit.CredentialMethod{Fields: []kit.CredentialField{{Key: "TOKEN", Secret: true}}},
		Bindings: []kit.CredentialBinding{
			{Target: "process-env", Name: "OUTFIT_TEST_BROWSER_CONTINUATION", Value: "1"},
			{Target: "process-env", Name: "OUTFIT_TEST_SECRET", Value: "{{secret.TOKEN}}"},
		},
	}
	result, err := (Executor{}).RunCommandTemplate(context.Background(), kit.CommandTemplate{
		Program: "trusted-cli", Args: []string{"-test.run=TestBrowserContinuationHelper", "--", "fail"},
	}, map[string]string{"TOKEN": secret}, credentials)
	if err == nil || !strings.Contains(err.Error(), `"authenticated":false`) || !strings.Contains(err.Error(), "<redacted>") || strings.Contains(err.Error(), secret) {
		t.Fatalf("failure output was not useful and redacted: result=%#v err=%v", result, err)
	}
}

func tokenCredentials(bindings ...kit.CredentialBinding) CredentialContext {
	return CredentialContext{
		Method: kit.CredentialMethod{
			ID: "managed-token", Label: "Managed token", Description: "Managed test token", Custody: "outfit",
			Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}},
		},
		Bindings: bindings,
	}
}

func TestHTTPExecutionStaysOnFixedOriginAndInjectsSecret(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/issues/A%2FB" && r.URL.EscapedPath() != "/v1/issues/A%2FB" {
			t.Fatalf("unexpected path %s", r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("missing auth")
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/v1/", PrivateNet: true}}
	op := kit.Operation{HTTP: &kit.HTTPOperation{Method: "GET", Path: "/issues/:id"}}
	credentials := tokenCredentials(kit.CredentialBinding{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.TOKEN}}"})
	result, err := (Executor{HTTPClient: server.Client()}).RunWithCredentials(context.Background(), mode, op, map[string]string{"id": "A/B"}, map[string]string{"TOKEN": "secret"}, credentials)
	if err != nil || result.Status != 200 {
		t.Fatalf("run: %v %#v", err, result)
	}
}

func TestDefaultHTTPClientRejectsPrivateAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("private server was reached") }))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/"}}
	op := kit.Operation{HTTP: &kit.HTTPOperation{Method: "GET", Path: "/"}}
	_, err := (Executor{}).Run(context.Background(), mode, op, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no safe address") {
		t.Fatalf("expected private-address refusal, got %v", err)
	}
}

func TestHTTPBodyAcceptsStructuredJSONArgument(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		query, ok := body["query"].(map[string]any)
		if !ok || query["status"] != "open" {
			t.Fatalf("argument was stringified: %#v", body)
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}}
	op := kit.Operation{HTTP: &kit.HTTPOperation{Method: "POST", Path: "/search", Body: []string{"query"}}}
	if _, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, op, map[string]string{"query": `{"status":"open"}`}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPUsesProviderNamesForSanitizedImportedArguments(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page-size") != "10" {
			t.Fatalf("provider query name was lost: %s", r.URL.RawQuery)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["display-name"] != "Acme" {
			t.Fatalf("provider body name was lost: %#v", body)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}}
	op := kit.Operation{HTTP: &kit.HTTPOperation{Method: "POST", Path: "/items", Query: []string{"page_size"}, QueryNames: map[string]string{"page_size": "page-size"}, Body: []string{"display_name"}, BodyNames: map[string]string{"display_name": "display-name"}}}
	if _, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, op, map[string]string{"page_size": "10", "display_name": "Acme"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPErrorStatusShowsProviderMessage(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"object":"error","status":404,"message":"Could not find page"}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}}
	operation := kit.Operation{HTTP: &kit.HTTPOperation{Method: "GET", Path: "/"}}
	_, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, operation, nil, nil)
	if err == nil || err.Error() != "upstream returned 404 Not Found: Could not find page" {
		t.Fatalf("error = %v", err)
	}
}

func TestHTTPRejectsProviderErrorEnvelopesOnSuccessfulStatus(t *testing.T) {
	tests := []struct {
		name, body, successField, errorField, want string
	}{
		{name: "Slack false", body: `{"ok":false,"error":"invalid_auth"}`, successField: "ok", errorField: "error", want: "invalid_auth"},
		{name: "GraphQL errors", body: `{"errors":[{"message":"bad token"},{"message":"no team"}]}`, errorField: "errors", want: "bad token; no team"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}}
			operation := kit.Operation{HTTP: &kit.HTTPOperation{Method: "GET", Path: "/", SuccessField: test.successField, ErrorField: test.errorField}}
			_, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, operation, nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHTTPBodyValuesOmitMissingOptionalAndPreserveJSONTypes(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		variables := body["variables"].(map[string]any)
		if variables["limit"] != float64(10) {
			t.Fatalf("numeric variable was stringified: %#v", body)
		}
		if _, exists := variables["cursor"]; exists {
			t.Fatalf("missing optional variable was emitted: %#v", body)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}}
	op := kit.Operation{HTTP: &kit.HTTPOperation{Method: "POST", Path: "/graphql", BodyValues: map[string]any{"query": "query", "variables": map[string]any{"limit": "{{arg.limit}}", "cursor": "{{arg.cursor}}"}}}}
	if _, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, op, map[string]string{"limit": "10"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCLIUsesArgvWithoutShellExpansion(t *testing.T) {
	mode := kit.Mode{Executor: kit.Executor{Kind: "cli", Binary: "printf"}}
	op := kit.Operation{CLI: &kit.CLIOperation{Args: []string{"%s", "{{arg.value}}"}}}
	result, err := (Executor{}).Run(context.Background(), mode, op, map[string]string{"value": "$(false); hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(result.Raw)) != "$(false); hello" {
		t.Fatalf("shell expansion occurred: %q", result.Raw)
	}
}

func TestDeclaredCLIStreamWritesLiveOutputAndStopsWithContext(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	mode := kit.Mode{Executor: kit.Executor{Kind: "cli", Binary: binary}}
	operation := kit.Operation{Name: "tail", Class: "read", Interaction: "stream", CLI: &kit.CLIOperation{Args: []string{"-test.run=TestStreamingHelper"}}}
	credentials := CredentialContext{Bindings: []kit.CredentialBinding{{Target: "process-env", Name: "OUTFIT_TEST_STREAM", Value: "1"}}}
	started := time.Now()
	_, err = (Executor{}).RunWithCredentialsIO(ctx, mode, operation, nil, nil, credentials, &output, &output)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stream did not stop with its context: %v", err)
	}
	if time.Since(started) > 2*time.Second || !strings.Contains(output.String(), "first streamed line") {
		t.Fatalf("stream was buffered or did not stop promptly: elapsed=%s output=%q", time.Since(started), output.String())
	}
}

func TestCLIOptionalArgumentsAreOmittedOrAppendedWithoutShell(t *testing.T) {
	mode := kit.Mode{Executor: kit.Executor{Kind: "cli", Binary: "printf"}}
	op := kit.Operation{CLI: &kit.CLIOperation{Args: []string{"%s", "base"}, OptionalArgs: []kit.CLIOptionalArg{{Flag: "--repo", Argument: "repo"}}}}
	plan, err := (Executor{}).Plan(mode, op, nil)
	if err != nil || strings.Contains(strings.Join(plan.Argv, " "), "--repo") {
		t.Fatalf("empty optional arg was not omitted: %#v %v", plan, err)
	}
	plan, err = (Executor{}).Plan(mode, op, map[string]string{"repo": "acme/app"})
	if err != nil || strings.Join(plan.Argv, " ") != "%s base --repo acme/app" {
		t.Fatalf("optional arg was not appended exactly: %#v %v", plan, err)
	}
}

func TestCLIPlanModelsBooleanFlagsExactly(t *testing.T) {
	mode := kit.Mode{Executor: kit.Executor{Kind: "cli", Binary: "go", Args: []string{"tool", "{{arg.scope}}"}}}
	op := kit.Operation{Name: "list", Class: "read", Input: map[string]any{"type": "object", "properties": map[string]any{"scope": map[string]any{"type": "string"}, "json": map[string]any{"type": "boolean"}}, "required": []string{"scope"}, "additionalProperties": false}, CLI: &kit.CLIOperation{Args: []string{"list"}, OptionalArgs: []kit.CLIOptionalArg{{Flag: "--json", Argument: "json", Boolean: true}}}}
	plan, err := (Executor{}).Plan(mode, op, map[string]string{"scope": "issues", "json": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan.Argv, " "); got != "tool issues list --json" {
		t.Fatalf("unexpected argv: %s", got)
	}
	if _, err := (Executor{}).Plan(mode, op, map[string]string{"scope": "issues", "json": "yes"}); err == nil {
		t.Fatal("invalid boolean flag value was accepted")
	}
}

func TestCLIPlanIncludesRedactedStdin(t *testing.T) {
	mode := kit.Mode{Executor: kit.Executor{Kind: "cli", Binary: "acme"}}
	op := kit.Operation{Name: "login", Class: "read", Input: map[string]any{"type": "object", "properties": map[string]any{"account": map[string]any{"type": "string"}}, "required": []string{"account"}, "additionalProperties": false}, CLI: &kit.CLIOperation{Args: []string{"login", "{{arg.account}}"}, Stdin: "{{secret.TOKEN}}\n"}}
	plan, err := (Executor{}).PlanWithCredentials(mode, op, map[string]string{"account": "work"}, tokenCredentials())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Stdin != "<secret:TOKEN>\n" || strings.Contains(plan.Stdin, "actual-secret") || !strings.Contains(plan.CredentialBoundary, "stdin") {
		t.Fatalf("stdin plan was incomplete or unredacted: %#v", plan)
	}
}

func TestUnauthenticatedPlanStatesNoCredentialRequired(t *testing.T) {
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: "https://api.example.test/"}}
	op := kit.Operation{Name: "status", Class: "read", HTTP: &kit.HTTPOperation{Method: "GET", Path: "/status"}}
	plan, err := (Executor{}).Plan(mode, op, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CredentialBoundary != "no credential is required" {
		t.Fatalf("unexpected credential boundary: %s", plan.CredentialBoundary)
	}
}

func TestMCPTransportRejectsInsecureEndpoint(t *testing.T) {
	mode := kit.Mode{Executor: kit.Executor{Kind: "mcp", Endpoint: "http://mcp.example.test/mcp"}}
	if _, _, err := mcpTransport(context.Background(), mode, nil, map[string]string{"TOKEN": "do-not-send"}, tokenCredentials()); err == nil {
		t.Fatal("insecure MCP endpoint was accepted")
	}
}

func TestPlanIsExactAndRedactsCredentialWithoutIO(t *testing.T) {
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: "https://api.example.test/v1/"}}
	op := kit.Operation{Name: "get_issue", Class: "read", Input: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}, HTTP: &kit.HTTPOperation{Method: "GET", Path: "/issues/:id", Query: []string{"expand"}}}
	credentials := tokenCredentials(kit.CredentialBinding{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.TOKEN}}"})
	plan, err := (Executor{}).PlanWithCredentials(mode, op, map[string]string{"id": "A/B"}, credentials)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Destination != "https://api.example.test" || plan.Path != "/v1/issues/A%2FB" || plan.Headers["Authorization"] != "Bearer <secret:TOKEN>" {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	encoded, _ := json.Marshal(plan)
	if strings.Contains(string(encoded), "actual-secret") {
		t.Fatal("plan leaked credential")
	}
	if _, err := (Executor{}).PlanWithCredentials(mode, op, map[string]string{}, credentials); err == nil || !strings.Contains(err.Error(), "missing required argument id") {
		t.Fatalf("missing argument was not rejected: %v", err)
	}
}

func TestHTTPRetriesStandardRetryAfterHeaderThenStops(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.Header.Get("User-Agent") != "Fixture (https://example.test, 1)" || r.URL.Query().Get("with_counts") != "true" {
			t.Errorf("executor header or fixed query missing: %v %s", r.Header, r.URL.RawQuery)
		}
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true, Headers: map[string]string{"User-Agent": "Fixture (https://example.test, 1)"}}}
	op := kit.Operation{HTTP: &kit.HTTPOperation{Method: "GET", Path: "/guilds", QueryValues: map[string]string{"with_counts": "true"}}}
	if _, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, op, nil, nil); err != nil || attempts != 2 {
		t.Fatalf("retry: err=%v attempts=%d", err, attempts)
	}
	plan, err := (Executor{}).Plan(mode, op, nil)
	if err != nil || plan.Headers["User-Agent"] == "" || !strings.Contains(plan.Path, "with_counts=true") {
		t.Fatalf("plan = %#v %v", plan, err)
	}
}

func TestHTTPDoesNotSleepPastLongRateLimit(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"retry_after":3600,"global":true}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}}
	op := kit.Operation{HTTP: &kit.HTTPOperation{Method: "POST", Path: "/messages", Body: []string{"content"}}}
	started := time.Now()
	_, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, op, map[string]string{"content": "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "retry after 1h0m0s") || attempts != 1 || time.Since(started) > 5*time.Second {
		t.Fatalf("long rate limit: err=%v attempts=%d", err, attempts)
	}
}

func TestHTTPStringArgumentsAndLargeIntegersKeepExactValues(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"content":"true"`) || !strings.Contains(string(raw), `"ref":{"id":"345678901234567899"}`) || !strings.Contains(string(raw), `"tags":[345678901234567899]`) {
			t.Errorf("body lost exact values: %s", raw)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}}
	op := kit.Operation{HTTP: &kit.HTTPOperation{Method: "POST", Path: "/m", Body: []string{"content", "tags"}, BodyValues: map[string]any{"ref": map[string]any{"id": "{{arg.id}}"}}, StringArguments: []string{"content", "id"}}}
	if _, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, op, map[string]string{"content": "true", "id": "345678901234567899", "tags": "[345678901234567899]"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestComposeRunsReadStepsAndProjectsFields(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/g/1/channels":
			_, _ = io.WriteString(w, `[{"id":"a","last_message_id":"9","noise":true}]`)
		case "/g/1/threads":
			_, _ = io.WriteString(w, `{"threads":[{"id":"t","last_message_id":"8","noise":1}],"members":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}, Operations: []kit.Operation{
		kit.NormalizeOperation(kit.Operation{Name: "channels", Class: "read", HTTP: &kit.HTTPOperation{Method: "GET", Path: "/g/:guild/channels"}}),
		kit.NormalizeOperation(kit.Operation{Name: "threads", Class: "read", HTTP: &kit.HTTPOperation{Method: "GET", Path: "/g/:guild/threads"}}),
	}}
	composite := kit.NormalizeOperation(kit.Operation{Name: "activity", Class: "read", Input: map[string]any{"properties": map[string]any{"guild": map[string]any{"type": "string", "description": "Guild.", "examples": []any{"1"}}}, "required": []any{"guild"}}, Compose: []kit.ComposeStep{
		{As: "channels", Operation: "channels", Fields: []string{"id", "last_message_id"}},
		{As: "threads", Operation: "threads", ItemsPath: "$.threads", Fields: []string{"id", "last_message_id"}},
	}})
	result, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, composite, map[string]string{"guild": "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Raw) != `{"channels":[{"id":"a","last_message_id":"9"}],"threads":[{"id":"t","last_message_id":"8"}]}` {
		t.Fatalf("compose result = %s", result.Raw)
	}
	plan, err := (Executor{}).Plan(mode, composite, map[string]string{"guild": "1"})
	if err != nil || plan.Executor != "compose" || len(plan.Steps) != 2 || plan.Steps[1].Path != "/g/1/threads" {
		t.Fatalf("compose plan = %#v %v", plan, err)
	}
	if _, err := (Executor{}).Plan(mode, composite, nil); err == nil {
		t.Fatal("compose plan accepted a missing required argument")
	}
}
