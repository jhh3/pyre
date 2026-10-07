package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/runtime/codex/codexschema"
)

// allRequirements: the adapter's requirements plus the fields internal/accounts
// decodes from account/rateLimits/read.
func allRequirements() []codexschema.Req {
	return append(SchemaRequirements(), accounts.CodexSchemaRequirements()...)
}

// generatedSchema runs the installed codex (PYRE_CODEX_BIN or PATH) once per
// test binary; skips when codex is absent.
func generatedSchema(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("PYRE_CODEX_BIN")
	if bin == "" {
		p, err := exec.LookPath("codex")
		if err != nil {
			t.Skip("codex not installed; schema drift check skipped")
		}
		bin = p
	}
	dir := t.TempDir()
	if err := codexschema.Generate(context.Background(), bin, dir); err != nil {
		t.Fatalf("generate schema: %v", err)
	}
	if out, err := exec.Command(bin, "--version").Output(); err == nil {
		t.Logf("codex %s", strings.TrimSpace(string(out)))
	}
	return dir
}

// TestCodexSchemaDrift: everything the adapter sends/decodes exists in the
// installed Codex's app-server schema.
func TestCodexSchemaDrift(t *testing.T) {
	dir := generatedSchema(t)
	s, err := codexschema.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	reqs := allRequirements()
	if probs := s.Check(reqs); len(probs) > 0 {
		t.Fatalf("Codex app-server schema drift (%d problem(s)) — update internal/runtime/codex/types.go and the adapter:\n  %s",
			len(probs), strings.Join(probs, "\n  "))
	}
	t.Logf("%d requirements satisfied (%d schema files, %d definitions)", len(reqs), s.Files, len(s.Defs))
}

// TestCodexSchemaDriftDetectsRemoval deliberately removes a used field and a
// used method from the generated schema; the check must fail.
func TestCodexSchemaDriftDetectsRemoval(t *testing.T) {
	dir := generatedSchema(t)
	removed := 0
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		b, _ := os.ReadFile(p)
		var root map[string]any
		if json.Unmarshal(b, &root) != nil {
			return nil
		}
		changed := false
		strip := func(def map[string]any) {
			if props, ok := def["properties"].(map[string]any); ok {
				if _, ok := props["clientUserMessageId"]; ok {
					delete(props, "clientUserMessageId")
					changed = true
					removed++
				}
			}
		}
		if defs, ok := root["definitions"].(map[string]any); ok {
			if d, ok := defs["TurnStartParams"].(map[string]any); ok {
				strip(d)
			}
		}
		if filepath.Base(p) == "TurnStartParams.json" {
			strip(root)
		}
		if filepath.Base(p) == "ClientRequest.json" {
			var keep []any
			for _, v := range root["oneOf"].([]any) {
				if strings.Contains(mustJSON(v), `"turn/steer"`) {
					changed = true
					continue
				}
				keep = append(keep, v)
			}
			root["oneOf"] = keep
		}
		if changed {
			nb, _ := json.Marshal(root)
			return os.WriteFile(p, nb, 0o600)
		}
		return nil
	})
	if err != nil || removed == 0 {
		t.Fatalf("mutation failed: removed=%d err=%v", removed, err)
	}
	s, err := codexschema.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	probs := strings.Join(s.Check(allRequirements()), "\n")
	for _, want := range []string{`TurnStartParams: field "clientUserMessageId" missing`, `ClientRequest turn/steer: method missing`} {
		if !strings.Contains(probs, want) {
			t.Fatalf("drift not detected: want %q in\n%s", want, probs)
		}
	}
}

// TestCodexSchemaDriftCoversAccounts: renaming a RateLimitSnapshot field that
// only internal/accounts decodes is caught (the class of bug where accounts
// decoded a non-existent "limitReached"), and a decode of a field the
// installed Codex does not have is flagged.
func TestCodexSchemaDriftCoversAccounts(t *testing.T) {
	dir := generatedSchema(t)
	s, err := codexschema.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []codexschema.Req{{Type: "RateLimitSnapshot", Fields: []string{"limitReached"}, Why: "legacy"}}
	if probs := s.Check(legacy); len(probs) != 1 || !strings.Contains(probs[0], `RateLimitSnapshot: field "limitReached" missing`) {
		t.Fatalf("legacy limitReached not flagged: %v", probs)
	}
	snap, ok := s.Defs["RateLimitSnapshot"]
	if !ok {
		t.Fatal("RateLimitSnapshot definition missing")
	}
	for _, f := range []string{"spendControlReached"} { // decoded only by internal/accounts
		props, _ := snap["properties"].(map[string]any)
		if _, ok := props[f]; !ok {
			t.Fatalf("%s not in generated RateLimitSnapshot", f)
		}
		delete(props, f)
	}
	probs := strings.Join(s.Check(allRequirements()), "\n")
	if !strings.Contains(probs, `RateLimitSnapshot: field "spendControlReached" missing (internal/accounts ParseCodexRateLimits)`) {
		t.Fatalf("accounts-only field removal not detected:\n%s", probs)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// TestCodexSchemaCheckerFixture exercises the checker without codex.
func TestCodexSchemaCheckerFixture(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ClientRequest.json", `{"oneOf":[{"properties":{"method":{"enum":["turn/start"]},"params":{"$ref":"#/definitions/TurnStartParams"}}}],
	 "definitions":{"TurnStartParams":{"properties":{"threadId":{},"input":{}}},
	  "Item":{"oneOf":[{"properties":{"type":{"enum":["userMessage"]},"clientId":{}}},{"properties":{"type":{"enum":["agentMessage"]},"text":{}}}]},
	  "Status":{"anyOf":[{"$ref":"#/definitions/Lit"},{"type":"null"}]},"Lit":{"enum":["idle","active"]}}}`)
	s, err := codexschema.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	probs := s.Check([]codexschema.Req{
		{Kind: codexschema.ClientRequest, Method: "turn/start", Fields: []string{"threadId", "clientUserMessageId"}},
		{Kind: codexschema.ClientRequest, Method: "turn/steer"},
		{Type: "Item", Variant: "userMessage", Fields: []string{"clientId", "text"}},
		{Type: "Item", Variant: "agentMessage", Fields: []string{"text"}},
		{Type: "Status", Enum: []string{"idle", "notLoaded"}},
		{Type: "Missing", Fields: []string{"x"}},
	})
	want := []string{
		`ClientRequest turn/start: field "clientUserMessageId" missing`,
		`ClientRequest turn/steer: method missing`,
		`Item[type=userMessage]: field "text" missing`,
		`Status: value "notLoaded" missing`,
		`Missing: definition "Missing" missing`,
	}
	joined := strings.Join(probs, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Fatalf("want %q in\n%s", w, joined)
		}
	}
	if len(probs) != len(want) {
		t.Fatalf("unexpected problems:\n%s", joined)
	}
}
