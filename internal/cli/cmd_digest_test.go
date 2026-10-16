package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/digest"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func digestApp(t *testing.T) (*App, env, string) {
	t.Helper()
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	e := newEnv(t, nil)
	state := digest.StateFile(e[EnvConfig])
	in := digest.SyntheticInput()
	if err := digest.SaveLastSeen(state, digest.LastSeen{Cursor: in.SinceCursor, At: in.Since}); err != nil {
		t.Fatal(err)
	}
	return &App{API: &digest.FixtureAPI{API: b, In: in}}, e, state
}

func TestDigestCommandSinceLastMarksSeen(t *testing.T) {
	app, e, state := digestApp(t)
	r := run(t, e, app, "digest")
	expectCode(t, r, 0)
	for _, want := range []string{
		"While you were away · since ",
		"5 agents, 2 need you",
		"Needs you\n  ! synth-api-refactor",
		"Approve: go test -bench ./...",
		"  ✗ synth-flaky-ci",
		"Failed: no eligible Codex account",
		"Done\n  ✓ synth-codex-check",
		"  ✓ synth-docs-pass     Updated the digest docs and added tests for ranking, e.g. needs-you first.",
		"Still working\n  ● synth-search-index",
		"pyre://approval/apr_synthA",
		"pyre://session/ses_synthC?run=run_synthC&seq=812",
		"artifact: synth digest spec v2  pyre://artifact_version/ver_synthC2",
		"files: docs/digest.md, internal/digest/digest_test.go",
		"Also needs you\n  ! synth-linux was offline for 12 minutes",
		"2 quiet sessions collapsed",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q\n%s", want, r)
		}
	}
	ls, ok := digest.LoadLastSeen(state)
	if !ok || ls.Cursor != digest.SyntheticInput().HeadCursor {
		t.Fatalf("mark not moved to the head: %+v", ls)
	}
	// Nothing new since the mark: a calm empty digest.
	r = run(t, e, app, "digest")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "Nothing happened") {
		t.Fatalf("second run:\n%s", r)
	}
}

func TestDigestCommandExplicitSinceJSONKeepsMark(t *testing.T) {
	app, e, state := digestApp(t)
	before, _ := os.ReadFile(state)
	r := run(t, e, app, "digest", "--since", "2026-10-05T09:00:00Z", "--json")
	expectCode(t, r, 0)
	var d digest.Digest
	if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
		t.Fatalf("json: %v\n%s", err, r)
	}
	var ids []string
	for _, a := range d.Agents {
		ids = append(ids, a.SessionID+":"+string(a.Rank))
	}
	if got := strings.Join(ids, " "); got != "ses_synthA:needs_you ses_synthB:needs_you ses_synthH:done ses_synthC:done ses_synthD:working" {
		t.Fatalf("order: %s", got)
	}
	after, _ := os.ReadFile(state)
	if string(before) != string(after) {
		t.Fatal("an explicit --since must not move the last-seen mark")
	}
	r = run(t, e, app, "digest", "--since", "soon")
	expectCode(t, r, ExitUsage)
}

func TestDigestSummarizeIsOptionalAndNeverFatal(t *testing.T) {
	app, e, _ := digestApp(t)
	// The fake "model" echoes its prompt: the summary is printed above the digest.
	r := run(t, e.with(EnvDigestSummarizeCmd, "cat"), app, "digest", "--no-mark", "--summarize")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "  You are catching the owner") || !strings.Contains(r.stdout, "Needs you\n  ! synth-api-refactor") {
		t.Fatalf("summarize:\n%s", r)
	}
	app2, e2, _ := digestApp(t)
	r = run(t, e2.with(EnvDigestSummarizeCmd, "false"), app2, "digest", "--no-mark", "--summarize")
	expectCode(t, r, 0)
	if !strings.Contains(r.stderr, "--summarize failed") || !strings.Contains(r.stdout, "synth-api-refactor") {
		t.Fatalf("failed summarizer must fall back to the deterministic digest:\n%s", r)
	}
}

func TestMCPGetDigestReadOnly(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	state := filepath.Join(t.TempDir(), "digest-state.json")
	old := mcpDigestStatePath
	mcpDigestStatePath = func() string { return state }
	t.Cleanup(func() { mcpDigestStatePath = old })
	cl := startMCP(t, &Ops{API: &digest.FixtureAPI{API: b, In: digest.SyntheticInput()}, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	out, isErr := cl.tool("get_digest", map[string]any{"since": "2026-10-05T09:00:00Z", "format": "json"})
	var d digest.Digest
	if isErr || json.Unmarshal(out, &d) != nil || len(d.Agents) != 5 || d.Agents[0].Link != "pyre://approval/apr_synthA" {
		t.Fatalf("get_digest: %s", out)
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("get_digest must not write the owner's last-seen mark")
	}
	var ann *ToolAnnotations
	for _, tl := range MCPTools() {
		if tl.Name == "get_digest" {
			ann = tl.Annotations
		}
	}
	if ann == nil || !ann.ReadOnlyHint || ann.DestructiveHint {
		t.Fatalf("get_digest annotations: %+v", ann)
	}
}
