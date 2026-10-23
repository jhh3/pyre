package main

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// The pyred and pyre binaries embed the tz database: notes and asks show
// Pacific times (America/Los_Angeles) on hosts without one (Windows), where
// a fixed offset would be an hour off half the year.
func TestBinariesEmbedTZData(t *testing.T) {
	for _, f := range []string{"main.go", "../pyre/main.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, im := range file.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == "time/tzdata" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not import time/tzdata", f)
		}
	}
}
