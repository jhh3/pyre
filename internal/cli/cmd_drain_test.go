package cli

import (
	"context"
	"slices"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func drainSV(id string, h c.Harness, node string, st c.RunState, reason string, archived bool) c.SessionView {
	sv := c.SessionView{Session: c.Session{SessionID: id, Harness: h, Archived: archived}}
	if st != "" {
		sv.Run = &c.Run{RunID: "run_" + id[4:], SessionID: id, NodeID: node, State: st, StateReason: reason}
	}
	return sv
}

func TestDrainSelect(t *testing.T) {
	const n = "node_synth-a"
	svs := []c.SessionView{
		drainSV("ses_synth-run", c.HarnessClaude, n, c.RunRunning, "", false),
		drainSV("ses_synth-idle", c.HarnessCodex, n, c.RunWaitingInput, "", false),
		drainSV("ses_synth-bg", c.HarnessClaude, n, c.RunWaitingBackground, "", false),
		drainSV("ses_synth-start", c.HarnessClaude, n, c.RunStarting, "", false),
		drainSV("ses_synth-unk", c.HarnessClaude, n, c.RunUnknown, "", false),
		drainSV("ses_synth-appr", c.HarnessClaude, n, c.RunWaitingApproval, "", false),
		drainSV("ses_synth-exited", c.HarnessClaude, n, c.RunExited, "", false),
		drainSV("ses_synth-failed", c.HarnessClaude, n, c.RunFailed, "", false),
		drainSV("ses_synth-exiting", c.HarnessClaude, n, c.RunExiting, "", false),
		drainSV("ses_synth-arch", c.HarnessClaude, n, c.RunRunning, "", true),
		drainSV("ses_synth-other", c.HarnessClaude, "node_synth-b", c.RunRunning, "", false),
		drainSV("ses_synth-norun", c.HarnessClaude, n, "", "", false),
		drainSV("ses_synth-shell", c.HarnessShell, n, c.RunRunning, "", false),
	}
	drain, skipped := drainSelect(svs, n)
	var got []string
	for _, sv := range drain {
		got = append(got, sv.Session.SessionID)
	}
	want := []string{"ses_synth-run", "ses_synth-idle", "ses_synth-bg", "ses_synth-start", "ses_synth-unk", "ses_synth-appr"}
	if !slices.Equal(got, want) {
		t.Fatalf("drain: got %v want %v", got, want)
	}
	if len(skipped) != 1 || skipped[0].SessionID != "ses_synth-shell" || skipped[0].Action != "skipped" {
		t.Fatalf("skipped: %+v", skipped)
	}
}

func TestClassifyResume(t *testing.T) {
	const n = "node_synth-a"
	lost := "run process not found after pyred restart (exit not observed)"
	failedResume := drainSV("ses_synth-fr", c.HarnessClaude, n, c.RunFailed, "launch failed: synthetic", false)
	failedResume.Run.PreviousRunID = "run_synth-prev"
	failedResume.Run.ContinuationKind = c.ContinuationNativeResume
	cases := []struct {
		sv        c.SessionView
		all       bool
		want      string
		wantAllTo string // with --all ("" = same)
	}{
		{drainSV("ses_synth-arch", c.HarnessClaude, n, c.RunExited, lost, true), false, resumeArchived, ""},
		{drainSV("ses_synth-lost", c.HarnessClaude, n, c.RunExited, lost, false), false, resumeLaunch, ""},
		{drainSV("ses_synth-lostf", c.HarnessCodex, n, c.RunFailed, "Run Process Not Found (Exit Not Observed)", false), false, resumeLaunch, ""},
		{failedResume, false, resumeLaunch, ""},
		{drainSV("ses_synth-exit", c.HarnessClaude, n, c.RunExited, "stopped by pyre (/exit)", false), false, resumeSkip, resumeLaunch},
		{drainSV("ses_synth-fail", c.HarnessClaude, n, c.RunFailed, "exit status 1", false), false, resumeSkip, resumeLaunch},
		{drainSV("ses_synth-live", c.HarnessCodex, n, c.RunWaitingInput, "", false), false, resumeNote, ""},
		{drainSV("ses_synth-run", c.HarnessCodex, n, c.RunRunning, "", false), false, resumeNote, ""},
		{drainSV("ses_synth-bg", c.HarnessClaude, n, c.RunWaitingBackground, "", false), false, resumeNote, ""},
		{drainSV("ses_synth-start", c.HarnessClaude, n, c.RunStarting, "", false), false, resumeWait, ""},
		{drainSV("ses_synth-unk", c.HarnessClaude, n, c.RunUnknown, "", false), false, resumeKeep, ""},
		{drainSV("ses_synth-exiting", c.HarnessClaude, n, c.RunExiting, "", false), false, resumeKeep, ""},
		{drainSV("ses_synth-norun", c.HarnessClaude, n, "", "", false), false, resumeSkip, resumeLaunch},
	}
	for _, tc := range cases {
		if got, why := classifyResume(tc.sv, false); got != tc.want {
			t.Errorf("%s: got %s (%s) want %s", tc.sv.Session.SessionID, got, why, tc.want)
		}
		wantAll := tc.wantAllTo
		if wantAll == "" {
			wantAll = tc.want
		}
		if got, why := classifyResume(tc.sv, true); got != wantAll {
			t.Errorf("%s --all: got %s (%s) want %s", tc.sv.Session.SessionID, got, why, wantAll)
		}
	}
}

func TestDrainTagsAndIDs(t *testing.T) {
	tags, ch := withTag([]string{"a"}, "drained:node_x")
	if !ch || !slices.Equal(tags, []string{"a", "drained:node_x"}) {
		t.Fatalf("add: %v %v", tags, ch)
	}
	if again, ch := withTag(tags, "drained:node_x"); ch || len(again) != 2 {
		t.Fatalf("add twice: %v %v", again, ch)
	}
	rm, ch := withoutTag(append(tags, "drained:node_x"), "drained:node_x")
	if !ch || !slices.Equal(rm, []string{"a"}) {
		t.Fatalf("remove: %v %v", rm, ch)
	}
	if again, ch := withoutTag(rm, "drained:node_x"); ch || len(again) != 1 {
		t.Fatalf("remove twice: %v %v", again, ch)
	}
	for _, id := range []string{
		drainMessageID("node_01JSYNTH", "ses_synth-a", 29000000),
		drainMessageID("node_weird id/x", "ses_a_b+c", 1) + "-stop-q",
		resumeNoteID("ses_synth-a", "run_01JSYNTH"),
	} {
		if !c.ValidID(id, c.PrefixMessage) {
			t.Errorf("invalid message ID %q", id)
		}
	}
	if drainMessageID("node_a", "ses_b", 7) != drainMessageID("node_a", "ses_b", 7) {
		t.Fatal("message ID not stable")
	}
}

// Drain then resume against the mock hub: tags are added once, a rerun
// changes nothing, and live sessions get the restart note and lose the tag.
func TestNodeDrainResumeCLI(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	ctx := c.WithPrincipal(context.Background(), testOwner)
	page, err := h.b.ListSessions(ctx, c.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Pick the node with the most drainable sessions.
	best, bestN := "", 0
	for _, sv := range page.Items {
		if sv.Run == nil {
			continue
		}
		if d, _ := drainSelect(page.Items, sv.Run.NodeID); len(d) > bestN {
			best, bestN = sv.Run.NodeID, len(d)
		}
	}
	if bestN == 0 {
		t.Skip("fixture has no live sessions")
	}
	tag := drainTag(best)

	r := run(t, e, nil, "--json", "node", "drain", best, "--dry-run")
	expectCode(t, r, 0)
	dr := mustJSON[drainResult](t, r)
	if len(dr.Sessions) != bestN || !dr.DryRun {
		t.Fatalf("dry run: %s", r)
	}
	if sv, _ := h.b.GetSession(ctx, c.GetRequest{ID: dr.Sessions[0].SessionID}); slices.Contains(sv.Session.Tags, tag) {
		t.Fatal("dry run tagged a session")
	}

	r = run(t, e, nil, "--json", "node", "drain", best, "--minutes", "3")
	expectCode(t, r, 0)
	dr = mustJSON[drainResult](t, r)
	for _, it := range dr.Sessions {
		if it.Error != "" || !it.Tagged || it.MessageID == "" {
			t.Fatalf("drain item: %+v\n%s", it, r)
		}
		sv, _ := h.b.GetSession(ctx, c.GetRequest{ID: it.SessionID})
		if !slices.Contains(sv.Session.Tags, tag) {
			t.Fatalf("%s not tagged: %v", it.SessionID, sv.Session.Tags)
		}
	}
	r = run(t, e, nil, "--json", "node", "drain", best, "--minutes", "3")
	expectCode(t, r, 0)
	for _, it := range mustJSON[drainResult](t, r).Sessions {
		sv, _ := h.b.GetSession(ctx, c.GetRequest{ID: it.SessionID})
		if it.Tagged || countTag(sv.Session.Tags, tag) != 1 {
			t.Fatalf("rerun re-tagged %s: %v", it.SessionID, sv.Session.Tags)
		}
	}

	// --all is the resume flag here, not the group's persistent --all.
	r = run(t, e, nil, "--json", "node", "resume", best, "--dry-run", "--all")
	expectCode(t, r, 0)
	if rr := mustJSON[resumeResult](t, r); !rr.All || len(rr.Sessions) != bestN {
		t.Fatalf("resume --all dry run: %s", r)
	}

	// The reboot: one run lost (exit not observed), one ended deliberately.
	if bestN < 3 {
		t.Skip("fixture node has fewer than 3 live sessions")
	}
	li := slices.IndexFunc(dr.Sessions, func(it nodeOpItem) bool {
		sv, _ := h.b.GetSession(ctx, c.GetRequest{ID: it.SessionID})
		return sv.Run != nil && sv.Run.NativeRef != nil && c.HasCap(sv.Run.Capabilities, c.CapResumeNative)
	})
	if li < 0 {
		t.Skip("no natively resumable fixture session")
	}
	lost, quit := dr.Sessions[li], dr.Sessions[(li+1)%len(dr.Sessions)]
	if err := h.b.SetRunState(lost.RunID, c.RunExited, "run process not found after pyred restart (exit not observed)"); err != nil {
		t.Fatal(err)
	}
	if err := h.b.SetRunState(quit.RunID, c.RunExited, "stopped by pyre (/exit)"); err != nil {
		t.Fatal(err)
	}
	tctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.b.Run(tctx, 5*time.Millisecond) // launches come up

	r = run(t, e, nil, "--json", "node", "resume", best, "--dry-run")
	expectCode(t, r, 0)
	rr := mustJSON[resumeResult](t, r)
	if len(rr.Sessions) != bestN || rr.Counts[resumeLaunch] != 1 || rr.Counts[resumeSkip] != 1 || rr.Counts[resumeNote] != bestN-2 {
		t.Fatalf("resume dry run: %s", r)
	}
	r = run(t, e, nil, "--json", "node", "resume", best)
	expectCode(t, r, 0)
	rr = mustJSON[resumeResult](t, r)
	for _, it := range rr.Sessions {
		sv, _ := h.b.GetSession(ctx, c.GetRequest{ID: it.SessionID})
		switch it.SessionID {
		case lost.SessionID:
			if it.Action != "resumed" || it.NewRunID == "" || it.NewRunID == lost.RunID || it.MessageID != resumeNoteID(lost.SessionID, it.NewRunID) {
				t.Fatalf("lost session not resumed: %+v\n%s", it, r)
			}
		case quit.SessionID:
			if it.Action != resumeSkip || it.NewRunID != "" {
				t.Fatalf("deliberately ended session: %+v", it)
			}
		default:
			if it.Action != resumeNote || it.MessageID == "" {
				t.Fatalf("live session: %+v", it)
			}
		}
		if it.Error != "" || slices.Contains(sv.Session.Tags, tag) {
			t.Fatalf("%s: error %q or still tagged %v", it.SessionID, it.Error, sv.Session.Tags)
		}
	}
	// Rerun: nothing is tagged any more.
	r = run(t, e, nil, "--json", "node", "resume", best)
	expectCode(t, r, 0)
	if rr := mustJSON[resumeResult](t, r); len(rr.Sessions) != 0 {
		t.Fatalf("rerun found tagged sessions: %s", r)
	}
}

func countTag(tags []string, tag string) int {
	n := 0
	for _, t := range tags {
		if t == tag {
			n++
		}
	}
	return n
}
