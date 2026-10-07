package kit

import (
	"path/filepath"
	"testing"
)

func TestScaffoldIsImmediatelyValid(t *testing.T) {
	directory, err := Scaffold(t.TempDir(), "sample-api", "http")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(directory); err != nil {
		t.Fatalf("scaffold not valid: %v", err)
	}
	if filepath.Base(directory) != "sample-api" {
		t.Fatal(directory)
	}
}

func TestDiffCallsBoundaryChangesSecurityRelevant(t *testing.T) {
	before := Package{Key: "sample-api", Modes: []Mode{{ID: "rest", Executor: Executor{Kind: "http", BaseURL: "https://one.example/"}, Operations: []Operation{{Name: "read", Class: "read"}}}}}
	after := before
	after.Modes = []Mode{{ID: "rest", Executor: Executor{Kind: "http", BaseURL: "https://two.example/"}, Operations: []Operation{{Name: "read", Class: "write"}}}}
	changes := Diff(before, after)
	if len(changes) != 2 || changes[0].Severity != "security" || changes[1].Severity != "security" {
		t.Fatalf("unexpected diff %#v", changes)
	}
}
