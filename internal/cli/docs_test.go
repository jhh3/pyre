package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/integrations/hive/wsimport"
)

// splitShell splits a command line with double-quote support (enough for
// the README examples).
func splitShell(t *testing.T, line string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for _, r := range line {
		switch {
		case r == '"':
			inQ, have = !inQ, true
		case r == ' ' && !inQ:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if inQ {
		t.Fatalf("unbalanced quotes: %s", line)
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// TestReadmeExamples runs every documented example against the mock hub.
func TestReadmeExamples(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "agents", "tools", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(b), "Every line below runs against the mock")
	if !ok {
		t.Fatal("README examples marker missing")
	}
	block := after[strings.Index(after, "```sh")+5:]
	block = block[:strings.Index(block, "```")]
	h := newHub(t)
	e := newEnv(t, h)
	n := 0
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pyre ") {
			continue
		}
		n++
		args := splitShell(t, strings.TrimPrefix(line, "pyre "))
		if r := run(t, e, nil, args...); r.code != 0 {
			t.Errorf("README example failed: %s\n%s", line, r)
		}
	}
	if n < 15 {
		t.Fatalf("only %d examples found", n)
	}
}

func TestMigrateWsMailDryRun(t *testing.T) {
	src := filepath.Join("..", "..", "integrations", "hive", "testdata", "ws")
	if _, err := os.Stat(src); err != nil {
		t.Skip("ws testdata not present")
	}
	root := filepath.Join(t.TempDir(), "ws-copy")
	if _, err := wsimport.CopyTree(src, root); err != nil {
		t.Fatal(err)
	}
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "migrate", "ws-mail", "--root", root)
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "DRY RUN") {
		t.Fatalf("migrate must default to a dry run: %s", r)
	}
	expectCode(t, run(t, e, nil, "migrate", "ws-mail"), ExitUsage)
}
