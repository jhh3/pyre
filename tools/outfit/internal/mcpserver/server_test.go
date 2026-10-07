package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/invoke"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/vault"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCompactLocalFacadePrefersCLIAndDispatchesReads(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Package{
		Key: "sample-api", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{ID: "no-auth", Label: "No auth", Description: "Native test CLI", Recommended: true, Custody: "none"}},
		Modes: []kit.Mode{{
			ID: "cli", Executor: kit.Executor{Kind: "cli", Binary: "printf"}, CredentialMethods: []string{"no-auth"}, DefaultCredentialMethod: "no-auth",
			Operations: []kit.Operation{
				{Name: "hello", Title: "Say hello", Description: "Read a greeting.", Class: "read", CLI: &kit.CLIOperation{Args: []string{"hello"}}},
				{Name: "change", Title: "Change greeting", Description: "Write a greeting.", Class: "write", CLI: &kit.CLIOperation{Args: []string{"changed"}}},
			},
		}},
	}
	manifest, _ := json.Marshal(pkg)
	loaded := kit.Loaded{Package: pkg, Raw: manifest}
	if err := store.InstallPackage(ctx, loaded, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, _ := vault.Open(paths)
	ref, err := secret.Put(ctx, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(ctx, state.Connection{ID: "con_1", Name: "sample", ConnectorKey: "sample-api", SelectedMode: "cli", CredentialMethod: "no-auth", Status: "active", CredentialRef: ref, SafeIdentity: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	service := connection.Service{State: store, Vault: secret}
	server, err := New(ctx, store, service)
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go server.Run(ctx, serverTransport)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 4 {
		t.Fatalf("unexpected tools %#v %v", listed, err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Name == "sample__hello" || strings.Contains(tool.Name, "__") {
			t.Fatalf("per-operation MCP tool leaked into compact facade: %s", tool.Name)
		}
	}
	sort.Strings(names)
	wantNames := []string{"outfit_describe", "outfit_get_url", "outfit_run", "outfit_search"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("unexpected compact tool set %v", names)
	}

	searched, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "outfit_search", Arguments: map[string]any{"query": "greeting"}})
	if err != nil || searched.IsError || searched.StructuredContent == nil {
		t.Fatalf("search failed %#v %v", searched, err)
	}
	assertStructuredContains(t, searched.StructuredContent, "outfit sample hello --help")

	described, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "outfit_describe", Arguments: map[string]any{"connection": "sample", "operation": "hello"}})
	if err != nil || described.IsError || described.StructuredContent == nil {
		t.Fatalf("describe failed %#v %v", described, err)
	}
	assertStructuredContains(t, described.StructuredContent, "Use the typed CLI command")
	assertStructuredContains(t, described.StructuredContent, "outfit sample hello")

	called, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "outfit_run", Arguments: map[string]any{"connection": "sample", "operation": "hello", "arguments": map[string]any{}}})
	if err != nil || called.IsError || called.StructuredContent == nil {
		t.Fatalf("call failed %#v %v", called, err)
	}
	assertStructuredContains(t, called.StructuredContent, "hello")

	blocked, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "outfit_run", Arguments: map[string]any{"connection": "sample", "operation": "change", "arguments": map[string]any{}}})
	if err != nil || !blocked.IsError || blocked.StructuredContent == nil {
		t.Fatalf("mutation should require CLI %#v %v", blocked, err)
	}
	assertStructuredContains(t, blocked.StructuredContent, "cli_required_for_mutation")
	assertStructuredContains(t, blocked.StructuredContent, "outfit sample change --write")
}

func assertStructuredContains(t *testing.T, value any, expected string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), expected) {
		t.Fatalf("structured result does not contain %q: %s", expected, encoded)
	}
}

func TestEnvelopeResultSpillsLargeFallbackPayload(t *testing.T) {
	t.Setenv("OUTFIT_AGENT_MAX_BYTES", "4096")
	result := envelopeResult(invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "large", Class: "read", Data: strings.Repeat("evidence", 1024), Meta: invoke.Meta{Pages: 1}}, false)
	prepared, ok := result.StructuredContent.(invoke.Envelope)
	if !ok || prepared.Meta.Spill == nil {
		t.Fatalf("large MCP result did not spill: %#v", result.StructuredContent)
	}
	defer os.Remove(prepared.Meta.Spill.Path)
	full, err := os.ReadFile(prepared.Meta.Spill.Path)
	if err != nil || !strings.Contains(string(full), strings.Repeat("evidence", 10)) {
		t.Fatalf("spill file missing full evidence: %v", err)
	}
}
