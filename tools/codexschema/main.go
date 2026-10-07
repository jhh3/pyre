// Command codexschema generates the Codex app-server JSON Schema for the
// installed codex (empty temporary CODEX_HOME) and checks the methods,
// fields and enum values Pyre's Codex adapter (internal/runtime/codex) and
// account usage parser (internal/accounts) rely on.
//
//	go run ./tools/codexschema                       # generate + check (exit 1 on drift)
//	go run ./tools/codexschema -dir DIR              # check an existing generate-json-schema output
//	go run ./tools/codexschema -out DIR -index F     # keep the output; write a method index (JSON)
//	go run ./tools/codexschema -require Type.field   # ad-hoc extra field checks (repeatable)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/runtime/codex"
	"github.com/jhh3/pyre/internal/runtime/codex/codexschema"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	bin := flag.String("codex", "", "codex executable (default: codex on PATH)")
	dir := flag.String("dir", "", "existing generate-json-schema output to check (skips generation)")
	out := flag.String("out", "", "keep generated output in this directory")
	index := flag.String("index", "", "write a compact method index (JSON) to this file")
	var extra multi
	flag.Var(&extra, "require", "extra Type.field requirement (repeatable)")
	flag.Parse()

	src := *dir
	if src == "" {
		b := *bin
		if b == "" {
			p, err := exec.LookPath("codex")
			if err != nil {
				fail("codex not found on PATH (use -codex)")
			}
			b = p
		}
		src = *out
		if src == "" {
			tmp, err := os.MkdirTemp("", "codexschema")
			if err != nil {
				fail(err.Error())
			}
			defer os.RemoveAll(tmp)
			src = tmp
		} else if err := os.MkdirAll(src, 0o755); err != nil {
			fail(err.Error())
		}
		if err := codexschema.Generate(context.Background(), b, src); err != nil {
			fail(err.Error())
		}
		if v, err := exec.Command(b, "--version").Output(); err == nil {
			fmt.Printf("codex: %s\n", strings.TrimSpace(string(v)))
		}
	}
	s, err := codexschema.Load(src)
	if err != nil {
		fail(err.Error())
	}
	reqs := append(codex.SchemaRequirements(), accounts.CodexSchemaRequirements()...)
	for _, e := range extra {
		typ, field, ok := strings.Cut(e, ".")
		if !ok {
			fail("-require wants Type.field, got " + e)
		}
		reqs = append(reqs, codexschema.Req{Type: typ, Fields: []string{field}, Why: "-require"})
	}
	if *index != "" {
		b, _ := json.MarshalIndent(s.Summary(), "", " ")
		if err := os.WriteFile(*index, append(b, '\n'), 0o644); err != nil {
			fail(err.Error())
		}
	}
	probs := s.Check(reqs)
	fmt.Printf("schema: %d files, %d definitions; %d requirements\n", s.Files, len(s.Defs), len(reqs))
	if len(probs) > 0 {
		fmt.Printf("DRIFT (%d):\n  %s\n", len(probs), strings.Join(probs, "\n  "))
		os.Exit(1)
	}
	fmt.Println("ok: all methods/fields/values used by the Codex adapter and accounts parser are present")
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "codexschema:", msg)
	os.Exit(2)
}
