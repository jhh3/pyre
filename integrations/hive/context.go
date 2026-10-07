package hive

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// PointerRequest asks for a few memory pointers to append to a launch brief.
type PointerRequest struct {
	ProjectID string
	SessionID string
	// Query is a hint (e.g. the session title or task). Without it, pointers
	// are limited to this project's bridged notes; Pyre never dumps global
	// history into a new session.
	Query    string
	MaxItems int           // default 5
	MaxBytes int           // default 1 KiB
	Timeout  time.Duration // default 2s; a launch never waits longer
}

// Pointer is one retrieval reference (title + ref), not content.
type Pointer struct {
	Title         string    `json:"title"`
	Ref           string    `json:"ref"` // "hive:<uid>" — resolve with memory.get
	Kind          string    `json:"kind"`
	At            time.Time `json:"at"`
	Authoritative bool      `json:"authoritative"`
	NoteID        string    `json:"note_id,omitempty"` // hub note when bridged
	Line          string    `json:"line"`              // rendered brief line
}

// Pointers is a bounded block for LaunchContext.
type Pointers struct {
	Text       string                `json:"text"`
	Bytes      int                   `json:"bytes"`
	Truncated  bool                  `json:"truncated"`
	Items      []Pointer             `json:"items"`
	References []contracts.ObjectRef `json:"references"` // hub notes among the pointers
	Freshness  *contracts.Freshness  `json:"freshness,omitempty"`
}

const pointerHeader = "Memory pointers (Hive; use memory.get <ref> for detail; not authoritative unless marked):\n"

// LaunchPointers returns a few memory pointers for a new session. It never
// returns an error and never blocks longer than the timeout: when Hive is
// slow or absent the result is empty with a stale Freshness explaining why.
func (c *Client) LaunchPointers(ctx context.Context, req PointerRequest) Pointers {
	if req.MaxItems <= 0 {
		req.MaxItems = 5
	}
	if req.MaxBytes <= 0 {
		req.MaxBytes = 1024
	}
	if req.Timeout <= 0 {
		req.Timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	out := Pointers{Items: []Pointer{}, References: []contracts.ObjectRef{}}

	query := strings.TrimSpace(req.Query)
	if query == "" && req.ProjectID != "" {
		// Bridged notes carry the project id in their marker, which is one FTS
		// token: this finds the project's explicit notes/handoffs/decisions.
		query = req.ProjectID
	}
	if query == "" {
		return out
	}
	res, err := c.Search(ctx, contracts.MemorySearchParams{Query: query, ProjectID: req.ProjectID, Limit: req.MaxItems * 2})
	if err != nil {
		out.Freshness = &contracts.Freshness{ObservedAt: c.env.clock().Now(), Source: "hive", Stale: true,
			Reason: "memory pointers omitted: " + truncBytes(err.Error(), 200)}
		return out
	}
	f := res.Freshness
	out.Freshness = &f

	var candidates []Pointer
	for _, h := range res.Hits {
		if req.Query == "" && req.ProjectID != "" && h.ProjectID != req.ProjectID {
			continue // no query hint: project material only
		}
		p := Pointer{Title: h.Title, Ref: "hive:" + h.ID, Kind: h.Kind, At: h.At, Authoritative: h.Authoritative}
		if m, ok := markerFromSource(h.Source); ok {
			p.NoteID = m
		}
		tag := h.Kind
		if h.Authoritative {
			tag += ", authoritative"
		}
		p.Line = fmt.Sprintf("- %s %s: %s [%s]\n", h.At.Format("2006-01-02"), tag, truncBytes(h.Title, 120), p.Ref)
		candidates = append(candidates, p)
	}
	items, text, truncated := fitPointers(candidates, req.MaxItems, req.MaxBytes, nil)
	out.Items, out.Text, out.Bytes = items, text, len(text)
	out.Truncated = truncated || (res.Truncated && len(items) == req.MaxItems)
	for _, p := range items {
		if p.NoteID != "" {
			out.References = append(out.References, contracts.ObjectRef{Kind: contracts.KindNote, ID: p.NoteID})
		}
	}
	return out
}

// fitPointers keeps whole lines within maxItems/maxBytes (header included),
// skipping pointers to hub notes listed in skip.
func fitPointers(cands []Pointer, maxItems, maxBytes int, skip map[string]bool) ([]Pointer, string, bool) {
	items := []Pointer{}
	var b strings.Builder
	truncated := false
	for _, p := range cands {
		if p.NoteID != "" && skip[p.NoteID] {
			continue
		}
		if len(items) >= maxItems {
			truncated = true
			break
		}
		need := len(p.Line)
		if b.Len() == 0 {
			need += len(pointerHeader)
		}
		if b.Len()+need > maxBytes {
			truncated = true
			break
		}
		if b.Len() == 0 {
			b.WriteString(pointerHeader)
		}
		b.WriteString(p.Line)
		items = append(items, p)
	}
	return items, b.String(), truncated
}

func markerFromSource(src string) (string, bool) {
	for _, f := range strings.Fields(src) {
		if v, ok := strings.CutPrefix(f, "pyre_note="); ok {
			return v, true
		}
	}
	return "", false
}

// AppendPointers adds memory pointers to a hub LaunchContext without
// exceeding maxBytes for the whole brief (0 = the contract default of
// 8 KiB). Pointers to hub notes the brief already references are skipped,
// whole lines are dropped from the end when out of room, and the Hive
// freshness replaces the hub's placeholder memory freshness.
func AppendPointers(lc *contracts.LaunchContext, p Pointers, maxBytes int) {
	if maxBytes <= 0 {
		maxBytes = 8 << 10
	}
	if p.Freshness != nil {
		f := *p.Freshness
		lc.MemoryFreshness = &f
	}
	if len(p.Items) == 0 {
		return
	}
	skip := map[string]bool{}
	for _, r := range lc.References {
		if r.Kind == contracts.KindNote {
			skip[r.ID] = true
		}
	}
	sep := ""
	switch {
	case lc.Text == "" || strings.HasSuffix(lc.Text, "\n\n"):
	case strings.HasSuffix(lc.Text, "\n"):
		sep = "\n"
	default:
		sep = "\n\n"
	}
	room := maxBytes - len(lc.Text) - len(sep)
	items, text, truncated := fitPointers(p.Items, len(p.Items), room, skip)
	if truncated || p.Truncated {
		lc.Truncated = true
	}
	if text == "" {
		return
	}
	lc.Text += sep + text
	lc.Bytes = len(lc.Text)
	for _, it := range items {
		if it.NoteID != "" {
			lc.References = append(lc.References, contracts.ObjectRef{Kind: contracts.KindNote, ID: it.NoteID})
		}
	}
}
