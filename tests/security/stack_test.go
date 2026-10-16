//go:build !windows

package security

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/tests/resilience/faultnet"
	"github.com/jhh3/pyre/tests/resilience/stack"
)

func skipShort(t *testing.T) {
	if testing.Short() {
		t.Skip("real-stack scenario; skipped with -short")
	}
}

func liveSession(t *testing.T, cl *client.Client, nodeID string, h contracts.Harness, cwd string) contracts.SessionView {
	t.Helper()
	res, err := cl.StartSession(stack.OwnerCtx(t), contracts.StartSessionRequest{NodeID: nodeID, Harness: h, Cwd: cwd, Title: "sec-" + string(h)})
	if err != nil {
		t.Fatal(err)
	}
	var sv contracts.SessionView
	stack.WaitFor(t, 30*time.Second, "run live", func() bool {
		sv, err = cl.GetSession(context.Background(), contracts.GetRequest{ID: res.Session.Session.SessionID})
		return err == nil && sv.Run != nil && (sv.Run.State == contracts.RunWaitingInput || sv.Run.State == contracts.RunRunning)
	})
	return sv
}

// TestStackRevokedNode: revoking a node through the API cuts its live
// connection, its credential stops working on every surface, reconnect
// attempts back off (no storm), and the daemon process keeps running (runs
// are not killed by the hub's decision).
func TestStackRevokedNode(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	px, err := faultnet.New(hub.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	node := stack.EnrollNode(t, hub, dir, "revoked")
	node.HubURL = "https://" + px.Addr()
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	sv := liveSession(t, cl, node.NodeID, contracts.HarnessMock, dir)

	if _, err := cl.RevokeNode(stack.OwnerCtx(t), contracts.RevokeRequest{ID: node.NodeID, Reason: "synthetic revoke"}); err != nil {
		t.Fatal(err)
	}
	stack.WaitFor(t, 15*time.Second, "node revoked and disconnected", func() bool {
		n, err := cl.GetNode(context.Background(), contracts.GetRequest{ID: node.NodeID})
		return err == nil && n.Connection.State == contracts.ConnRevoked
	})
	acceptedAt := px.Stats().Accepted
	time.Sleep(8 * time.Second)
	attempts := px.Stats().Accepted - acceptedAt
	if attempts > 4 {
		t.Fatalf("revoked node made %d connection attempts in 8s; rejected links must back off to the maximum delay", attempts)
	}
	if n, _ := cl.GetNode(context.Background(), contracts.GetRequest{ID: node.NodeID}); n.Connection.State != contracts.ConnRevoked {
		t.Fatalf("revoked node state = %s", n.Connection.State)
	}
	nc := client.New(hub.URL, node.Token, client.Options{CertSHA256: hub.Pin, MaxAttempts: 1})
	if _, err := nc.WhoAmI(context.Background()); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("revoked node token on the API: %v", err)
	}
	if !node.Proc.Running() {
		t.Fatalf("pyred exited after revocation:\n%s", node.Proc.Tail(2000))
	}
	v, _ := cl.GetSession(context.Background(), contracts.GetRequest{ID: sv.Session.SessionID})
	runs, _ := cl.ListRuns(context.Background(), contracts.ListRunsRequest{SessionID: sv.Session.SessionID})
	if len(runs.Items) != 1 {
		t.Fatalf("revocation reassigned/relaunched the run: %d runs", len(runs.Items))
	}
	if strings.Contains(node.Proc.Log(), node.Token) || strings.Contains(hub.Proc.Log(), node.Token) {
		t.Fatal("node token appears in a process log")
	}
	t.Logf("EVIDENCE revoked node: state=revoked, %d reconnect attempts in 8s, token rejected (401), pyred alive, run %s state %v (1 run)",
		attempts, sv.Run.RunID, func() any {
			if v.Run != nil {
				return v.Run.State
			}
			return nil
		}())
}

type allowSpec struct {
	rel string // path relative to the scanned root (prefix match)
	why string
}

// scanTree scans root for secrets. Files matching allow are expected
// credential stores: they are excluded from the leak check but must be
// private (mode & 0o077 == 0).
func scanTree(t *testing.T, s *diagnostics.Scanner, root string, allow []allowSpec) []string {
	t.Helper()
	var leaks []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if d.IsDir() {
			if info.Mode().Perm()&0o077 != 0 && rel != "." {
				leaks = append(leaks, "directory not private: "+rel+" "+info.Mode().Perm().String())
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		for _, a := range allow {
			if strings.HasPrefix(rel, a.rel) {
				if info.Mode().Perm()&0o077 != 0 {
					leaks = append(leaks, "credential store not private: "+rel+" "+info.Mode().Perm().String())
				}
				return nil
			}
		}
		f, err := s.ScanFile(p)
		if err != nil {
			return err
		}
		for _, x := range diagnostics.Secrets(f) {
			leaks = append(leaks, x.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return leaks
}

// TestStackSecretScanAfterRun drives a real hub + daemon through enrollment,
// shell and mock runs (session tokens minted and injected), messages, a
// published artifact and a revocation, then scans the hub data directory,
// the node state directory and both process logs. Credentials may exist
// only in the designated private credential stores.
func TestStackSecretScanAfterRun(t *testing.T) {
	skipShort(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "scan")
	harness := contracts.HarnessMock
	node.Extra = []string{"--dev-mock"} // the real pyred: mock + native shell adapters
	if _, err := exec.LookPath("tmux"); err == nil {
		harness = contracts.HarnessShell
		tm := supervisor.Tmux{Socket: stack.TmuxSocket(node.State), OwnedRoot: node.State}
		t.Cleanup(func() { _ = tm.Kill(context.Background()) })
	}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	ctx := stack.OwnerCtx(t)
	shellRun := liveSession(t, cl, node.NodeID, harness, dir)
	mockRun := liveSession(t, cl, node.NodeID, contracts.HarnessMock, dir)
	for i := 0; i < 3; i++ {
		m, err := cl.SendMessage(ctx, contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: mockRun.Session.SessionID}, Body: contracts.MessageBody{Text: "synthetic scan message"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = m
	}
	if _, err := cl.Publish(ctx, bytes.NewReader([]byte("synthetic artifact for scan")), client.PublishMeta{Title: "scan", Filename: "scan.txt"}); err != nil {
		t.Fatal(err)
	}
	// A pairing (one-time code) and a second enrollment ticket that is never redeemed.
	pt, err := cl.CreatePairing(ctx, contracts.PairingRequest{ClientName: "phone", Kind: "android"})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := cl.CreateEnrollment(ctx, contracts.EnrollmentRequest{Name: "never-redeemed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.StopSession(ctx, contracts.SessionCommandRequest{SessionID: shellRun.Session.SessionID}); err != nil {
		t.Logf("stop shell session: %v", err)
	}
	time.Sleep(3 * time.Second)
	node.Proc.Stop()
	hub.Proc.Stop()

	s := diagnostics.NewScanner().
		AddKnown("owner-token", hub.Owner.Token).AddKnown("node-token", node.Token).
		AddKnown("pairing-code", pt.Code).AddKnown("enroll-secret", tk.Secret)
	s.IgnorePII = true

	hubLeaks := scanTree(t, s, hub.Data, []allowSpec{
		{rel: "owner-credential.json", why: "bootstrap owner credential written by pyre-hub init"},
		{rel: filepath.Join("tls", "hub-key.pem"), why: "hub TLS private key"},
	})
	nodeLeaks := scanTree(t, s, node.State, []allowSpec{
		{rel: "node-credential.json", why: "node credential"},
		{rel: "pyred.db", why: "daemon_secrets holds session tokens for live runs (local socket identity)"},
	})
	logLeaks := diagnostics.Secrets(s.ScanString("hub.log", hub.Proc.Log()))
	logLeaks = append(logLeaks, diagnostics.Secrets(s.ScanString("node.log", node.Proc.Log()))...)
	var all []string
	all = append(all, hubLeaks...)
	all = append(all, nodeLeaks...)
	for _, x := range logLeaks {
		all = append(all, x.String())
	}
	if len(all) > 0 {
		t.Fatalf("secrets outside the designated credential stores:\n%s", strings.Join(all, "\n"))
	}
	if len(hub.Proc.Log()) == 0 || len(node.Proc.Log()) == 0 {
		t.Fatal("empty process logs; log scan would be vacuous")
	}

	// Inside pyred.db: session tokens belong only in daemon_secrets.
	db, err := sqlitex.Open(filepath.Join(node.State, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	journal := countTokensInTable(t, db, "nodelink_commands", "command")
	t.Logf("EVIDENCE scanned hub data dir, node state dir and logs: no credentials outside owner-credential.json, tls/hub-key.pem, node-credential.json, pyred.db (all 0600); session tokens in nodelink_commands journal: %d", journal)
	if journal > 0 {
		t.Fatalf("%d session token(s) stored in the node command journal (nodelink_commands)", journal)
	}
}

func countTokensInTable(t *testing.T, db *sql.DB, table, col string) int {
	t.Helper()
	rows, err := db.Query(`SELECT ` + col + ` FROM ` + table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	s := diagnostics.NewScanner()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		for _, f := range s.ScanBytes(table, b) {
			if f.Rule == diagnostics.RulePyreToken {
				n++
			}
		}
	}
	return n
}
