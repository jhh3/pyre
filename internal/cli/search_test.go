package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestSearchCommandGroupsBySessionAndListsOfflineMachines(t *testing.T) {
	h := newHub(t)
	if err := h.b.SetNodeConnected("node_synth-linux", false); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, h)

	r := run(t, e, nil, "--json", "search", "synthetic", "task")
	expectCode(t, r, 0)
	resp := mustJSON[c.SearchResponse](t, r)
	if len(resp.Hits) == 0 || resp.Query != "synthetic task" {
		t.Fatalf("hits: %s", r)
	}
	offline := false
	for _, n := range resp.Nodes {
		offline = offline || (n.NodeID == "node_synth-linux" && n.State == c.SearchNodeOffline)
	}
	if !offline {
		t.Fatalf("offline node not reported: %+v", resp.Nodes)
	}
	for _, hit := range resp.Hits {
		if hit.NodeID == "node_synth-linux" {
			t.Fatalf("offline node searched: %+v", hit)
		}
		if !strings.Contains(hit.Snippet, c.SearchMarkOpen) {
			t.Fatalf("snippet not marked: %q", hit.Snippet)
		}
	}

	r = run(t, e, nil, "search", "synthetic", "task", "--since", "3650d")
	expectCode(t, r, 0)
	for _, want := range []string{"synthetic task — ", "pyre read ses_", "--cursor 1", "not fully searched:", "synth-linux (not searched (offline))", "you "} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("human output missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "\x1b[") {
		t.Fatal("color codes without a terminal")
	}

	expectCode(t, run(t, e, nil, "search", "x", "--since", "soon"), ExitUsage)
	expectCode(t, run(t, e, nil, "search"), ExitUsage)
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d": now.AddDate(0, 0, -7), "36h": now.Add(-36 * time.Hour), "90m": now.Add(-90 * time.Minute), "2w": now.AddDate(0, 0, -14),
	} {
		if got, err := parseSince(in, now); err != nil || !got.Equal(want) {
			t.Fatalf("%s: %v %v", in, got, err)
		}
	}
	if _, err := parseSince("2026-10-01", now); err != nil {
		t.Fatal(err)
	}
}

func TestMCPSearchToolIsReadOnly(t *testing.T) {
	var found *MCPTool
	for _, tl := range MCPTools() {
		if tl.Name == "search" {
			tl := tl
			found = &tl
		}
	}
	if found == nil || found.Annotations == nil || !found.Annotations.ReadOnlyHint || found.Annotations.DestructiveHint {
		t.Fatalf("search tool %+v", found)
	}
	h := newHub(t)
	o := &Ops{API: h.b}
	resp, err := o.Search(context.Background(), SearchParams{Query: "synthetic", Limit: 2})
	if err != nil || len(resp.Hits) != 2 || !resp.Truncated {
		t.Fatalf("ops search %+v %v", resp, err)
	}
}
