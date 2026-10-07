package doing

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCommand(t *testing.T) {
	cases := map[string]string{
		"go test ./internal/...":                                      RunningTests,
		"cd /x/y && go -C /x test ./pkg -run Foo":                     RunningTests,
		"/bin/zsh -lc 'npm run test -- --watch=false'":                RunningTests,
		"./gradlew :app:testDebugUnitTest check":                      RunningTests,
		"pytest -q tests/":                                            RunningTests,
		"gh pr checks 42":                                             "running: gh pr checks 42",
		"cd /repo && git status --short":                              "running: git status --short",
		"GITHUB_TOKEN=abc123 gh api /user":                            "running: gh api /user",
		"curl -H 'Authorization: Bearer xyz' https://example.invalid": "running: curl -H",
		"/usr/local/bin/python3 tool.py":                              "running: python3 tool.py",
		"":                                                            "running a command",
	}
	for in, want := range cases {
		if got := Command(in); got != want {
			t.Errorf("Command(%q) = %q, want %q", in, got, want)
		}
	}
	long := Command("some-really-long-binary-name with many arguments that go on and on")
	if utf8.RuneCountInString(long) > MaxLen || !strings.HasSuffix(long, "…") {
		t.Errorf("long command not clipped: %q", long)
	}
	if got := Command("tool sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); strings.Contains(got, "sk-ant") {
		t.Errorf("opaque token leaked: %q", got)
	}
}

func TestClaudeTool(t *testing.T) {
	in := func(kv ...string) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"Edit", in("file_path", "/home/u/src/app/useCanvas.ts"), "editing useCanvas.ts"},
		{"Read", in("file_path", "/a/b/README.md"), "reading README.md"},
		{"Grep", in("pattern", "foo"), SearchingCode},
		{"Task", nil, Subagent},
		{"WebSearch", nil, Browsing},
		{"mcp__pyre__send_message", nil, "using pyre"},
		{"mcp__claude_ai_Claude_Docs__read", nil, "using Claude_Docs"},
		{"Bash", in("command", "go test ./..."), RunningTests},
	}
	for _, c := range cases {
		if got := ClaudeTool(c.name, c.input); got != c.want {
			t.Errorf("ClaudeTool(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInflight(t *testing.T) {
	var f Inflight
	if f.Phrase() != "" {
		t.Fatal("empty inflight has a phrase")
	}
	f.Start("a", "reading a.go")
	f.Start("b", "reading b.go")
	f.Start("c", "reading c.go")
	if got := f.Phrase(); got != "reading 3 files" {
		t.Fatalf("parallel reads = %q", got)
	}
	f.Done("a")
	f.Done("b")
	if got := f.Phrase(); got != "reading c.go" {
		t.Fatalf("one read left = %q", got)
	}
	f.Start("d", RunningTests)
	if got := f.Phrase(); got != RunningTests {
		t.Fatalf("latest = %q", got)
	}
	f.Clear()
	if f.Phrase() != "" {
		t.Fatal("clear left a phrase")
	}
}
