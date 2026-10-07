package configuration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// omap is an order-preserving JSON object used when Pyre must edit managed
// keys inside a user-owned JSON file: unrelated keys keep their order and
// values, so the diff shows only the managed change.
type omap struct {
	keys []string
	vals map[string]any // values: *omap, []any, string, json.Number, bool, nil
}

func newOmap() *omap { return &omap{vals: map[string]any{}} }

func parseOrdered(b []byte) (*omap, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return newOmap(), nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeOrdered(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON document")
	}
	m, ok := v.(*omap)
	if !ok {
		return nil, fmt.Errorf("top-level JSON value is not an object")
	}
	return m, nil
}

func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := newOmap()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				m.set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return m, nil
		case '[':
			var a []any
			for dec.More() {
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			if a == nil {
				a = []any{}
			}
			return a, nil
		}
	}
	return tok, nil
}

func (m *omap) set(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

func (m *omap) del(k string) {
	if _, ok := m.vals[k]; !ok {
		return
	}
	delete(m.vals, k)
	for i, x := range m.keys {
		if x == k {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
}

// getPath returns the value at segs.
func (m *omap) getPath(segs []string) (any, bool) {
	cur := m
	for i, s := range segs {
		v, ok := cur.vals[s]
		if !ok {
			return nil, false
		}
		if i == len(segs)-1 {
			return v, true
		}
		next, ok := v.(*omap)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

// setPath sets segs, creating intermediate objects.
func (m *omap) setPath(segs []string, v any) error {
	cur := m
	for i, s := range segs {
		if i == len(segs)-1 {
			cur.set(s, toOrdered(v))
			return nil
		}
		next, ok := cur.vals[s].(*omap)
		if !ok {
			if _, exists := cur.vals[s]; exists {
				return fmt.Errorf("%s is not an object", JoinKey(segs[:i+1]...))
			}
			next = newOmap()
			cur.set(s, next)
		}
		cur = next
	}
	return nil
}

// delPath deletes segs and prunes objects that become empty because of it.
func (m *omap) delPath(segs []string) {
	if len(segs) == 1 {
		m.del(segs[0])
		return
	}
	next, ok := m.vals[segs[0]].(*omap)
	if !ok {
		return
	}
	next.delPath(segs[1:])
	if len(next.keys) == 0 {
		m.del(segs[0])
	}
}

func toOrdered(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := newOmap()
		for _, k := range sortedMapKeys(t) {
			m.set(k, toOrdered(t[k]))
		}
		return m
	case []any:
		a := make([]any, len(t))
		for i, x := range t {
			a[i] = toOrdered(x)
		}
		return a
	}
	return v
}

func (m *omap) plain() map[string]any {
	out := make(map[string]any, len(m.keys))
	for _, k := range m.keys {
		out[k] = plainValue(m.vals[k])
	}
	return out
}

func plainValue(v any) any {
	switch t := v.(type) {
	case *omap:
		return t.plain()
	case []any:
		a := make([]any, len(t))
		for i, x := range t {
			a[i] = plainValue(x)
		}
		return a
	case json.Number:
		return normalize(t)
	}
	return v
}

// marshal renders the object with two-space indentation.
func (m *omap) marshal() []byte {
	var b bytes.Buffer
	writeOrdered(&b, m, 0)
	b.WriteByte('\n')
	return b.Bytes()
}

func writeOrdered(b *bytes.Buffer, v any, depth int) {
	ind := strings.Repeat("  ", depth)
	switch t := v.(type) {
	case *omap:
		if len(t.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range t.keys {
			b.WriteString(ind + "  ")
			writeScalar(b, k)
			b.WriteString(": ")
			writeOrdered(b, t.vals[k], depth+1)
			if i < len(t.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(ind + "}")
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, x := range t {
			b.WriteString(ind + "  ")
			writeOrdered(b, x, depth+1)
			if i < len(t)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(ind + "]")
	default:
		writeScalar(b, t)
	}
}

func writeScalar(b *bytes.Buffer, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b.Write(bytes.TrimRight(buf.Bytes(), "\n"))
}

func sortedMapKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sortStrings(ks)
	return ks
}
