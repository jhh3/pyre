package claude

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// RecentCwds reads the cwd Claude records on transcript lines, newest
// first and distinct, so a resume whose launch worktree was removed can
// start where the conversation went on working.
func TestRecentCwdsIn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "synthetic.jsonl")
	lines := []string{
		`{"type":"user","cwd":"/synthetic/wt/launch"}`,
		`{"type":"assistant","cwd":"/synthetic/wt/launch/apps/server"}`,
		`not json`,
		`{"type":"summary"}`,
		`{"type":"user","cwd":"/synthetic/wt/launch"}`,
		`{"type":"user","cwd":"/synthetic/wt/second"}`,
		`{"type":"assistant","cwd":"/synthetic/wt/second"}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{"/synthetic/wt/second", "/synthetic/wt/launch", "/synthetic/wt/launch/apps/server"}
	if got := recentCwdsIn(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := recentCwdsIn(filepath.Join(t.TempDir(), "missing.jsonl")); got != nil {
		t.Fatalf("missing transcript: %v", got)
	}
}
