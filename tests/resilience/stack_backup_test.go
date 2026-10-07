package resilience

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
	"github.com/jhh3/pyre/tests/resilience/stack"
)

// TestStackBackupRestoreRoundTrip (A19): back up a live hub, lose it,
// restore into a fresh data directory and serve on the same address. The
// node reconnects with its existing credential and pin (no re-enrollment,
// no duplicate node); projects, document revisions, exact artifact bytes,
// sessions/runs and delivered messages are intact; the restored hub keeps
// working (new delivery succeeds).
func TestStackBackupRestoreRoundTrip(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "backup")
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	ctx := stack.OwnerCtx(t)

	proj, err := cl.CreateProject(ctx, contracts.CreateProjectRequest{Name: "backup-project", Brief: "synthetic brief v1"})
	if err != nil {
		t.Fatal(err)
	}
	brief, err := cl.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: proj.BriefDocumentID})
	if err != nil {
		t.Fatal(err)
	}
	rev2, err := cl.ProposeRevision(ctx, contracts.ProposeRevisionRequest{DocumentID: brief.Document.DocumentID, BaseRevisionID: brief.Revision.RevisionID, Content: "synthetic brief v2"})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("synthetic artifact bytes for backup\n"), 3000)
	pub, err := cl.Publish(ctx, bytes.NewReader(payload), client.PublishMeta{ProjectID: &proj.ProjectID, Title: "backup-artifact", Filename: "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	sv := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "backup-session")
	m1, err := cl.SendMessage(ctx, contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sv.Session.SessionID}, Body: contracts.MessageBody{Text: "before backup"}})
	if err != nil {
		t.Fatal(err)
	}
	stack.WaitFor(t, 30*time.Second, "m1 accepted", func() bool {
		v, err := msgState(cl, m1.Envelope.MessageID)
		return err == nil && v.Status.Runtime == contracts.DeliveryAccepted
	})

	archive := filepath.Join(dir, "hub-backup.tar")
	pass := passphraseFile(t, dir)
	if out, err := stack.RunCLI(bins.Hub, "backup", "--data", hub.Data, "--out", archive, "--passphrase-file", pass); err != nil {
		t.Fatalf("backup of a live hub: %v\n%s", err, out)
	}
	if st, err := os.Stat(archive); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode: %v %v", st, err)
	}

	// Lose the hub.
	hub.Proc.Kill()
	if err := os.RemoveAll(hub.Data); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "hub-restored")
	if out, err := stack.RunCLI(bins.Hub, "restore", "--in", archive, "--data", restored); err == nil {
		t.Fatalf("restore of an encrypted backup without a passphrase succeeded:\n%s", out)
	}
	if out, err := stack.RunCLI(bins.Hub, "restore", "--in", archive, "--data", restored, "--passphrase-file", pass); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	hub.UseData(restored)
	hub.Start(t) // same address, restored TLS identity: the old pin must still verify
	cl = hub.Client()
	ctx = stack.OwnerCtx(t)
	if _, err := cl.WhoAmI(ctx); err != nil {
		t.Fatalf("owner credential/pin after restore: %v", err)
	}
	node.WaitConnected(t, 90*time.Second)

	gp, err := cl.GetProject(ctx, contracts.GetRequest{ID: proj.ProjectID})
	if err != nil || gp.Name != "backup-project" {
		t.Fatalf("project after restore: %+v %v", gp, err)
	}
	doc, err := cl.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: brief.Document.DocumentID})
	if err != nil || doc.Revision.RevisionID != rev2.Revision.RevisionID || doc.Revision.Content != "synthetic brief v2" {
		t.Fatalf("document head after restore: %+v %v", doc.Revision, err)
	}
	rc, _, err := cl.OpenContent(ctx, pub.Version.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("artifact bytes differ after restore")
	}
	v, err := msgState(cl, m1.Envelope.MessageID)
	if err != nil || v.Status.Runtime != contracts.DeliveryAccepted {
		t.Fatalf("message after restore: %+v %v", v.Status, err)
	}
	gs, err := cl.GetSession(ctx, contracts.GetRequest{ID: sv.Session.SessionID})
	if err != nil || gs.Run == nil || gs.Run.RunID != sv.Run.RunID {
		t.Fatalf("session after restore: %+v %v", gs.Run, err)
	}
	nodes, _ := cl.ListNodes(ctx, contracts.ListNodesRequest{})
	if len(nodes.Items) != 1 {
		t.Fatalf("%d nodes after restore", len(nodes.Items))
	}
	m2, err := cl.SendMessage(ctx, contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sv.Session.SessionID}, Body: contracts.MessageBody{Text: "after restore"}})
	if err != nil {
		t.Fatal(err)
	}
	stack.WaitFor(t, 60*time.Second, "m2 accepted on restored hub", func() bool {
		v, err := msgState(cl, m2.Envelope.MessageID)
		return err == nil && v.Status.Runtime == contracts.DeliveryAccepted
	})
	st, _ := os.Stat(archive)
	evidence(t, "backup of live hub (%d bytes) restored to fresh dir on same address: pin/owner token valid, node reconnected without re-enroll, project/doc head/artifact bytes/message/run intact, new delivery accepted", st.Size())
}

// TestStackRestoreReconcilesReportGap: after the hub is restored from a
// backup older than the node's acknowledged reports, a run that ended after
// the backup must not stay unknown, and a session started after the backup
// must come back with its title. The node notices the regressed committed
// watermark on Welcome and sends a run snapshot (contract v1.5).
func TestStackRestoreReconcilesReportGap(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "gap")
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	ctx := stack.OwnerCtx(t)
	ended := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "gap-ends-after-backup")

	archive := filepath.Join(dir, "hub-backup.tar")
	pass := passphraseFile(t, dir)
	if out, err := stack.RunCLI(bins.Hub, "backup", "--data", hub.Data, "--out", archive, "--passphrase-file", pass); err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	// After the backup: one session starts, the other's run ends.
	fresh := startSession(t, cl, node.NodeID, contracts.HarnessMock, dir, "gap-started-after-backup")
	if _, err := cl.StopSession(ctx, contracts.SessionCommandRequest{SessionID: ended.Session.SessionID, Reason: "synthetic stop after backup"}); err != nil {
		t.Fatal(err)
	}
	stack.WaitFor(t, 30*time.Second, "stopped run ended", func() bool {
		v, err := cl.GetSession(context.Background(), contracts.GetRequest{ID: ended.Session.SessionID})
		return err == nil && v.Run != nil && v.Run.State.Terminal()
	})

	hub.Proc.Kill()
	if err := os.RemoveAll(hub.Data); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "hub-restored")
	if out, err := stack.RunCLI(bins.Hub, "restore", "--in", archive, "--data", restored, "--passphrase-file", pass); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	hub.UseData(restored)
	hub.Start(t)
	cl = hub.Client()
	node.WaitConnected(t, 90*time.Second)

	var ev, fv contracts.SessionView
	stack.WaitFor(t, 30*time.Second, "restored hub reconciles the ended run and the new session's title", func() bool {
		var err error
		if ev, err = cl.GetSession(context.Background(), contracts.GetRequest{ID: ended.Session.SessionID}); err != nil {
			return false
		}
		if fv, err = cl.GetSession(context.Background(), contracts.GetRequest{ID: fresh.Session.SessionID}); err != nil {
			return false
		}
		return ev.Run != nil && ev.Run.State.Terminal() && fv.Session.Title == "gap-started-after-backup" && fv.Run != nil && !fv.Run.State.Terminal()
	})
	if ev.Run.State == contracts.RunUnknown || fv.Run.RunID != fresh.Run.RunID {
		t.Fatalf("after reconcile: ended=%s fresh run=%s", ev.Run.State, fv.Run.RunID)
	}
	evidence(t, "hub restored from a pre-gap backup: run ended after the backup reconciled to %s (not unknown); session started after the backup recovered title %q", ev.Run.State, fv.Session.Title)
}

// TestStackBackupArchiveSecrets inspects what a hub backup carries. Node
// and session credentials must be hashes only. The TLS private key is
// included by design; the architecture asks for encrypted backups.
func TestStackBackupArchiveSecrets(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "bk-sec")
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	startSession(t, hub.Client(), node.NodeID, contracts.HarnessMock, dir, "bk-sec")
	archive := filepath.Join(dir, "b.tar")
	if out, err := stack.RunCLI(bins.Hub, "backup", "--data", hub.Data, "--out", archive); err == nil {
		t.Fatalf("backup without a passphrase or --unencrypted succeeded:\n%s", out)
	}
	if out, err := stack.RunCLI(bins.Hub, "backup", "--data", hub.Data, "--out", archive, "--passphrase-file", passphraseFile(t, dir)); err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	s := diagnostics.NewScanner().AddKnown("owner-token", hub.Owner.Token).AddKnown("node-token", node.Token)
	s.IgnorePII = true
	f, err := s.ScanFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	var credLeaks, keys []string
	for _, x := range diagnostics.Secrets(f) {
		if x.Rule == diagnostics.RulePrivateKey {
			keys = append(keys, x.String())
		} else {
			credLeaks = append(credLeaks, x.String())
		}
	}
	if len(credLeaks) > 0 {
		t.Fatalf("backup archive carries plaintext credentials:\n%s", strings.Join(credLeaks, "\n"))
	}
	if len(keys) > 0 {
		t.Fatalf("backup archive exposes the hub TLS private key (%d PEM key block(s))", len(keys))
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("MANIFEST.json")) || bytes.Contains(raw, []byte("hub.db")) {
		t.Fatal("backup archive is not encrypted (plaintext tar names visible)")
	}
}

// passphraseFile writes a synthetic backup passphrase (0600) for the CLI.
func passphraseFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "backup-passphrase")
	if err := os.WriteFile(p, []byte("synthetic backup passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
