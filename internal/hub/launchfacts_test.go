package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// factDocs serves one synthetic fact sheet.
type factDocs struct {
	contracts.DocumentAPI
	project, content string
	by               *contracts.Principal // head revision author (default: the owner)
}

func (f factDocs) author() contracts.Principal {
	if f.by != nil {
		return *f.by
	}
	return contracts.Principal{Kind: contracts.PrincipalOwner, ID: "owner"}
}

func (f factDocs) GetDocument(ctx context.Context, req contracts.GetDocumentRequest) (contracts.DocumentWithRevision, error) {
	if req.DocumentID != "doc_synthfacts" && (req.ProjectID != f.project || req.Slug != contracts.FactSheetSlug) {
		return contracts.DocumentWithRevision{}, contracts.Errorf(contracts.ErrNotFound, "no such document")
	}
	return contracts.DocumentWithRevision{Document: contracts.Document{DocumentID: "doc_synthfacts", Slug: contracts.FactSheetSlug},
		Revision: contracts.DocumentRevision{RevisionID: "rev_synthfacts", Seq: 2, Content: f.content, Author: f.author()}}, nil
}

func TestChildLaunchContextCarriesFactSheet(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	const pid = "prj_synthetic000000000000001"
	x.h.cfg.Documents = factDocs{project: pid, content: "# Fact sheet\n\nIntro.\n\n- GPU provider is modal and fal — owner: \"use fal like i'm telling you\" (Oct 13 2026)\n"}
	parent := x.start(owner, "op_synth-facts-parent", nodeMac)
	if _, err := x.db.Exec(`UPDATE sessions SET project_id=? WHERE session_id=?`, pid, parent.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	launch := func(op string, project *string) contracts.StartRunCommand {
		t.Helper()
		if _, err := x.h.StartSession(as(sesPrincipal(parent.Session.SessionID)), contracts.StartSessionRequest{
			Mutation: contracts.Mutation{OperationID: op}, NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/child", ProjectID: project}); err != nil {
			t.Fatal(err)
		}
		sent := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
		var sr contracts.StartRunCommand
		json.Unmarshal(sent[len(sent)-1].Payload, &sr)
		return sr
	}
	sr := launch("op_synth-facts-child", nil) // joins the parent's project
	if sr.LaunchContext == nil {
		t.Fatal("no launch context for a child in a project with a fact sheet")
	}
	text := sr.LaunchContext.Text
	for _, want := range []string{"## Project facts", "use fal like i'm telling you", "Intro.", "propose_fact", "rev_synthfacts"} {
		if !strings.Contains(text, want) {
			t.Fatalf("launch context lacks %q:\n%s", want, text)
		}
	}
	// A grant recorded for the parent's tree reaches the next child.
	if _, err := x.h.CreateGrant(as(owner), contracts.CreateGrantRequest{Mutation: contracts.Mutation{OperationID: "op_synth-grant"}, SessionID: parent.Session.SessionID,
		Scope: contracts.GrantScopeSession, Action: "merge", Option: "merge", Condition: "reviewed and CI green", Quote: "merge when reviewed and green"}); err != nil {
		t.Fatal(err)
	}
	granted := launch("op_synth-facts-child-2", nil).LaunchContext.Text
	for _, want := range []string{"## Standing go-aheads from the owner", "- merge → \"merge\" — note: reviewed and CI green — set by the owner: \"merge when reviewed and green\"", "## Project facts"} {
		if !strings.Contains(granted, want) {
			t.Fatalf("launch context lacks %q:\n%s", want, granted)
		}
	}
	if strings.Contains(text, "# Fact sheet") {
		t.Fatalf("title line repeated:\n%s", text)
	}
	none := ""
	if sr := launch("op_synth-facts-none", &none); sr.LaunchContext == nil || strings.Contains(sr.LaunchContext.Text, "Project facts") || !strings.Contains(sr.LaunchContext.Text, "merge when reviewed") {
		t.Fatalf("a child outside the project gets its tree's grants, not the facts: %+v", sr.LaunchContext)
	}
}

// Backstop: a fact sheet whose head revision a session wrote is not put
// into launch context (the store refuses such writes; older data or a
// future path must not leak in as the owner's word).
func TestStandingContextIgnoresFactSheetNotWrittenByOwner(t *testing.T) {
	x := newHarness(t)
	const pid = "prj_synthetic000000000000001"
	agent := contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synthagent"}
	pp := pid
	x.h.cfg.Documents = factDocs{project: pid, content: "- made-up owner rule\n", by: &agent}
	if text, _ := x.h.standingContext(context.Background(), &pp, ""); strings.Contains(text, "made-up") {
		t.Fatalf("a session-written fact sheet reached launch context:\n%s", text)
	}
	x.h.cfg.Documents = factDocs{project: pid, content: "- approved rule\n", by: &contracts.FactSheetPrincipal}
	if text, _ := x.h.standingContext(context.Background(), &pp, ""); !strings.Contains(text, "approved rule") {
		t.Fatalf("the hub's approved-fact revision is left out:\n%s", text)
	}
}

// Fact sheet and grants together stay within StandingContextMaxBytes.
func TestStandingContextCapped(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	const pid = "prj_synthetic000000000000001"
	x.h.cfg.Documents = factDocs{project: pid, content: strings.Repeat("- a long synthetic fact line that goes on — owner: \"yes\"\n", 400)}
	pp := pid
	for i := range 30 {
		if _, err := x.h.CreateGrant(as(owner), contracts.CreateGrantRequest{Mutation: contracts.Mutation{OperationID: "op_synth-cap-" + string(rune('a'+i))},
			Scope: contracts.GrantScopeProject, ProjectID: &pp, Action: "merge-" + string(rune('a'+i)), Option: "merge", Quote: strings.Repeat("go ahead ", 60)}); err != nil {
			t.Fatal(err)
		}
	}
	text, _ := x.h.standingContext(context.Background(), &pp, "")
	if len(text) > contracts.StandingContextMaxBytes+300 || !strings.Contains(text, "truncated") || !strings.Contains(text, "more: `pyre grant ls`") {
		t.Fatalf("standing context %d bytes:\n%s", len(text), text[max(len(text)-600, 0):])
	}
}
