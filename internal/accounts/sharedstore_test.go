package accounts

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// Profiles made before the store was shared (or holding their own copies)
// are merged into the shared store without losing a file; idempotent.
func TestMigrateSharedStoresMergesProfiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.cfg.ProfileRoot
	shared := filepath.Join(e.cfg.SharedRoot, "claude")
	tokA := filepath.Join(root, "acct_synth-a", "claude-token")
	loginB := filepath.Join(root, "acct_synth-b", "claude")
	codex := filepath.Join(root, "acct_synth-c", "codex")
	const uuid = "00000000-0000-4000-8000-0000000000aa"

	// A: everything still its own (an old token profile).
	writeFile(t, filepath.Join(tokA, "projects", "-synth-work", uuid+".jsonl"), "synthetic transcript A\n")
	writeFile(t, filepath.Join(tokA, "projects", "-synth-work", "same.jsonl"), "identical\n")
	writeFile(t, filepath.Join(tokA, "projects", "-synth-work", "clash.jsonl"), "from A\n")
	writeFile(t, filepath.Join(tokA, "session-env", uuid, "hook-1.sh"), "export SYNTH=1\n")
	writeFile(t, filepath.Join(tokA, "shell-snapshots", "snapshot-synth.sh"), "# synthetic\n")
	writeFile(t, filepath.Join(tokA, "history.jsonl"), `{"display":"synthetic A"}`)
	writeFile(t, filepath.Join(tokA, "sessions", "123.json"), "{}") // live-process registry: stays local
	writeFile(t, filepath.Join(tokA, ".credentials.json"), "{}")    // never touched
	// The store already holds some of it.
	writeFile(t, filepath.Join(shared, "projects", "-synth-work", "same.jsonl"), "identical\n")
	writeFile(t, filepath.Join(shared, "projects", "-synth-work", "clash.jsonl"), "from store\n")
	writeFile(t, filepath.Join(shared, "history.jsonl"), `{"display":"synthetic store"}`+"\n")
	// B: projects already linked, session-env still its own.
	if err := os.MkdirAll(loginB, 0o700); err != nil {
		t.Fatal(err)
	}
	sharedCanon, _ := Canonicalize(shared)
	if err := os.Symlink(filepath.Join(sharedCanon, "projects"), filepath.Join(loginB, "projects")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(loginB, "session-env", "00000000-0000-4000-8000-0000000000bb", "hook-1.sh"), "export SYNTH=2\n")
	writeFile(t, filepath.Join(codex, "sessions", "rollout-synth.jsonl"), "codex\n") // codex profiles are not migrated

	res := e.svc.MigrateSharedStores(ctx)
	if res.Profiles != 2 || len(res.Conflicts) != 0 {
		t.Fatalf("migration %+v", res)
	}
	for _, dir := range []string{tokA, loginB} {
		for _, it := range claudeShared {
			p := filepath.Join(dir, it.name)
			fi, err := os.Lstat(p)
			if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
				t.Fatalf("%s is not a link: %v", p, err)
			}
			if tg, _ := os.Readlink(p); tg != filepath.Join(sharedCanon, it.name) {
				t.Fatalf("%s -> %s", p, tg)
			}
			if _, err := os.Lstat(p + migratingSuffix); err == nil {
				t.Fatalf("aside left: %s", p+migratingSuffix)
			}
		}
	}
	if !sharesTranscriptStore(contracts.HarnessClaude, tokA, loginB) {
		t.Fatal("profiles do not share the transcript store")
	}
	// Nothing lost: every file is reachable from either profile.
	if readFile(t, filepath.Join(loginB, "projects", "-synth-work", uuid+".jsonl")) != "synthetic transcript A\n" ||
		readFile(t, filepath.Join(loginB, "projects", "-synth-work", "same.jsonl")) != "identical\n" ||
		readFile(t, filepath.Join(tokA, "projects", "-synth-work", "clash.jsonl")) != "from store\n" ||
		readFile(t, filepath.Join(tokA, "projects", "-synth-work", "clash.jsonl.from-acct_synth-a-claude-token")) != "from A\n" ||
		readFile(t, filepath.Join(loginB, "session-env", uuid, "hook-1.sh")) != "export SYNTH=1\n" ||
		readFile(t, filepath.Join(tokA, "session-env", "00000000-0000-4000-8000-0000000000bb", "hook-1.sh")) != "export SYNTH=2\n" ||
		readFile(t, filepath.Join(loginB, "shell-snapshots", "snapshot-synth.sh")) != "# synthetic\n" {
		t.Fatal("merged content wrong")
	}
	if h := readFile(t, filepath.Join(shared, "history.jsonl")); h != `{"display":"synthetic store"}`+"\n"+`{"display":"synthetic A"}` {
		t.Fatalf("history %q", h)
	}
	if fi, err := os.Lstat(filepath.Join(tokA, "sessions")); err != nil || !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		t.Fatal("sessions/ must stay per profile")
	}
	if readFile(t, filepath.Join(tokA, ".credentials.json")) != "{}" {
		t.Fatal("credentials touched")
	}
	if fi, err := os.Lstat(filepath.Join(codex, "sessions")); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
		t.Fatal("codex profile migrated")
	}
	state := filepath.Dir(root)
	before := fileState(t, state)
	if again := e.svc.MigrateSharedStores(ctx); len(again.Adopted) != 0 || len(again.Conflicts) != 0 {
		t.Fatalf("second pass not idempotent: %+v", again)
	}
	sameState(t, "state after a second pass", before, fileState(t, state))

	// An interrupted earlier pass (aside left behind) is finished.
	writeFile(t, filepath.Join(tokA, "plans"+migratingSuffix, "plan-synth.md"), "synthetic plan\n")
	if r := e.svc.MigrateSharedStores(ctx); len(r.Conflicts) != 0 {
		t.Fatalf("aside pass %+v", r)
	}
	if readFile(t, filepath.Join(loginB, "plans", "plan-synth.md")) != "synthetic plan\n" {
		t.Fatal("aside not merged")
	}
	if _, err := os.Lstat(filepath.Join(tokA, "plans"+migratingSuffix)); err == nil {
		t.Fatal("aside not removed")
	}
}

// A fresh token profile links the whole conversation store (createProfile).
func TestTokenProfileSharesConversationStore(t *testing.T) {
	e := newTokenEnv(t)
	a := e.addToken("acct_synth-ta", "ta", contracts.HarnessClaude)
	b := e.addToken("acct_synth-tb", "tb", contracts.HarnessClaude)
	for _, it := range claudeShared {
		ra, err1 := Canonicalize(filepath.Join(a.Profile.Dir, it.name))
		rb, err2 := Canonicalize(filepath.Join(b.Profile.Dir, it.name))
		if err1 != nil || err2 != nil || ra != rb || !strings.HasPrefix(ra, canonicalOrClean(e.cfg.SharedRoot)) {
			t.Fatalf("%s: %s vs %s (%v %v)", it.name, ra, rb, err1, err2)
		}
	}
}
