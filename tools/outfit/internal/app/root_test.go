package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/invoke"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/toolchain"
	"github.com/jhh3/outfit/internal/vault"
	"github.com/spf13/cobra"
)

func TestVersionCommand(t *testing.T) {
	cmd := versionCommand(BuildInfo{Version: "1.2.3", Commit: "abc", Date: "today"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.Execute()
	if got := output.String(); !strings.Contains(got, "outfit 1.2.3 (abc, today)") {
		t.Fatalf("unexpected version output: %q", got)
	}
}

func TestSessionContextAcceptsThePackagedHookAdapterFlag(t *testing.T) {
	t.Setenv("OUTFIT_HOME", t.TempDir())
	command := sessionCommand()
	command.SetArgs([]string{"context", "--hook", "auto"})
	if err := command.Execute(); err != nil {
		t.Fatalf("packaged SessionStart command failed: %v", err)
	}
}

func TestFreshInstallGetExplainsHowToConnectSupportedProvider(t *testing.T) {
	t.Setenv("OUTFIT_HOME", t.TempDir())
	command := getCommand()
	command.SetArgs([]string{"https://app.slack.com/archives/C012ABC/p1712345678123456"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "[link_connection_required]") || !strings.Contains(err.Error(), "outfit connector add") {
		t.Fatalf("fresh-install diagnostic = %v", err)
	}
}

func TestCLIHealthUsesManagedToolInsteadOfGlobalPath(t *testing.T) {
	root := t.TempDir()
	manager := toolchain.Manager{Paths: state.Paths{Root: root}}
	requirement := kit.ToolRequirement{ID: "gws", Binary: "definitely-not-on-path", Version: "1", Source: kit.ToolSource{Checksums: map[string]string{}}}
	platform := goruntime.GOOS + "-" + goruntime.GOARCH
	directory := filepath.Join(root, "tools", "gws", requirement.Version, platform)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "gws")
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := toolchain.Receipt{ID: "gws", Version: "1", Platform: platform, Path: binary, ActualSHA: "fixture"}
	encoded, _ := json.Marshal(receipt)
	if err := os.WriteFile(filepath.Join(directory, "receipt.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Use("gws", "1", platform); err != nil {
		t.Fatal(err)
	}
	pkg := kit.Package{Tools: []kit.ToolRequirement{requirement}}
	mode := kit.Mode{Executor: kit.Executor{Kind: "cli", Tool: "gws"}}
	if err := cliExecutorHealth(pkg, mode, manager); err != nil {
		t.Fatalf("managed binary was diagnosed through PATH: %v", err)
	}
}

func TestToolPathPrintsSelectedManagedBinary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OUTFIT_HOME", root)
	paths, err := state.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	manager := toolchain.Manager{Paths: paths}
	requirement := toolchain.GWSRequirement()
	platform := goruntime.GOOS + "-" + goruntime.GOARCH
	directory := filepath.Join(root, "tools", "gws", requirement.Version, platform)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "gws")
	if goruntime.GOOS == "windows" {
		binary += ".exe"
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := toolchain.Receipt{ID: "gws", Display: "GWS", Binary: "gws", Version: requirement.Version, Platform: platform, Path: binary, ActualSHA: requirement.Source.Checksums[platform]}
	encoded, _ := json.Marshal(receipt)
	if err := os.WriteFile(filepath.Join(directory, "receipt.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Use(requirement.ID, requirement.Version, platform); err != nil {
		t.Fatal(err)
	}

	command := toolPathCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"gws"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != binary {
		t.Fatalf("tool path = %q, want %q", output.String(), binary)
	}
}

func TestCollectFieldsUsesDeclaredEnvironmentWithoutPrintingValue(t *testing.T) {
	t.Setenv("ACME_TOKEN", "super-secret-value")
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetErr(&output)
	values := map[string]string{}
	fields := []kit.CredentialField{{Key: "TOKEN", Label: "Acme token", Secret: true, Environment: []string{"ACME_TOKEN"}}}
	if err := collectFields(command, fields, values); err != nil {
		t.Fatal(err)
	}
	if values["TOKEN"] != "super-secret-value" || !strings.Contains(output.String(), "ACME_TOKEN") || strings.Contains(output.String(), "super-secret-value") {
		t.Fatalf("environment collection was not safe: value matched=%t output=%q", values["TOKEN"] == "super-secret-value", output.String())
	}
}

func TestCollectFieldsNeverPromptsForProviderGeneratedOAuthValues(t *testing.T) {
	command := &cobra.Command{}
	values := map[string]string{"OAUTH_CLIENT_ID": "client"}
	fields := []kit.CredentialField{
		{Key: "OAUTH_CLIENT_ID", Generated: true},
		{Key: "OAUTH_ACCESS_TOKEN", Secret: true, Generated: true},
		{Key: "OAUTH_REFRESH_TOKEN", Secret: true, Generated: true},
	}
	if err := collectFields(command, fields, values); err != nil {
		t.Fatalf("generated OAuth field was prompted: %v", err)
	}
}

func TestPluginInstallPlanRefreshesExistingHostCaches(t *testing.T) {
	got, err := pluginInstallPlan("both")
	if err != nil {
		t.Fatal(err)
	}
	want := []hostCommand{
		{program: "codex", args: []string{"plugin", "remove", "cw@personal"}, optional: true},
		{program: "codex", args: []string{"plugin", "marketplace", "add", "jhh3/outfit", "--ref", "main"}},
		{program: "codex", args: []string{"plugin", "marketplace", "upgrade", "outfit"}},
		{program: "codex", args: []string{"plugin", "add", "outfit@outfit"}},
		{program: "claude", args: []string{"plugin", "uninstall", "cw@jhh3-cw", "--scope", "user", "--yes"}, optional: true},
		{program: "claude", args: []string{"plugin", "marketplace", "add", "jhh3/outfit"}},
		{program: "claude", args: []string{"plugin", "marketplace", "update", "jhh3-outfit"}},
		{program: "claude", args: []string{"plugin", "install", "outfit@jhh3-outfit", "--scope", "user", "--yes"}},
		{program: "claude", args: []string{"plugin", "update", "outfit@jhh3-outfit", "--scope", "user", "--yes"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("install plan = %#v, want %#v", got, want)
	}
}

func TestPluginInstallPlanRejectsUnknownHost(t *testing.T) {
	if _, err := pluginInstallPlan("cursor"); err == nil {
		t.Fatal("expected unknown host to be rejected before running any command")
	}
}

func TestRenderConnectorGuideCoversEveryModeAndFromScratchAction(t *testing.T) {
	t.Setenv("ACME_TOKEN", "hidden-value")
	pkg := kit.Package{
		Key: "acme", Display: "Acme", Version: "1.0.0", Description: "Acme resources", Category: "Test",
		CredentialMethods: []kit.CredentialMethod{
			{ID: "token", Label: "Managed token", Description: "Use an Acme token.", Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Access token", Description: "Create in Acme Settings.", Secret: true, Environment: []string{"ACME_TOKEN"}}}},
			{ID: "adopted-host-oauth", Label: "Imported OAuth", Custody: "oauth", Fields: []kit.CredentialField{{Key: "OAUTH_ACCESS_TOKEN", Generated: true, Secret: true}}},
		},
		Modes: []kit.Mode{
			{ID: "rest", Label: "REST", Description: "Fixed API", Recommendation: kit.Recommendation{Rank: 1, Reason: "Most reliable"}, Executor: kit.Executor{Kind: "http", BaseURL: "https://api.acme.test/"}, CredentialMethods: []string{"token"}, DefaultCredentialMethod: "token", Operations: []kit.Operation{{Name: "viewer"}}},
			{ID: "hosted_mcp", Label: "Hosted MCP", Description: "Agent tools", Recommendation: kit.Recommendation{Rank: 2, Reason: "Broad tools"}, Executor: kit.Executor{Kind: "mcp", Endpoint: "https://mcp.acme.test/mcp"}, CredentialMethods: []string{"adopted-host-oauth"}, DefaultCredentialMethod: "adopted-host-oauth"},
		},
	}
	var output bytes.Buffer
	renderConnectorGuide(&output, pkg)
	guide := output.String()
	for _, want := range []string{"REST (rest) · RECOMMENDED", "Create in Acme Settings.", "ACME_TOKEN detected; value hidden", "outfit connector add acme --mode rest", "Hosted MCP (hosted_mcp)", "codex mcp add acme --url https://mcp.acme.test/mcp", "claude mcp add --transport http --scope user acme", "outfit connector imports", "discovered from the hosted server after OAuth import", "outfit connector test acme"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("guide omitted %q:\n%s", want, guide)
		}
	}
	if strings.Contains(guide, "hidden-value") {
		t.Fatalf("guide exposed environment secret: %s", guide)
	}
}

func TestParseOperationArgumentsAcceptsSchemaFlagsAndSlackAliases(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"channel_id": map[string]any{"type": "string"},
		"message_ts": map[string]any{"type": "string"},
		"limit":      map[string]any{"type": "integer"},
	}}
	values := stringMapFlag{}
	dryRun, jsonOutput := false, false
	help, err := parseOperationArguments([]string{"--channel", "C123", "--ts=123.456", "--limit", "20", "--dry-run"}, schema, &values, &dryRun, &jsonOutput)
	if err != nil || help || !dryRun || values["channel_id"] != "C123" || values["message_ts"] != "123.456" || values["limit"] != "20" {
		t.Fatalf("parsed=%#v dry=%t help=%t err=%v", values, dryRun, help, err)
	}
}

func TestParseOperationArgumentsAcceptsAgentFriendlySchemaSpellings(t *testing.T) {
	tests := []struct {
		tokens []string
		schema map[string]any
		key    string
		value  string
	}{
		{tokens: []string{"cus_123"}, schema: map[string]any{"properties": map[string]any{"customer_id": map[string]any{"type": "string"}}, "required": []string{"customer_id"}}, key: "customer_id", value: "cus_123"},
		{tokens: []string{"cus_json"}, schema: map[string]any{"properties": map[string]any{"customer_id": map[string]any{"type": "string"}}, "required": []any{"customer_id"}}, key: "customer_id", value: "cus_json"},
		{tokens: []string{"fileId=drive_123"}, schema: map[string]any{"properties": map[string]any{"file_id": map[string]any{"type": "string"}}}, key: "file_id", value: "drive_123"},
		{tokens: []string{"payment_intent=pi_123"}, schema: map[string]any{"properties": map[string]any{"payment_intent_id": map[string]any{"type": "string"}}}, key: "payment_intent_id", value: "pi_123"},
		{tokens: []string{"--q", "needle"}, schema: map[string]any{"properties": map[string]any{"query": map[string]any{"type": "string"}}}, key: "query", value: "needle"},
	}
	for _, test := range tests {
		values := stringMapFlag{}
		if _, err := parseOperationArguments(test.tokens, test.schema, &values, new(bool), new(bool)); err != nil || values[test.key] != test.value {
			t.Fatalf("tokens=%v values=%#v err=%v", test.tokens, values, err)
		}
	}
}

func TestParseOperationArgumentsExplainsUnknownFlag(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{"channel_id": map[string]any{"type": "string"}, "message_ts": map[string]any{"type": "string"}}}
	values := stringMapFlag{}
	_, err := parseOperationArguments([]string{"--channe-id", "C123"}, schema, &values, new(bool), new(bool))
	if err == nil || !strings.Contains(err.Error(), "did you mean --channel-id") || !strings.Contains(err.Error(), "--arg KEY=value") {
		t.Fatalf("unknown flag error = %v", err)
	}
}

func TestInvocationArgumentsAcceptJSONFileStdinAndFlagOverrides(t *testing.T) {
	command := &cobra.Command{}
	command.SetIn(strings.NewReader(`{"owner":"stdin-owner","number":42,"labels":["bug"]}`))
	options := operationCLIOptions{}
	schema := map[string]any{"properties": map[string]any{"owner": map[string]any{"type": "string"}, "number": map[string]any{"type": "integer"}, "labels": map[string]any{"type": "array"}}}
	help, err := parseInvocationArguments([]string{"--input", "-", "--owner", "flag-owner", "--json", "--all", "--max-pages=3", "--max-items", "50"}, schema, &options)
	if err != nil || help {
		t.Fatalf("parse failed: help=%t err=%v", help, err)
	}
	if err := loadInvocationInput(command, &options); err != nil {
		t.Fatal(err)
	}
	if options.Values["owner"] != "flag-owner" || options.Values["number"] != "42" || options.Values["labels"] != `["bug"]` || !options.JSON || !options.All || options.MaxPages != 3 || options.MaxItems != 50 {
		t.Fatalf("options = %#v", options)
	}
}

func TestDynamicConnectionNamespaceUsesNormalizedBlessedSchema(t *testing.T) {
	t.Setenv("OUTFIT_HOME", t.TempDir())
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	env, err := openBaseEnvironment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := env.state.CreateConnection(context.Background(), state.Connection{ID: "con_github", Name: "github", ConnectorKey: "github", SelectedMode: "cli", CredentialMethod: "native-login", Status: "active", SafeIdentity: map[string]string{}}); err != nil {
		env.state.Close()
		t.Fatal(err)
	}
	env.state.Close()

	root := &cobra.Command{Use: "outfit", SilenceErrors: true, SilenceUsage: true}
	registerDynamicConnectionCommands(root)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"github", "get-pull-request", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"outfit github get-pull-request", "--owner", "--repo", "--number", "Preferred agent interface: typed Outfit CLI", "--json-args", "--input args.json"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("dynamic help omitted %q:\n%s", expected, output.String())
		}
	}
}

func TestDynamicConnectionInventoryOnlyLoadsForPotentialConnectionNamespace(t *testing.T) {
	root := &cobra.Command{Use: "outfit"}
	root.AddCommand(&cobra.Command{Use: "version", Aliases: []string{"v"}})
	for _, test := range []struct {
		arguments []string
		want      bool
	}{{nil, false}, {[]string{"--help"}, true}, {[]string{"--version"}, false}, {[]string{"version"}, false}, {[]string{"v"}, false}, {[]string{"slack", "read-thread"}, true}} {
		if got := wantsDynamicConnectionCommands(root, test.arguments); got != test.want {
			t.Fatalf("arguments=%v got=%t want=%t", test.arguments, got, test.want)
		}
	}
}

func TestAgentFacingInventoryCommandsUseEnvelopeAutomatically(t *testing.T) {
	t.Setenv("OUTFIT_HOME", t.TempDir())
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	t.Setenv("OUTFIT_AGENT", "1")
	for _, command := range []*cobra.Command{listCommand(), searchCommand(), doctorCommand()} {
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		if command.Name() == "search" {
			command.SetArgs([]string{"issues"})
		}
		if err := command.Execute(); err != nil {
			t.Fatalf("%s failed: %v", command.Name(), err)
		}
		var envelope invoke.Envelope
		if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || envelope.Schema != invoke.Schema || !envelope.OK {
			t.Fatalf("%s did not return an agent envelope: %s (%v)", command.Name(), output.String(), err)
		}
	}
}

func TestSearchAndDescribeAdvertiseThePreferredPastedLinkRoute(t *testing.T) {
	t.Setenv("OUTFIT_HOME", t.TempDir())
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	t.Setenv("OUTFIT_AGENT", "1")
	env, err := openBaseEnvironment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := env.state.CreateConnection(context.Background(), state.Connection{ID: "con_slack_link", Name: "slack", ConnectorKey: "slack", SelectedMode: "cli", CredentialMethod: "native-login", Status: "active", SafeIdentity: map[string]string{}}); err != nil {
		env.state.Close()
		t.Fatal(err)
	}
	env.state.Close()

	search := searchCommand()
	var searchOutput bytes.Buffer
	search.SetOut(&searchOutput)
	search.SetErr(&searchOutput)
	search.SetArgs([]string{"read", "slack", "thread", "link"})
	if err := search.Execute(); err != nil {
		t.Fatal(err)
	}
	var searchEnvelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(searchOutput.Bytes(), &searchEnvelope); err != nil || len(searchEnvelope.Data) == 0 || searchEnvelope.Data[0]["operation"] != "get_url" || searchEnvelope.Data[0]["command"] != "outfit get URL" {
		t.Fatalf("search did not prioritize the link route: %s (%v)", searchOutput.String(), err)
	}

	describe := describeCommand()
	var describeOutput bytes.Buffer
	describe.SetOut(&describeOutput)
	describe.SetErr(&describeOutput)
	describe.SetArgs([]string{"slack"})
	if err := describe.Execute(); err != nil {
		t.Fatal(err)
	}
	var describeEnvelope struct {
		Data struct {
			LinkRoutes []linkRouteDescription `json:"link_routes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(describeOutput.Bytes(), &describeEnvelope); err != nil || len(describeEnvelope.Data.LinkRoutes) != 1 {
		t.Fatalf("describe did not include the selected link route: %s (%v)", describeOutput.String(), err)
	}
	route := describeEnvelope.Data.LinkRoutes[0]
	if route.Operation != "get_url" || route.UnderlyingOperation != "slack_read_thread" || route.Command != "outfit get URL" || route.DefaultDepth != "heavy" {
		t.Fatalf("unexpected link route: %#v", route)
	}
}

func TestOperationUsesAgentEnvelopeWithoutJSONFlag(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OUTFIT_HOME", root)
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	t.Setenv("OUTFIT_AGENT", "1")
	ctx := context.Background()
	env, err := openBaseEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pkg := kit.Package{Key: "sample-agent", Version: "1", CredentialMethods: []kit.CredentialMethod{{ID: "none", Custody: "none"}}, Modes: []kit.Mode{{ID: "cli", Executor: kit.Executor{Kind: "cli", Binary: "printf"}, CredentialMethods: []string{"none"}, DefaultCredentialMethod: "none", Operations: []kit.Operation{{Name: "hello", Class: "read", CLI: &kit.CLIOperation{Args: []string{"hello-agent"}}}}}}}
	manifest, _ := json.Marshal(pkg)
	if err := env.state.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: manifest}, "trusted"); err != nil {
		env.state.Close()
		t.Fatal(err)
	}
	secret, err := vault.Open(env.state.Paths())
	if err != nil {
		env.state.Close()
		t.Fatal(err)
	}
	ref, err := secret.Put(ctx, map[string]string{})
	if err != nil {
		env.state.Close()
		t.Fatal(err)
	}
	if err := env.state.CreateConnection(ctx, state.Connection{ID: "con_agent", Name: "sample", ConnectorKey: pkg.Key, SelectedMode: "cli", CredentialMethod: "none", CredentialRef: ref, Status: "active", SafeIdentity: map[string]string{}}); err != nil {
		env.state.Close()
		t.Fatal(err)
	}
	env.state.Close()

	command := &cobra.Command{}
	command.SetContext(ctx)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	if err := executeOperationCommand(command, "sample", "hello", nil, false, operationCLIOptions{}); err != nil {
		t.Fatal(err)
	}
	var envelope invoke.Envelope
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || !envelope.OK || envelope.Data != "hello-agent" {
		t.Fatalf("unexpected automatic operation envelope: %s (%v)", output.String(), err)
	}
}

func TestRenderInvocationEnvelopeSpillsOnlyInAgentMode(t *testing.T) {
	t.Setenv("OUTFIT_AGENT", "1")
	t.Setenv("OUTFIT_AGENT_MAX_BYTES", "4096")
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := renderInvocationEnvelope(command, invoke.Envelope{Schema: invoke.Schema, OK: true, Data: strings.Repeat("large", 1024), Meta: invoke.Meta{Pages: 1}}); err != nil {
		t.Fatal(err)
	}
	var envelope invoke.Envelope
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || envelope.Meta.Spill == nil {
		t.Fatalf("large agent result was not spilled: %s (%v)", output.String(), err)
	}
	defer os.Remove(envelope.Meta.Spill.Path)
}
