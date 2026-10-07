// Package codexschema loads the JSON Schema that `codex app-server
// generate-json-schema` emits for the installed Codex version and checks
// that the methods, fields and enum values Pyre's Codex adapter relies on
// still exist (WP07 / SOTA review #13). Generated output is version
// specific, so a Codex upgrade that drops or renames something Pyre decodes
// fails loudly instead of silently zeroing a field.
package codexschema

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Method index kinds (top-level schema files).
const (
	ClientRequest      = "ClientRequest"
	ClientNotification = "ClientNotification"
	ServerRequest      = "ServerRequest"
	ServerNotification = "ServerNotification"
)

// Schema is a merged view of all generated schema files.
type Schema struct {
	Defs    map[string]map[string]any    // definition name -> schema node
	Methods map[string]map[string]string // kind -> method -> params definition ("" if none)
	Files   int
}

// Generate runs `codex app-server generate-json-schema --experimental --out
// outDir` with an empty, temporary CODEX_HOME so no user configuration or
// login is read or touched.
func Generate(ctx context.Context, codexBin, outDir string) error {
	home, err := os.MkdirTemp("", "codexschema-home")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBin, "app-server", "generate-json-schema", "--experimental", "--out", outDir)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("generate-json-schema: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Load reads every *.json under dir.
func Load(dir string) (*Schema, error) {
	s := &Schema{Defs: map[string]map[string]any{}, Methods: map[string]map[string]string{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var root map[string]any
		if json.Unmarshal(b, &root) != nil {
			return nil
		}
		s.Files++
		for _, key := range []string{"definitions", "$defs"} {
			if defs, ok := root[key].(map[string]any); ok {
				for name, v := range defs {
					if m, ok := v.(map[string]any); ok {
						if _, seen := s.Defs[name]; !seen {
							s.Defs[name] = m
						}
					}
				}
			}
		}
		base := strings.TrimSuffix(filepath.Base(p), ".json")
		if _, seen := s.Defs[base]; !seen {
			s.Defs[base] = root
		}
		switch base {
		case ClientRequest, ClientNotification, ServerRequest, ServerNotification:
			s.indexMethods(base, root)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(s.Methods) == 0 {
		return nil, fmt.Errorf("codexschema: no method index files (ClientRequest.json …) under %s", dir)
	}
	return s, nil
}

func (s *Schema) indexMethods(kind string, root map[string]any) {
	m := s.Methods[kind]
	if m == nil {
		m = map[string]string{}
		s.Methods[kind] = m
	}
	for _, v := range variants(root) {
		props, _ := v["properties"].(map[string]any)
		meth, _ := props["method"].(map[string]any)
		for _, name := range stringEnum(meth) {
			params := ""
			if pm, ok := props["params"].(map[string]any); ok {
				params = refName(pm)
			}
			m[name] = params
		}
	}
}

func variants(n map[string]any) []map[string]any {
	var out []map[string]any
	for _, k := range []string{"oneOf", "anyOf"} {
		if arr, ok := n[k].([]any); ok {
			for _, x := range arr {
				if m, ok := x.(map[string]any); ok {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

func refName(n map[string]any) string {
	if r, ok := n["$ref"].(string); ok {
		return r[strings.LastIndex(r, "/")+1:]
	}
	for _, v := range variants(n) { // anyOf [$ref, null]
		if r := refName(v); r != "" {
			return r
		}
	}
	return ""
}

func stringEnum(n map[string]any) []string {
	var out []string
	if arr, ok := n["enum"].([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	if c, ok := n["const"].(string); ok {
		out = append(out, c)
	}
	return out
}

func (s *Schema) resolve(n map[string]any, depth int) map[string]any {
	for depth < 20 {
		r, ok := n["$ref"].(string)
		if !ok {
			return n
		}
		next := s.Defs[r[strings.LastIndex(r, "/")+1:]]
		if next == nil {
			return n
		}
		n = next
		depth++
	}
	return n
}

// properties returns the property names of a definition. For unions,
// variant selects the member whose "type" discriminator equals it; with no
// variant the union of all members' properties is returned.
func (s *Schema) properties(n map[string]any, variant string, depth int) map[string]bool {
	out := map[string]bool{}
	if depth > 12 {
		return out
	}
	n = s.resolve(n, 0)
	if props, ok := n["properties"].(map[string]any); ok && variant == "" {
		for k := range props {
			out[k] = true
		}
	}
	if all, ok := n["allOf"].([]any); ok {
		for _, x := range all {
			if m, ok := x.(map[string]any); ok {
				for k := range s.properties(m, variant, depth+1) {
					out[k] = true
				}
			}
		}
	}
	for _, v := range variants(n) {
		v = s.resolve(v, 0)
		if variant != "" {
			props, _ := v["properties"].(map[string]any)
			disc, _ := props["type"].(map[string]any)
			match := false
			for _, e := range stringEnum(disc) {
				match = match || e == variant
			}
			if !match {
				for k := range s.properties(v, variant, depth+1) {
					out[k] = true
				}
				continue
			}
			for k := range props {
				out[k] = true
			}
			continue
		}
		for k := range s.properties(v, "", depth+1) {
			out[k] = true
		}
	}
	return out
}

// enumValues collects string enum/const values reachable from a definition
// (including unions of string literals and object variants' "type").
func (s *Schema) enumValues(n map[string]any, depth int) map[string]bool {
	out := map[string]bool{}
	if depth > 12 {
		return out
	}
	n = s.resolve(n, 0)
	for _, e := range stringEnum(n) {
		out[e] = true
	}
	if props, ok := n["properties"].(map[string]any); ok {
		if disc, ok := props["type"].(map[string]any); ok {
			for _, e := range stringEnum(disc) {
				out[e] = true
			}
		}
		// externally tagged variants: {"acceptWithExecpolicyAmendment": {...}}
		if len(props) == 1 {
			for k := range props {
				out[k] = true
			}
		}
	}
	for _, v := range variants(n) {
		for k := range s.enumValues(v, depth+1) {
			out[k] = true
		}
	}
	return out
}

// Req is one thing Pyre relies on.
type Req struct {
	// Kind+Method: the method must exist in that index; Fields/Enum then
	// apply to its params type unless Type is set.
	Kind, Method string
	// Type is a definition name (e.g. "TurnStartParams", "ThreadItem").
	Type string
	// Variant selects a union member by its "type" discriminator.
	Variant string
	Fields  []string
	Enum    []string
	// Why is shown in failures.
	Why string
}

func (r Req) label() string {
	parts := []string{}
	if r.Method != "" {
		parts = append(parts, r.Kind+" "+r.Method)
	}
	if r.Type != "" {
		t := r.Type
		if r.Variant != "" {
			t += "[type=" + r.Variant + "]"
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " / ")
}

// Check returns one problem string per missing method/type/field/value.
func (s *Schema) Check(reqs []Req) []string {
	var probs []string
	for _, r := range reqs {
		typ := r.Type
		if r.Method != "" {
			params, ok := s.Methods[r.Kind][r.Method]
			if !ok {
				probs = append(probs, fmt.Sprintf("%s: method missing (%s)", r.label(), r.Why))
				continue
			}
			if typ == "" {
				typ = params
			}
		}
		if typ == "" {
			continue
		}
		def := s.Defs[typ]
		if def == nil {
			probs = append(probs, fmt.Sprintf("%s: definition %q missing (%s)", r.label(), typ, r.Why))
			continue
		}
		if len(r.Fields) > 0 {
			have := s.properties(def, r.Variant, 0)
			if r.Variant != "" && len(have) == 0 {
				probs = append(probs, fmt.Sprintf("%s: union member missing (%s)", r.label(), r.Why))
				continue
			}
			for _, f := range r.Fields {
				if !have[f] {
					probs = append(probs, fmt.Sprintf("%s: field %q missing (%s)", r.label(), f, r.Why))
				}
			}
		}
		if len(r.Enum) > 0 {
			have := s.enumValues(def, 0)
			for _, e := range r.Enum {
				if !have[e] {
					probs = append(probs, fmt.Sprintf("%s: value %q missing (%s)", r.label(), e, r.Why))
				}
			}
		}
	}
	sort.Strings(probs)
	return probs
}

// Index is a compact, committable summary of a schema.
type Index struct {
	Methods map[string][]string `json:"methods"`
	Files   int                 `json:"files"`
	Defs    int                 `json:"definitions"`
}

// Summary returns method names per kind.
func (s *Schema) Summary() Index {
	ix := Index{Methods: map[string][]string{}, Files: s.Files, Defs: len(s.Defs)}
	for k, m := range s.Methods {
		for name := range m {
			ix.Methods[k] = append(ix.Methods[k], name)
		}
		sort.Strings(ix.Methods[k])
	}
	return ix
}
