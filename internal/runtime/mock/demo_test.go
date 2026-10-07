package mock

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

var updateDemo = flag.Bool("update-demo", false, "rewrite tests/fixtures/mock/demo-fleet.json")

// demoFleet builds a larger synthetic fleet for UI development and
// screenshots: several nodes (one disconnected, one at capacity), sessions
// in every run state, project and standalone work, pending approvals and
// questions, mail in each delivery state, artifacts, and attention items.
func demoFleet() *c.FixtureBundle {
	at := func(min int) time.Time { return FixtureEpoch.Add(time.Duration(min) * time.Minute) }
	pt := func(t time.Time) *time.Time { return &t }
	sp := func(s string) *string { return &s }
	owner := c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"}
	caps := []string{c.CapAttachNative, c.CapAttachReadOnly, c.CapInputQueue, c.CapInputReconcile, c.CapApprovals, c.CapQuestions, c.CapInterrupt, c.CapStop, c.CapResumeNative, c.CapResumeHandoff, c.CapTranscript, c.CapScreen}
	codexCaps := append(append([]string(nil), caps...), c.CapInputSteer)
	harn := func(h c.Harness, v string, ok bool, why string) c.HarnessInfo {
		cs := caps
		if h == c.HarnessCodex {
			cs = codexCaps
		}
		if !ok {
			cs = []string{}
		}
		return c.HarnessInfo{Harness: h, Version: v, Available: ok, Reason: why, Capabilities: cs, ProbedAt: at(-3)}
	}
	node := func(id, name, platform, arch string, conn c.NodeConnection, active, max int, load float64, cpus int) c.Node {
		return c.Node{
			NodeID: id, Name: name, Hostname: name + ".synthetic", SSHAlias: name, Platform: platform, Arch: arch,
			Harnesses: []c.HarnessInfo{harn(c.HarnessClaude, "2.1.289", true, ""), harn(c.HarnessCodex, "0.160.0", true, ""), harn(c.HarnessShell, "tmux 3.5a", true, "")},
			Versions:  map[string]string{"pyred": "0.1.0-demo", "protocol": "1"}, Capabilities: []string{"launch", "attach.ssh"},
			Connection: conn, EnrolledAt: at(-20000), Labels: map[string]string{"demo": "true"},
			Resources: c.NodeResources{CPUCount: cpus, Load1: load, MemTotalMB: 65536, MemAvailMB: 30000, ActiveRuns: active, MaxRuns: max, PyreRSSMB: 35, PyreCPUPct: 0.3, ObservedAt: at(0)},
		}
	}
	connected := func(since int) c.NodeConnection {
		return c.NodeConnection{State: c.ConnConnected, LastSeenAt: pt(at(0)), ConnectedSince: pt(at(since)), Protocol: 1}
	}
	b := &c.FixtureBundle{SchemaVersion: c.SchemaVersion, Name: "demo-fleet", Synthetic: true,
		Description: "Mock-only demo fleet for UI development and screenshots: four nodes (one disconnected, one at capacity), sessions in every run state, project and standalone work, approvals, questions, mail in each delivery state, artifacts, and attention. Entirely synthetic; never runtime evidence."}
	studio := node("node_demo-studio", "demo-studio", "darwin", "arm64", connected(-600), 4, 10, 3.2, 12)
	big := node("node_demo-bigbox", "demo-bigbox", "linux", "amd64", connected(-3000), 4, 4, 11.5, 32)
	pi := node("node_demo-pi", "demo-pi", "linux", "arm64", c.NodeConnection{State: c.ConnDisconnected, LastSeenAt: pt(at(-42)), Protocol: 1}, 1, 2, 0, 4)
	pi.Harnesses[1] = harn(c.HarnessCodex, "", false, "codex not installed (synthetic)")
	laptop := node("node_demo-laptop", "demo-laptop", "darwin", "arm64", connected(-15), 0, 4, 0.8, 8)
	// Machine stats (contract v1.8): a healthy Mac and a Linux box that is
	// low on disk, swapping heavily and under memory pressure.
	studio.Resources.CPUPct, studio.Resources.Load5, studio.Resources.Load15 = 22, 2.9, 2.5
	studio.Resources.SwapTotalMB, studio.Resources.SwapUsedMB, studio.Resources.MemPressure = 4096, 512, c.MemPressureNormal
	studio.Resources.Disks = []c.DiskUsage{{Path: "/Users/demo", Roles: []string{"home", "data"}, TotalMB: 1907729, FreeMB: 801246}}
	big.Resources.CPUPct, big.Resources.Load5, big.Resources.Load15, big.Resources.MemAvailMB = 68.4, 10.2, 9.8, 9000
	big.Resources.SwapTotalMB, big.Resources.SwapUsedMB = 8192, 7270
	big.Resources.MemPressure, big.Resources.MemPressureSome10 = c.MemPressureWarn, 14.2
	big.Resources.Disks = []c.DiskUsage{{Path: "/home/demo", Roles: []string{"home", "data"}, TotalMB: 953869, FreeMB: 57232}}
	usage := func(run, ses string, procs int, rss, cpu float64, top string, topRSS float64) c.RunUsage {
		return c.RunUsage{RunID: run, SessionID: ses, Procs: procs, RSSMB: rss, CPUPct: cpu, Top: &c.ProcUsage{PID: 4200 + procs, Name: top, RSSMB: topRSS}, ObservedAt: at(0)}
	}
	big.Resources.Runs = []c.RunUsage{
		usage("run_demo-planner", "ses_demo-planner", 14, 1843, 41, "claude", 912),
		usage("run_demo-bench", "ses_demo-bench", 9, 2650, 96, "cargo", 1610),
		usage("run_demo-api", "ses_demo-api", 6, 612, 0.4, "claude", 455),
		usage("run_demo-docs", "ses_demo-docs", 8, 980, 12, "node", 402),
	}
	studio.Resources.Runs = []c.RunUsage{
		usage("run_demo-scratch", "ses_demo-scratch", 5, 540, 0.2, "claude", 430),
		usage("run_demo-question", "ses_demo-question", 4, 310, 0, "codex", 220),
		usage("run_demo-shell", "ses_demo-shell", 2, 6, 0.1, "tail", 2),
	}
	b.Nodes = []c.Node{studio, big, pi, laptop}

	prj := "prj_demo-atlas"
	b.Projects = []c.Project{{ProjectID: prj, Name: "Atlas (demo)", Description: "Synthetic demo project: rewrite the map tile service.", BriefDocumentID: "doc_demo-atlas-brief",
		Repos: []c.RepoBinding{{RepoID: "atlas", Name: "atlas", URL: "https://example.invalid/demo/atlas.git"}}, Checkouts: []c.CheckoutBinding{{NodeID: big.NodeID, Path: "/home/demo/src/atlas", RepoID: "atlas", Mode: c.CheckoutShared, Branch: "main"}},
		Version: 3, CreatedBy: owner, CreatedAt: at(-5000), UpdatedAt: at(-60)},
		{ProjectID: "prj_demo-firmware", Name: "Firmware (demo)", Description: "Synthetic demo project: sensor firmware and its tooling, across machines.",
			Repos: []c.RepoBinding{{RepoID: "firmware", Name: "firmware", URL: "https://example.invalid/demo/firmware.git"}}, Checkouts: []c.CheckoutBinding{{NodeID: pi.NodeID, Path: "/home/demo/src/firmware", RepoID: "firmware", Mode: c.CheckoutShared, Branch: "main"}, {NodeID: studio.NodeID, Path: "/home/demo/src/firmware", RepoID: "firmware", Mode: c.CheckoutShared, Branch: "main"}},
			Version: 1, CreatedBy: owner, CreatedAt: at(-3000), UpdatedAt: at(-300)}}
	brief := "# Atlas brief (demo)\n\nGoal: serve vector tiles under 50 ms p99.\n\n- Keep the public API stable.\n- Benchmarks live in `bench/`.\n"
	b.Documents = []c.Document{{DocumentID: "doc_demo-atlas-brief", ProjectID: sp(prj), Title: "Brief", Slug: "brief", MediaType: "text/markdown", HeadRevisionID: "rev_demo-brief-2", HeadSeq: 2, CreatedBy: owner, CreatedAt: at(-5000), UpdatedAt: at(-60)}}
	b.Revisions = []c.DocumentRevision{
		{RevisionID: "rev_demo-brief-1", DocumentID: "doc_demo-atlas-brief", Seq: 1, Author: owner, CreatedAt: at(-5000), Content: "# Atlas brief (demo)\n\nGoal: faster tiles.\n"},
		{RevisionID: "rev_demo-brief-2", DocumentID: "doc_demo-atlas-brief", Seq: 2, ParentRevisionID: "rev_demo-brief-1", Author: owner, CreatedAt: at(-60), Summary: "add target", Content: brief},
	}

	type spec struct {
		id, title, node string
		h               c.Harness
		st              c.RunState
		reason          string
		project         string // "", "atlas" or "firmware"
		started         int
		stale           bool
	}
	specs := []spec{
		{"ses_demo-planner", "atlas: plan tile cache", big.NodeID, c.HarnessClaude, c.RunRunning, "editing cache.go", "atlas", -90, false},
		{"ses_demo-bench", "atlas: run benchmarks", big.NodeID, c.HarnessCodex, c.RunWaitingApproval, "permission requested", "atlas", -70, false},
		{"ses_demo-api", "atlas: API compatibility review", big.NodeID, c.HarnessClaude, c.RunWaitingInput, "idle at prompt", "atlas", -50, false},
		{"ses_demo-docs", "atlas: docs pass", big.NodeID, c.HarnessClaude, c.RunWaitingBackground, "background task running", "atlas", -40, false},
		{"ses_demo-encoder", "atlas: encoder spike", studio.NodeID, c.HarnessClaude, c.RunRunning, "reading encoder.go", "atlas", -60, false},
		{"ses_demo-scratch", "scratch: regex helper", studio.NodeID, c.HarnessClaude, c.RunWaitingInput, "idle at prompt", "", -30, false},
		{"ses_demo-question", "dotfiles cleanup", studio.NodeID, c.HarnessCodex, c.RunWaitingApproval, "question pending", "", -25, false},
		{"ses_demo-shell", "firmware: serial log tail", studio.NodeID, c.HarnessShell, c.RunRunning, "tail -f (synthetic)", "firmware", -20, false},
		{"ses_demo-pi", "firmware: sensor driver", pi.NodeID, c.HarnessClaude, c.RunUnknown, "node disconnected; last observed running", "firmware", -300, true},
		{"ses_demo-done", "release notes", laptop.NodeID, c.HarnessClaude, c.RunExited, "exited 0", "", -400, false},
		{"ses_demo-failed", "firmware: flaky test bisect", studio.NodeID, c.HarnessCodex, c.RunFailed, "exited 1 (synthetic crash)", "firmware", -200, false},
		{"ses_demo-starting", "atlas: profile tiles", laptop.NodeID, c.HarnessClaude, c.RunStarting, "launching", "atlas", -1, false},
	}
	// The planner (demo-bigbox) started the benchmarks (demo-bigbox) and an
	// encoder spike (demo-studio), which started the tile profiler
	// (demo-laptop).
	parents := map[string]string{"ses_demo-bench": "ses_demo-planner", "ses_demo-encoder": "ses_demo-planner", "ses_demo-starting": "ses_demo-encoder"}
	for i, s := range specs {
		rc := caps
		if s.h == c.HarnessCodex {
			rc = codexCaps
		}
		if s.h == c.HarnessShell {
			rc = []string{c.CapAttachNative, c.CapAttachReadOnly, c.CapStop, c.CapScreen}
		}
		r := &c.Run{RunID: "run_demo-" + s.id[9:], SessionID: s.id, NodeID: s.node, Adapter: string(s.h) + "-demo", Harness: s.h, Cwd: "/home/demo/work/" + s.id[9:],
			Runtime: c.RuntimeVersion{Harness: s.h, Version: "demo"}, State: s.st, StateReason: s.reason, Freshness: c.Freshness{ObservedAt: at(0), Source: "mock:demo"},
			StartedAt: at(s.started), Capabilities: rc}
		if s.stale {
			r.Freshness = c.Freshness{ObservedAt: at(-42), Source: "mock:demo", Stale: true, Reason: "node disconnected"}
		}
		if s.st.Terminal() {
			code := 0
			if s.st == c.RunFailed {
				code = 1
			}
			r.ExitCode, r.EndedAt = &code, pt(at(s.started+60))
		}
		if s.h == c.HarnessClaude {
			r.AccountID = "acct_synth-claude-a"
		}
		var pid *string
		if s.project != "" {
			pid = sp("prj_demo-" + s.project)
			r.Checkout = &c.CheckoutBinding{NodeID: s.node, Path: "/home/demo/src/" + s.project, RepoID: s.project, Mode: c.CheckoutShared, Branch: "main"}
			r.Cwd = r.Checkout.Path
		}
		if s.id == "ses_demo-planner" {
			r.Subagents = []c.SubagentObservation{{ID: "sub-1", Kind: "Explore", State: "running", StartedAt: pt(at(-5)), Summary: "searching tile encoders"}}
		}
		sess := c.Session{SessionID: s.id, Harness: s.h, Title: s.title, ProjectID: pid, CreatedBy: owner, CreatedAt: at(s.started - i), CurrentRunID: r.RunID, Tags: []string{"demo"}}
		if p := parents[s.id]; p != "" { // a session tree across machines (the TUI indents it)
			sess.CreatedBy = c.Principal{Kind: c.PrincipalSession, ID: p}
			sess.Lineage.ParentSessionID = p
		}
		v := c.SessionView{Session: sess, Run: r, NodeName: map[string]string{studio.NodeID: studio.Name, big.NodeID: big.Name, pi.NodeID: pi.Name, laptop.NodeID: laptop.Name}[s.node], Freshness: r.Freshness}
		b.Sessions = append(b.Sessions, v)
	}
	opts := []c.ApprovalOption{{ID: "allow", Label: "Allow once", Kind: "allow"}, {ID: "allow_always", Label: "Always allow", Kind: "allow_always"}, {ID: "deny", Label: "Deny", Kind: "deny"}}
	b.Approvals = []c.Approval{
		{ApprovalID: "apr_demo-bench", SessionID: "ses_demo-bench", RunID: "run_demo-bench", RequestID: "demo-req-1", Kind: c.ApprovalPermission, Title: "Run `go test -bench . ./tiles/...`?", Body: "Shell command requested by the agent (synthetic).", Options: opts, CreatedAt: at(-4), State: c.ApprovalPending},
		{ApprovalID: "apr_demo-question", SessionID: "ses_demo-question", RunID: "run_demo-question", RequestID: "demo-req-2", Kind: c.ApprovalQuestion, Title: "Which shell config should I keep?", Body: "Found both .zshrc and .bashrc customizations (synthetic).",
			Options: []c.ApprovalOption{{ID: "zsh", Label: "Keep zsh", Kind: "choice"}, {ID: "bash", Label: "Keep bash", Kind: "choice"}}, AllowText: true, CreatedAt: at(-2), State: c.ApprovalPending},
	}
	for i := range b.Sessions {
		switch b.Sessions[i].Session.SessionID {
		case "ses_demo-bench":
			b.Sessions[i].Run.PendingApprovalIDs = []string{"apr_demo-bench"}
		case "ses_demo-question":
			b.Sessions[i].Run.PendingApprovalIDs = []string{"apr_demo-question"}
		}
	}
	msg := func(id, from, to, text string, rt c.RuntimeDeliveryState, ack c.AckState, bound, reason string) c.MessageView {
		m := c.MessageView{Envelope: c.MessageEnvelope{SchemaVersion: 1, MessageID: id, OperationID: "op_" + id[4:], Sender: c.Principal{Kind: c.PrincipalSession, ID: from},
			Recipient: c.Address{Kind: c.AddrSession, ID: to}, ProjectID: sp(prj), Mode: c.ModeQueue, Body: c.MessageBody{Text: text}, CreatedAt: at(-15)},
			Status: c.DeliveryStatus{Storage: c.StorageStored, StoredAt: pt(at(-15)), Runtime: rt, RuntimeReason: reason, BoundRunID: bound, Ack: ack, UpdatedAt: at(-14)}}
		if rt == c.DeliveryAccepted {
			m.Status.Receipts = []c.DeliveryReceipt{{MessageID: id, AttemptID: "dlv_" + id[4:], SessionID: to, RunID: bound, NodeID: big.NodeID, State: rt, Evidence: c.EvidenceMock, EvidenceRef: "mock:demo", ObservedAt: at(-14)}}
		}
		return m
	}
	b.Messages = []c.MessageView{
		msg("msg_demo-1", "ses_demo-planner", "ses_demo-api", "Synthetic: please check cache.go keeps the v1 API.", c.DeliveryAccepted, c.AckReplied, "run_demo-api", ""),
		msg("msg_demo-2", "ses_demo-api", "ses_demo-planner", "Synthetic: API unchanged; one deprecation note added.", c.DeliveryQueued, c.AckNone, "", "recipient busy; waiting for idle turn"),
		msg("msg_demo-3", "ses_demo-planner", "ses_demo-bench", "Synthetic: rerun benchmarks after the cache change.", c.DeliveryNodeReceived, c.AckNone, "", "recipient waiting on an approval; queued"),
		msg("msg_demo-4", "ses_demo-planner", "ses_demo-docs", "Synthetic: drop the old tiles section. [mock:indeterminate]", c.DeliveryIndeterminate, c.AckNone, "run_demo-docs", "node restarted after submitting input (synthetic)"),
	}
	b.Messages[1].Envelope.InReplyTo = "msg_demo-1"
	b.Messages[0].Status.AckRef = "msg_demo-2"
	owMsg := c.MessageView{Envelope: c.MessageEnvelope{SchemaVersion: 1, MessageID: "msg_demo-owner", OperationID: "op_demo-owner", Sender: c.Principal{Kind: c.PrincipalSession, ID: "ses_demo-planner"},
		Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, ProjectID: sp(prj), Mode: c.ModeQueue, Body: c.MessageBody{Text: "Synthetic: cache design ready; want me to proceed with LRU or ARC?"}, CreatedAt: at(-6), RequestAck: true},
		Status: c.DeliveryStatus{Storage: c.StorageStored, StoredAt: pt(at(-6)), Runtime: c.DeliveryNotApplicable, Ack: c.AckNone, UpdatedAt: at(-6)}}
	b.Messages = append(b.Messages, owMsg)
	for i := range b.Messages {
		b.Messages[i].Cursor = int64(i + 1)
	}
	report := "# Benchmark report (demo)\n\n| case | p99 |\n|---|---|\n| z10 | 41 ms |\n| z14 | 63 ms |\n"
	b.Artifacts = []c.Artifact{{ArtifactID: "art_demo-bench", ProjectID: sp(prj), SessionID: "ses_demo-bench", Title: "Tile benchmark report (demo)", LatestVersionID: "ver_demo-bench-1", VersionCount: 1, CreatedBy: c.Principal{Kind: c.PrincipalSession, ID: "ses_demo-bench"}, CreatedAt: at(-30), UpdatedAt: at(-30)}}
	b.Versions = []c.ArtifactVersion{{VersionID: "ver_demo-bench-1", ArtifactID: "art_demo-bench", Seq: 1, MediaType: "text/markdown", Filename: "bench.md", Author: c.Principal{Kind: c.PrincipalSession, ID: "ses_demo-bench"},
		Provenance: c.Provenance{SessionID: "ses_demo-bench", RunID: "run_demo-bench", NodeID: big.NodeID, ProjectID: sp(prj)}, CreatedAt: at(-30), ContentURL: "/v1/content/ver_demo-bench-1", ReviewRequested: true}}
	b.Contents = map[string]string{"ver_demo-bench-1": report}
	b.Notes = []c.Note{{NoteID: "note_demo-decision", Kind: c.NoteDecision, ProjectID: sp(prj), SessionID: "ses_demo-planner", Title: "Use an LRU tile cache (demo)", Body: "Synthetic decision: LRU with 512 MiB cap; revisit after benchmarks.", Author: owner, CreatedAt: at(-100), SyncState: "indexed"}}
	att := func(id string, kind c.AttentionKind, sev string, obj c.ObjectRef, sess, title string, min int) c.AttentionItem {
		return c.AttentionItem{AttentionID: id, Kind: kind, Severity: sev, GroupKey: string(kind) + ":" + obj.ID, Object: obj, SessionID: sess, ProjectID: sp(prj), Title: title, State: c.AttnOpen, Count: 1, CreatedAt: at(min), UpdatedAt: at(min), DeepLink: c.DeepLink(obj)}
	}
	b.Attention = []c.AttentionItem{
		att("att_demo-bench", c.AttnApproval, "urgent", c.ObjectRef{Kind: c.KindApproval, ID: "apr_demo-bench"}, "ses_demo-bench", "Approve: go test -bench", -4),
		att("att_demo-question", c.AttnQuestion, "urgent", c.ObjectRef{Kind: c.KindApproval, ID: "apr_demo-question"}, "ses_demo-question", "Question: which shell config?", -2),
		att("att_demo-indet", c.AttnIndeterminate, "urgent", c.ObjectRef{Kind: c.KindMessage, ID: "msg_demo-4"}, "ses_demo-docs", "Delivery outcome unknown", -13),
		att("att_demo-owner", c.AttnMessage, "urgent", c.ObjectRef{Kind: c.KindMessage, ID: "msg_demo-owner"}, "ses_demo-planner", "Message from atlas: plan tile cache", -6),
		att("att_demo-review", c.AttnReviewReady, "normal", c.ObjectRef{Kind: c.KindArtifactVersion, ID: "ver_demo-bench-1"}, "ses_demo-bench", "Tile benchmark report v1 ready for review", -30),
		att("att_demo-node", c.AttnNode, "normal", c.ObjectRef{Kind: c.KindNode, ID: pi.NodeID}, "", "demo-pi disconnected 42m ago", -42),
		att("att_demo-failed", c.AttnFailure, "normal", c.ObjectRef{Kind: c.KindSession, ID: "ses_demo-failed"}, "ses_demo-failed", "firmware: flaky test bisect exited 1", -140),
	}
	for i := range b.Attention {
		if b.Attention[i].Kind == c.AttnNode || b.Attention[i].SessionID == "ses_demo-failed" {
			b.Attention[i].ProjectID = nil
		}
	}
	return b
}

func TestDemoFleetFixture(t *testing.T) {
	b := demoFleet()
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join(DefaultFixtureDirs()[0], "..", "mock", "demo-fleet.json")
	if *updateDemo {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/runtime/mock -run TestDemoFleetFixture -update-demo)", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("tests/fixtures/mock/demo-fleet.json is stale; regenerate with -update-demo")
	}
	if _, err := c.DecodeFixture(got); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	// It loads into a backend alongside the contract fixtures and simulates.
	be, _ := newFixtureBackend(t)
	sv, err := be.GetSession(as(owner), c.GetRequest{ID: "ses_demo-api"})
	if err != nil || sv.Run.State != c.RunWaitingInput {
		t.Fatalf("demo session: %+v %v", sv, err)
	}
	be.Settle(0)
	m, _ := be.GetMessage(as(owner), c.GetRequest{ID: "msg_demo-3"})
	if m.Status.Runtime != c.DeliveryNodeReceived {
		t.Fatalf("message to a waiting-approval run must stay queued, got %s", m.Status.Runtime)
	}
}
