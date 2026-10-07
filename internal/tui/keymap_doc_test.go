package tui

import (
	"os"
	"strings"
	"testing"
)

// TestDesignDocListsEveryAction keeps the keymap reference in
// tests/tui/DESIGN.md in sync with actionDefs.
//
//	PYRE_PRINT_KEYS=1 go test ./internal/tui -run TestDesignDoc -v   # print the reference
func TestDesignDocListsEveryAction(t *testing.T) {
	if os.Getenv("PYRE_PRINT_KEYS") != "" {
		t.Log("\n" + DefaultKeymap().Reference())
	}
	b, err := os.ReadFile("../../tests/tui/DESIGN.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, a := range actionDefs {
		if !strings.Contains(doc, a.id) {
			t.Errorf("DESIGN.md keymap reference lacks %s", a.id)
		}
	}
}
