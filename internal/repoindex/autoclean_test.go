package repoindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

const synthSession = "ses_01SYNTHAUTOCLEAN0000000000"

const synthToken = "claim_01SYNTHTOKEN00000000000000"

// markFor writes a pyre-created marker (as Create does) and claims it for
// session with the create's token, as the launcher does once the session
// started ("" leaves it unclaimed).
func markFor(t *testing.T, wt, session string) {
	t.Helper()
	adm, ok := adminDir(wt)
	if !ok {
		t.Fatalf("%s is not a linked worktree", wt)
	}
	if err := os.WriteFile(filepath.Join(adm, MarkerFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\nclaim="+synthToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if session != "" {
		if got := Claim([]string{wt}, session, synthToken); len(got) != 1 {
			t.Fatalf("claim %s: %v", wt, got)
		}
	}
}

// autoFixture: a repo with an origin and worktrees in every state the
// automatic rules distinguish, all marked for synthSession unless noted.
func autoFixture(t *testing.T) (repo string, wts map[string]string) {
	t.Helper()
	home := isolate(t)
	remote := filepath.Join(home, "remote.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, remote, "init", "-q", "--bare")
	repo = mkrepo(t, filepath.Join(home, "code", "svc"))
	_ = os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("secret.env\nprod.env\nnode_modules/\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(repo, "deploy", "build"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "deploy", "build", "README"), []byte("tracked\n"), 0o644)
	git(t, repo, "add", ".gitignore", "deploy")
	git(t, repo, "commit", "-q", "-m", "ignore")
	git(t, repo, "remote", "add", "origin", remote)
	git(t, repo, "push", "-q", "origin", "main")
	git(t, repo, "fetch", "-q", "origin")
	wts = map[string]string{}
	add := func(name string, args ...string) string {
		p := filepath.Join(home, "wt", name)
		git(t, repo, append([]string{"worktree", "add", "-q"}, append(args, p, "HEAD")...)...)
		wts[name] = p
		markFor(t, p, synthSession)
		return p
	}
	commit := func(dir string) { // distinct content: identical commits would share a hash
		_ = os.WriteFile(filepath.Join(dir, "new.txt"), []byte(dir+"\n"), 0o644)
		git(t, dir, "add", "new.txt")
		git(t, dir, "commit", "-q", "-m", "work in "+filepath.Base(dir))
	}
	add("merged", "-b", "merged")
	commit(add("unpushed", "-b", "unpushed"))
	pushed := add("pushed", "-b", "pushed")
	commit(pushed)
	git(t, pushed, "push", "-q", "-u", "origin", "pushed")
	// Pushed under another name only: its own upstream does not hold it.
	elsewhere := add("elsewhere", "-b", "elsewhere")
	commit(elsewhere)
	git(t, elsewhere, "push", "-q", "origin", "elsewhere:other-name")
	git(t, repo, "fetch", "-q", "origin")
	// Pushed, then deleted on the remote: the stale tracking ref says
	// pushed, ls-remote does not, so the branch is kept.
	gone := add("gone", "-b", "gone")
	commit(gone)
	git(t, gone, "push", "-q", "-u", "origin", "gone")
	if _, err := Git(context.Background(), remote, "branch", "-D", "gone"); err != nil {
		t.Fatal(err)
	}
	dirty := add("dirty", "-b", "dirty")
	_ = os.WriteFile(filepath.Join(dirty, "README"), []byte("changed\n"), 0o644)
	secret := add("secret", "-b", "secret")
	_ = os.WriteFile(filepath.Join(secret, "secret.env"), []byte("SYNTHETIC=1\n"), 0o600)
	// An ignored secret inside a tracked folder named build/.
	buildSecret := add("build-secret", "-b", "build-secret")
	_ = os.WriteFile(filepath.Join(buildSecret, "deploy", "build", "prod.env"), []byte("SYNTHETIC=1\n"), 0o600)
	deps := add("deps", "-b", "deps")
	_ = os.MkdirAll(filepath.Join(deps, "node_modules", "left-pad"), 0o755)
	_ = os.WriteFile(filepath.Join(deps, "node_modules", "left-pad", "index.js"), []byte("synthetic\n"), 0o644)
	add("busy", "-b", "busy")
	add("detached-safe", "--detach")
	commit(add("detached-lost", "--detach"))
	locked := add("locked", "-b", "locked")
	git(t, repo, "worktree", "lock", locked)
	// The default branch checked out in a linked worktree.
	git(t, repo, "checkout", "-q", "-b", "dev")
	p := filepath.Join(home, "wt", "on-main")
	git(t, repo, "worktree", "add", "-q", p, "main")
	wts["on-main"] = p
	markFor(t, p, synthSession)
	// The owner's own worktree (no marker) and another session's.
	p = filepath.Join(home, "wt", "owners")
	git(t, repo, "worktree", "add", "-q", "-b", "owners", p, "main")
	wts["owners"] = p
	p = filepath.Join(home, "wt", "others")
	git(t, repo, "worktree", "add", "-q", "-b", "others", p, "main")
	wts["others"] = p
	markFor(t, p, "ses_01SYNTHOTHERSESSION00000000")
	return repo, wts
}

func busyOnly(wts map[string]string) InUse {
	return func(w contracts.Worktree, repo string, created time.Time) string {
		if Inside(w.Path, wts["busy"]) {
			return "synthetic live session works in it"
		}
		return ""
	}
}

func outcomes(rs []contracts.WorktreeRemoval) map[string]string {
	got := map[string]string{}
	for _, r := range rs {
		got[filepath.Base(r.Path)] = r.Outcome + "/" + r.BranchOutcome
	}
	return got
}

var wantAuto = map[string]string{
	"merged": "removed/deleted", "pushed": "removed/deleted", "detached-safe": "removed/kept", "deps": "removed/deleted",
	"gone": "removed/kept", "unpushed": "kept_unmerged/", "elsewhere": "kept_unmerged/", "detached-lost": "kept_unmerged/",
	"dirty": "kept_dirty/", "secret": "kept_dirty/", "build-secret": "kept_dirty/", "busy": "kept_in_use/", "on-main": "kept_in_use/",
}

func TestSweepRepoAutomaticRules(t *testing.T) {
	repo, wts := autoFixture(t)
	ctx := context.Background()
	keep := func(sid string) bool { return sid == "ses_01SYNTHOTHERSESSION00000000" } // still live
	// Nothing is idle yet: nothing listed.
	if rs := SweepRepo(ctx, repo, time.Hour, time.Now(), keep, busyOnly(wts), false); len(rs) != 0 {
		t.Fatalf("recent worktrees are not orphans: %+v", rs)
	}
	later := time.Now().Add(2 * time.Hour)
	dry := outcomes(SweepRepo(ctx, repo, time.Hour, later, keep, busyOnly(wts), true))
	if dry["merged"] != "would_remove/" {
		t.Fatalf("dry run: %v", dry)
	}
	if _, err := os.Stat(wts["merged"]); err != nil {
		t.Fatal("dry run removed a worktree")
	}
	got := outcomes(SweepRepo(ctx, repo, time.Hour, later, keep, busyOnly(wts), false))
	for k, v := range wantAuto {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"locked", "owners", "others", "svc"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s is never touched by the sweep: %v", k, got[k])
		}
	}
	for _, k := range []string{"unpushed", "elsewhere", "dirty", "secret", "busy", "detached-lost", "locked", "on-main", "owners", "others"} {
		if _, err := os.Stat(wts[k]); err != nil {
			t.Errorf("%s was removed", k)
		}
	}
	if _, err := os.Stat(filepath.Join(wts["secret"], "secret.env")); err != nil {
		t.Error("a gitignored secret was deleted")
	}
	if _, err := os.Stat(filepath.Join(wts["build-secret"], "deploy", "build", "prod.env")); err != nil {
		t.Error("an ignored secret inside a tracked build/ folder was deleted")
	}
	for _, b := range []string{"unpushed", "elsewhere", "gone", "main"} {
		if !branchExists(ctx, repo, b) {
			t.Errorf("branch %s must be kept", b)
		}
	}
}

func TestCleanSessionOnlyItsMarkedWorktrees(t *testing.T) {
	repo, wts := autoFixture(t)
	ctx := context.Background()
	// The session ran in the main checkout; its worktrees are recognised
	// by their marker only, never by when they were created.
	got := outcomes(CleanSession(ctx, ScanInput{SessionID: synthSession, Cwd: repo, Since: time.Now().Add(-time.Hour)}, busyOnly(wts)))
	for k, v := range wantAuto {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if got["locked"] != "kept_in_use/" {
		t.Errorf("locked: %q", got["locked"])
	}
	for _, k := range []string{"owners", "others"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s is not this session's: %v", k, got[k])
		}
		if _, err := os.Stat(wts[k]); err != nil {
			t.Errorf("%s was removed", k)
		}
	}
}

func TestClaimOnceWithToken(t *testing.T) {
	_, wts := autoFixture(t)
	if got := Claim([]string{wts["others"]}, synthSession, synthToken); len(got) != 0 {
		t.Fatalf("a claimed marker is not taken over: %v", got)
	}
	if MarkerSession(wts["others"]) != "ses_01SYNTHOTHERSESSION00000000" || MarkerSession(wts["owners"]) != "" {
		t.Fatal("marker sessions")
	}
	// An unclaimed marker with another token (or a pre-deploy marker with
	// none) is never claimed; the right token claims it once.
	markFor(t, wts["owners"], "")
	if got := Claim([]string{wts["owners"]}, synthSession, "claim_01SYNTHWRONGTOKEN0000000"); len(got) != 0 {
		t.Fatalf("wrong token claimed: %v", got)
	}
	adm, _ := adminDir(wts["owners"])
	_ = os.WriteFile(filepath.Join(adm, MarkerFile), []byte("2026-10-01T00:00:00Z\n"), 0o644)
	if got := Claim([]string{wts["owners"]}, synthSession, synthToken); len(got) != 0 {
		t.Fatalf("a marker without a token was claimed: %v", got)
	}
	markFor(t, wts["owners"], "")
	if got := Claim([]string{wts["owners"], filepath.Dir(wts["owners"])}, synthSession, synthToken); len(got) != 1 || !strings.HasSuffix(got[0], "owners") {
		t.Fatalf("claim with the token, never via a folder: %v", got)
	}
}

func TestCreateWritesClaimToken(t *testing.T) {
	home := isolate(t)
	r := mkrepo(t, filepath.Join(home, "code", "api"))
	res, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{r}, Slug: "synthetic task"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.ClaimToken == "" || MarkerSession(res.Cwd) != "" {
		t.Fatalf("token returned, no session yet: %+v", res)
	}
	if got := Claim([]string{res.Cwd}, synthSession, res.ClaimToken); len(got) != 1 || MarkerSession(res.Cwd) != synthSession {
		t.Fatalf("claimed: %v %q", got, MarkerSession(res.Cwd))
	}
}
