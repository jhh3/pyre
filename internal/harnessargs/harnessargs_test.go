package harnessargs

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// The Aide launch (Mac mini): development channel plugin plus an appended
// system prompt file, in the canonical "=" form.
func TestNormalizeAide(t *testing.T) {
	got, err := Normalize(contracts.HarnessClaude, []string{
		"--dangerously-load-development-channels", "plugin:sample-bridge@sample",
		"--append-system-prompt-file", "/Users/synthetic/Documents/sample/autonomy/aide.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--dangerously-load-development-channels=plugin:sample-bridge@sample",
		"--append-system-prompt-file=/Users/synthetic/Documents/sample/autonomy/aide.md",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	// Canonical input is a fixed point (a relaunch normalizes again).
	again, err := Normalize(contracts.HarnessClaude, got)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("not idempotent: %q %v", again, err)
	}
}

func TestNormalizeVariadicAndRepeat(t *testing.T) {
	got, err := Normalize(contracts.HarnessClaude, []string{
		"--add-dir", "/a", "/b", "--plugin-dir", "/p1", "--plugin-dir=/p2", "--channels", "server:x",
		"--append-system-prompt", "-starts with a dash",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--add-dir=/a", "--add-dir=/b", "--plugin-dir=/p1", "--plugin-dir=/p2", "--channels=server:x",
		"--append-system-prompt=-starts with a dash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		h    contracts.Harness
		args []string
		want string
	}{
		{contracts.HarnessClaude, []string{"--resume", "x"}, "controlled by Pyre"},
		{contracts.HarnessClaude, []string{"--session-id=x"}, "controlled by Pyre"},
		{contracts.HarnessClaude, []string{"--dangerously-skip-permissions"}, "controlled by Pyre"},
		{contracts.HarnessClaude, []string{"--permission-mode", "plan"}, "controlled by Pyre"},
		{contracts.HarnessClaude, []string{"--model", "opus"}, "controlled by Pyre"},
		{contracts.HarnessClaude, []string{"-p"}, "controlled by Pyre"},
		{contracts.HarnessClaude, []string{"--strict-mcp-config"}, "controlled by Pyre"},
		{contracts.HarnessClaude, []string{"--pyre-permissions=bypass"}, "controlled by Pyre"},
		{contracts.HarnessClaude, []string{"--verbose"}, "not allowed"},
		{contracts.HarnessClaude, []string{"hello"}, "not a flag"},
		{contracts.HarnessClaude, []string{"--add-dir"}, "needs a value"},
		{contracts.HarnessClaude, []string{"--append-system-prompt-file"}, "needs a value"},
		{contracts.HarnessClaude, []string{"--settings=/a", "--settings=/b"}, "given twice"},
		{contracts.HarnessClaude, []string{"--agents="}, "empty value"},
		{contracts.HarnessCodex, []string{"-c", "model=x"}, "no extra harness arguments"},
		{contracts.Harness("agy"), []string{"--foo"}, "no extra harness arguments"},
	}
	for _, c := range cases {
		_, err := Normalize(c.h, c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %q: err %v, want %q", c.h, c.args, err, c.want)
		}
		if err != nil && !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("%q: code %v", c.args, err)
		}
	}
}

func TestNormalizePassthroughAndBounds(t *testing.T) {
	if got, err := Normalize(contracts.HarnessShell, []string{"-c", "echo hi"}); err != nil || len(got) != 2 {
		t.Fatalf("shell: %q %v", got, err)
	}
	if got, err := Normalize(contracts.HarnessClaude, nil); err != nil || got != nil {
		t.Fatalf("empty: %q %v", got, err)
	}
	many := make([]string, MaxArgs+1)
	for i := range many {
		many[i] = "--add-dir=/x"
	}
	if _, err := Normalize(contracts.HarnessClaude, many); err == nil {
		t.Fatal("too many accepted")
	}
	if _, err := Normalize(contracts.HarnessClaude, []string{"--append-system-prompt=" + strings.Repeat("x", MaxBytes)}); err == nil {
		t.Fatal("too long accepted")
	}
}

func TestNormalizePathsAbsolute(t *testing.T) {
	for _, a := range [][]string{{"--append-system-prompt-file", "aide.md"}, {"--plugin-dir=~/p"}, {"--settings", `{"a":1}`}, {"--add-dir", "/ok", "rel"}} {
		if _, err := Normalize(contracts.HarnessClaude, a); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Errorf("%q: %v", a, err)
		}
	}
	if _, err := Normalize(contracts.HarnessClaude, []string{`--plugin-dir=C:\Users\synthetic\p`, `--mcp-config={"mcpServers":{}}`}); err != nil {
		t.Fatal(err)
	}
}
