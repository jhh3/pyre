package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestActionNeedsCheck(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "synthtool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	raw, _ := json.Marshal(contracts.NeedsCheckParams{Tools: []string{"synthtool", "nosuchsynthtool"}, Paths: []string{tool, filepath.Join(dir, "missing"), "relative"}})
	out, err := (&Daemon{}).actionNeedsCheck(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(contracts.NeedsCheckResult)
	if !res.Tools["synthtool"] || res.Tools["nosuchsynthtool"] || !res.Paths[tool] || res.Paths[filepath.Join(dir, "missing")] || res.Paths["relative"] {
		t.Fatalf("needs.check: %+v", res)
	}
	facts := toolFacts()
	if len(facts) != len(contracts.ProbedTools) {
		t.Fatalf("facts cover %d of %d probed tools", len(facts), len(contracts.ProbedTools))
	}
}
