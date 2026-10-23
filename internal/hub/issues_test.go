package hub

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- issues filed by sessions ----

func (f *childFixture) file(p contracts.Principal, op string, kind contracts.IssueKind, title, body string) contracts.FileIssueResponse {
	f.x.t.Helper()
	r, err := f.x.h.FileIssue(as(p), contracts.FileIssueRequest{Mutation: contracts.Mutation{OperationID: op}, Kind: kind, Title: title, Body: body})
	if err != nil {
		f.x.t.Fatalf("file %s: %v", op, err)
	}
	return r
}

// issueMail lists the triage messages Pyre sent (sender system:issues).
func (x *harness) issueMail() []contracts.MessageEnvelope {
	x.t.Helper()
	return x.envelopes(`sender_kind='system' AND sender_id=?`, issueSender.ID)
}

func (f *childFixture) setTriage(p contracts.Principal, op, sessionID string) (contracts.IssueTriage, error) {
	return f.x.h.SetIssueTriage(as(p), contracts.SetIssueTriageRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: sessionID})
}

func (f *childFixture) updateIssue(p contracts.Principal, req contracts.UpdateIssueRequest) (contracts.Issue, error) {
	return f.x.h.UpdateIssue(as(p), req)
}

func TestNormalizeIssueTitle(t *testing.T) {
	for in, want := range map[string]string{
		"send_message to ses_01JSYNTH0000000000000000AB reported delivered after 3s":    "send message to reported delivered after",
		"Disk 97% full on /home/synth (2026-10-21T12:00:00Z)":                           "disk full on",
		"gh missing: see https://example.invalid/synth/1 in run_01JSYNTH00000000000000": "gh missing see in",
		"Commit deadbeef12 broke `pyre ls`":                                             "commit broke pyre ls",
		"Login EXPIRED for codex":                                                       "login expired for codex",
	} {
		if got := NormalizeIssueTitle(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if IssueFingerprint(contracts.IssueMachine, "Disk full", nodeMac) == IssueFingerprint(contracts.IssueMachine, "Disk full", nodeLin) {
		t.Error("a machine issue's fingerprint names its node")
	}
	if IssueFingerprint(contracts.IssuePyreBug, "Disk full", nodeMac) != IssueFingerprint(contracts.IssuePyreBug, "disk full!", nodeLin) {
		t.Error("a pyre_bug fingerprint ignores the node")
	}
}

// A session files an issue: the hub fills the reporter (session, run,
// node, harness, pyred version, project) and the automatic context, and
// redacts credentials. A repeat with other IDs and numbers is an
// occurrence of the same issue with its own evidence.
func TestIssueFileFillsReporterAndDedupes(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	cid := f.child.Session.SessionID
	f.inProject(synthProject, cid)
	childP := sesPrincipal(cid)

	r1 := f.file(childP, "op_synth-i1", contracts.IssuePyreBug, "send_message to ses_01JSYNTH0000000000000000AB reported delivered after 3s",
		"Synthetic evidence: the recipient never saw it. token ghp_SYNTHETICsyntheticSYNTHETIC1234")
	is := r1.Issue
	switch {
	case r1.Outcome != contracts.IssueCreated || !strings.HasPrefix(is.IssueID, "iss_") || is.Status != contracts.IssueOpen || is.Occurrences != 1:
		t.Fatalf("created: %+v", r1)
	case is.ReporterSessionID != cid || is.ReporterRunID != f.child.Run.RunID || is.NodeID != nodeMac || is.Harness != contracts.HarnessClaude:
		t.Fatalf("reporter: %+v", is)
	case is.PyreVersion != "0.1.0-synth" || is.ProjectID == nil || *is.ProjectID != synthProject || is.ReporterTitle != "synthetic child":
		t.Fatalf("versions/project: %+v", is)
	case !strings.Contains(is.Context, "state: waiting_input") || !strings.Contains(is.Context, f.child.Run.RunID):
		t.Fatalf("context:\n%s", is.Context)
	case strings.Contains(is.Body, "ghp_SYNTHETIC") || !strings.Contains(is.Body, "Synthetic evidence"):
		t.Fatalf("body not redacted: %s", is.Body)
	}
	if r1.TriageSessionID != "" || len(x.issueMail()) != 0 {
		t.Fatalf("no triage session: nothing is sent: %+v %v", r1, x.issueMail())
	}

	// A retry with the same operation ID is the same answer, no occurrence.
	if again := f.file(childP, "op_synth-i1", contracts.IssuePyreBug, "send_message to ses_01JSYNTH0000000000000000AB reported delivered after 3s",
		"Synthetic evidence: the recipient never saw it. token ghp_SYNTHETICsyntheticSYNTHETIC1234"); again.Issue.IssueID != is.IssueID || again.Issue.Occurrences != 1 {
		t.Fatalf("replay: %+v", again)
	}

	x.clock.Add(time.Minute)
	r2 := f.file(sesPrincipal(f.parent.Session.SessionID), "op_synth-i2", contracts.IssuePyreBug, "Send_message to ses_01JSYNTH0000000000000000CD reported delivered after 41s", "Second synthetic sighting.")
	if r2.Outcome != contracts.IssueRecurred || r2.Issue.IssueID != is.IssueID || r2.Issue.Occurrences != 2 || !r2.Issue.LastSeenAt.After(is.FirstSeenAt) {
		t.Fatalf("recurred: %+v", r2)
	}
	d, err := x.h.GetIssue(as(childP), contracts.GetIssueRequest{IssueID: is.IssueID})
	if err != nil || len(d.Occurrences) != 2 || d.Occurrences[1].Body != "Second synthetic sighting." || d.Occurrences[1].SessionID != f.parent.Session.SessionID || d.Issue.Body != is.Body {
		t.Fatalf("detail: %+v %v", d, err)
	}

	// Another kind, or a machine issue on another node, is another issue.
	if r := f.file(childP, "op_synth-i3", contracts.IssueGuidance, "send_message to ses_01JSYNTH0000000000000000AB reported delivered after 3s", ""); r.Outcome != contracts.IssueCreated {
		t.Fatalf("other kind: %+v", r)
	}
	if _, err := x.h.FileIssue(as(childP), contracts.FileIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-m0"}, Kind: contracts.IssueMachine, Title: "gh not on PATH", NodeID: "node_synth-unknown"}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	x.connect(nodeLin, 0)
	m1 := f.file(childP, "op_synth-m1", contracts.IssueMachine, "gh not on PATH", "")
	m2, err := x.h.FileIssue(as(childP), contracts.FileIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-m2"}, Kind: contracts.IssueMachine, Title: "gh not on PATH", NodeID: nodeLin})
	if err != nil || m2.Outcome != contracts.IssueCreated || m2.Issue.IssueID == m1.Issue.IssueID || m2.Issue.NodeID != nodeLin {
		t.Fatalf("machine issue on another node: %+v %v", m2, err)
	}
	x.clock.Add(time.Second)
	if m3 := f.file(childP, "op_synth-m3", contracts.IssueMachine, "GH not on PATH!", ""); m3.Issue.IssueID != m1.Issue.IssueID {
		t.Fatalf("same machine issue: %+v", m3)
	}

	// Listing: open ones by default, most recently seen first.
	l, err := x.h.ListIssues(as(childP), contracts.ListIssuesRequest{})
	if err != nil || len(l.Issues) != 4 || l.Issues[0].IssueID != m1.Issue.IssueID {
		t.Fatalf("list: %+v %v", l, err)
	}
	if l, _ := x.h.ListIssues(as(owner), contracts.ListIssuesRequest{Kind: contracts.IssueMachine, NodeID: nodeLin}); len(l.Issues) != 1 {
		t.Fatalf("list machine on linux: %+v", l)
	}

	// Validation.
	for _, req := range []contracts.FileIssueRequest{{Kind: "nonsense", Title: "x"}, {Kind: contracts.IssueOther}} {
		req.OperationID = "op_synth-bad-" + string(req.Kind)
		if _, err := x.h.FileIssue(as(childP), req); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	if _, err := x.h.FileIssue(nodeCtx(nodeMac), contracts.FileIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-node"}, Kind: contracts.IssueOther, Title: "x"}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("node principal: %v", err)
	}
}

// The triage session hears about new issues once, about repeats only at
// 3, 10 and 50 reports, and the owner never gets any of it.
func TestIssueTriageMessages(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	childP := sesPrincipal(cid)
	if tr, err := f.setTriage(owner, "op_synth-t1", pid); err != nil || tr.SessionID != pid || tr.SessionTitle == "" {
		t.Fatalf("set triage: %+v %v", tr, err)
	}
	title := "pyre read hangs on run_01JSYNTH00000000000000 after %d minutes"
	r := f.file(childP, "op_synth-r1", contracts.IssuePyreBug, fmt.Sprintf(title, 1), "Synthetic: it hung.")
	mail := x.issueMail()
	if r.TriageSessionID != pid || len(mail) != 1 || mail[0].Recipient.ID != pid {
		t.Fatalf("new issue notice: %+v %+v", r, mail)
	}
	want := fmt.Sprintf("[pyre issue] pyre_bug: %q — %s from \"synthetic child\" (%s) on", r.Issue.Title, r.Issue.IssueID, cid)
	if body := mail[0].Body.Text; !strings.HasPrefix(body, want) || !strings.Contains(body, "(pyre 0.1.0-synth)") || !strings.Contains(body, "pyre issue show "+r.Issue.IssueID) ||
		!strings.Contains(body, "reporting session's words") || strings.Contains(body, "Synthetic: it hung.") {
		t.Fatalf("notice:\n%s\nwant prefix %s", body, want)
	}
	counts := map[int]int{}
	for i := 2; i <= 10; i++ {
		got := f.file(childP, fmt.Sprintf("op_synth-r%d", i), contracts.IssuePyreBug, fmt.Sprintf(title, i), "")
		counts[i] = len(x.issueMail())
		if (i == 3 || i == 10) != (got.TriageSessionID != "") {
			t.Fatalf("report %d: triage %q", i, got.TriageSessionID)
		}
	}
	if counts[2] != 1 || counts[3] != 2 || counts[9] != 2 || counts[10] != 3 {
		t.Fatalf("recurring thresholds: %v", counts)
	}
	if body := x.issueMail()[2].Body.Text; !strings.HasPrefix(body, "[pyre issue] recurring (10×): pyre_bug:") {
		t.Fatalf("recurring notice:\n%s", body)
	}
	if n := len(x.envelopes(`sender_kind='system' AND sender_id=? AND recipient_kind<>'session'`, issueSender.ID)); n != 0 {
		t.Fatalf("%d issue messages went somewhere other than the triage session", n)
	}
	var ownerItems int
	x.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient_kind='owner'`).Scan(&ownerItems)
	if ownerItems != 0 {
		t.Fatalf("owner got %d messages", ownerItems)
	}

	// An archived triage session gets nothing; the issue is still stored.
	if _, err := x.db.Exec(`UPDATE sessions SET archived=1 WHERE session_id=?`, pid); err != nil {
		t.Fatal(err)
	}
	if r := f.file(childP, "op_synth-arch", contracts.IssueOther, "synthetic other problem", ""); r.TriageSessionID != "" || r.Outcome != contracts.IssueCreated {
		t.Fatalf("archived triage: %+v", r)
	}
	if n := len(x.issueMail()); n != 3 {
		t.Fatalf("archived triage got mail: %d", n)
	}
	if tr, _ := x.h.GetIssueTriage(as(childP), struct{}{}); tr.SessionID != pid || !tr.Archived {
		t.Fatalf("triage view: %+v", tr)
	}
}

// Closing: fixed reopens on the next report (with a note and a message),
// wontfix stays closed silently, duplicate forwards later reports.
func TestIssueReopenAfterFixWontfixAndDuplicate(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	childP, triageP := sesPrincipal(cid), sesPrincipal(pid)
	if _, err := f.setTriage(owner, "op_synth-t1", pid); err != nil {
		t.Fatal(err)
	}
	a := f.file(childP, "op_synth-a1", contracts.IssuePyreBug, "inbox shows acked mail", "").Issue
	fixed, err := f.updateIssue(triageP, contracts.UpdateIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-a-fix"}, IssueID: a.IssueID,
		Status: contracts.IssueFixed, Note: "fixed in synthetic PR", Links: []string{"https://example.invalid/synth/pull/1"}})
	if err != nil || fixed.Status != contracts.IssueFixed || fixed.ResolvedAt == nil || fixed.Resolution != "fixed in synthetic PR" || len(fixed.Links) != 1 {
		t.Fatalf("fix: %+v %v", fixed, err)
	}
	before := len(x.issueMail())
	re := f.file(childP, "op_synth-a2", contracts.IssuePyreBug, "Inbox shows acked mail", "still happens")
	if re.Outcome != contracts.IssueReopened || re.Issue.Status != contracts.IssueOpen || re.Issue.ResolvedAt != nil || re.Issue.Occurrences != 2 || re.TriageSessionID != pid {
		t.Fatalf("reopen: %+v", re)
	}
	mail := x.issueMail()
	if len(mail) != before+1 || !strings.HasPrefix(mail[len(mail)-1].Body.Text, "[pyre issue] reported again after fix: pyre_bug: \"inbox shows acked mail\"") {
		t.Fatalf("reopen notice: %+v", mail)
	}
	d, _ := x.h.GetIssue(as(owner), contracts.GetIssueRequest{IssueID: a.IssueID})
	if len(d.Notes) != 2 || d.Notes[1].Text != "reported again after fix" || d.Notes[1].ToStatus != contracts.IssueOpen || d.Notes[0].ToStatus != contracts.IssueFixed {
		t.Fatalf("notes: %+v", d.Notes)
	}

	w := f.file(childP, "op_synth-w1", contracts.IssueGuidance, "guide says to poll the inbox", "").Issue
	if _, err := f.updateIssue(owner, contracts.UpdateIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-w-close"}, IssueID: w.IssueID, Status: contracts.IssueWontfix, Note: "intended"}); err != nil {
		t.Fatal(err)
	}
	before = len(x.issueMail())
	if r := f.file(childP, "op_synth-w2", contracts.IssueGuidance, "Guide says to poll the inbox", ""); r.Issue.Status != contracts.IssueWontfix || r.Issue.Occurrences != 2 || r.TriageSessionID != "" {
		t.Fatalf("wontfix repeat: %+v", r)
	}

	b := f.file(childP, "op_synth-b1", contracts.IssuePyreBug, "acked mail still listed in inbox", "").Issue
	if _, err := f.updateIssue(triageP, contracts.UpdateIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-b-dup"}, IssueID: b.IssueID, Status: contracts.IssueDuplicate, DuplicateOf: a.IssueID}); err != nil {
		t.Fatal(err)
	}
	if r := f.file(childP, "op_synth-b2", contracts.IssuePyreBug, "Acked mail still listed in inbox", ""); r.Issue.IssueID != a.IssueID || r.Issue.Occurrences != 3 {
		t.Fatalf("duplicate forwards: %+v", r)
	}
	// b's own new-issue notice, then a's third report (recurring); the
	// wontfix repeat sent nothing.
	if mail := x.issueMail(); len(mail) != before+2 || !strings.HasPrefix(mail[len(mail)-1].Body.Text, "[pyre issue] recurring (3×): pyre_bug: \"inbox shows acked mail\"") {
		t.Fatalf("mail after wontfix/duplicate: %d, want %d", len(mail), before+2)
	}
	if _, err := f.updateIssue(owner, contracts.UpdateIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-self-dup"}, IssueID: a.IssueID, Status: contracts.IssueDuplicate, DuplicateOf: a.IssueID}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("self duplicate: %v", err)
	}
}

// Any session files and reads; only the owner or the triage session
// updates issues; only the owner or the current triage session sets
// triage.
func TestIssuePermissions(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	parentP, childP := sesPrincipal(pid), sesPrincipal(cid)
	is := f.file(childP, "op_synth-p1", contracts.IssueMachine, "disk full", "").Issue
	note := func(p contracts.Principal, op string) error {
		_, err := f.updateIssue(p, contracts.UpdateIssueRequest{Mutation: contracts.Mutation{OperationID: op}, IssueID: is.IssueID, Note: "synthetic note"})
		return err
	}
	if err := note(childP, "op_synth-n1"); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("a session without the triage role updated an issue: %v", err)
	}
	if _, err := f.setTriage(parentP, "op_synth-s1", pid); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("a session claimed triage: %v", err)
	}
	if _, err := f.setTriage(owner, "op_synth-s2", pid); err != nil {
		t.Fatal(err)
	}
	if err := note(parentP, "op_synth-n2"); err != nil {
		t.Fatalf("triage session note: %v", err)
	}
	if _, err := f.setTriage(childP, "op_synth-s3", cid); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("a non-triage session moved triage: %v", err)
	}
	// The triage session hands the role on; then it no longer updates.
	if tr, err := f.setTriage(parentP, "op_synth-s4", cid); err != nil || tr.SessionID != cid || tr.SetBy == nil || tr.SetBy.ID != pid {
		t.Fatalf("hand on: %+v %v", tr, err)
	}
	if err := note(parentP, "op_synth-n3"); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("former triage session updated: %v", err)
	}
	if err := note(childP, "op_synth-n4"); err != nil {
		t.Fatal(err)
	}
	if tr, err := f.setTriage(childP, "op_synth-s5", ""); err != nil || tr.SessionID != "" {
		t.Fatalf("triage off: %+v %v", tr, err)
	}
	if err := note(owner, "op_synth-n5"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.GetIssue(as(parentP), contracts.GetIssueRequest{IssueID: is.IssueID}); err != nil {
		t.Fatalf("a session reads issues: %v", err)
	}
	if _, err := f.updateIssue(owner, contracts.UpdateIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-empty"}, IssueID: is.IssueID}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("empty update: %v", err)
	}
}

func TestIssuesFeatureAdvertised(t *testing.T) {
	x := newHarness(t)
	hr, err := x.h.Hello(as(owner), contracts.HelloRequest{Protocol: contracts.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range hr.Features {
		if f == contracts.FeatureIssues {
			return
		}
	}
	t.Fatalf("features: %v", hr.Features)
}

// A title that normalizes to (almost) nothing does not merge with
// unrelated reports: it is matched on its whole text.
func TestIssueShortTitlesDoNotMerge(t *testing.T) {
	if fp := IssueFingerprint(contracts.IssuePyreBug, "/usr/bin/pyre 0x1f2e3d4c", ""); fp == "pyre_bug:" || !strings.Contains(fp, "/usr/bin/pyre") {
		t.Fatalf("fingerprint %q", fp)
	}
	f := newChildFixture(t, nil)
	childP := sesPrincipal(f.child.Session.SessionID)
	a := f.file(childP, "op_synth-s1", contracts.IssuePyreBug, "/usr/bin/pyre 0x1f2e3d4c", "")
	b := f.file(childP, "op_synth-s2", contracts.IssuePyreBug, "/opt/pyre/bin/pyred 42", "")
	if a.Issue.IssueID == b.Issue.IssueID || b.Outcome != contracts.IssueCreated {
		t.Fatalf("unrelated short titles merged: %+v %+v", a, b)
	}
	if c := f.file(childP, "op_synth-s3", contracts.IssuePyreBug, "/usr/bin/pyre  0x1f2e3d4c", ""); c.Issue.IssueID != a.Issue.IssueID {
		t.Fatalf("the same short title is the same issue: %+v", c)
	}
}

// Related IDs, links and notes are redacted and bounded per item.
func TestIssueRelatedAndLinksRedactedAndBounded(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	childP := sesPrincipal(f.child.Session.SessionID)
	r, err := x.h.FileIssue(as(childP), contracts.FileIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rl1"}, Kind: contracts.IssueOther,
		Title: "synthetic", Related: []string{"ses_01JSYNTH0000000000000000AB", "GITHUB_TOKEN=synthetic0123456789"},
		Body: "ANTHROPIC_API_KEY=sk-ant-api03-SYNTHETICsyntheticSYNTHETIC00 and https://u:synthpass99@h.example.invalid/x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.Issue.Related, " ") + r.Issue.Body
	for _, leak := range []string{"synthetic0123456789", "SYNTHETICsyntheticSYNTHETIC00", "synthpass99"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("leaked %q: %s", leak, joined)
		}
	}
	if _, err := x.h.FileIssue(as(childP), contracts.FileIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rl2"}, Kind: contracts.IssueOther,
		Title: "synthetic", Related: []string{strings.Repeat("x", contracts.IssueMaxRef+1)}}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("long related: %v", err)
	}
	up, err := f.updateIssue(owner, contracts.UpdateIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rl3"}, IssueID: r.Issue.IssueID,
		Note: "token: synthetic-note-secret", Links: []string{"https://u:synthpass77@h.example.invalid/pr/1"}})
	if err != nil || strings.Contains(strings.Join(up.Links, " "), "synthpass77") {
		t.Fatalf("link: %+v %v", up, err)
	}
	if d, _ := x.h.GetIssue(as(owner), contracts.GetIssueRequest{IssueID: r.Issue.IssueID}); strings.Contains(d.Notes[0].Text, "synthetic-note-secret") {
		t.Fatalf("note: %+v", d.Notes)
	}
	if _, err := f.updateIssue(owner, contracts.UpdateIssueRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rl4"}, IssueID: r.Issue.IssueID,
		Links: []string{"https://example.invalid/" + strings.Repeat("x", contracts.IssueMaxLink)}}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("long link: %v", err)
	}
}

// A session filing many new issues: past the hourly cap they are stored
// without notices and the triage session hears once; past the stored
// occurrence cap a repeat only counts.
func TestIssueFloodAndOccurrenceCaps(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	childP := sesPrincipal(cid)
	if _, err := f.setTriage(owner, "op_synth-t", pid); err != nil {
		t.Fatal(err)
	}
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima", "mike"}
	var last contracts.FileIssueResponse
	for i, w := range words {
		last = f.file(childP, fmt.Sprintf("op_synth-f%d", i), contracts.IssueOther, "synthetic problem "+w, "")
		if (i >= contracts.IssueMaxNewPerHour) != last.Throttled {
			t.Fatalf("issue %d throttled=%v", i, last.Throttled)
		}
	}
	mail := x.issueMail()
	if len(mail) != contracts.IssueMaxNewPerHour+1 || !strings.Contains(mail[len(mail)-1].Body.Text, "filed more than 10 new issues in the last hour") {
		t.Fatalf("flood: %d messages, last %q", len(mail), mail[len(mail)-1].Body.Text)
	}
	l, _ := x.h.ListIssues(as(owner), contracts.ListIssuesRequest{})
	if len(l.Issues) != len(words) {
		t.Fatalf("throttled issues are still stored: %d", len(l.Issues))
	}
	// An hour later the cap is free again.
	x.clock.Add(61 * time.Minute)
	if r := f.file(childP, "op_synth-later", contracts.IssueOther, "synthetic problem november", ""); r.Throttled || r.TriageSessionID != pid {
		t.Fatalf("after an hour: %+v", r)
	}

	// Occurrence evidence stops at IssueMaxStoredOccurrences; the count goes on.
	n := contracts.IssueMaxStoredOccurrences + 5
	var is contracts.Issue
	for i := 1; i <= n; i++ {
		is = f.file(childP, fmt.Sprintf("op_synth-o%d", i), contracts.IssuePyreBug, "synthetic repeat", fmt.Sprintf("evidence %d", i)).Issue
	}
	var stored int
	x.db.QueryRow(`SELECT COUNT(*) FROM issue_occurrences WHERE issue_id=?`, is.IssueID).Scan(&stored)
	if is.Occurrences != n || stored != contracts.IssueMaxStoredOccurrences {
		t.Fatalf("occurrences %d, stored %d", is.Occurrences, stored)
	}
	d, _ := x.h.GetIssue(as(owner), contracts.GetIssueRequest{IssueID: is.IssueID})
	if len(d.Occurrences) != 50 || d.Occurrences[len(d.Occurrences)-1].Seq != contracts.IssueMaxStoredOccurrences {
		t.Fatalf("detail: %d occurrences", len(d.Occurrences))
	}
}

// Triage notices go through an outbox: a pending one is sent by
// maintenance, and one for a triage session that is gone is dropped.
func TestIssueNoticeOutbox(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	now := "2026-10-04T12:00:00Z"
	for _, row := range [][]string{{"op_synth-pending", pid}, {"op_synth-gone", "ses_01JSYNTHGONE000000000000000"}} {
		if _, err := x.db.Exec(`INSERT INTO issue_notices(op,triage_session_id,text,created_at,updated_at) VALUES(?,?,?,?,?)`, row[0], row[1], "[pyre issue] synthetic queued", now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	rows, _ := x.db.Query(`SELECT op, state FROM issue_notices`)
	for rows.Next() {
		var op, st string
		rows.Scan(&op, &st)
		states[op] = st
	}
	rows.Close()
	if states["op_synth-pending"] != "sent" || states["op_synth-gone"] != "dropped" {
		t.Fatalf("states: %v", states)
	}
	if mail := x.issueMail(); len(mail) != 1 || mail[0].Recipient.ID != pid {
		t.Fatalf("mail: %+v", mail)
	}
}
