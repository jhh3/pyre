package claudeplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The owner's seven agent slash commands (2026-10-13).
var wantSkills = []string{"brief", "cleanup", "deliver", "kickoff", "open-items", "pair", "ship"}

func TestSkillsFrontmatter(t *testing.T) {
	var names []string
	for _, s := range Skills() {
		names = append(names, s.Name)
		if !strings.HasSuffix(s.Short, ".") || strings.Contains(s.Short, "\n") || len(s.Short) > 200 {
			t.Errorf("%s: short line %q", s.Name, s.Short)
		}
		if !strings.Contains(s.Description, "/"+s.Name) {
			t.Errorf("%s: description does not name /%s (model invocation cue)", s.Name, s.Name)
		}
		if !strings.HasPrefix(s.Usage, "/"+s.Name) {
			t.Errorf("%s: usage %q", s.Name, s.Usage)
		}
		if strings.Contains(s.Body, "$ARGUMENTS") != (s.ArgumentHint != "") {
			t.Errorf("%s: $ARGUMENTS use and argument-hint disagree", s.Name)
		}
	}
	if strings.Join(names, ",") != strings.Join(wantSkills, ",") {
		t.Fatalf("skills %v, want %v", names, wantSkills)
	}
	if s, ok := LookupSkill("/pyre-session:ship"); !ok || s.Usage != "/ship [PR or branch]" {
		t.Fatalf("lookup: %+v %v", s, ok)
	}
}

func TestParseSkillRejectsMalformed(t *testing.T) {
	for _, in := range []string{"no frontmatter", "---\nname: x\n", "---\nname: x\n---\nbody", "---\ndescription: d\n---\n", "---\nname x\n---\n"} {
		if _, err := ParseSkill([]byte(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
	s, err := ParseSkill([]byte("---\nname: x\ndescription: \"One. Two.\"\nargument-hint: \"[a]\"\n---\nbody\n"))
	if err != nil || s.Short != "One." || s.Usage != "/x [a]" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestMaterializeSkills(t *testing.T) {
	cfg := t.TempDir()
	// The owner's own skill with a Pyre name, and an unrelated one.
	own := filepath.Join(cfg, "skills", "ship")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("synthetic owner ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(cfg, "skills", "retired", "SKILL.md")
	_ = os.MkdirAll(filepath.Dir(other), 0o700)
	if err := os.WriteFile(other, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}

	skipped, err := MaterializeSkills(cfg, "v-test")
	if err != nil || strings.Join(skipped, ",") != "ship" {
		t.Fatalf("skipped %v, err %v", skipped, err)
	}
	for _, name := range wantSkills {
		d := filepath.Join(cfg, "skills", name)
		b, err := os.ReadFile(filepath.Join(d, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if name == "ship" {
			if string(b) != "synthetic owner ship" {
				t.Fatalf("the owner's skill was rewritten: %q", b)
			}
			if _, err := os.Stat(filepath.Join(d, ManagedMarker)); err == nil {
				t.Fatal("the owner's skill got a marker")
			}
			continue
		}
		if s, err := ParseSkill(b); err != nil || s.Name != name {
			t.Fatalf("%s: %+v %v", name, s, err)
		}
		m, err := os.ReadFile(filepath.Join(d, ManagedMarker))
		if err != nil || !strings.HasPrefix(string(m), "pyre v-test\nsha256 ") {
			t.Fatalf("%s marker %q %v", name, m, err)
		}
	}
	if b, _ := os.ReadFile(other); string(b) != "synthetic" {
		t.Fatal("an unrelated skill was touched")
	}

	// Identical content: no rewrite, even for a new Pyre version.
	brief := filepath.Join(cfg, "skills", "brief", "SKILL.md")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(brief, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeSkills(cfg, "v-test2"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(brief); !fi.ModTime().Equal(old) {
		t.Fatalf("identical skill rewritten (mtime %v)", fi.ModTime())
	}

	// Changed content (an older Pyre's text): updated.
	if err := os.WriteFile(brief, []byte("synthetic stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeSkills(cfg, "v-test3"); err != nil {
		t.Fatal(err)
	}
	s, _ := LookupSkill("brief")
	if b, _ := os.ReadFile(brief); string(b) != s.Body {
		t.Fatalf("stale skill not updated: %q", b)
	}
	if m, _ := os.ReadFile(filepath.Join(cfg, "skills", "brief", ManagedMarker)); !strings.HasPrefix(string(m), "pyre v-test3\n") {
		t.Fatalf("marker not updated: %q", m)
	}
	leftovers, _ := filepath.Glob(filepath.Join(cfg, "skills", ".*"))
	leftovers2, _ := filepath.Glob(filepath.Join(cfg, "skills", "*", ".*.tmp*"))
	if len(leftovers)+len(leftovers2) != 0 {
		t.Fatalf("temp files left: %v %v", leftovers, leftovers2)
	}
}

// TestMaterializeSkillsConcurrent: runs sharing a profile race safely.
func TestMaterializeSkillsConcurrent(t *testing.T) {
	for round := 0; round < 20; round++ {
		cfg := t.TempDir()
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				skipped, err := MaterializeSkills(cfg, "v-test")
				if err == nil && len(skipped) != 0 {
					err = fmt.Errorf("skipped %v", skipped)
				}
				errs[i] = err
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range Skills() {
			b, err := os.ReadFile(filepath.Join(cfg, "skills", s.Name, "SKILL.md"))
			if err != nil || string(b) != s.Body {
				t.Fatalf("%s: %v", s.Name, err)
			}
			if _, err := os.Stat(filepath.Join(cfg, "skills", s.Name, ManagedMarker)); err != nil {
				t.Fatal(err)
			}
		}
		if left, _ := filepath.Glob(filepath.Join(cfg, "skills", ".*")); len(left) != 0 {
			t.Fatalf("temp dirs left: %v", left)
		}
	}
}
