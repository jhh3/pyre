package cli

import (
	"encoding"
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OutputSchemaFor derives a JSON Schema (2020-12 dialect, the MCP
// 2025-11-25 default) for the JSON encoding of t, which must be a struct.
// It follows encoding/json exactly as Pyre's contract types use it:
//
//   - json tags name fields; "-" skips; fields without omitempty are required;
//   - anonymous embedded structs without a tag are flattened;
//   - pointers, slices and maps may encode as null and are nullable;
//   - time.Time is an RFC 3339 string; json.RawMessage and interfaces are any;
//   - other types with custom JSON/text marshalers are left unconstrained.
//
// Named struct types live in $defs (so recursive types such as Principal
// terminate); the root object is inlined because MCP requires an
// outputSchema of type "object". Unknown properties are allowed so adding
// a contract field is not a breaking schema change. Pyre does not use the
// invopop/jsonschema reflector here because it does not model encoding/json
// nullability, and clients that validate structuredContent would reject a
// null slice or pointer.
func OutputSchemaFor(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	g := &schemaGen{defs: map[string]map[string]any{}, names: map[reflect.Type]string{}}
	root := g.structSchema(t)
	if len(g.defs) > 0 {
		defs := map[string]any{}
		for k, v := range g.defs {
			defs[k] = v
		}
		root["$defs"] = defs
	}
	return root
}

type schemaGen struct {
	defs  map[string]map[string]any
	names map[reflect.Type]string
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	rawType       = reflect.TypeFor[json.RawMessage]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
	pkgPathRe     = regexp.MustCompile(`[A-Za-z0-9_./-]+\.`)
	nonIdentRe    = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

func nullable(s map[string]any) map[string]any {
	if ty, ok := s["type"].(string); ok && len(s) <= 3 {
		out := map[string]any{}
		for k, v := range s {
			out[k] = v
		}
		out["type"] = []any{ty, "null"}
		return out
	}
	return map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
}

func (g *schemaGen) defName(t reflect.Type) string {
	if n, ok := g.names[t]; ok {
		return n
	}
	base := strings.Trim(nonIdentRe.ReplaceAllString(pkgPathRe.ReplaceAllString(t.Name(), ""), "_"), "_")
	if base == "" {
		base = "Anon"
	}
	name := base
	for i := 2; ; i++ {
		taken := false
		for _, n := range g.names {
			taken = taken || n == name
		}
		if !taken {
			break
		}
		name = base + "_" + strconv.Itoa(i)
	}
	g.names[t] = name
	return name
}

func (g *schemaGen) schema(t reflect.Type) map[string]any {
	if t == rawType {
		return map[string]any{}
	}
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	if t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface &&
		(t.Implements(marshalerType) || reflect.PointerTo(t).Implements(marshalerType) ||
			t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler)) {
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return nullable(g.schema(t.Elem()))
	case reflect.Interface:
		return map[string]any{}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": []any{"string", "null"}} // base64
		}
		return map[string]any{"type": []any{"array", "null"}, "items": g.schema(t.Elem())}
	case reflect.Array:
		return map[string]any{"type": "array", "items": g.schema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": []any{"object", "null"}, "additionalProperties": g.schema(t.Elem())}
	case reflect.Struct:
		if t.Name() == "" {
			return g.structSchema(t)
		}
		name := g.defName(t)
		if _, ok := g.defs[name]; !ok {
			g.defs[name] = map[string]any{} // placeholder: breaks recursion
			g.defs[name] = g.structSchema(t)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	}
	return map[string]any{}
}

func (g *schemaGen) structSchema(t reflect.Type) map[string]any {
	properties := map[string]any{}
	var required []any
	g.fields(t, properties, &required)
	s := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func (g *schemaGen) fields(t reflect.Type, properties map[string]any, required *[]any) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			et := ft
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				g.fields(et, properties, required)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		properties[name] = g.schema(ft)
		if !strings.Contains(","+opts+",", ",omitempty,") && !strings.Contains(","+opts+",", ",omitzero,") {
			*required = append(*required, name)
		}
	}
}
