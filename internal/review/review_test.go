package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/artifacts"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
)

type env struct {
	proj *projects.Store
	arts *artifacts.Store
	rev  *Store
	sink *svctest.Sink
}

// newEnv composes the three WP11 services exactly as the hub will.
func newEnv(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	db := svctest.OpenDB(t)
	sink := svctest.NewSink(t, db)
	proj, err := projects.New(ctx, db, sink, projects.Options{})
	if err != nil {
		t.Fatal(err)
	}
	arts, err := artifacts.New(ctx, db, sink, artifacts.Options{DataDir: filepath.Join(t.TempDir(), "artifacts"), Projects: proj})
	if err != nil {
		t.Fatal(err)
	}
	proj.SetArtifactIndex(arts)
	rev, err := New(ctx, db, sink, proj, arts, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return env{proj: proj, arts: arts, rev: rev, sink: sink}
}

func publish(t *testing.T, e env, ctx context.Context, req contracts.BeginUploadRequest, data []byte) contracts.PublishReceipt {
	t.Helper()
	sum := sha256.Sum256(data)
	req.Mutation, req.Size, req.Digest = svctest.Op(), int64(len(data)), contracts.Digest("sha256:"+hex.EncodeToString(sum[:]))
	u, err := e.arts.BeginUpload(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.arts.UploadChunk(ctx, u.UploadID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	r, err := e.arts.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustCode(t *testing.T, err error, code contracts.ErrorCode) {
	t.Helper()
	if !contracts.IsCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func ptr(s string) *string { return &s }

func TestArtifactReviewSurvivesNewVersion(t *testing.T) {
	e := newEnv(t)
	owner := svctest.Owner()
	producer := svctest.Session("ses_synth-b")
	p, _ := e.proj.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "Synthetic Outfit"})
	v1 := publish(t, e, producer, contracts.BeginUploadRequest{Title: "Synthetic test report", ProjectID: &p.ProjectID, Filename: "report.md"}, []byte("# Report\n\nAll synthetic tests pass.\n"))
	v1ref := contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: v1.Version.VersionID}

	c1, err := e.rev.CreateComment(owner, contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: v1ref,
		Anchor: &contracts.Anchor{Kind: "line", Value: "3"}, Body: "Synthetic: include the failure count.", Mentions: []string{"ses_synth-b", "ses_synth-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if c1.ProjectID == nil || *c1.ProjectID != p.ProjectID || len(c1.Mentions) != 1 || c1.Anchor.Value != "3" || c1.Resolved {
		t.Fatalf("comment: %+v", c1)
	}
	v2 := publish(t, e, producer, contracts.BeginUploadRequest{ArtifactID: v1.Artifact.ArtifactID, Filename: "report.md"}, []byte("# Report\n\nAll synthetic tests pass. Failures: 0.\n"))
	if v2.Version.PreviousVersionID != v1.Version.VersionID {
		t.Fatal("version link missing")
	}
	reply, err := e.rev.CreateComment(producer, contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: v1ref, InReplyTo: c1.CommentID, Body: "Fixed in v2."})
	if err != nil || reply.InReplyTo != c1.CommentID {
		t.Fatalf("reply: %v", err)
	}
	_, err = e.rev.CreateComment(producer, contracts.CreateCommentRequest{Mutation: svctest.Op(),
		Target: contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: v2.Version.VersionID}, InReplyTo: c1.CommentID, Body: "wrong target"})
	mustCode(t, err, contracts.ErrInvalid)
	c2, err := e.rev.CreateComment(owner, contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: v2.Version.VersionID}, Body: "Looks good."})
	if err != nil {
		t.Fatal(err)
	}

	onV1, _ := e.rev.ListComments(owner, contracts.ListCommentsRequest{Target: &v1ref})
	if len(onV1.Items) != 2 || onV1.Items[0].CommentID != c1.CommentID || onV1.Items[0].Target != v1ref {
		t.Fatalf("v1 comments moved: %+v", onV1.Items)
	}
	all, _ := e.rev.ListComments(owner, contracts.ListCommentsRequest{ArtifactID: v1.Artifact.ArtifactID})
	byProject, _ := e.rev.ListComments(owner, contracts.ListCommentsRequest{ProjectID: &p.ProjectID})
	if len(all.Items) != 3 || len(byProject.Items) != 3 {
		t.Fatalf("by artifact %d, by project %d", len(all.Items), len(byProject.Items))
	}

	res := contracts.ResolveCommentRequest{Mutation: svctest.Op(), CommentID: c1.CommentID, Resolved: true}
	r1, err := e.rev.ResolveComment(producer, res)
	if err != nil || !r1.Resolved || r1.ResolvedBy == nil || r1.ResolvedBy.ID != "ses_synth-b" {
		t.Fatalf("resolve: %v %+v", err, r1)
	}
	if again, err := e.rev.ResolveComment(producer, res); err != nil || !again.Resolved {
		t.Fatalf("replay: %v", err)
	}
	unresolved, _ := e.rev.ListComments(owner, contracts.ListCommentsRequest{ArtifactID: v1.Artifact.ArtifactID, Unresolved: true})
	if len(unresolved.Items) != 2 {
		t.Fatalf("unresolved %d", len(unresolved.Items))
	}
	un, err := e.rev.ResolveComment(owner, contracts.ResolveCommentRequest{Mutation: svctest.Op(), CommentID: c1.CommentID, Resolved: false})
	if err != nil || un.Resolved || un.ResolvedBy != nil {
		t.Fatalf("unresolve: %v %+v", err, un)
	}
	noop, err := e.rev.ResolveComment(owner, contracts.ResolveCommentRequest{Mutation: svctest.Op(), CommentID: c2.CommentID, Resolved: false})
	if err != nil || noop.Resolved {
		t.Fatalf("noop: %v", err)
	}

	created := e.sink.Events(t, contracts.EvCommentCreated)
	if len(created) != 3 {
		t.Fatalf("comment.created events %d", len(created))
	}
	var payload contracts.ReviewComment // pinned v1 payload shape
	json.Unmarshal(created[0].Payload, &payload)
	if payload.CommentID != c1.CommentID || len(payload.Mentions) != 1 || payload.Mentions[0] != "ses_synth-b" || payload.Target != v1ref || created[0].ProjectID == nil {
		t.Fatalf("payload: %s", created[0].Payload)
	}
	resolved := e.sink.Events(t, contracts.EvCommentResolved)
	if len(resolved) != 2 {
		t.Fatalf("comment.resolved events %d (no-op must not emit)", len(resolved))
	}
	var rp contracts.ReviewComment
	json.Unmarshal(resolved[0].Payload, &rp)
	if rp.CommentID != c1.CommentID || !rp.Resolved || rp.ResolvedBy == nil {
		t.Fatalf("resolved payload: %s", resolved[0].Payload)
	}
}

func TestDocumentRevisionReview(t *testing.T) {
	e := newEnv(t)
	owner := svctest.Owner()
	p, _ := e.proj.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P", Brief: "# Brief\n"})
	head, _ := e.proj.GetDocument(owner, contracts.GetDocumentRequest{DocumentID: p.BriefDocumentID})
	r1 := contracts.ObjectRef{Kind: contracts.KindDocumentRevision, ID: head.Revision.RevisionID}
	c, err := e.rev.CreateComment(owner, contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: r1, Anchor: &contracts.Anchor{Kind: "quote", Value: "Brief"}, Body: "Name the goal."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.proj.ProposeRevision(owner, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: p.BriefDocumentID, BaseRevisionID: r1.ID, Content: "# Brief\n\nGoal: X.\n"}); err != nil {
		t.Fatal(err)
	}
	byDoc, _ := e.rev.ListComments(owner, contracts.ListCommentsRequest{DocumentID: p.BriefDocumentID})
	if len(byDoc.Items) != 1 || byDoc.Items[0].Target != r1 || byDoc.Items[0].CommentID != c.CommentID {
		t.Fatalf("doc comments: %+v", byDoc.Items)
	}
}

func TestStandaloneAndValidation(t *testing.T) {
	e := newEnv(t)
	owner := svctest.Owner()
	v := publish(t, e, owner, contracts.BeginUploadRequest{Title: "scratch", Filename: "a.txt"}, []byte("a"))
	ref := contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: v.Version.VersionID}
	om, err := e.rev.CreateComment(svctest.Session("ses_synth-b"), contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: ref, Body: "please look", Mentions: []string{MentionOwner}})
	if err != nil || len(om.Mentions) != 1 || om.Mentions[0] != "owner" {
		t.Fatalf("owner mention: %v %+v", err, om)
	}
	req := contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: ref, Body: "standalone review"}
	c, err := e.rev.CreateComment(owner, req)
	if err != nil || c.ProjectID != nil {
		t.Fatalf("standalone: %v %+v", err, c)
	}
	again, err := e.rev.CreateComment(owner, req)
	if err != nil || again.CommentID != c.CommentID {
		t.Fatalf("replay: %v", err)
	}
	req.Body = "changed"
	_, err = e.rev.CreateComment(owner, req)
	mustCode(t, err, contracts.ErrConflict)
	page, _ := e.rev.ListComments(owner, contracts.ListCommentsRequest{ProjectID: ptr("")})
	if len(page.Items) != 2 {
		t.Fatalf("standalone filter %d", len(page.Items))
	}
	got, err := e.rev.GetComment(owner, contracts.GetRequest{ID: c.CommentID})
	if err != nil || got.Body != "standalone review" {
		t.Fatalf("get: %v", err)
	}
	_, err = e.rev.GetComment(owner, contracts.GetRequest{ID: "cmt_missing"})
	mustCode(t, err, contracts.ErrNotFound)

	bad := []contracts.CreateCommentRequest{
		{Target: contracts.ObjectRef{Kind: contracts.KindArtifact, ID: v.Artifact.ArtifactID}, Body: "x"},
		{Target: ref, Body: "   "},
		{Target: ref, Body: "x", Mentions: []string{"not-a-session"}},
		{Target: ref, Body: "x", Anchor: &contracts.Anchor{Kind: "pixel", Value: "1"}},
	}
	for i, b := range bad {
		b.Mutation = svctest.Op()
		if _, err := e.rev.CreateComment(owner, b); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("case %d: want invalid, got %v", i, err)
		}
	}
	_, err = e.rev.CreateComment(owner, contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: "ver_missing"}, Body: "x"})
	mustCode(t, err, contracts.ErrNotFound)
	_, err = e.rev.CreateComment(owner, contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: ref, Body: "x", InReplyTo: "cmt_missing"})
	mustCode(t, err, contracts.ErrNotFound)
	_, err = e.rev.CreateComment(context.Background(), contracts.CreateCommentRequest{Mutation: svctest.Op(), Target: ref, Body: "x"})
	mustCode(t, err, contracts.ErrUnauthorized)
	_, err = e.rev.ResolveComment(owner, contracts.ResolveCommentRequest{Mutation: svctest.Op(), CommentID: "cmt_missing", Resolved: true})
	mustCode(t, err, contracts.ErrNotFound)
}

func TestLaunchContextIncludesArtifactPointers(t *testing.T) {
	e := newEnv(t)
	owner := svctest.Owner()
	p, _ := e.proj.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P", Brief: "Goal: X."})
	v := publish(t, e, owner, contracts.BeginUploadRequest{Title: "Report", ProjectID: &p.ProjectID, Filename: "r.md"}, []byte("r"))
	lc, err := e.proj.GetLaunchContext(owner, contracts.ContextRequest{ProjectID: &p.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range lc.References {
		if r.Kind == contracts.KindArtifactVersion && r.ID == v.Version.VersionID {
			found = true
		}
	}
	if !found || lc.BriefRevision == "" {
		t.Fatalf("launch context: %+v", lc)
	}
}
