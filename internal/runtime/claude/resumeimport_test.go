package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// A session that ran on the default login resumes on an account profile:
// the transcript and its sidecar dir are copied into the profile's home.
func TestResumeImportsTranscriptFromDefaultLogin(t *testing.T) {
	base := t.TempDir()
	def := filepath.Join(base, "home", ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", def)
	cwd := filepath.Join(base, "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	sid := "b6160473-db4b-450d-b703-57a81659085a"
	src := guessTranscriptPath(def, cwd, sid)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(src), sid, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(src), sid, "subagents", "a.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(base, "profiles", "acct_X", "claude-token")
	a := &Adapter{o: Options{TranscriptHomes: []string{filepath.Join(base, "profiles", "*", "claude*")}}}

	prev := contracts.Run{Cwd: cwd, NativeRef: &contracts.NativeRef{Kind: "claude_session", ID: sid}}
	if !a.NativeThreadOnNode(prev) {
		t.Fatal("transcript on the default login not found")
	}
	if err := a.importTranscript(profile, cwd, sid); err != nil {
		t.Fatal(err)
	}
	dst := guessTranscriptPath(profile, cwd, sid)
	if b, err := os.ReadFile(dst); err != nil || string(b) != `{"type":"user"}`+"\n" {
		t.Fatalf("transcript not imported: %q %v", b, err)
	}
	if !isFile(filepath.Join(filepath.Dir(dst), sid, "subagents", "a.jsonl")) {
		t.Fatal("sidecar dir not imported")
	}
	if !isFile(src) {
		t.Fatal("original transcript must stay")
	}
	// Nowhere on the node: no native thread.
	prev.NativeRef.ID = "00000000-0000-4000-8000-000000000000"
	if a.NativeThreadOnNode(prev) {
		t.Fatal("found a transcript that does not exist")
	}
}
