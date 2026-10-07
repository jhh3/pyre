package review

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
)

// TestFixture06Conformance replays the frozen v1 fixture
// "document-conflict-artifact-review" against the real services and checks
// that the error shapes, record relationships and URLs match it.
func TestFixture06Conformance(t *testing.T) {
	fx, err := contracts.LoadFixture(filepath.Join("..", "..", "tests", "fixtures", "contracts", "06-document-conflict-artifact-review.json"))
	if err != nil {
		t.Fatal(err)
	}
	ex := map[string]contracts.FixtureExchange{}
	for _, x := range fx.Exchanges {
		ex[x.Name] = x
	}
	e := newEnv(t)
	owner := svctest.Owner()
	sesA, sesB := svctest.Session("ses_synth-a"), svctest.Session("ses_synth-b")

	// Documents: the brief at revision 1, A commits revision 2, B conflicts.
	rev1 := fx.Revisions[0]
	p, err := e.proj.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: fx.Projects[0].Name, Brief: rev1.Content})
	if err != nil {
		t.Fatal(err)
	}
	brief, _ := e.proj.GetDocument(owner, contracts.GetDocumentRequest{ProjectID: p.ProjectID, Slug: fx.Documents[0].Slug})
	if brief.Document.Title != fx.Documents[0].Title || brief.Document.MediaType != fx.Documents[0].MediaType ||
		brief.Revision.Size != rev1.Size || brief.Revision.Digest != rev1.Digest {
		t.Fatalf("brief shape differs from fixture: %+v", brief.Document)
	}
	won, err := e.proj.ProposeRevision(sesA, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: brief.Document.DocumentID,
		BaseRevisionID: brief.Revision.RevisionID, Content: fx.Revisions[1].Content, Summary: fx.Revisions[1].Summary})
	if err != nil || won.Revision.Seq != fx.Revisions[1].Seq || won.Revision.ParentRevisionID != brief.Revision.RevisionID ||
		won.Revision.Size != fx.Revisions[1].Size || won.Revision.Digest != fx.Revisions[1].Digest {
		t.Fatalf("revision 2: %v %+v", err, won.Revision)
	}
	var stale contracts.ProposeRevisionRequest
	if err := json.Unmarshal(ex["stale propose"].Request, &stale); err != nil {
		t.Fatal(err)
	}
	stale.OperationID = svctest.Op().OperationID
	stale.DocumentID, stale.BaseRevisionID = brief.Document.DocumentID, brief.Revision.RevisionID
	_, err = e.proj.ProposeRevision(sesB, stale)
	assertErrorShape(t, err, ex["stale propose"].Error)
	got := contracts.AsError(err)
	if got.Details["current_head"] != won.Revision.RevisionID {
		t.Fatalf("current_head %v", got.Details["current_head"])
	}
	props, _ := e.proj.ListProposals(owner, contracts.ListProposalsRequest{DocumentID: brief.Document.DocumentID})
	want := fx.Proposals[0]
	if len(props.Items) != 1 || props.Items[0].Content != want.Content || props.Items[0].State != want.State ||
		props.Items[0].HeadAtConflict != won.Revision.RevisionID || props.Items[0].Author.ID != want.Author.ID {
		t.Fatalf("proposal differs from fixture: %+v", props.Items)
	}

	// Artifacts: two immutable versions, the comment stays on version 1.
	fv1, fv2 := fx.Versions[0], fx.Versions[1]
	v1 := publish(t, e, sesB, contracts.BeginUploadRequest{Title: fx.Artifacts[0].Title, ProjectID: &p.ProjectID, Filename: fv1.Filename,
		Provenance: fv1.Provenance}, []byte(fx.Contents[fv1.VersionID]))
	if v1.Version.Size != fv1.Size || v1.Version.Digest != fv1.Digest || v1.Version.MediaType != fv1.MediaType || v1.Version.ContentURL != "/v1/content/"+v1.Version.VersionID || v1.Version.PreviewURL != "" {
		t.Fatalf("v1 differs from fixture: %+v", v1.Version)
	}
	v2 := publish(t, e, sesB, contracts.BeginUploadRequest{ArtifactID: v1.Artifact.ArtifactID, Filename: fv2.Filename, Provenance: fv2.Provenance}, []byte(fx.Contents[fv2.VersionID]))
	if v2.Version.Seq != 2 || v2.Version.PreviousVersionID != v1.Version.VersionID || v2.Version.MediaType != fv2.MediaType ||
		v2.Version.Size != fv2.Size || v2.Version.Digest != fv2.Digest ||
		v2.Version.PreviewURL != strings.Replace(fv2.PreviewURL, fv2.VersionID, v2.Version.VersionID, 1) {
		t.Fatalf("v2 differs from fixture: %+v", v2.Version)
	}
	if v2.Artifact.VersionCount != fx.Artifacts[0].VersionCount || v2.Artifact.SessionID != fx.Artifacts[0].SessionID {
		t.Fatalf("artifact differs from fixture: %+v", v2.Artifact)
	}
	fc := fx.Comments[0]
	c, err := e.rev.CreateComment(owner, contracts.CreateCommentRequest{Mutation: svctest.Op(),
		Target: contracts.ObjectRef{Kind: fc.Target.Kind, ID: v1.Version.VersionID}, Anchor: fc.Anchor, Body: fc.Body, Mentions: fc.Mentions})
	if err != nil || c.Target.ID != v1.Version.VersionID || *c.ProjectID != p.ProjectID || c.Anchor.Kind != fc.Anchor.Kind {
		t.Fatalf("comment: %v %+v", err, c)
	}

	// Finalize with a wrong digest.
	data := []byte("synthetic bytes")
	u, err := e.arts.BeginUpload(sesB, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "bad", Filename: "bad.txt", Size: int64(len(data)),
		Digest: "sha256:" + contracts.Digest(strings.Repeat("0", 64))})
	if err != nil {
		t.Fatal(err)
	}
	e.arts.UploadChunk(sesB, u.UploadID, 0, bytes.NewReader(data))
	_, err = e.arts.FinalizeUpload(sesB, contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID})
	assertErrorShape(t, err, ex["finalize with wrong digest"].Error)
}

// assertErrorShape compares code, message, retryability, ref kind and
// detail keys (IDs differ between the fixture and a live run).
func assertErrorShape(t *testing.T, err error, want *contracts.Error) {
	t.Helper()
	got := contracts.AsError(err)
	if got == nil || want == nil {
		t.Fatalf("got %v want %v", got, want)
	}
	if got.Code != want.Code || got.Message != want.Message || got.Retryable != want.Retryable {
		t.Fatalf("error %+v, fixture %+v", got, want)
	}
	if (got.Ref == nil) != (want.Ref == nil) || (got.Ref != nil && got.Ref.Kind != want.Ref.Kind) {
		t.Fatalf("ref %+v, fixture %+v", got.Ref, want.Ref)
	}
	keys := func(m map[string]any) []string {
		var k []string
		for key := range m {
			k = append(k, key)
		}
		sort.Strings(k)
		return k
	}
	for _, k := range keys(want.Details) {
		if _, ok := got.Details[k]; !ok {
			t.Fatalf("missing detail %q in %v", k, got.Details)
		}
	}
}
