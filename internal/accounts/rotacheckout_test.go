package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// singleOwner asserts the core invariant: the profile holds a credential
// if and only if the rota snapshot is locked.
func (r *rotaEnv) singleOwner(dir, lock, kc string) bool {
	r.t.Helper()
	_, lerr := os.Stat(lock)
	locked := lerr == nil
	has := r.profileCred(dir, kc) != nil
	if locked != has {
		r.t.Fatalf("two owners or none: lock=%v profileCredential=%v", locked, has)
	}
	return locked
}

func TestRotaCheckoutTransferAndCheckin(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	snapPath := r.writeRotaClaude("A", "g1", exp)
	snapBefore, _ := os.ReadFile(snapPath)
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", false)
	lock := filepath.Join(r.claudeAcc, rotaName("A")+".pyre-lock")
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	if r.singleOwner(info.Dir, lock, "") {
		t.Fatal("checked out before use")
	}
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1", SessionID: "ses_1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProfileDir != info.Dir || res.Env["CLAUDE_CONFIG_DIR"] != info.Dir || res.CredentialKind != CredRotaCheckout {
		t.Fatalf("resolution %+v", res)
	}
	if !r.singleOwner(info.Dir, lock, "") {
		t.Fatal("not checked out")
	}
	cred := r.profileCred(info.Dir, "")
	if cred["refreshToken"] != "pyre-synthetic-A-refresh-g1" || cred["refreshTokenExpiresAt"] == nil {
		t.Fatalf("profile credential %v", cred != nil)
	}
	if fi, _ := os.Stat(filepath.Join(info.Dir, ".credentials.json")); fi.Mode().Perm() != 0o600 {
		t.Fatal("profile credential must be 0600")
	}
	if id, _ := claudeIdentity(info.Dir); id != claudeUUID("A") {
		t.Fatal("profile identity not set from the snapshot")
	}
	var l LockInfo
	lb, _ := os.ReadFile(lock)
	json.Unmarshal(lb, &l)
	if l.NodeID != "node_synth" || l.PID != os.Getpid() || len(l.RunIDs) != 1 || l.RunIDs[0] != "run_1" || l.ProfileDir != info.Dir {
		t.Fatalf("lock %+v", l)
	}
	if strings.Contains(string(lb), "pyre-synthetic") {
		t.Fatal("lock file contains a secret")
	}
	if r.fleet.holder("acct_synth-a") != "node_synth" {
		t.Fatal("fleet lease not taken")
	}
	// A second run shares the checkout; releasing one keeps it.
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_2"}); err != nil {
		t.Fatal(err)
	}
	r.svc.Release("run_1")
	if !r.singleOwner(info.Dir, lock, "") {
		t.Fatal("checked in while run_2 still uses it")
	}
	json.Unmarshal(mustRead(t, lock), &l)
	if len(l.RunIDs) != 1 || l.RunIDs[0] != "run_2" {
		t.Fatalf("lock runs %v", l.RunIDs)
	}
	// Last run ends: ownership moves back to rota.
	r.svc.Release("run_2")
	if r.singleOwner(info.Dir, lock, "") {
		t.Fatal("still checked out after the last run")
	}
	if after, _ := os.ReadFile(snapPath); string(after) != string(snapBefore) {
		t.Fatal("snapshot changed although nothing refreshed")
	}
	if r.fleet.holder("acct_synth-a") != "" || r.fleet.releases != 1 {
		t.Fatal("fleet lease not released")
	}
	cs, _ := r.svc.Checkouts(ctx)
	if len(cs) != 1 || cs[0].State != coReleased {
		t.Fatalf("checkouts %+v", cs)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRotaWriteBackAfterRefresh(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	t0 := time.Now().Add(time.Hour)
	snapPath := r.writeRotaClaude("A", "g1", t0)
	orig := readSnap(t, snapPath)
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	// Claude refreshes inside the profile (single-use refresh token rotates).
	r.simulateClaudeRefresh(info.Dir, "", "A", "g2", t0.Add(8*time.Hour))
	r.svc.SyncCheckouts(ctx)
	s := readSnap(t, snapPath)
	if s.ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-g2" || s.ClaudeAiOauth["accessToken"] != "pyre-synthetic-A-access-g2" {
		t.Fatal("rotated tokens not written back")
	}
	// rota's format and other keys survive.
	if s.Name != orig.Name || s.SavedAt != orig.SavedAt || s.OAuthAccount["accountUuid"] != claudeUUID("A") ||
		s.ClaudeAiOauth["refreshTokenExpiresAt"] != orig.ClaudeAiOauth["refreshTokenExpiresAt"] {
		t.Fatalf("snapshot keys lost: %+v", s)
	}
	raw := mustRead(t, snapPath)
	if !strings.HasSuffix(string(raw), "}\n") || !strings.Contains(string(raw), "\n  \"") {
		t.Fatal("snapshot not in rota's indent-2 + newline format")
	}
	if fi, _ := os.Stat(snapPath); fi.Mode().Perm() != 0o600 {
		t.Fatal("snapshot mode changed")
	}
	if m, _ := filepath.Glob(filepath.Join(r.claudeAcc, "*tmp*")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
	// Claude blanks the store (login rejected): never written back.
	os.WriteFile(filepath.Join(info.Dir, ".credentials.json"), []byte(`{}`), 0o600)
	r.svc.SyncCheckouts(ctx)
	if readSnap(t, snapPath).ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-g2" {
		t.Fatal("blank profile credential overwrote rota")
	}
	// Another refresh right before the run ends is written back on release.
	r.simulateClaudeRefresh(info.Dir, "", "A", "g3", t0.Add(16*time.Hour))
	r.svc.Release("run_1")
	if readSnap(t, snapPath).ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-g3" {
		t.Fatal("final write-back on release missing")
	}
	if r.profileCred(info.Dir, "") != nil {
		t.Fatal("profile still holds the credential after check-in")
	}
}

func TestRotaSnapshotAdvancedElsewhereNotOverwritten(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	t0 := time.Now().Add(time.Hour)
	snapPath := r.writeRotaClaude("A", "g1", t0)
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"})
	r.simulateClaudeRefresh(info.Dir, "", "A", "g2", t0.Add(time.Hour))
	r.writeRotaClaude("A", "gX", t0.Add(5*time.Hour)) // an unpatched tool refreshed rota's copy
	r.svc.SyncCheckouts(ctx)
	if readSnap(t, snapPath).ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-gX" {
		t.Fatal("newer snapshot overwritten")
	}
	cs, _ := r.svc.Checkouts(ctx)
	if !strings.Contains(cs[0].Detail, "newer") {
		t.Fatalf("conflict not surfaced: %q", cs[0].Detail)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestRotaCrashRecovery(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	t0 := time.Now().Add(time.Hour)
	snapPath := r.writeRotaClaude("A", "g1", t0)
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", false)
	lock := filepath.Join(r.claudeAcc, rotaName("A")+".pyre-lock")
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"})
	r.simulateClaudeRefresh(info.Dir, "", "A", "g2", t0.Add(8*time.Hour))
	// Daemon crashes (no Release, no sync). A new daemon instance starts.
	s2, err := New(ctx, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.RecoverRotaCheckouts(ctx, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if readSnap(t, snapPath).ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-g2" {
		t.Fatal("recovery did not write back the newer credential")
	}
	if r.singleOwner(info.Dir, lock, "") {
		t.Fatal("recovery left the account checked out")
	}
	if a, _ := s2.assignmentForRun(ctx, s2.db, "run_1"); a.ReleasedAt == nil {
		t.Fatal("dead run's assignment not released")
	}
	if r.fleet.holder("acct_synth-a") != "" {
		t.Fatal("fleet lease not released on recovery")
	}

	// Orphan: the database lost the checkout record; the lock and profile remain.
	r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_2"})
	r.simulateClaudeRefresh(info.Dir, "", "A", "g3", t0.Add(16*time.Hour))
	r.svc.db.Exec(`DELETE FROM accounts_checkout`)
	r.svc.db.Exec(`UPDATE accounts_assignment SET released_at=? WHERE run_id='run_2'`, sqlitex.FormatTime(time.Now()))
	var l LockInfo
	json.Unmarshal(mustRead(t, lock), &l)
	l.PID = deadPID(t)
	b, _ := json.Marshal(l)
	os.WriteFile(lock, b, 0o600)
	if err := s2.RecoverRotaCheckouts(ctx, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if readSnap(t, snapPath).ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-g3" {
		t.Fatal("orphan recovery did not write back")
	}
	if r.singleOwner(info.Dir, lock, "") {
		t.Fatal("orphan lock not resolved")
	}
	// A live run keeps its checkout across a daemon restart.
	r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_3"})
	s3, _ := New(ctx, r.cfg)
	s3.RecoverRotaCheckouts(ctx, func(id string) bool { return id == "run_3" })
	if !r.singleOwner(info.Dir, lock, "") {
		t.Fatal("recovery checked in an account a live run uses")
	}
}

func TestRotaLockHonouredAndFleetLease(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	r.writeRotaClaude("A", "g1", time.Now().Add(time.Hour))
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", false)
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	lock := filepath.Join(r.claudeAcc, rotaName("A")+".pyre-lock")

	// Another holder (another daemon/node, or a manual lock) owns it.
	os.WriteFile(lock, []byte(`{"holder":"pyre","node_id":"node_other","pid":1}`), 0o600)
	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_1"})
	if err == nil || !strings.Contains(err.Error(), "node_other") {
		t.Fatalf("lock not honoured: %v", err)
	}
	if r.profileCred(info.Dir, "") != nil {
		t.Fatal("credential written despite the lock")
	}
	os.Remove(lock)

	// Fleet: another node holds the refresh family.
	r.fleet.held["acct_synth-a"] = FleetLease{LeaseID: "x", NodeID: "node_far", ExpiresAt: time.Now().Add(time.Hour)}
	_, err = r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_2"})
	if err == nil {
		t.Fatal("checked out while another node holds the fleet lease")
	}
	if _, serr := os.Stat(lock); serr == nil || r.profileCred(info.Dir, "") != nil {
		t.Fatal("failed checkout left a lock or a credential behind")
	}
	delete(r.fleet.held, "acct_synth-a")
	// Hub unreachable: deny by default.
	r.fleet.unreachable = true
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_3"}); err == nil {
		t.Fatal("checkout allowed with the hub unreachable")
	}
	// ...unless the owner marked the account a per-machine independent login.
	r.svc.cfg.Rota.Independent = map[string]bool{"acct_synth-a": true}
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_4"}); err != nil {
		t.Fatal(err)
	}
	// A second node sharing the same rota directory can never take it too.
	db2, _ := sqlitex.Open(filepath.Join(r.base, "node2.db"))
	defer db2.Close()
	cfg2 := r.cfg
	cfg2.DB, cfg2.NodeID, cfg2.ProfileRoot = db2, "node_two", filepath.Join(r.base, "state2", "profiles")
	cfg2.Rota.Independent = map[string]bool{"acct_synth-a": true}
	s2, _ := New(ctx, cfg2)
	a, _ := r.svc.Account(ctx, "acct_synth-a")
	s2.UpsertAccount(ctx, a)
	info2, err := s2.CreateRotaProfile(ctx, a, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	s2.IngestObservation(ctx, contracts.AccountObservation{AccountID: "acct_synth-a", Provider: contracts.ProviderAnthropic, ObservedAt: r.clock.Now(),
		Eligibility: contracts.EligibleYes, Source: "mock", Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: 1}}})
	if _, err := s2.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_x"}); err == nil {
		t.Fatal("second holder checked out a locked account")
	}
	if r.profileCred(info2.Dir, "") != nil {
		t.Fatal("two live copies")
	}
}

func TestRotaNeverChecksOutLiveDefault(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	// DEFAULT is the node's live login (~/.claude.json, ~/.codex/auth.json).
	r.writeRotaClaude("DEFAULT", "g1", time.Now().Add(time.Hour))
	r.writeRotaClaude("B", "g1", time.Now().Add(time.Hour))
	pd := r.addRotaAccount("acct_synth-default", contracts.HarnessClaude, "DEFAULT", true)
	r.svc.cfg.Rota.Independent["acct_synth-b"] = true
	pb := r.addRotaAccount("acct_synth-b", contracts.HarnessClaude, "B", false)
	r.svc.cfg.Rota.Independent = map[string]bool{"acct_synth-default": true, "acct_synth-b": true}
	r.observe("acct_synth-default", contracts.ProviderAnthropic, 1, time.Minute)
	r.observe("acct_synth-b", contracts.ProviderAnthropic, 60, time.Minute)
	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-default"), RunID: "run_1"})
	if err == nil || !strings.Contains(err.Error(), "live default") {
		t.Fatalf("live default checked out: %v", err)
	}
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_2"})
	if err != nil || res.ProfileDir != pb.Dir {
		t.Fatalf("eligible should skip the live default: %+v %v", res.Assignment, err)
	}
	if r.profileCred(pd.Dir, "") != nil {
		t.Fatal("live default credential copied")
	}
	// If the owner `rota switch`es B to be the live default while checked
	// in, it is no longer eligible.
	r.svc.Release("run_2")
	writeJSON(t, r.userClaudeJSON, map[string]any{"oauthAccount": map[string]any{"accountUuid": claudeUUID("B")}}, 0o600)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-b"), RunID: "run_3"}); err == nil {
		t.Fatal("new live default checked out")
	}
	// Codex: same rule against ~/.codex/auth.json.
	r.writeRotaCodex("DEFAULT")
	r.svc.cfg.Rota.Independent["acct_synth-xd"] = true
	r.addRotaAccount("acct_synth-xd", contracts.HarnessCodex, "DEFAULT", false)
	r.svc.cfg.Rota.Independent["acct_synth-xd"] = true
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-xd"), RunID: "run_4"}); err == nil {
		t.Fatal("live default codex login checked out")
	}
}

// TestRotaLiveDefaultFromRotaHome: a node running under an isolated HOME
// (its own ~/.claude.json has no login) still refuses the OWNER's live
// default login when rota lives under another home (pyred --rota-home).
func TestRotaLiveDefaultFromRotaHome(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	// The daemon's own (isolated) ~/.claude.json: onboarding state, no login.
	writeJSON(t, r.userClaudeJSON, map[string]any{"hasCompletedOnboarding": true}, 0o600)
	owner := filepath.Join(t.TempDir(), "owner.claude.json")
	writeJSON(t, owner, map[string]any{"oauthAccount": map[string]any{"accountUuid": claudeUUID("DEFAULT")}}, 0o600)
	r.svc.cfg.Rota.LiveClaudeJSON = owner
	r.writeRotaClaude("DEFAULT", "g1", time.Now().Add(time.Hour))
	pd := r.addRotaAccount("acct_synth-default", contracts.HarnessClaude, "DEFAULT", false)
	r.svc.cfg.Rota.Independent = map[string]bool{"acct_synth-default": true}
	r.observe("acct_synth-default", contracts.ProviderAnthropic, 1, time.Minute)
	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-default"), RunID: "run_1"})
	if err == nil || !strings.Contains(err.Error(), "live default") {
		t.Fatalf("owner's live default checked out from an isolated-HOME node: %v", err)
	}
	if r.profileCred(pd.Dir, "") != nil {
		t.Fatal("live default credential copied")
	}
}

func TestRotaEligibleFallsThroughRefusedCheckout(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	r.writeRotaClaude("A", "g1", time.Now().Add(time.Hour))
	r.writeRotaClaude("B", "g1", time.Now().Add(time.Hour))
	r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", false)
	pb := r.addRotaAccount("acct_synth-b", contracts.HarnessClaude, "B", false)
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 1, time.Minute)  // more headroom
	r.observe("acct_synth-b", contracts.ProviderAnthropic, 50, time.Minute) // fallback
	r.fleet.held["acct_synth-a"] = FleetLease{LeaseID: "x", NodeID: "node_far", ExpiresAt: time.Now().Add(time.Hour)}
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"})
	if err != nil || res.ProfileDir != pb.Dir {
		t.Fatalf("want B after A's fleet lease was refused: %+v %v", res.Assignment, err)
	}
	if a, _ := r.svc.assignmentForRun(ctx, r.svc.db, "run_1"); a.AccountID != "acct_synth-b" || a.ReleasedAt != nil {
		t.Fatalf("assignment %+v", a)
	}
	// Only B's family is leased by this node.
	if r.fleet.holder("acct_synth-b") != "node_synth" || r.fleet.holder("acct_synth-a") != "node_far" {
		t.Fatal("fleet leases wrong")
	}
	// Cross-provider is never considered.
	r.writeRotaCodex("X")
	r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", true)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-x"), RunID: "run_2"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("cross-provider: %v", err)
	}
}

// macOS: Pyre profiles always use the 0600 credential file; Pyre never
// creates a keychain item for a profile (an item created by pyred needs an
// ACL approval before claude may read it: an invisible GUI prompt that hung
// every launch). Items earlier versions created are migrated to the file and
// deleted; the owner's default item and other config dirs' items are never
// touched.
func TestRotaKeychainCheckout(t *testing.T) {
	var shim, kc, logf string
	r := newRotaEnv(t, func(c *Config) {
		shim, kc, logf = writeKeychainShim(t, t.TempDir())
		c.GOOS, c.SecurityPath = "darwin", shim
	})
	ctx := context.Background()
	t0 := time.Now().Add(time.Hour)
	snapPath := r.writeRotaClaude("A", "g1", t0)
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
	lock := filepath.Join(r.claudeAcc, rotaName("A")+".pyre-lock")
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	// The owner's default item and another config dir's item exist.
	otherSvc := ClaudeKeychainService("/synthetic/other/config")
	os.WriteFile(filepath.Join(kc, "Claude Code-credentials"), []byte("7b7d"), 0o600)
	os.WriteFile(filepath.Join(kc, otherSvc), []byte("7b7d"), 0o600)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kc, ClaudeKeychainService(info.Dir))); err == nil {
		t.Fatal("Pyre created a keychain item for the profile")
	}
	if cred := r.profileCred(info.Dir, ""); cred["refreshToken"] != "pyre-synthetic-A-refresh-g1" {
		t.Fatal("credential file not written")
	}
	if st, err := os.Stat(filepath.Join(info.Dir, ".credentials.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode: %v %v", st, err)
	}
	cos, _ := r.svc.Checkouts(ctx)
	if len(cos) != 1 || !strings.Contains(cos[0].Detail, "profile store: file") {
		t.Fatalf("store not disclosed: %+v", cos)
	}
	r.singleOwner(info.Dir, lock, "")
	r.simulateClaudeRefresh(info.Dir, "", "A", "g2", t0.Add(8*time.Hour))
	r.svc.SyncCheckouts(ctx)
	if readSnap(t, snapPath).ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-g2" {
		t.Fatal("file refresh not written back")
	}
	r.svc.Release("run_1")
	if r.singleOwner(info.Dir, lock, "") {
		t.Fatal("credential file survived check-in")
	}

	// A leftover item for this profile (an earlier Pyre version) would
	// shadow the file: it is removed before the file is written. A locked
	// keychain cannot delete it, so that checkout is refused cleanly.
	os.WriteFile(filepath.Join(kc, ClaudeKeychainService(info.Dir)), []byte("7b7d"), 0o600)
	os.WriteFile(filepath.Join(kc, "locked"), nil, 0o600)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_2"}); err == nil {
		t.Fatal("checkout although a profile item shadows the file and cannot be removed")
	}
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("lock left after a failed checkout")
	}
	os.Remove(filepath.Join(kc, "locked"))
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kc, ClaudeKeychainService(info.Dir))); err == nil {
		t.Fatal("leftover profile item not removed")
	}
	if r.profileCred(info.Dir, "")["refreshToken"] != "pyre-synthetic-A-refresh-g2" {
		t.Fatal("checkout did not write the file")
	}

	// Migration at start: a checked-out profile whose live (newer)
	// credential sits in a Pyre-created item gets it in the file; the item
	// is deleted.
	r.simulateClaudeRefresh(info.Dir, kc, "A", "g3", t0.Add(16*time.Hour))
	r.svc.MigrateProfileKeychain(ctx)
	if _, err := os.Stat(filepath.Join(kc, ClaudeKeychainService(info.Dir))); err == nil {
		t.Fatal("migrated item not deleted")
	}
	if r.profileCred(info.Dir, "")["refreshToken"] != "pyre-synthetic-A-refresh-g3" {
		t.Fatal("item credential not moved to the file")
	}
	r.svc.SyncCheckouts(ctx)
	if readSnap(t, snapPath).ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-g3" {
		t.Fatal("migrated credential not written back")
	}
	// An older item never overwrites a newer file.
	r.simulateClaudeRefresh(info.Dir, kc, "A", "g0", t0)
	r.svc.MigrateProfileKeychain(ctx)
	if r.profileCred(info.Dir, "")["refreshToken"] != "pyre-synthetic-A-refresh-g3" {
		t.Fatal("older item overwrote the file")
	}
	// Locked keychain: an unreadable item is left for the next start.
	r.simulateClaudeRefresh(info.Dir, kc, "A", "g4", t0.Add(24*time.Hour))
	os.WriteFile(filepath.Join(kc, "locked"), nil, 0o600)
	r.svc.MigrateProfileKeychain(ctx)
	if _, err := os.Stat(filepath.Join(kc, ClaudeKeychainService(info.Dir))); err != nil {
		t.Fatal("unreadable item deleted before migration")
	}
	os.Remove(filepath.Join(kc, "locked"))
	r.svc.MigrateProfileKeychain(ctx)
	if r.profileCred(info.Dir, "")["refreshToken"] != "pyre-synthetic-A-refresh-g4" {
		t.Fatal("deferred migration lost the credential")
	}
	r.svc.Release("run_3")
	if r.singleOwner(info.Dir, lock, "") {
		t.Fatal("credential survived check-in")
	}
	// Checked in: a leftover item is just deleted (rota is authoritative).
	os.WriteFile(filepath.Join(kc, ClaudeKeychainService(info.Dir)), []byte("7b7d"), 0o600)
	r.svc.MigrateProfileKeychain(ctx)
	if _, err := os.Stat(filepath.Join(kc, ClaudeKeychainService(info.Dir))); err == nil {
		t.Fatal("leftover item of a checked-in profile kept")
	}
	if _, err := os.Stat(filepath.Join(info.Dir, ".credentials.json")); err == nil {
		t.Fatal("checked-in profile got a credential file")
	}
	// Never the owner's default item, never another config dir's item, and
	// never a secret in the security log or a write to the keychain.
	for _, f := range []string{"Claude Code-credentials", otherSvc} {
		if _, err := os.Stat(filepath.Join(kc, f)); err != nil {
			t.Fatalf("touched %q", f)
		}
	}
	lb := string(mustRead(t, logf))
	if strings.Contains(lb, "add-generic-password") || strings.Contains(lb, "pyre-synthetic") ||
		strings.Contains(lb, "Claude Code-credentials\n") || strings.Contains(lb, otherSvc) {
		t.Fatalf("security log: %q", lb)
	}
}

func TestRotaCodexCheckoutAndBusyHome(t *testing.T) {
	busy := &fakeBusy{busy: map[string]bool{}}
	r := newRotaEnv(t, func(c *Config) { c.Rota.HomeBusy = busy })
	ctx := context.Background()
	snapPath := r.writeRotaCodex("X")
	info := r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", true)
	lock := filepath.Join(filepath.Dir(snapPath), "pyre-lock")
	r.observe("acct_synth-x", contracts.ProviderOpenAI, 10, time.Minute)
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1"})
	if err != nil || res.Env["CODEX_HOME"] != info.Dir {
		t.Fatalf("%+v %v", res, err)
	}
	if string(mustRead(t, filepath.Join(info.Dir, "auth.json"))) != string(mustRead(t, snapPath)) {
		t.Fatal("codex auth.json not transferred verbatim")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("codex snapshot not locked")
	}
	// Codex refreshes in the profile.
	var a map[string]any
	json.Unmarshal(mustRead(t, filepath.Join(info.Dir, "auth.json")), &a)
	a["tokens"].(map[string]any)["refresh_token"] = "pyre-synthetic-X-refresh-g2"
	a["last_refresh"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	writeJSON(t, filepath.Join(info.Dir, "auth.json"), a, 0o600)
	r.svc.SyncCheckouts(ctx)
	if !strings.Contains(string(mustRead(t, snapPath)), "pyre-synthetic-X-refresh-g2") {
		t.Fatal("codex refresh not written back")
	}
	// The app-server is still up after the run ends: check-in waits.
	busy.busy[info.Dir] = true
	r.svc.Release("run_1")
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("checked in while the codex home is busy")
	}
	cs, _ := r.svc.Checkouts(ctx)
	if cs[0].State != coPending {
		t.Fatalf("state %s", cs[0].State)
	}
	busy.busy[info.Dir] = false
	r.svc.SyncCheckouts(ctx)
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("pending check-in not completed")
	}
	if _, err := os.Stat(filepath.Join(info.Dir, "auth.json")); err == nil {
		t.Fatal("profile auth.json survived check-in")
	}
}

func TestRotaUsageProbeUsesSnapshotWithoutRefresh(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	r.writeRotaClaude("A", "g1", time.Now().Add(time.Hour))
	r.writeRotaClaude("E", "g1", time.Now().Add(-time.Hour)) // expired access token
	r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
	r.addRotaAccount("acct_synth-e", contracts.HarnessClaude, "E", true)
	r.svc.ObserveDue(ctx, "")
	if r.fake.calls("A") != 1 || r.fake.calls("E") != 0 || r.fake.refreshes() != 0 {
		t.Fatalf("calls A=%d E=%d refresh=%d", r.fake.calls("A"), r.fake.calls("E"), r.fake.refreshes())
	}
	if o := latest(t, r.testEnv, "acct_synth-a"); o.Eligibility != contracts.EligibleYes {
		t.Fatalf("%+v", o)
	}
	if o := latest(t, r.testEnv, "acct_synth-e"); o.Eligibility != contracts.EligibleUnknown {
		t.Fatalf("%+v", o)
	}
}

func TestRotaNoSecretsInLogsDBErrors(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	t0 := time.Now().Add(time.Hour)
	r.writeRotaClaude("A", "g1", t0)
	r.writeRotaClaude("DEFAULT", "g1", t0)
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", false)
	r.svc.cfg.Rota.Independent = map[string]bool{"acct_synth-d": true}
	r.addRotaAccount("acct_synth-d", contracts.HarnessClaude, "DEFAULT", true)
	r.svc.ObserveDue(ctx, "")
	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-d"), RunID: "run_0"})
	r.err(err)
	r.fleet.unreachable = true
	_, err = r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_1"})
	r.err(err)
	r.fleet.unreachable = false
	_, err = r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_2"})
	r.err(err)
	r.simulateClaudeRefresh(info.Dir, "", "A", "g2", t0.Add(time.Hour))
	r.svc.SyncCheckouts(ctx)
	out, _ := r.svc.Handlers()[ActionAccountCheckouts](ctx, nil)
	ob, _ := json.Marshal(out)
	r.svc.Release("run_2")
	r.svc.db.Exec(`PRAGMA wal_checkpoint(FULL)`)
	hay := map[string]string{"logs": r.logs.String(), "errors": strings.Join(r.errs, "\n"), "reports": string(r.rep.all()), "action": string(ob)}
	for _, sfx := range []string{"", "-wal"} {
		if b, err := os.ReadFile(r.dbPath + sfx); err == nil {
			hay["db"+sfx] = string(b)
		}
	}
	for where, h := range hay {
		if strings.Contains(h, "pyre-synthetic-") {
			t.Errorf("credential material in %s", where)
		}
	}
	if !strings.Contains(r.logs.String(), "checked out") || !strings.Contains(r.logs.String(), "checked in") {
		t.Fatal("checkout lifecycle not logged")
	}
}

// `rota pyre-unlock` while Pyre holds the account: rota is authoritative
// again; Pyre never writes back over it and only deletes its own copy.
func TestRotaUnlockRevokesWriteBack(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	t0 := time.Now().Add(time.Hour)
	snapPath := r.writeRotaClaude("A", "g1", t0)
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
	lock := filepath.Join(r.claudeAcc, rotaName("A")+".pyre-lock")
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	os.Remove(lock) // rota pyre-unlock
	r.simulateClaudeRefresh(info.Dir, "", "A", "g2", t0.Add(time.Hour))
	r.svc.SyncCheckouts(ctx)
	if readSnap(t, snapPath).ClaudeAiOauth["refreshToken"] != "pyre-synthetic-A-refresh-g1" {
		t.Fatal("wrote back after the owner unlocked")
	}
	r.svc.Release("run_1")
	if r.profileCred(info.Dir, "") != nil {
		t.Fatal("profile copy kept after revoked check-in")
	}
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("lock recreated")
	}
}

func TestRegisterAction(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	r.writeRotaClaude("A", "g1", time.Now().Add(time.Hour))
	r.writeRotaClaude("DEFAULT", "g1", time.Now().Add(time.Hour))
	writeJSON(t, r.userClaudeJSON, map[string]any{"oauthAccount": map[string]any{"accountUuid": claudeUUID("DEFAULT")},
		"hasCompletedOnboarding": true, "lastOnboardingVersion": "1.0.0", "theme": nil, "claudeInChromeDefaultEnabled": false,
		"autoUpdates": false, "hasCompletedClaudeInChromeOnboarding": true}, 0o600)
	h := r.svc.Handlers()[ActionAccountRegister]
	out, err := h(ctx, json.RawMessage(`{"account":{"account_id":"acct_synth-a","label":"acct-A","rota_ref":"`+rotaName("A")+`"},"harness":"claude","source":"rota","bind_rota_identity":true}`))
	if err != nil {
		t.Fatal(err)
	}
	res := out.(RegisterResult)
	if res.ProviderRef != claudeUUID("A") || !res.Verified || res.Grant.Kind != CredRotaCheckout || res.Profile.LoginCommand != "" {
		t.Fatalf("%+v", res)
	}
	a, _ := r.svc.Account(ctx, "acct_synth-a")
	if a.ProviderRef != claudeUUID("A") || a.Provider != contracts.ProviderAnthropic {
		t.Fatalf("account %+v", a)
	}
	var cj map[string]any
	json.Unmarshal(mustRead(t, filepath.Join(res.Profile.Dir, ".claude.json")), &cj)
	if cj["hasCompletedOnboarding"] != true || cj["lastOnboardingVersion"] != "1.0.0" || cj["oauthAccount"] != nil {
		t.Fatalf("onboarding flags not composed (or oauthAccount leaked): %v", cj)
	}
	// The owner's answer to Claude's browser-extension prompt is carried
	// over; without it every second launch in a profile stops at that
	// modal (observed live with Claude 2.1.289 on server).
	if v, ok := cj["claudeInChromeDefaultEnabled"]; !ok || v != false {
		t.Fatalf("browser-extension choice not composed: %v", cj)
	}
	// The owner's completed "Claude in Chrome" onboarding is carried over:
	// without it a fresh profile's first launch with Claude 2.1.285 (laptop)
	// stops at that modal, so the unattended run never takes its first prompt.
	if v, ok := cj["hasCompletedClaudeInChromeOnboarding"]; !ok || v != true {
		t.Fatalf("Claude in Chrome onboarding not composed: %v", cj)
	}
	// autoUpdates is NOT copied into the profile: Claude 2.1.285 migrates a
	// config autoUpdates=false into env.DISABLE_AUTOUPDATER in settings.json,
	// which is linked to the owner's file (observed on laptop). The owner's
	// opt-out reaches Claude as a launch environment variable instead
	// (TestOwnerAutoUpdateOptOutEnv).
	if _, ok := cj["autoUpdates"]; ok {
		t.Fatalf("autoUpdates must not be composed into the profile: %v", cj)
	}
	if _, ok := cj["theme"]; ok {
		t.Fatal("null keys must not be copied")
	}
	// The live default is refused at registration.
	_, err = h(ctx, json.RawMessage(`{"account":{"account_id":"acct_synth-d","rota_ref":"`+rotaName("DEFAULT")+`"},"harness":"claude","source":"rota","bind_rota_identity":true}`))
	if !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("live default registered: %v", err)
	}
	// A provider_ref that disagrees with the snapshot is refused.
	_, err = h(ctx, json.RawMessage(`{"account":{"account_id":"acct_synth-a2","rota_ref":"`+rotaName("A")+`","provider_ref":"someone-else"},"harness":"claude","source":"rota"}`))
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("mismatched provider_ref: %v", err)
	}
	if _, err := r.svc.Account(ctx, "acct_synth-a2"); err == nil {
		t.Fatal("refused registration left an account behind")
	}
}

// TestRotaCheckinWaitsForProfileUsers: a helper that outlived the Claude run
// but is still bound to the profile (e.g. a memory tool's detached
// enrichment worker that calls `claude -p`; seen in the W-memconn live run)
// keeps the checkout pending. A refresh it makes meanwhile is written back,
// and check-in completes once it exits: rota never ends up with a stale
// refresh token.
func TestRotaCheckinWaitsForProfileUsers(t *testing.T) {
	inUse := map[string][]int{}
	r := newRotaEnv(t, func(c *Config) {
		c.Rota.ProfileUsers = func(_ context.Context, dir string) ([]int, bool, error) { return inUse[dir], true, nil }
	})
	ctx := context.Background()
	snap := r.writeRotaClaude("A", "g1", time.Now().Add(time.Hour))
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
	lock := strings.TrimSuffix(snap, ".json") + ".pyre-lock"
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	inUse[info.Dir] = []int{4242}
	r.svc.Release("run_1")
	cs, _ := r.svc.Checkouts(ctx)
	if len(cs) != 1 || cs[0].State != coPending || !strings.Contains(cs[0].Detail, "4242") {
		t.Fatalf("check-in should wait for the helper: %+v", cs)
	}
	if _, err := os.Stat(lock); err != nil || r.profileCred(info.Dir, "") == nil {
		t.Fatal("credential moved back while a process still uses the profile")
	}
	// The helper refreshes the login meanwhile.
	r.simulateClaudeRefresh(info.Dir, "", "A", "g2", time.Now().Add(2*time.Hour))
	delete(inUse, info.Dir)
	r.svc.SyncCheckouts(ctx)
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("pending check-in not completed after the helper exited")
	}
	if r.profileCred(info.Dir, "") != nil {
		t.Fatal("profile credential survived check-in")
	}
	if !strings.Contains(string(mustRead(t, snap)), "pyre-synthetic-A-refresh-g2") {
		t.Fatal("the helper's refresh was not written back to rota")
	}
	cs, _ = r.svc.Checkouts(ctx)
	if cs[0].State != coReleased {
		t.Fatalf("state %s", cs[0].State)
	}
}

func TestProfileUsersProc(t *testing.T) {
	root := t.TempDir()
	put := func(pid, env string) {
		os.MkdirAll(filepath.Join(root, pid), 0o700)
		os.WriteFile(filepath.Join(root, pid, "environ"), []byte(env), 0o600)
	}
	put("101", "HOME=/h\x00CLAUDE_CONFIG_DIR=/p/acct/claude\x00PATH=/bin\x00")
	put("102", "CODEX_HOME=/p/acct/claude/\x00")
	put("103", "CLAUDE_CONFIG_DIR=/p/acct/claude-other\x00") // another profile
	put("104", "X=CLAUDE_CONFIG_DIR=/p/acct/claude\x00")     // not the variable itself
	put("105", "CLAUDE_CONFIG_DIR=/p/acct/claude\x00")       // this process
	put("106", "")                                           // kernel thread / unreadable
	os.MkdirAll(filepath.Join(root, "self"), 0o700)
	got, err := profileUsersProc(context.Background(), root, "/p/acct/claude", 105)
	if err != nil || len(got) != 2 || got[0] != 101 || got[1] != 102 {
		t.Fatalf("profile users = %v %v", got, err)
	}
	if got, _ := profileUsersProc(context.Background(), root, "/", 0); len(got) != 0 {
		t.Fatalf("root dir matched: %v", got)
	}
}

// TestProfileUsersLive runs a real child process bound to a profile (Linux).
func TestProfileUsersLive(t *testing.T) {
	dir := t.TempDir()
	if _, ok, _ := ProfileUsers(context.Background(), dir); !ok {
		t.Skip("process environments are not scanned on " + runtime.GOOS)
	}
	cmd := exec.Command("sleep", "5")
	cmd.Env = []string{"CLAUDE_CONFIG_DIR=" + dir}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pids, _, err := ProfileUsers(context.Background(), dir)
		if err == nil && len(pids) == 1 && pids[0] == cmd.Process.Pid {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("child bound to the profile not found")
}

// TestRotaCheckinWaitsForRealProfileProcess is TestRotaCheckinWaitsForProfileUsers
// with the default ProfileUsers (Linux /proc scan) and a real process bound
// to the profile: check-in stays pending while it runs and completes after
// it exits.
func TestRotaCheckinWaitsForRealProfileProcess(t *testing.T) {
	if _, ok, _ := ProfileUsers(context.Background(), t.TempDir()); !ok {
		t.Skip("process environments are not scanned on " + runtime.GOOS)
	}
	r := newRotaEnv(t)
	ctx := context.Background()
	snap := r.writeRotaClaude("A", "g1", time.Now().Add(time.Hour))
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
	lock := strings.TrimSuffix(snap, ".json") + ".pyre-lock"
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command("sleep", "30")
	helper.Env = []string{"CLAUDE_CONFIG_DIR=" + info.Dir}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		if !exited {
			helper.Process.Kill()
			helper.Wait()
		}
	}()
	r.svc.Release("run_1")
	cs, _ := r.svc.Checkouts(ctx)
	if len(cs) != 1 || cs[0].State != coPending || !strings.Contains(cs[0].Detail, strconv.Itoa(helper.Process.Pid)) {
		t.Fatalf("check-in should wait for pid %d: %+v", helper.Process.Pid, cs)
	}
	if _, err := os.Stat(lock); err != nil || r.profileCred(info.Dir, "") == nil {
		t.Fatal("credential moved back while a real process still uses the profile")
	}
	helper.Process.Kill()
	helper.Wait()
	exited = true
	r.svc.SyncCheckouts(ctx)
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("pending check-in not completed after the process exited")
	}
	if cs, _ = r.svc.Checkouts(ctx); cs[0].State != coReleased {
		t.Fatalf("state %s (%s)", cs[0].State, cs[0].Detail)
	}
}

// TestClaudeProfileEnvDisablesUpdater: every Claude launch in a profile gets
// DISABLE_AUTOUPDATER=1 (whatever the owner's own preference), so a
// Pyre-launched Claude never self-updates into the owner's installation nor
// auto-updates plugin marketplaces in the owner's plugin directories; nothing
// is written to any config file.
func TestClaudeProfileEnvDisablesUpdater(t *testing.T) {
	for _, owner := range []any{false, true, nil} {
		r := newRotaEnv(t)
		ctx := context.Background()
		r.writeRotaClaude("A", "g1", time.Now().Add(time.Hour))
		writeJSON(t, r.userClaudeJSON, map[string]any{"autoUpdates": owner}, 0o600)
		before := mustRead(t, r.userClaudeJSON)
		r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
		r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
		res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1", SessionID: "ses_1"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Env["DISABLE_AUTOUPDATER"] != "1" || res.Env["CLAUDE_CONFIG_DIR"] == "" {
			t.Fatalf("owner autoUpdates=%v: launch env %v", owner, res.Env)
		}
		if !bytes.Equal(before, mustRead(t, r.userClaudeJSON)) {
			t.Fatal("owner ~/.claude.json written")
		}
		r.svc.Release("run_1")
	}
	if ClaudeProfileEnv()["DISABLE_AUTOUPDATER"] != "1" {
		t.Fatal("ClaudeProfileEnv")
	}
}

// TestRotaCheckinWaitsForDaemonHold: a background job of this daemon that
// still uses an ended run's profile (Pyre's Hive capture of a Codex rollout)
// holds the profile; check-in waits for the hold, then completes.
func TestRotaCheckinWaitsForDaemonHold(t *testing.T) {
	r := newRotaEnv(t, func(c *Config) {
		c.Rota.ProfileUsers = func(context.Context, string) ([]int, bool, error) { return nil, true, nil }
	})
	ctx := context.Background()
	snap := r.writeRotaClaude("A", "g1", time.Now().Add(time.Hour))
	info := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", true)
	lock := strings.TrimSuffix(snap, ".json") + ".pyre-lock"
	r.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	release := r.svc.HoldProfile(info.Dir)
	r.svc.Release("run_1")
	cs, _ := r.svc.Checkouts(ctx)
	if len(cs) != 1 || cs[0].State != coPending || !strings.Contains(cs[0].Detail, "hold") {
		t.Fatalf("check-in should wait for the hold: %+v", cs)
	}
	if _, err := os.Stat(lock); err != nil || r.profileCred(info.Dir, "") == nil {
		t.Fatal("credential moved back while the daemon still holds the profile")
	}
	release()
	release() // idempotent
	r.svc.SyncCheckouts(ctx)
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("check-in not completed after the hold was released")
	}
	if r.svc.profileHolds(info.Dir) != 0 {
		t.Fatal("hold count not back to zero")
	}
}
