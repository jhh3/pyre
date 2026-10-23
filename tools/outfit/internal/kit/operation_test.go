package kit

import (
	"encoding/json"
	"math"
	"testing"
)

func TestEffectiveInputCompilesTemplatesAndPreservesExplicitTypes(t *testing.T) {
	operation := Operation{
		Input: map[string]any{"properties": map[string]any{"number": map[string]any{"type": "integer", "description": "PR number"}}},
		CLI: &CLIOperation{
			Args:         []string{"api", "repos/{{arg.owner}}/{{arg.repo}}/pulls/{{arg.number}}"},
			OptionalArgs: []CLIOptionalArg{{Flag: "--limit", Argument: "limit"}, {Flag: "--verbose", Argument: "verbose", Boolean: true}},
		},
	}
	schema := EffectiveInput(operation)
	properties := schema["properties"].(map[string]any)
	for _, name := range []string{"owner", "repo", "number", "limit", "verbose"} {
		if properties[name] == nil {
			t.Fatalf("missing inferred property %s: %#v", name, schema)
		}
	}
	if properties["number"].(map[string]any)["type"] != "integer" || properties["verbose"].(map[string]any)["type"] != "boolean" {
		t.Fatalf("explicit/boolean types were not preserved: %#v", properties)
	}
	required := schema["required"].([]string)
	if len(required) != 3 || required[0] != "number" || required[1] != "owner" || required[2] != "repo" {
		t.Fatalf("required = %#v", required)
	}
}

func TestParseNormalizesBlessedGitHubTemplateInputs(t *testing.T) {
	raw := []byte(`apiVersion: outfit.jhh3.dev/connector/v1alpha2
key: abc
display: ABC
version: 1
credentialMethods:
  - {id: none, label: None, description: No auth., recommended: true, custody: none}
modes:
  - id: cli
    label: CLI
    description: CLI mode.
    recommendation: {rank: 1, reason: Exact local tool.}
    executor: {kind: cli, binary: abc}
    credentialMethods: [none]
    defaultCredentialMethod: none
    operations:
      - {name: get_item, description: Read item., class: read, cli: {args: [get, '{{arg.id}}']}}
skill: {summary: Use ABC.}
`)
	loaded, err := Parse(raw, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	properties := loaded.Package.Modes[0].Operations[0].Input["properties"].(map[string]any)
	if properties["id"] == nil {
		t.Fatalf("normalized schema = %#v", loaded.Package.Modes[0].Operations[0].Input)
	}
}

func TestArgumentAliasesAreGeneratedOnlyWhenUnambiguous(t *testing.T) {
	properties := map[string]any{
		"channel_id": map[string]any{"type": "string"},
		"message_ts": map[string]any{"type": "string"},
		"thread_ts":  map[string]any{"type": "string"},
		"query":      map[string]any{"type": "string"},
	}
	aliases := ArgumentAliases(properties)
	if !contains(aliases["channel_id"], "channel") || !contains(aliases["query"], "q") {
		t.Fatalf("missing friendly aliases: %#v", aliases)
	}
	if contains(aliases["message_ts"], "ts") || contains(aliases["thread_ts"], "ts") {
		t.Fatalf("ambiguous ts alias was emitted: %#v", aliases)
	}
}

func TestAuditOperationSchemaRejectsWrongTypedDefaultsAndExamples(t *testing.T) {
	operation := Operation{Input: map[string]any{"type": "object", "properties": map[string]any{
		"limit": map[string]any{"type": "integer", "description": "Limit", "default": "ten"},
		"query": map[string]any{"type": "string", "description": "Query", "examples": []any{12}},
	}}}
	issues := AuditOperationSchema(operation)
	if len(issues) != 2 {
		t.Fatalf("schema issues = %v", issues)
	}
}

func TestAuditOperationSchemaRejectsInvalidEnums(t *testing.T) {
	operation := Operation{Input: map[string]any{"type": "object", "properties": map[string]any{
		"mode":  map[string]any{"type": "string", "description": "Mode", "enum": []any{"fast", 42}, "default": "slow"},
		"empty": map[string]any{"type": "string", "description": "Empty", "enum": []any{}},
	}}}
	issues := AuditOperationSchema(operation)
	if len(issues) != 3 {
		t.Fatalf("schema issues = %v", issues)
	}
}

func TestSchemaIntegerAcceptsWholeJSONNumbers(t *testing.T) {
	for _, value := range []any{10, int64(10), uint8(3), 10.0, float32(2), json.Number("25"), json.Number("1e2")} {
		if !schemaValueMatches("integer", value) {
			t.Errorf("integer rejected %#v (%T)", value, value)
		}
	}
	for _, value := range []any{1.5, json.Number("2.5"), math.NaN(), math.Inf(1), "10", true} {
		if schemaValueMatches("integer", value) {
			t.Errorf("integer accepted %#v (%T)", value, value)
		}
	}
	if !schemaValueMatches("number", json.Number("1.5")) || schemaValueMatches("number", json.Number("x")) {
		t.Error("number check mishandles json.Number")
	}
}
