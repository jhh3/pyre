package digest

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// SyntheticNow is the fixed "now" of the synthetic fixture.
var SyntheticNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// SyntheticInput is an obviously synthetic scenario (synth-* names, fake
// IDs) used by tests, the TUI demo and the shared fixture in
// tests/fixtures/digest: one agent waiting on an approval, one failed run,
// one finished agent that changed files and published an artifact, one
// still working, a quiet shell, an archived session, a finished Codex run
// without transcript, a machine alert and noise to collapse.
func SyntheticInput() Input {
	now := SyntheticNow
	since := now.Add(-3 * time.Hour)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, time.UTC) }
	ptr := func(i int) *int { return &i }
	tp := func(t time.Time) *time.Time { return &t }
	sess := func(id, title string, h c.Harness, node string, run *c.Run, archived bool) c.SessionView {
		sv := c.SessionView{Session: c.Session{SessionID: id, Title: title, Harness: h, CreatedAt: at(8, 0), Archived: archived}, NodeName: node, Run: run}
		if run != nil {
			run.SessionID = id
			run.Harness = h
			run.NodeID = "node_" + node
			sv.Session.CurrentRunID = run.RunID
		}
		return sv
	}
	run := func(id string, st c.RunState, cwd string) *c.Run {
		return &c.Run{RunID: id, State: st, Cwd: cwd, StartedAt: at(8, 0), Capabilities: []string{c.CapTranscript, c.CapScreen}}
	}
	runFailed := run("run_synthB", c.RunFailed, "/synth/ci")
	runFailed.StateReason = "no eligible Codex account"
	runFailed.EndedAt = tp(at(10, 30))
	runDoneCodex := run("run_synthH", c.RunExited, "/synth/h")
	runDoneCodex.ExitCode = ptr(0)
	runDoneCodex.EndedAt = tp(at(11, 0))
	runArchived := run("run_synthG", c.RunExited, "/synth/g")
	runArchived.EndedAt = tp(at(9, 30))

	sessions := []c.SessionView{
		sess("ses_synthA", "synth-api-refactor", c.HarnessClaude, "synth-mac", run("run_synthA", c.RunWaitingApproval, "/synth/api"), false),
		sess("ses_synthB", "synth-flaky-ci", c.HarnessCodex, "synth-linux", runFailed, false),
		sess("ses_synthC", "synth-docs-pass", c.HarnessClaude, "synth-mac", run("run_synthC", c.RunWaitingInput, "/synth/repo"), false),
		sess("ses_synthD", "synth-search-index", c.HarnessClaude, "synth-linux", run("run_synthD", c.RunRunning, "/synth/vault"), false),
		sess("ses_synthE", "synth-shell", c.HarnessShell, "synth-linux", run("run_synthE", c.RunRunning, "/synth"), false),
		sess("ses_synthF", "synth-old", c.HarnessClaude, "synth-mac", run("run_synthF", c.RunExited, "/synth/f"), false),
		sess("ses_synthG", "synth-archived", c.HarnessClaude, "synth-mac", runArchived, true),
		sess("ses_synthH", "synth-codex-check", c.HarnessCodex, "synth-mac", runDoneCodex, false),
	}

	cur := int64(100)
	var events []c.Event
	add := func(kind c.EventKind, sid string, t time.Time, payload any) {
		cur++
		b, _ := json.Marshal(payload)
		events = append(events, c.Event{EventID: "evt_synth" + itoa(cur), Cursor: cur, Origin: "hub", Kind: kind, SessionID: sid,
			PayloadVersion: 1, Payload: b, OccurredAt: t, IngestedAt: t})
	}
	// Before the cutoff (cursor <= 100): must not count.
	events = append(events, c.Event{EventID: "evt_synth90", Cursor: 90, Kind: c.EvArtifactPublished, SessionID: "ses_synthF",
		Payload: mustJSON(c.PublishReceipt{Artifact: c.Artifact{ArtifactID: "art_synthOld", SessionID: "ses_synthF", Title: "synth old report"},
			Version: c.ArtifactVersion{VersionID: "ver_synthOld", Seq: 1}}), OccurredAt: at(8, 30), IngestedAt: at(8, 30)})

	add(c.EvRunObserved, "ses_synthA", at(11, 54), c.Run{RunID: "run_synthA", SessionID: "ses_synthA", State: c.RunWaitingApproval})
	add(c.EvApprovalRequested, "ses_synthA", at(11, 55), map[string]any{"approval_id": "apr_synthA", "session_id": "ses_synthA"})
	add(c.EvRunEnded, "ses_synthB", at(10, 30), *runFailed)
	add(c.EvArtifactPublished, "ses_synthC", at(10, 5), c.PublishReceipt{
		Artifact: c.Artifact{ArtifactID: "art_synthC", SessionID: "ses_synthC", Title: "synth digest spec"},
		Version:  c.ArtifactVersion{VersionID: "ver_synthC2", ArtifactID: "art_synthC", Seq: 2, Provenance: c.Provenance{SessionID: "ses_synthC", RunID: "run_synthC"}}})
	add(c.EvMessageStored, "ses_synthD", at(10, 10), c.MessageView{Envelope: c.MessageEnvelope{MessageID: "msg_synthCD",
		Sender: c.Principal{Kind: c.PrincipalSession, ID: "ses_synthC"}, Recipient: c.Address{Kind: c.AddrSession, ID: "ses_synthD"},
		Body: c.MessageBody{Text: "Synthetic: the docs are ready for review. Ping me if the index needs them."}}})
	add(c.EvDocumentRevised, "ses_synthC", at(10, 6), c.DocumentRevision{RevisionID: "rev_synthC", DocumentID: "doc_synthBrief", Seq: 4,
		Author: c.Principal{Kind: c.PrincipalSession, ID: "ses_synthC"}, Summary: "synth brief: digest section"})
	for i := 0; i < 40; i++ { // noise: shell observations
		add(c.EvRunObserved, "ses_synthE", at(9, 10+i), c.Run{RunID: "run_synthE", SessionID: "ses_synthE", State: c.RunRunning})
	}
	add(c.EvRunObserved, "ses_synthD", at(11, 50), c.Run{RunID: "run_synthD", SessionID: "ses_synthD", State: c.RunRunning})
	add(c.EvRunEnded, "ses_synthH", at(11, 0), *runDoneCodex)
	add(c.EvSessionUpdated, "ses_synthG", at(9, 40), c.Session{SessionID: "ses_synthG", Archived: true})

	attention := []c.AttentionItem{
		{AttentionID: "att_synthA", Kind: c.AttnApproval, Severity: "urgent", SessionID: "ses_synthA", Title: "Approve: go test -bench ./...",
			Object: c.ObjectRef{Kind: c.KindApproval, ID: "apr_synthA"}, State: c.AttnOpen, Count: 1, CreatedAt: at(11, 55), UpdatedAt: at(11, 55),
			DeepLink: "pyre://approval/apr_synthA"},
		{AttentionID: "att_synthNode", Kind: c.AttnNode, Severity: "normal", Title: "synth-linux was offline for 12 minutes",
			Object: c.ObjectRef{Kind: c.KindNode, ID: "node_synthLinux"}, State: c.AttnOpen, Count: 1, CreatedAt: at(10, 40), UpdatedAt: at(10, 40),
			DeepLink: "pyre://node/node_synthLinux"},
		{AttentionID: "att_synthOld", Kind: c.AttnMessage, Severity: "normal", SessionID: "ses_synthF", Title: "Old synthetic message",
			State: c.AttnOpen, Count: 1, CreatedAt: at(7, 0), UpdatedAt: at(7, 0), DeepLink: "pyre://attention/att_synthOld"},
	}

	entry := func(seq int64, runID string, t time.Time, kind, text string) c.TranscriptEntry {
		return c.TranscriptEntry{Seq: seq, RunID: runID, At: t, Kind: kind, Text: text}
	}
	transcripts := map[string]c.ReadSessionResponse{
		"ses_synthA": {Entries: []c.TranscriptEntry{
			entry(40, "run_synthA", at(8, 50), "user", "Refactor the synthetic API layer."),
			entry(41, "run_synthA", at(11, 40), "assistant", "The handlers are split; running the benchmarks next."),
			entry(42, "run_synthA", at(11, 41), "tool", "Edit: /synth/api/handlers.go; Bash: go test ./..."),
			entry(43, "run_synthA", at(11, 54), "tool", "Bash: go test -bench ./..."),
		}},
		"ses_synthC": {Entries: []c.TranscriptEntry{
			entry(800, "run_synthC", at(8, 55), "assistant", "Earlier synthetic message before the cutoff."),
			entry(801, "run_synthC", at(9, 10), "user", "Write the synthetic digest docs and tests."),
			entry(802, "run_synthC", at(9, 20), "tool", "Read: /synth/repo/README.md; Edit: /synth/repo/docs/digest.md"),
			entry(803, "run_synthC", at(9, 21), "tool", "tool result"),
			entry(804, "run_synthC", at(9, 40), "tool", "Write: /synth/repo/internal/digest/digest_test.go; Bash: go test ./internal/digest"),
			entry(805, "run_synthC", at(9, 50), "user", "Also publish the spec."),
			entry(806, "run_synthC", at(10, 4), "tool", "MultiEdit: /synth/repo/docs/digest.md"),
			entry(812, "run_synthC", at(10, 8), "assistant", "## Summary\n\n**Updated the digest docs** and added tests for ranking, e.g. needs-you first. Published the spec as v2.\n\n- docs/digest.md\n- internal/digest/digest_test.go"),
			entry(813, "run_synthC", at(10, 8), "system", "turn ended (58m0s)"),
		}},
		"ses_synthD": {Entries: []c.TranscriptEntry{
			entry(2001, "run_synthD", at(9, 30), "user", "Index the synthetic vault."),
			entry(2002, "run_synthD", at(9, 31), "tool", "command: synth-index --shards 7 (exit 0)"),
			entry(2003, "run_synthD", at(10, 12), "pyre_message", "[pyre message msg_synthCD] the docs are ready"),
			entry(2004, "run_synthD", at(11, 50), "assistant", "Indexing the synthetic vault: 3 of 7 shards done. The rest will take about an hour."),
			entry(2005, "run_synthD", at(11, 51), "tool", "file change: /synth/vault/index/shard-3.idx, /synth/vault/index/manifest.json"),
		}},
		"ses_synthG": {Entries: []c.TranscriptEntry{
			entry(10, "run_synthG", at(9, 20), "assistant", "Synthetic archived run finished its check."),
		}},
		"ses_synthH": {Entries: []c.TranscriptEntry{}},
	}
	return Input{Since: since, SinceCursor: 100, HeadCursor: cur, Now: now, SinceLabel: "last seen",
		Events: events, Sessions: sessions, Attention: attention, Transcripts: transcripts}
}

func mustJSON(v any) c.RawJSON {
	b, _ := json.Marshal(v)
	return b
}

func itoa(n int64) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// FixtureAPI serves an Input through the contracts.API read operations the
// digest uses (events with kinds/paging/retention, sessions, attention,
// transcripts); every other operation goes to the embedded API. Tests and
// the TUI digest demo use it with SyntheticInput.
type FixtureAPI struct {
	c.API
	In Input
	// Oldest is the oldest retained event cursor (default 1).
	Oldest int64
}

func (f *FixtureAPI) ListEvents(_ context.Context, req c.ListEventsRequest) (c.ListEventsResponse, error) {
	head, oldest := f.In.HeadCursor, max(f.Oldest, 1)
	if req.AfterCursor > head || req.AfterCursor < oldest-1 {
		return c.ListEventsResponse{}, c.Errorf(c.ErrResetRequired, "cursor %d outside retained events", req.AfterCursor).
			WithDetail("oldest_cursor", oldest).WithDetail("snapshot_cursor", head)
	}
	kinds := map[c.EventKind]bool{}
	for _, k := range req.Kinds {
		kinds[k] = true
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	resp := c.ListEventsResponse{Events: []c.Event{}, HeadCursor: head, NextCursor: req.AfterCursor}
	evs := append([]c.Event(nil), f.In.Events...)
	sort.Slice(evs, func(i, j int) bool { return evs[i].Cursor < evs[j].Cursor })
	for _, ev := range evs {
		if ev.Cursor <= req.AfterCursor || ev.Cursor < oldest || (len(kinds) > 0 && !kinds[ev.Kind]) ||
			(req.SessionID != "" && ev.SessionID != req.SessionID) {
			continue
		}
		resp.Events = append(resp.Events, ev)
		resp.NextCursor = ev.Cursor
		if len(resp.Events) >= limit {
			return resp, nil
		}
	}
	resp.NextCursor = max(resp.NextCursor, head)
	return resp, nil
}

func (f *FixtureAPI) ListSessions(_ context.Context, req c.ListSessionsRequest) (c.Page[c.SessionView], error) {
	var out []c.SessionView
	for _, s := range f.In.Sessions {
		if s.Session.Archived && !req.IncludeArchived {
			continue
		}
		out = append(out, s)
	}
	return c.Page[c.SessionView]{Items: out}, nil
}

func (f *FixtureAPI) GetSession(_ context.Context, req c.GetRequest) (c.SessionView, error) {
	for _, s := range f.In.Sessions {
		if s.Session.SessionID == req.ID {
			return s, nil
		}
	}
	return c.SessionView{}, c.Errorf(c.ErrNotFound, "session %s not found", req.ID)
}

func (f *FixtureAPI) ListAttention(_ context.Context, req c.ListAttentionRequest) (c.Page[c.AttentionItem], error) {
	states := map[c.AttentionState]bool{}
	for _, s := range req.States {
		states[s] = true
	}
	var out []c.AttentionItem
	for _, a := range f.In.Attention {
		if len(states) > 0 && !states[a.State] {
			continue
		}
		out = append(out, a)
	}
	return c.Page[c.AttentionItem]{Items: out}, nil
}

func (f *FixtureAPI) ReadSession(_ context.Context, req c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	tr, ok := f.In.Transcripts[req.SessionID]
	if !ok {
		return c.ReadSessionResponse{Entries: []c.TranscriptEntry{}}, nil
	}
	return tr, nil
}
