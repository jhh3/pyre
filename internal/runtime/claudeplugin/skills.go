package claudeplugin

// Agent slash commands: Claude Code skills (skills/<name>/SKILL.md) that
// the Claude adapter writes into the session's config dir before launch
// (MaterializeSkills), so each Pyre-launched Claude session has /kickoff,
// /ship, ... by bare name (plugin skills and commands are only reachable as
// /<plugin>:<name>, Claude Code 2.1.296). The embedded files are the single
// source: `pyre commands` and the TUI help read their frontmatter.

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed skills/*/SKILL.md
var skillFS embed.FS

// Skill is one embedded skill's frontmatter plus its file.
type Skill struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argument_hint,omitempty"`
	Short        string `json:"short"` // the description's first sentence
	Usage        string `json:"usage"` // "/name [args]"
	Body         string `json:"-"`     // the whole SKILL.md
}

// Skills returns the embedded skills sorted by name (parsed once; do not
// modify the result). It panics on a malformed file: they are compiled in
// and covered by tests.
var Skills = sync.OnceValue(loadSkills)

func loadSkills() []Skill {
	dirs, err := fs.ReadDir(skillFS, "skills")
	if err != nil {
		panic(err)
	}
	var out []Skill
	for _, d := range dirs {
		b, err := skillFS.ReadFile("skills/" + d.Name() + "/SKILL.md")
		if err != nil {
			panic(err)
		}
		s, err := ParseSkill(b)
		if err != nil {
			panic(fmt.Sprintf("claudeplugin: skills/%s: %v", d.Name(), err))
		}
		if s.Name != d.Name() {
			panic(fmt.Sprintf("claudeplugin: skills/%s is named %q", d.Name(), s.Name))
		}
		out = append(out, s)
	}
	return out
}

// LookupSkill returns the embedded skill name (a leading "/" or
// "pyre-session:" is accepted).
func LookupSkill(name string) (Skill, bool) {
	name = strings.TrimPrefix(strings.TrimPrefix(name, "/"), Name+":")
	for _, s := range Skills() {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// ParseSkill reads a SKILL.md's frontmatter: single-line "key: value"
// scalars between "---" lines (values optionally double-quoted). name and
// description are required.
func ParseSkill(b []byte) (Skill, error) {
	text := string(b)
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Skill{}, fmt.Errorf("no frontmatter")
	}
	head, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Skill{}, fmt.Errorf("unterminated frontmatter")
	}
	s := Skill{Body: text}
	for _, line := range strings.Split(head, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return Skill{}, fmt.Errorf("frontmatter line %q is not key: value", line)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		switch strings.TrimSpace(k) {
		case "name":
			s.Name = v
		case "description":
			s.Description = v
		case "argument-hint":
			s.ArgumentHint = v
		}
	}
	if s.Name == "" || s.Description == "" {
		return Skill{}, fmt.Errorf("frontmatter needs name and description")
	}
	s.Short = s.Description
	if i := strings.Index(s.Description, ". "); i > 0 {
		s.Short = s.Description[:i+1]
	}
	s.Usage = "/" + s.Name
	if s.ArgumentHint != "" {
		s.Usage += " " + s.ArgumentHint
	}
	return s, nil
}

// ManagedMarker marks a skill dir Pyre wrote (contents: Pyre version and
// the SKILL.md hash). A same-named skill dir without it is the owner's.
const ManagedMarker = ".pyre-managed"

// MaterializeSkills writes the embedded skills into
// <configDir>/skills/<name>/SKILL.md, each marked with ManagedMarker. A skill
// dir without the marker (the owner's own skill, or a symlink) is never
// touched: its name is returned in skipped. A marked dir is rewritten only
// when its SKILL.md differs. Nothing else is deleted. Profiles are shared by
// concurrent runs, so every write is a temp file or dir plus rename.
func MaterializeSkills(configDir, version string) (skipped []string, err error) {
	if configDir == "" {
		return nil, fmt.Errorf("claudeplugin: no Claude config dir")
	}
	root := filepath.Join(configDir, "skills")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	var errs []error
	for _, s := range Skills() {
		owner, err := materializeSkill(root, s, version)
		if owner {
			skipped = append(skipped, s.Name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("skill %s: %w", s.Name, err))
		}
	}
	return skipped, errors.Join(errs...)
}

func skillMarker(s Skill, version string) []byte {
	sum := sha256.Sum256([]byte(s.Body))
	return []byte("pyre " + version + "\nsha256 " + hex.EncodeToString(sum[:]) + "\n")
}

// materializeSkill installs one skill; owner reports a dir that is not
// Pyre's.
func materializeSkill(root string, s Skill, version string) (owner bool, err error) {
	d := filepath.Join(root, s.Name)
	if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
		// Fresh: build the whole dir aside and rename it in, so a crash or a
		// racing run never leaves an unmarked (owner-looking) dir behind.
		tmp, err := os.MkdirTemp(root, "."+s.Name+".pyre-tmp")
		if err != nil {
			return false, err
		}
		defer os.RemoveAll(tmp) // no-op once renamed
		if err := os.WriteFile(filepath.Join(tmp, "SKILL.md"), []byte(s.Body), 0o600); err != nil {
			return false, err
		}
		if err := os.WriteFile(filepath.Join(tmp, ManagedMarker), skillMarker(s, version), 0o600); err != nil {
			return false, err
		}
		if err := os.Rename(tmp, d); err == nil {
			return false, nil
		}
		// Lost a race (d now exists): fall through and check it.
	} else if err != nil {
		return false, err
	}
	if fi, err := os.Lstat(d); err != nil {
		return false, err
	} else if !fi.IsDir() {
		return true, nil
	}
	if _, err := os.Stat(filepath.Join(d, ManagedMarker)); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if cur, err := os.ReadFile(filepath.Join(d, "SKILL.md")); err == nil && bytes.Equal(cur, []byte(s.Body)) {
		return false, nil
	}
	if err := writeFileUnique(filepath.Join(d, "SKILL.md"), []byte(s.Body)); err != nil {
		return false, err
	}
	return false, writeFileUnique(filepath.Join(d, ManagedMarker), skillMarker(s, version))
}

// writeFileUnique is an atomic write whose temp name is unique, so
// concurrent writers do not trip over each other's temp file.
func writeFileUnique(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), path)
	}
	if werr != nil {
		os.Remove(f.Name())
	}
	return werr
}
