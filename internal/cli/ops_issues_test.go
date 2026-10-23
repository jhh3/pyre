package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// report_issue attaches the node's pyred log lines naming the run: found
// from $PYRE_RUN_DIR (<state>/runs/<run>), last n only, redacted.
func TestIssuePyredLogLines(t *testing.T) {
	state := t.TempDir()
	run := "run_01JSYNTHISSUE0000000000000"
	env := map[string]string{"PYRE_RUN_DIR": filepath.Join(state, "runs", run), "HOME": t.TempDir()}
	path := pyredLogPath(func(k string) string { return env[k] })
	if want := filepath.Join(state, "logs", "pyred.log"); path != want {
		t.Fatalf("path %s, want %s", path, want)
	}
	if got := pyredLogLines(path, run, 10); got != "" {
		t.Fatalf("no log: %q", got)
	}
	var b strings.Builder
	for i := 0; i < 15; i++ {
		b.WriteString("level=INFO msg=\"synthetic\" run=" + run + " n=" + string(rune('a'+i)) + "\n")
		b.WriteString("level=INFO msg=\"other\" run=run_01JSYNTHOTHER00000000000000\n")
	}
	b.WriteString("level=WARN msg=\"auth\" run=" + run + " token ghp_SYNTHETICsyntheticSYNTHETIC1234\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := pyredLogLines(path, run, 3)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "n=n") || strings.Contains(got, "OTHER") || strings.Contains(got, "ghp_SYNTHETIC") {
		t.Fatalf("lines:\n%s", got)
	}
}
