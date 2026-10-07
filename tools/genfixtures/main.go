// Command genfixtures writes the shared synthetic contract fixtures to
// tests/fixtures/contracts/. Fixtures are generated from the Go contract
// types so they always decode strictly. Run `make fixtures` after changing
// contracts; CI fails if the committed fixtures differ from generated ones.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

var base = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func at(min int) time.Time      { return base.Add(time.Duration(min) * time.Minute) }
func pt(t time.Time) *time.Time { return &t }
func ip(i int) *int             { return &i }

var (
	owner   = c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"}
	android = c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-android", Name: "owner phone (synthetic)"}
	nodeMac = c.Node{
		NodeID: "node_synth-mac", Name: "synth-mac", Hostname: "synth-mac.local", SSHAlias: "synth-mac",
		Platform: "darwin", Arch: "arm64",
		Harnesses: []c.HarnessInfo{
			{Harness: c.HarnessClaude, Version: "2.1.289", Available: true, Capabilities: claudeCaps, ProbedAt: at(-5)},
			{Harness: c.HarnessCodex, Version: "0.160.0", Available: true, Capabilities: codexCaps, ProbedAt: at(-5)},
			{Harness: c.HarnessShell, Version: "tmux 3.5a", Available: true, Capabilities: shellCaps, ProbedAt: at(-5)},
		},
		Versions:     map[string]string{"pyred": "0.1.0-synth", "protocol": "1", "tmux": "3.5a"},
		Capabilities: []string{"launch", "attach.ssh"},
		Connection:   c.NodeConnection{State: c.ConnConnected, LastSeenAt: pt(at(0)), ConnectedSince: pt(at(-60)), Protocol: 1},
		Resources: c.NodeResources{CPUCount: 10, Load1: 2.1, MemTotalMB: 16384, MemAvailMB: 6100, ActiveRuns: 2, MaxRuns: 8, PyreRSSMB: 38, PyreCPUPct: 0.4, ObservedAt: at(0),
			// Machine stats (v1.8): nearly full disk, heavy swap, pressure warn.
			CPUPct: 18.5, Load5: 2.4, Load15: 2.2, SwapTotalMB: 9216, SwapUsedMB: 7816, MemPressure: c.MemPressureWarn,
			Disks: []c.DiskUsage{{Path: "/Users/synth", Roles: []string{"home", "data"}, TotalMB: 471482, FreeMB: 15510}},
			Runs: []c.RunUsage{
				{RunID: "run_synth-a-1", SessionID: "ses_synth-a", Procs: 11, RSSMB: 1229.4, CPUPct: 35.2, Top: &c.ProcUsage{PID: 4321, Name: "claude", RSSMB: 702.1}, ObservedAt: at(0)},
				{RunID: "run_synth-standalone-1", SessionID: "ses_synth-standalone", Procs: 4, RSSMB: 410.0, CPUPct: 0.3, Top: &c.ProcUsage{PID: 4400, Name: "claude", RSSMB: 380.5}, ObservedAt: at(0)},
			}},
		EnrolledAt: at(-10000),
	}
	nodeLinux = c.Node{
		NodeID: "node_synth-linux", Name: "synth-linux", Hostname: "synth-linux", SSHAlias: "synth-linux",
		Platform: "linux", Arch: "amd64",
		Harnesses: []c.HarnessInfo{
			{Harness: c.HarnessClaude, Version: "2.1.289", Available: true, Capabilities: claudeCaps, ProbedAt: at(-5)},
			{Harness: c.HarnessCodex, Available: false, Reason: "codex executable not found on node PATH", Capabilities: []string{}, ProbedAt: at(-5)},
			{Harness: c.HarnessShell, Version: "tmux 3.4", Available: true, Capabilities: shellCaps, ProbedAt: at(-5)},
		},
		Versions:     map[string]string{"pyred": "0.1.0-synth", "protocol": "1", "tmux": "3.4"},
		Capabilities: []string{"launch", "attach.ssh"},
		Connection:   c.NodeConnection{State: c.ConnConnected, LastSeenAt: pt(at(0)), ConnectedSince: pt(at(-300)), Protocol: 1},
		Resources: c.NodeResources{CPUCount: 32, Load1: 5.5, MemTotalMB: 123904, MemAvailMB: 74000, ActiveRuns: 1, MaxRuns: 16, PyreRSSMB: 41, PyreCPUPct: 0.3, ObservedAt: at(0),
			CPUPct: 7.2, Load5: 4.9, Load15: 4.1, SwapTotalMB: 8192, SwapUsedMB: 310, MemPressure: c.MemPressureNormal, MemPressureSome10: 0.4,
			Disks: []c.DiskUsage{{Path: "/home/synth", Roles: []string{"home", "data"}, TotalMB: 953869, FreeMB: 612400}},
			Runs: []c.RunUsage{
				{RunID: "run_synth-project-1", SessionID: "ses_synth-project", Procs: 23, RSSMB: 2310.7, CPUPct: 112.5, Top: &c.ProcUsage{PID: 9001, Name: "go", RSSMB: 980.2}, ObservedAt: at(0)},
				{RunID: "run_synth-b-1", SessionID: "ses_synth-b", Procs: 6, RSSMB: 655.0, CPUPct: 1.1, Top: &c.ProcUsage{PID: 9100, Name: "claude", RSSMB: 512.3}, ObservedAt: at(0)},
			}},
		EnrolledAt: at(-9000),
	}
	claudeCaps = []string{c.CapAttachNative, c.CapAttachReadOnly, c.CapInputQueue, c.CapInputReconcile, c.CapApprovals, c.CapInterrupt, c.CapStop, c.CapResumeNative, c.CapResumeHandoff, c.CapSubagentsObserve, c.CapTranscript, c.CapScreen, c.CapAccountProfile, c.CapHooks, c.CapAgentTools}
	codexCaps  = []string{c.CapAttachNative, c.CapAttachReadOnly, c.CapInputQueue, c.CapInputSteer, c.CapApprovals, c.CapInterrupt, c.CapStop, c.CapResumeNative, c.CapResumeHandoff, c.CapTranscript, c.CapScreen, c.CapAccountProfile, c.CapAgentTools}
	shellCaps  = []string{c.CapAttachNative, c.CapAttachReadOnly, c.CapStop, c.CapScreen}
)

func fresh(src string, min int) c.Freshness {
	return c.Freshness{ObservedAt: at(min), Source: src}
}

func run(id, ses, node string, h c.Harness, st c.RunState, caps []string) *c.Run {
	return &c.Run{
		RunID: id, SessionID: ses, NodeID: node, Adapter: string(h) + "-synth", Harness: h,
		Cwd: "/home/synth/work/" + ses, Runtime: c.RuntimeVersion{Harness: h, Version: "synthetic"},
		State: st, Freshness: fresh("node:"+node, 0), StartedAt: at(-30), Capabilities: caps,
	}
}

func session(id string, h c.Harness, title string, project *string, r *c.Run, node string) c.SessionView {
	s := c.Session{SessionID: id, Harness: h, Title: title, ProjectID: project, CreatedBy: owner, CreatedAt: at(-30)}
	if r != nil {
		s.CurrentRunID = r.RunID
	}
	return c.SessionView{Session: s, Run: r, NodeName: node, Freshness: fresh("hub", 0)}
}

func msg(id, from, to string, text string, st c.DeliveryStatus) c.MessageView {
	return c.MessageView{
		Envelope: c.MessageEnvelope{
			SchemaVersion: 1, MessageID: id, OperationID: "op_" + id[4:],
			Sender:    c.Principal{Kind: c.PrincipalSession, ID: from},
			Recipient: c.Address{Kind: c.AddrSession, ID: to},
			Mode:      c.ModeQueue, Body: c.MessageBody{Text: text}, CreatedAt: at(-10),
		},
		Status: st,
	}
}

func raw(v any) c.RawJSON {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func digestOf(s string) (c.Digest, int64) {
	sum := sha256.Sum256([]byte(s))
	return c.Digest("sha256:" + hex.EncodeToString(sum[:])), int64(len(s))
}

func bundle(name, desc string, reqs, cases []string) *c.FixtureBundle {
	return &c.FixtureBundle{SchemaVersion: c.SchemaVersion, Name: name, Description: desc, Synthetic: true, Requirements: reqs, Cases: cases}
}

func main() {
	out := flag.String("out", "tests/fixtures/contracts", "output directory")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		panic(err)
	}
	bundles := []*c.FixtureBundle{
		f01Launch(), f02Disconnected(), f03Delivery(), f04Idempotency(), f05Lease(),
		f06DocsArtifacts(), f07Accounts(), f08Fallback(), f09Approvals(), f10Environment(),
	}
	for i, b := range bundles {
		// Make digests/sizes truthful wherever content is known.
		for j := range b.Revisions {
			if r := &b.Revisions[j]; r.Content != "" {
				r.Digest, r.Size = digestOf(r.Content)
			}
		}
		for j := range b.Versions {
			if v := &b.Versions[j]; b.Contents[v.VersionID] != "" {
				v.Digest, v.Size = digestOf(b.Contents[v.VersionID])
			}
		}
		if err := b.Validate(); err != nil {
			panic(err)
		}
		data, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			panic(err)
		}
		data = append(data, '\n')
		p := filepath.Join(*out, fmt.Sprintf("%02d-%s.json", i+1, b.Name))
		if err := os.WriteFile(p, data, 0o644); err != nil {
			panic(err)
		}
	}
}

// 1. Standalone session launch and a project-associated launch.
func f01Launch() *c.FixtureBundle {
	b := bundle("launch-standalone-and-project", "A projectless Claude session on the Mac and a project-associated Codex session in a separate worktree. Standalone sessions have full basic capability.", []string{"R02", "R05"}, []string{"A02", "A07"})
	b.Nodes = []c.Node{nodeMac, nodeLinux}
	prj := "prj_synth-outfit"
	b.Projects = []c.Project{{
		ProjectID: prj, Name: "Synthetic Outfit", Description: "Synthetic project with two repos", BriefDocumentID: "doc_synth-brief",
		Repos:     []c.RepoBinding{{RepoID: "outfit", Name: "outfit", URL: "https://example.invalid/synth/outfit.git"}, {RepoID: "notes", Name: "notes"}},
		Checkouts: []c.CheckoutBinding{{NodeID: "node_synth-linux", Path: "/home/synth/src/outfit", RepoID: "outfit", Mode: c.CheckoutShared, Branch: "main"}},
		Version:   1, CreatedBy: owner, CreatedAt: at(-500), UpdatedAt: at(-500),
	}, {
		ProjectID: "prj_synth-research", Name: "Synthetic research (no repos)", Repos: []c.RepoBinding{}, Checkouts: []c.CheckoutBinding{},
		Version: 1, CreatedBy: owner, CreatedAt: at(-400), UpdatedAt: at(-400),
	}}
	b.Documents = []c.Document{{DocumentID: "doc_synth-brief", ProjectID: &prj, Title: "Brief", Slug: "brief", MediaType: "text/markdown", HeadRevisionID: "rev_synth-brief-1", HeadSeq: 1, CreatedBy: owner, CreatedAt: at(-500), UpdatedAt: at(-500)}}
	b.Revisions = []c.DocumentRevision{{RevisionID: "rev_synth-brief-1", DocumentID: "doc_synth-brief", Seq: 1, Digest: "sha256:synthetic", Size: 64, Author: owner, CreatedAt: at(-500), Content: "# Synthetic brief\n\nShip the synthetic API client. See decisions doc.\n"}}
	r1 := run("run_synth-standalone-1", "ses_synth-standalone", "node_synth-mac", c.HarnessClaude, c.RunWaitingInput, claudeCaps)
	r1.Cwd = "/Users/synth/scratch"
	r1.Checkout = &c.CheckoutBinding{NodeID: "node_synth-mac", Path: "/Users/synth/scratch", Mode: c.CheckoutPlain}
	r1.NativeRef = &c.NativeRef{Kind: "claude_session", ID: "00000000-0000-4000-8000-000000000001"}
	r1.AccountID = "acct_synth-claude-a"
	r2 := run("run_synth-project-1", "ses_synth-project", "node_synth-linux", c.HarnessClaude, c.RunRunning, claudeCaps)
	r2.Checkout = &c.CheckoutBinding{NodeID: "node_synth-linux", Path: "/home/synth/src/outfit-wt/api-client", RepoID: "outfit", Mode: c.CheckoutWorktree, Branch: "api-client"}
	r2.Cwd = r2.Checkout.Path
	r2.NativeRef = &c.NativeRef{Kind: "claude_session", ID: "00000000-0000-4000-8000-000000000002"}
	r2.AccountID = "acct_synth-claude-b"
	b.Sessions = []c.SessionView{
		session("ses_synth-standalone", c.HarnessClaude, "scratch (standalone)", nil, r1, "synth-mac"),
		session("ses_synth-project", c.HarnessClaude, "outfit API client", &prj, r2, "synth-linux"),
	}
	b.Exchanges = []c.FixtureExchange{{
		Name: "start standalone session", Op: c.OpStartSession, Principal: &owner,
		Request:     raw(c.StartSessionRequest{Mutation: c.Mutation{OperationID: "op_synth-start-1"}, NodeID: "node_synth-mac", Harness: c.HarnessClaude, Cwd: "/Users/synth/scratch", AccountPolicy: c.AccountPolicy{Mode: c.AccountEligible}}),
		Response:    raw(c.StartSessionResponse{Session: b.Sessions[0], CommandID: "cmd_synth-start-1"}),
		Explanation: "No project is needed; project_id is null.",
	}, {
		Name: "start project session in a worktree", Op: c.OpStartSession, Principal: &owner,
		Request:  raw(c.StartSessionRequest{Mutation: c.Mutation{OperationID: "op_synth-start-2"}, NodeID: "node_synth-linux", Harness: c.HarnessClaude, ProjectID: &prj, Checkout: &c.CheckoutRequest{RepoID: "outfit", Mode: c.CheckoutWorktree, Branch: "api-client"}, AccountPolicy: c.AccountPolicy{Mode: c.AccountEligible}, IncludeContext: true}),
		Response: raw(c.StartSessionResponse{Session: b.Sessions[1], CommandID: "cmd_synth-start-2"}),
	}}
	return b
}

// 2. Node disconnected with a live run and stale fleet status.
func f02Disconnected() *c.FixtureBundle {
	b := bundle("node-disconnected-live-run", "The Linux node lost its control connection 7 minutes ago. Its run is reported with stale freshness and state unknown — not failed, not idle.", []string{"R01", "R14"}, []string{"A01", "A19"})
	n := nodeLinux
	n.Connection = c.NodeConnection{State: c.ConnDisconnected, LastSeenAt: pt(at(-7)), Protocol: 1}
	b.Nodes = []c.Node{nodeMac, n}
	r := run("run_synth-offline-1", "ses_synth-offline", "node_synth-linux", c.HarnessClaude, c.RunUnknown, claudeCaps)
	r.StateReason = "node disconnected; last observed running"
	r.Freshness = c.Freshness{ObservedAt: at(-7), Source: "node:node_synth-linux", Stale: true, Reason: "node disconnected"}
	sv := session("ses_synth-offline", c.HarnessClaude, "long refactor", nil, r, "synth-linux")
	sv.Freshness = c.Freshness{ObservedAt: at(-7), Source: "hub-cache", Stale: true, Reason: "node disconnected"}
	b.Sessions = []c.SessionView{sv}
	b.Events = []c.Event{{EventID: "evt_synth-conn-1", Cursor: 101, Origin: "hub", Kind: c.EvNodeConnection, Object: c.ObjectRef{Kind: c.KindNode, ID: "node_synth-linux"}, PayloadVersion: 1, Payload: raw(n.Connection), OccurredAt: at(-7), IngestedAt: at(-7)}}
	return b
}

// 3. Message queued, runtime accepted, replied, rejected, and indeterminate.
func f03Delivery() *c.FixtureBundle {
	b := bundle("message-delivery-states", "Independent delivery facts: hub storage, runtime delivery, acknowledgement. Accepted means a supported input interface accepted it — not that the agent completed anything.", []string{"R04"}, []string{"A04", "A05", "A06"})
	b.Nodes = []c.Node{nodeMac, nodeLinux}
	ra := run("run_synth-a-1", "ses_synth-a", "node_synth-mac", c.HarnessClaude, c.RunRunning, claudeCaps)
	rb := run("run_synth-b-1", "ses_synth-b", "node_synth-linux", c.HarnessClaude, c.RunWaitingInput, claudeCaps)
	b.Sessions = []c.SessionView{session("ses_synth-a", c.HarnessClaude, "coordinator", nil, ra, "synth-mac"), session("ses_synth-b", c.HarnessClaude, "worker", nil, rb, "synth-linux")}
	st := func(rt c.RuntimeDeliveryState, ack c.AckState) c.DeliveryStatus {
		return c.DeliveryStatus{Storage: c.StorageStored, StoredAt: pt(at(-10)), Runtime: rt, Ack: ack, UpdatedAt: at(-9)}
	}
	queued := msg("msg_synth-queued", "ses_synth-a", "ses_synth-b", "Synthetic: please run the tests when free.", st(c.DeliveryQueued, c.AckNone))
	queued.Status.RuntimeReason = "recipient busy; waiting for idle turn"
	accepted := msg("msg_synth-accepted", "ses_synth-a", "ses_synth-b", "Synthetic: review artifact ver_synth-report-1.", st(c.DeliveryAccepted, c.AckNone))
	accepted.Envelope.Attachments = []c.Attachment{{ArtifactVersionID: "ver_synth-report-1"}}
	accepted.Status.BoundRunID = "run_synth-b-1"
	accepted.Status.Receipts = []c.DeliveryReceipt{{MessageID: "msg_synth-accepted", AttemptID: "dlv_synth-1", SessionID: "ses_synth-b", RunID: "run_synth-b-1", NodeID: "node_synth-linux", State: c.DeliveryAccepted, Evidence: c.EvidenceHookReceipt, EvidenceRef: "hook:UserPromptSubmit:synthetic-1", ObservedAt: at(-9), OriginSeq: 41}}
	replied := msg("msg_synth-replied", "ses_synth-a", "ses_synth-b", "Synthetic: what's the status?", st(c.DeliveryAccepted, c.AckReplied))
	replied.Status.BoundRunID = "run_synth-b-1"
	replied.Status.AckRef = "msg_synth-reply"
	reply := msg("msg_synth-reply", "ses_synth-b", "ses_synth-a", "Synthetic: tests pass; published ver_synth-report-1.", st(c.DeliveryAccepted, c.AckNone))
	reply.Envelope.InReplyTo = "msg_synth-replied"
	reply.Envelope.ThreadID = "thr_synth-status"
	replied.Envelope.ThreadID = "thr_synth-status"
	reply.Status.BoundRunID = "run_synth-a-1"
	rejected := msg("msg_synth-rejected", "ses_synth-a", "ses_synth-b", "Synthetic: steer now.", st(c.DeliveryRejected, c.AckNone))
	rejected.Envelope.Mode = c.ModeSteer
	rejected.Status.RuntimeReason = "unsupported: adapter does not advertise input.steer"
	indet := msg("msg_synth-indeterminate", "ses_synth-a", "ses_synth-b", "Synthetic: delete the temp branch.", st(c.DeliveryIndeterminate, c.AckNone))
	indet.Status.BoundRunID = "run_synth-b-1"
	indet.Status.RuntimeReason = "node restarted after submitting input and before recording a receipt; no evidence found yet"
	indet.Status.Receipts = []c.DeliveryReceipt{{MessageID: "msg_synth-indeterminate", AttemptID: "dlv_synth-2", SessionID: "ses_synth-b", RunID: "run_synth-b-1", NodeID: "node_synth-linux", State: c.DeliveryIndeterminate, Evidence: c.EvidenceNone, Detail: "crash window", ObservedAt: at(-8), OriginSeq: 44}}
	pending := msg("msg_synth-pending-local", "ses_synth-a", "ses_synth-b", "Synthetic: queued while hub unreachable.", c.DeliveryStatus{Storage: c.StoragePendingLocal, Runtime: c.DeliveryQueued, Ack: c.AckNone, UpdatedAt: at(-1)})
	// v1.6: accepted through the Claude doorbell while a human was attached
	// (no paste): one receipt with the node-minted attempt, inbox-read
	// evidence, consumed at the read.
	doorbell := msg("msg_synth-doorbell", "ses_synth-a", "ses_synth-b", "Synthetic: sent while the owner was attached.", st(c.DeliveryAccepted, c.AckAcknowledged))
	doorbell.Status.BoundRunID = "run_synth-b-1"
	doorbell.Status.AckRef = "op_synth-ack-doorbell"
	doorbell.Status.ConsumedAt = pt(at(-9))
	doorbell.Status.RuntimeReason = "doorbell: read with the pyre inbox tool"
	doorbell.Status.Receipts = []c.DeliveryReceipt{{MessageID: "msg_synth-doorbell", AttemptID: "dlv_synth-3", SessionID: "ses_synth-b", RunID: "run_synth-b-1", NodeID: "node_synth-linux", State: c.DeliveryAccepted, Evidence: c.EvidenceClaudeInboxRead, EvidenceRef: "evt_synth-posttooluse-1", Detail: "doorbell: read with the pyre inbox tool", ObservedAt: at(-9), OriginSeq: 47, Consumed: true}}
	b.Messages = []c.MessageView{queued, accepted, replied, reply, rejected, indet, pending, doorbell}
	for i := range b.Messages {
		b.Messages[i].Cursor = int64(i + 1)
	}
	b.Attention = []c.AttentionItem{{AttentionID: "att_synth-indet", Kind: c.AttnIndeterminate, Severity: "urgent", GroupKey: "indeterminate:msg_synth-indeterminate", Object: c.ObjectRef{Kind: c.KindMessage, ID: "msg_synth-indeterminate"}, SessionID: "ses_synth-b", Title: "Delivery outcome unknown", Summary: "Inspect evidence before reissuing.", State: c.AttnOpen, Count: 1, CreatedAt: at(-8), UpdatedAt: at(-8), DeepLink: c.DeepLink(c.ObjectRef{Kind: c.KindMessage, ID: "msg_synth-indeterminate"})}}
	return b
}

// 4. Duplicate operation with identical body and conflicting body.
func f04Idempotency() *c.FixtureBundle {
	b := bundle("duplicate-operation", "Same operation_id + same body returns the stored result; same operation_id + different body is a conflict. A repeated message_id never creates a second stored message.", []string{"R04", "R14"}, []string{"A05"})
	sess := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"}
	req := c.SendMessageRequest{Mutation: c.Mutation{OperationID: "op_synth-dup-1"}, MessageID: "msg_synth-dup", Recipient: c.Address{Kind: c.AddrSession, ID: "ses_synth-b"}, Mode: c.ModeQueue, Body: c.MessageBody{Text: "Synthetic: run lint."}}
	resp := msg("msg_synth-dup", "ses_synth-a", "ses_synth-b", "Synthetic: run lint.", c.DeliveryStatus{Storage: c.StorageStored, StoredAt: pt(at(0)), Runtime: c.DeliveryQueued, Ack: c.AckNone, UpdatedAt: at(0)})
	resp.Envelope.OperationID = "op_synth-dup-1"
	resp.Cursor = 7
	req2 := req
	req2.Body = c.MessageBody{Text: "Synthetic: run lint AND deploy."}
	req3 := req
	req3.OperationID = "op_synth-dup-2"
	b.Exchanges = []c.FixtureExchange{
		{Name: "first send", Op: c.OpSendMessage, Principal: &sess, Request: raw(req), Response: raw(resp)},
		{Name: "retry with identical body", Op: c.OpSendMessage, Principal: &sess, Request: raw(req), Response: raw(resp), Explanation: "Returns the original stored message; no new message or delivery."},
		{Name: "same operation_id, different body", Op: c.OpSendMessage, Principal: &sess, Request: raw(req2), Error: &c.Error{Code: c.ErrConflict, Message: "operation_id reused with a different request body", Ref: &c.ObjectRef{Kind: c.KindOperation, ID: "op_synth-dup-1"}}},
		{Name: "new operation_id, same message_id, same content", Op: c.OpSendMessage, Principal: &sess, Request: raw(req3), Response: raw(resp), Explanation: "Message storage is deduplicated by message_id; identical content returns the existing message."},
		{Name: "protocol mismatch", Op: c.OpHello, Request: raw(c.HelloRequest{Protocol: 99, Client: "pyre-cli/9.0.0"}), Error: &c.Error{Code: c.ErrProtocolMismatch, Message: "client protocol 99 not supported; hub supports 1..1", Details: map[string]any{"min_protocol": 1, "protocol": 1}}},
	}
	return b
}

// 5. Input lease with conflicting raw writes queued, structured mail continuing, takeover.
func f05Lease() *c.FixtureBundle {
	b := bundle("input-lease-takeover", "The owner holds a raw-input lease on a run (attached terminal). A second client's raw write is refused with the lease visible; structured peer mail still delivers per adapter semantics; an explicit takeover is recorded.", []string{"R02", "R04", "R12"}, []string{"A04"})
	r := run("run_synth-leased", "ses_synth-leased", "node_synth-mac", c.HarnessClaude, c.RunWaitingInput, claudeCaps)
	lease := c.InputLease{LeaseID: "lease_synth-1", RunID: r.RunID, Holder: owner, AcquiredAt: at(-3), RenewedAt: at(0), ExpiresAt: at(2)}
	r.Lease = &lease
	b.Sessions = []c.SessionView{session("ses_synth-leased", c.HarnessClaude, "pairing session", nil, r, "synth-mac")}
	taken := c.InputLease{LeaseID: "lease_synth-2", RunID: r.RunID, Holder: android, AcquiredAt: at(1), RenewedAt: at(1), ExpiresAt: at(3), TakeoverOf: "lease_synth-1"}
	released := lease
	released.ReleasedAt = pt(at(1))
	b.Leases = []c.InputLease{released, taken}
	b.Exchanges = []c.FixtureExchange{
		{Name: "competing acquire without takeover", Op: c.OpAcquireLease, Principal: &android, Request: raw(c.LeaseRequest{Mutation: c.Mutation{OperationID: "op_synth-lease-2"}, RunID: r.RunID}), Error: &c.Error{Code: c.ErrConflict, Message: "run input is leased by owner (synthetic) until 12:02Z", Ref: &c.ObjectRef{Kind: c.KindLease, ID: "lease_synth-1"}}},
		{Name: "explicit takeover", Op: c.OpAcquireLease, Principal: &android, Request: raw(c.LeaseRequest{Mutation: c.Mutation{OperationID: "op_synth-lease-3"}, RunID: r.RunID, Takeover: true}), Response: raw(taken)},
	}
	m := msg("msg_synth-during-lease", "ses_synth-peer", "ses_synth-leased", "Synthetic: FYI the build is green.", c.DeliveryStatus{Storage: c.StorageStored, StoredAt: pt(at(0)), Runtime: c.DeliveryQueued, RuntimeReason: "human holds input lease; queued until the run is idle and unleased or the adapter's structured path accepts", Ack: c.AckNone, UpdatedAt: at(0)})
	b.Messages = []c.MessageView{m}
	return b
}

// 6. Document update conflict and immutable artifact publication/review.
func f06DocsArtifacts() *c.FixtureBundle {
	b := bundle("document-conflict-artifact-review", "Two sessions edit the brief from revision 1; one commits revision 2, the other gets a conflict with its proposal preserved. An artifact has two immutable versions; a comment stays on version 1.", []string{"R05", "R06"}, []string{"A08", "A09"})
	prj := "prj_synth-outfit"
	alice := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"}
	bob := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-b"}
	b.Projects = []c.Project{{ProjectID: prj, Name: "Synthetic Outfit", BriefDocumentID: "doc_synth-brief", Repos: []c.RepoBinding{}, Checkouts: []c.CheckoutBinding{}, Version: 1, CreatedBy: owner, CreatedAt: at(-500), UpdatedAt: at(-500)}}
	b.Documents = []c.Document{{DocumentID: "doc_synth-brief", ProjectID: &prj, Title: "Brief", Slug: "brief", MediaType: "text/markdown", HeadRevisionID: "rev_synth-brief-2", HeadSeq: 2, CreatedBy: owner, CreatedAt: at(-500), UpdatedAt: at(-2)}}
	b.Revisions = []c.DocumentRevision{
		{RevisionID: "rev_synth-brief-1", DocumentID: "doc_synth-brief", Seq: 1, Digest: "sha256:synthetic-1", Size: 20, Author: owner, CreatedAt: at(-500), Content: "# Brief\n\nGoal: X.\n"},
		{RevisionID: "rev_synth-brief-2", DocumentID: "doc_synth-brief", Seq: 2, ParentRevisionID: "rev_synth-brief-1", Digest: "sha256:synthetic-2", Size: 40, Author: alice, CreatedAt: at(-2), Summary: "add constraint", Content: "# Brief\n\nGoal: X.\nConstraint: keep API stable.\n"},
	}
	b.Proposals = []c.DocumentProposal{{ProposalID: "prp_synth-bob-1", DocumentID: "doc_synth-brief", BaseRevisionID: "rev_synth-brief-1", Content: "# Brief\n\nGoal: X and Y.\n", Author: bob, CreatedAt: at(-1), State: c.ProposalConflicted, HeadAtConflict: "rev_synth-brief-2"}}
	b.Exchanges = []c.FixtureExchange{{
		Name: "stale propose", Op: c.OpProposeRevision, Principal: &bob,
		Request: raw(c.ProposeRevisionRequest{Mutation: c.Mutation{OperationID: "op_synth-prop-bob"}, DocumentID: "doc_synth-brief", BaseRevisionID: "rev_synth-brief-1", Content: "# Brief\n\nGoal: X and Y.\n"}),
		Error:   &c.Error{Code: c.ErrConflict, Message: "document head moved", Ref: &c.ObjectRef{Kind: c.KindDocument, ID: "doc_synth-brief"}, Details: map[string]any{"current_head": "rev_synth-brief-2", "proposal_id": "prp_synth-bob-1"}},
	}}
	prov := c.Provenance{SessionID: "ses_synth-b", RunID: "run_synth-b-1", NodeID: "node_synth-linux", ProjectID: &prj}
	b.Artifacts = []c.Artifact{{ArtifactID: "art_synth-report", ProjectID: &prj, SessionID: "ses_synth-b", Title: "Synthetic test report", LatestVersionID: "ver_synth-report-2", VersionCount: 2, CreatedBy: bob, CreatedAt: at(-20), UpdatedAt: at(-5)}}
	b.Versions = []c.ArtifactVersion{
		{VersionID: "ver_synth-report-1", ArtifactID: "art_synth-report", Seq: 1, Digest: "sha256:7c1e0b6f0e5d0f7b9b5c0d8f9b1e6a4b3d2c1f0e9d8c7b6a5f4e3d2c1b0a9f8e", Size: 29, MediaType: "text/markdown", Filename: "report.md", Author: bob, Provenance: prov, CreatedAt: at(-20), ContentURL: "/v1/content/ver_synth-report-1"},
		{VersionID: "ver_synth-report-2", ArtifactID: "art_synth-report", Seq: 2, PreviousVersionID: "ver_synth-report-1", Digest: "sha256:synthetic-v2", Size: 43, MediaType: "text/html", Filename: "report.html", Author: bob, Provenance: prov, CreatedAt: at(-5), ContentURL: "/v1/content/ver_synth-report-2", PreviewURL: "/preview/ver_synth-report-2/{token}"},
	}
	b.Contents = map[string]string{"ver_synth-report-1": "# Report\n\nAll synthetic tests pass.\n", "ver_synth-report-2": "<h1>Report</h1><script>/*sandboxed*/</script>"}
	b.Comments = []c.ReviewComment{{CommentID: "cmt_synth-1", Target: c.ObjectRef{Kind: c.KindArtifactVersion, ID: "ver_synth-report-1"}, Anchor: &c.Anchor{Kind: "line", Value: "3"}, Author: android, Body: "Synthetic: include the failure count.", Mentions: []string{"ses_synth-b"}, CreatedAt: at(-15), UpdatedAt: at(-15), ProjectID: &prj}}
	b.Attention = []c.AttentionItem{{AttentionID: "att_synth-review", Kind: c.AttnReviewReady, Severity: "normal", GroupKey: "review:art_synth-report", Object: c.ObjectRef{Kind: c.KindArtifactVersion, ID: "ver_synth-report-2"}, ProjectID: &prj, Title: "Synthetic test report v2 ready for review", State: c.AttnOpen, Count: 2, CreatedAt: at(-20), UpdatedAt: at(-5), DeepLink: c.DeepLink(c.ObjectRef{Kind: c.KindArtifactVersion, ID: "ver_synth-report-2"})}}
	b.Exchanges = append(b.Exchanges, c.FixtureExchange{
		Name: "finalize with wrong digest", Op: c.OpFinalizeUpload, Principal: &bob,
		Request: raw(c.FinalizeUploadRequest{Mutation: c.Mutation{OperationID: "op_synth-fin-bad"}, UploadID: "upl_synth-bad"}),
		Error:   &c.Error{Code: c.ErrConflict, Message: "uploaded bytes do not match declared sha256; nothing was published", Ref: &c.ObjectRef{Kind: c.KindUpload, ID: "upl_synth-bad"}},
	})
	return b
}

// 7. Fresh, stale, exhausted, and unknown account availability.
func f07Accounts() *c.FixtureBundle {
	b := bundle("account-availability", "Four Claude/Codex subscription accounts with fresh, stale, exhausted, and unknown observations. Unknown/stale never means unlimited.", []string{"R08"}, []string{"A11", "A13"})
	mk := func(id string, p c.Provider, label string, obs c.AccountObservation, asg []c.AccountAssignment) c.AccountView {
		obs.AccountID = id
		obs.Provider = p
		return c.AccountView{Account: c.Account{AccountID: id, Provider: p, Label: label, Kind: "subscription", CreatedAt: at(-9000)}, Latest: &obs, Grants: []c.DeviceGrant{{GrantID: "grant_" + id[5:] + "-mac", AccountID: id, NodeID: "node_synth-mac", Harness: harnessFor(p), ProfileRef: label, Mechanism: mech(p), Kind: credKind(p), ProfileDir: "/Users/synth/.local/state/pyre/profiles/" + id + "/" + string(harnessFor(p)), MaxProcs: maxProcs(p), State: c.GrantValid, RefreshOwner: "harness", Generation: 1, ObservedAt: at(-2)}}, Assignments: asg}
	}
	b.Accounts = []c.AccountView{
		mk("acct_synth-claude-a", c.ProviderAnthropic, "claude-personal-a", c.AccountObservation{ReportingNode: "node_synth-mac", ObservedAt: at(-1), Source: "rota:usage-endpoint", Confidence: "high", Eligibility: c.EligibleYes, Windows: []c.UsageWindow{{Name: "five_hour", UsedPercent: 22, ResetAt: pt(at(140))}, {Name: "seven_day", UsedPercent: 41, ResetAt: pt(at(5000))}}},
			[]c.AccountAssignment{{AssignmentID: "asg_synth-1", AccountID: "acct_synth-claude-a", RunID: "run_synth-standalone-1", SessionID: "ses_synth-standalone", NodeID: "node_synth-mac", Reason: "most headroom (fresh)", AssignedAt: at(-30), Freshness: fresh("rota", -31)}}),
		mk("acct_synth-claude-b", c.ProviderAnthropic, "claude-personal-b", c.AccountObservation{ReportingNode: "node_synth-linux", ObservedAt: at(-95), Source: "rota:usage-endpoint", Confidence: "low", Eligibility: c.EligibleUnknown, Stale: true, Detail: "usage endpoint rate-limited; last good sample 95m old", Windows: []c.UsageWindow{{Name: "five_hour", UsedPercent: 60}}}, []c.AccountAssignment{}),
		mk("acct_synth-claude-c", c.ProviderAnthropic, "claude-work-c", c.AccountObservation{ReportingNode: "node_synth-mac", ObservedAt: at(-3), Source: "limit-event", Confidence: "high", Eligibility: c.EligibleExhausted, ResetAt: pt(at(80)), Detail: "usage limit reached", Windows: []c.UsageWindow{{Name: "five_hour", UsedPercent: 100, ResetAt: pt(at(80))}}}, []c.AccountAssignment{}),
		mk("acct_synth-codex-a", c.ProviderOpenAI, "codex-personal-a", c.AccountObservation{ReportingNode: "node_synth-linux", ObservedAt: at(-600), Source: "codex:app-server", Confidence: "low", Eligibility: c.EligibleUnknown, Stale: true, Detail: "no observation since node restart"}, []c.AccountAssignment{}),
	}
	b.Accounts[1].Grants[0].State = c.GrantUnknown
	for _, a := range b.Accounts {
		b.Observations = append(b.Observations, *a.Latest)
	}
	return b
}

func harnessFor(p c.Provider) c.Harness {
	if p == c.ProviderOpenAI {
		return c.HarnessCodex
	}
	return c.HarnessClaude
}

func credKind(p c.Provider) c.CredentialKind {
	if p == c.ProviderOpenAI {
		return c.CredCodexHomeFile
	}
	return c.CredClaudeProfileStore
}

func maxProcs(p c.Provider) int {
	if p == c.ProviderOpenAI {
		return 1
	}
	return 0
}

func mech(p c.Provider) string {
	if p == c.ProviderOpenAI {
		return "codex_home"
	}
	return "claude_config_dir"
}

// 8. Same-provider fallback with native resume and with handoff-only capability.
func f08Fallback() *c.FixtureBundle {
	b := bundle("same-provider-fallback", "A Claude run hit a usage limit and continued natively on another Claude account (new run, same session). A Codex run hit a limit where native cross-account resume is unproven, so a linked handoff successor was created. A third case has no eligible account: work is preserved and the owner is notified. No cross-provider or API-billing fallback.", []string{"R08", "R04", "R13"}, []string{"A12"})
	old := run("run_synth-fb-1", "ses_synth-fb", "node_synth-mac", c.HarnessClaude, c.RunExited, claudeCaps)
	old.AccountID = "acct_synth-claude-c"
	old.EndedAt = pt(at(-3))
	old.ExitCode = ip(0)
	old.StateReason = "quiesced after usage limit"
	nw := run("run_synth-fb-2", "ses_synth-fb", "node_synth-mac", c.HarnessClaude, c.RunRunning, claudeCaps)
	nw.AccountID = "acct_synth-claude-a"
	nw.PreviousRunID = old.RunID
	nw.ContinuationKind = c.ContinuationNativeResume
	nw.NativeRef = &c.NativeRef{Kind: "claude_session", ID: "00000000-0000-4000-8000-0000000000fb"}
	b.Runs = []c.Run{*old}
	b.Sessions = []c.SessionView{session("ses_synth-fb", c.HarnessClaude, "migration work", nil, nw, "synth-mac")}
	codexOld := run("run_synth-fbx-1", "ses_synth-fbx", "node_synth-linux", c.HarnessCodex, c.RunExited, codexCaps)
	codexOld.AccountID = "acct_synth-codex-a"
	codexNew := run("run_synth-fbx-2", "ses_synth-fbx2", "node_synth-linux", c.HarnessCodex, c.RunStarting, codexCaps)
	codexNew.AccountID = "acct_synth-codex-b"
	codexNew.ContinuationKind = c.ContinuationHandoff
	codexNew.PreviousRunID = codexOld.RunID
	s2 := session("ses_synth-fbx2", c.HarnessCodex, "api work (handoff successor)", nil, codexNew, "synth-linux")
	s2.Session.Lineage = c.Lineage{ForkedFrom: "ses_synth-fbx", HandoffNoteID: "note_synth-handoff-1"}
	b.Sessions = append(b.Sessions, session("ses_synth-fbx", c.HarnessCodex, "api work (limited)", nil, codexOld, "synth-linux"), s2)
	b.Notes = []c.Note{{NoteID: "note_synth-handoff-1", Kind: c.NoteHandoff, SessionID: "ses_synth-fbx", Title: "Handoff: api work", Body: "Synthetic handoff: branch api-client, last green commit abc123, next: add retries.", Author: c.Principal{Kind: c.PrincipalSystem, ID: "pyre"}, CreatedAt: at(-2), Provenance: c.Provenance{SessionID: "ses_synth-fbx", RunID: "run_synth-fbx-1", NodeID: "node_synth-linux"}}}
	b.Fallbacks = []c.FallbackPlan{
		{SessionID: "ses_synth-fb", FromRunID: old.RunID, FromAccountID: "acct_synth-claude-c", Outcome: c.FallbackNativeResume, ToAccountID: "acct_synth-claude-a", NewRunID: nw.RunID, Reason: "usage limit; acct_synth-claude-a eligible (fresh, 22% 5h)", PlannedAt: at(-3)},
		{SessionID: "ses_synth-fbx", FromRunID: codexOld.RunID, FromAccountID: "acct_synth-codex-a", Outcome: c.FallbackHandoff, ToAccountID: "acct_synth-codex-b", NewRunID: codexNew.RunID, Reason: "cross-account native resume unproven for codex; created linked successor with handoff", UncertainWork: true, PlannedAt: at(-2)},
		{SessionID: "ses_synth-standalone", FromRunID: "run_synth-standalone-1", FromAccountID: "acct_synth-claude-a", Outcome: c.FallbackWaiting, Reason: "no eligible anthropic subscription (c exhausted until 13:20Z, b unknown/stale); work preserved", PlannedAt: at(-1)},
	}
	b.Attention = []c.AttentionItem{{AttentionID: "att_synth-pool", Kind: c.AttnAccountPool, Severity: "urgent", GroupKey: "pool:anthropic", Object: c.ObjectRef{Kind: c.KindSession, ID: "ses_synth-standalone"}, SessionID: "ses_synth-standalone", Title: "No eligible Claude account", Summary: "Session paused and resumable; earliest reset 13:20Z.", State: c.AttnOpen, Count: 1, CreatedAt: at(-1), UpdatedAt: at(-1), DeepLink: c.DeepLink(c.ObjectRef{Kind: c.KindSession, ID: "ses_synth-standalone"})}}
	return b
}

// 9. Pending/resolved/expired approval deep link.
func f09Approvals() *c.FixtureBundle {
	b := bundle("approval-deeplinks", "A pending permission request, one resolved natively in the terminal (stale phone link must show the existing resolution), and one expired by run replacement.", []string{"R13", "R02"}, []string{"A18"})
	r := run("run_synth-apr", "ses_synth-apr", "node_synth-mac", c.HarnessClaude, c.RunWaitingApproval, claudeCaps)
	r.PendingApprovalIDs = []string{"apr_synth-pending"}
	b.Sessions = []c.SessionView{session("ses_synth-apr", c.HarnessClaude, "deploy prep", nil, r, "synth-mac")}
	opts := []c.ApprovalOption{{ID: "allow", Label: "Allow once", Kind: "allow"}, {ID: "deny", Label: "Deny", Kind: "deny"}}
	b.Approvals = []c.Approval{
		{ApprovalID: "apr_synth-pending", SessionID: "ses_synth-apr", RunID: r.RunID, RequestID: "toolu_synth_01", Kind: c.ApprovalPermission, Title: "Run `make deploy-staging`?", Body: "Bash command requested by the agent (synthetic).", Options: opts, CreatedAt: at(-2), State: c.ApprovalPending},
		{ApprovalID: "apr_synth-resolved", SessionID: "ses_synth-apr", RunID: r.RunID, RequestID: "toolu_synth_00", Kind: c.ApprovalPermission, Title: "Edit config.yaml?", Options: opts, CreatedAt: at(-20), State: c.ApprovalResolved, Resolution: &c.ApprovalResolution{OptionID: "allow", Actor: c.Principal{Kind: c.PrincipalSystem, ID: "native-terminal"}, ResolvedAt: at(-19), Source: "native"}},
		{ApprovalID: "apr_synth-expired", SessionID: "ses_synth-apr", RunID: "run_synth-apr-old", RequestID: "toolu_synth_old", Kind: c.ApprovalQuestion, Title: "Which branch?", AllowText: true, CreatedAt: at(-90), State: c.ApprovalWithdrawn},
	}
	answered := b.Approvals[0]
	answered.State = c.ApprovalResolved
	answered.Resolution = &c.ApprovalResolution{OptionID: "allow", Actor: android, ResolvedAt: at(0), Source: "pyre"}
	b.Exchanges = []c.FixtureExchange{
		{Name: "answer pending", Op: c.OpAnswerApproval, Principal: &android, Request: raw(c.AnswerApprovalRequest{Mutation: c.Mutation{OperationID: "op_synth-apr-1"}, ApprovalID: "apr_synth-pending", RunID: r.RunID, OptionID: "allow"}), Response: raw(answered)},
		{Name: "double tap returns existing result", Op: c.OpAnswerApproval, Principal: &android, Request: raw(c.AnswerApprovalRequest{Mutation: c.Mutation{OperationID: "op_synth-apr-1"}, ApprovalID: "apr_synth-pending", RunID: r.RunID, OptionID: "allow"}), Response: raw(answered), Explanation: "Same operation_id: stored result returned, runtime answered once."},
		{Name: "stale link to natively resolved approval", Op: c.OpAnswerApproval, Principal: &android, Request: raw(c.AnswerApprovalRequest{Mutation: c.Mutation{OperationID: "op_synth-apr-2"}, ApprovalID: "apr_synth-resolved", RunID: r.RunID, OptionID: "deny"}), Error: &c.Error{Code: c.ErrConflict, Message: "approval already resolved (allow, native terminal)", Ref: &c.ObjectRef{Kind: c.KindApproval, ID: "apr_synth-resolved"}}},
		{Name: "answer for replaced run", Op: c.OpAnswerApproval, Principal: &android, Request: raw(c.AnswerApprovalRequest{Mutation: c.Mutation{OperationID: "op_synth-apr-3"}, ApprovalID: "apr_synth-expired", RunID: "run_synth-apr-old", Text: "main"}), Error: &c.Error{Code: c.ErrExpired, Message: "approval withdrawn: its run was replaced", Ref: &c.ObjectRef{Kind: c.KindApproval, ID: "apr_synth-expired"}}},
	}
	b.Attention = []c.AttentionItem{{AttentionID: "att_synth-apr", Kind: c.AttnApproval, Severity: "urgent", GroupKey: "approval:apr_synth-pending", Object: c.ObjectRef{Kind: c.KindApproval, ID: "apr_synth-pending"}, SessionID: "ses_synth-apr", Title: "Approve: make deploy-staging", State: c.AttnOpen, Count: 1, CreatedAt: at(-2), UpdatedAt: at(-2), DeepLink: c.DeepLink(c.ObjectRef{Kind: c.KindApproval, ID: "apr_synth-pending"})}}
	return b
}

// 10. Environment drift, staged update, rollback, protocol mismatch.
func f10Environment() *c.FixtureBundle {
	b := bundle("environment-drift-update-rollback", "An adopted user config with a local edit (drift), a staged Claude update whose health check failed and was rolled back, a running session pinned to its original version, and a plan preview with a restart-required change.", []string{"R10", "R11"}, []string{"A15", "A16"})
	prj := "prj_synth-outfit"
	b.Environments = []c.EnvironmentGeneration{{
		GenerationID: "env_synth-3", NodeID: "node_synth-mac", Profile: "default", ProjectID: &prj, CreatedAt: at(-60),
		RuntimeVersions:  map[string]string{"claude": "2.1.289", "codex": "0.160.0"},
		ConfigDigests:    map[string]c.Digest{"claude/settings.json#pyre-managed": "sha256:synthetic-settings"},
		Skills:           []c.ExtensionRef{{Name: "synth-review", Layer: c.LayerManagedProject, Managed: true, Version: "1.0.0"}, {Name: "my-local-skill", Layer: c.LayerAdoptedUser, Managed: false}},
		Plugins:          []c.ExtensionRef{{Name: "synth-plugin", Harness: c.HarnessClaude, Layer: c.LayerManagedGlobal, Managed: true, Version: "0.3.1"}},
		MCPServers:       []c.ExtensionRef{{Name: "pyre", Layer: c.LayerManagedGlobal, Managed: true}},
		ConnectionGrants: []string{"conn_synth-github"},
		Values:           []c.ResolvedValue{{Key: "claude.model", Value: "default", Layer: c.LayerAdoptedUser}, {Key: "claude.permissions.defaultMode", Value: "default", Layer: c.LayerManagedProject, Overrode: []c.ConfigLayer{c.LayerAdoptedUser}, RestartRequired: true}},
		Digest:           "sha256:synthetic-env-3", PreviousID: "env_synth-2",
	}}
	b.Drift = []c.DriftItem{{Path: "~/.claude/settings.json", Key: "hooks.Stop", Kind: "unmanaged_addition", Detail: "user-added hook outside the Pyre-managed section is preserved"}, {Path: "~/.codex/config.toml", Key: "model", Kind: "modified", Expected: "gpt-synth", Actual: "gpt-synth-mini"}}
	b.Tools = []c.ToolVersion{
		{Tool: "claude", Version: "2.1.289", State: "active", InstalledAt: at(-5000), Health: "ok", InUseByRuns: []string{"run_synth-standalone-1"}},
		{Tool: "claude", Version: "2.1.300", State: "failed", InstalledAt: at(-10), Health: "failed: health check exited 1 (synthetic)"},
		{Tool: "claude", Version: "2.1.280", State: "previous", InstalledAt: at(-20000), Health: "ok"},
	}
	b.Plans = []c.ChangePlan{{PlanID: "plan_synth-1", NodeID: "node_synth-mac", Scope: "project:" + prj, Changes: []c.PlanChange{{Path: "~/.claude/skills/synth-review/SKILL.md", Action: "create"}, {Path: "~/.claude/settings.json#pyre-managed", Action: "update", Diff: "- permissions.defaultMode: acceptEdits\n+ permissions.defaultMode: default", Backup: "~/.local/state/pyre/backups/settings.json.20261004T1200Z"}}, Conflicts: []c.DriftItem{}, RestartRequired: []string{"ses_synth-standalone"}, ResultDigest: "sha256:synthetic-plan-1"}}
	b.Connections = []c.Connection{{ConnectionID: "conn_synth-github", Name: "github-synth", Service: "github", Modes: []string{"local_credential", "remote_invoke"}, Availability: []c.ConnectionAvailability{{NodeID: "node_synth-mac", State: "verified", Mode: "local_credential", VerifiedAt: at(-5)}, {NodeID: "node_synth-linux", State: "missing", Mode: "remote_invoke", VerifiedAt: at(-5), Detail: "invocations route to node_synth-mac"}}, Scope: c.ConnectionScope{Projects: []string{prj}}}}
	b.Attention = []c.AttentionItem{{AttentionID: "att_synth-update", Kind: c.AttnUpdate, Severity: "normal", GroupKey: "update:claude:node_synth-mac", Object: c.ObjectRef{Kind: c.KindNode, ID: "node_synth-mac"}, Title: "claude 2.1.300 failed health check; rolled back to 2.1.289", State: c.AttnOpen, Count: 1, CreatedAt: at(-10), UpdatedAt: at(-10), DeepLink: c.DeepLink(c.ObjectRef{Kind: c.KindNode, ID: "node_synth-mac"})}}
	b.Exchanges = []c.FixtureExchange{{Name: "node with newer protocol", Op: "node.connect", Request: raw(c.NodeHello{NodeID: "node_synth-linux", Protocol: 2, MinProtocol: 2, PyredVersion: "0.9.0-synth"}), Error: &c.Error{Code: c.ErrProtocolMismatch, Message: "node requires protocol >= 2; hub supports 1..1. Upgrade the hub before this node, or roll the node back.", Details: map[string]any{"hub_protocol": 1}}}}
	return b
}
