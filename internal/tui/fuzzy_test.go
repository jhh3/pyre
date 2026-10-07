package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func rankStrings(q string, cands []string) []string {
	type sc struct {
		s     string
		score int
	}
	var out []sc
	for _, c := range cands {
		if s, _, ok := fuzzyMatch(q, c, false); ok {
			out = append(out, sc{c, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	res := make([]string, len(out))
	for i, o := range out {
		res[i] = o.s
	}
	return res
}

// Ordering cases mirror fzf's behaviour (prefix and word-start matches,
// consecutive runs, camelCase, delimiters, gaps).
func TestFuzzyOrdering(t *testing.T) {
	cases := []struct {
		q    string
		in   []string
		want []string
	}{
		{"work", []string{"network", "w o r k", "work log"}, []string{"work log", "w o r k", "network"}},
		{"dp", []string{"dumpster", "deploy prep"}, []string{"deploy prep", "dumpster"}},
		{"gb", []string{"garbage", "gitBranch"}, []string{"gitBranch", "garbage"}},
		{"app", []string{"happy", "internal/app"}, []string{"internal/app", "happy"}},
		{"oac", []string{"outfit API client", "coordinator"}, []string{"outfit API client"}},
		{"stale", []string{"◌ stale · synth-linux", "status: late"}, []string{"◌ stale · synth-linux", "status: late"}},
		{"xyz", []string{"coordinator", "worker"}, nil},
	}
	for _, c := range cases {
		got := rankStrings(c.q, c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q over %q: got %q want %q", c.q, c.in, got, c.want)
		}
	}
}

func TestFuzzySmartCaseAndPositions(t *testing.T) {
	if _, _, ok := fuzzyMatch("API", "rapid", false); ok {
		t.Error("upper-case pattern must match case-sensitively")
	}
	if _, _, ok := fuzzyMatch("api", "outfit API client", false); !ok {
		t.Error("lower-case pattern must ignore case")
	}
	_, pos, ok := fuzzyMatch("dp", "deploy prep", true)
	if !ok || !reflect.DeepEqual(pos, []int{0, 7}) {
		t.Errorf("positions = %v (want word starts [0 7])", pos)
	}
	_, pos, _ = fuzzyMatch("work", "migration work", true)
	if !reflect.DeepEqual(pos, []int{10, 11, 12, 13}) {
		t.Errorf("consecutive positions = %v", pos)
	}
}

func TestFilterMatchIsStrict(t *testing.T) {
	if filterOK("linux", "deploy prep", "claude", "synth-mac", "approval", "/Users/synth/work/deploy") {
		t.Error("scattered letters must not pass the list filter")
	}
	if !filterOK("linux", "worker", "claude", "synth-linux") {
		t.Error("substring in a secondary field must match")
	}
	if !filterOK("oac", "outfit API client") {
		t.Error("word-start acronym must match")
	}
	if !filterOK("api linux", "outfit API client", "synth-linux") {
		t.Error("every term may match a different field")
	}
}

func TestParseKeySeq(t *testing.T) {
	cases := map[string][]string{
		"<leader><leader>": {"<leader>", "<leader>"},
		"<leader>s":        {"<leader>", "s"},
		"gg":               {"g", "g"},
		"g g":              {"g", "g"},
		"ctrl+d":           {"ctrl+d"},
		"<c-d>":            {"ctrl+d"},
		"<s-tab>":          {"shift+tab"},
		"<cr>":             {"enter"},
		"shift+enter":      {"shift+enter"},
		"G":                {"G"},
		"space":            {"space"},
	}
	for in, want := range cases {
		got, err := parseKeySeq(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseKeySeq(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "hyperduper+x", "<nope>", "ctrl+banana"} {
		if _, err := parseKeySeq(bad); err == nil {
			t.Errorf("parseKeySeq(%q) should fail", bad)
		}
	}
}

func TestKeysTOMLOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.toml")
	body := `# synthetic test config
leader = ","
jk_escape = false
which_key_delay_ms = 50

[keys]
"sessions.attach" = ["o", "enter"]   # trailing comment
"nav.down" = [
  "ctrl+n",
  "<c-j>",
]
"nav.up" = []
"no.such" = "x"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	km, err := LoadKeymap(path)
	if err != nil {
		t.Fatal(err)
	}
	if km.Leader != "," || km.JKEscape || km.WhichKey != 50*time.Millisecond {
		t.Fatalf("settings not applied: %+v", km)
	}
	if a, _ := km.lookup([]string{"sessions", "nav", "global"}, []string{"o"}); a != "sessions.attach" {
		t.Errorf("o -> %q", a)
	}
	if a, _ := km.lookup([]string{"sessions", "nav", "global"}, []string{"j"}); a != "" {
		t.Errorf("j should be unbound after override, got %q", a)
	}
	if a, _ := km.lookup([]string{"nav"}, []string{"ctrl+j"}); a != "nav.down" {
		t.Errorf("<c-j> -> %q", a)
	}
	if a, more := km.lookup([]string{"global"}, []string{","}); a != "" || !more {
		t.Errorf("leader prefix: exact=%q more=%v", a, more)
	}
	if a, _ := km.lookup([]string{"global"}, []string{",", ","}); a != "global.finder" {
		t.Errorf(",, -> %q", a)
	}
	if len(km.Warnings) != 1 || !strings.Contains(km.Warnings[0], "no.such") {
		t.Errorf("warnings = %v", km.Warnings)
	}
	if ref := km.Reference(); !strings.Contains(ref, "o / enter") || !strings.Contains(ref, "(unbound)") {
		t.Errorf("reference does not show effective keys:\n%s", ref)
	}
	// Missing file: defaults, nothing created.
	missing := filepath.Join(dir, "absent.toml")
	if _, err := LoadKeymap(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("LoadKeymap must not create the file")
	}
	// Syntax error: defaults plus the error.
	os.WriteFile(path, []byte("[keys]\n\"nav.down\" = j\n"), 0o600)
	if km, err := LoadKeymap(path); err == nil || km.Leader != "space" {
		t.Errorf("bad file: err=%v leader=%q", err, km.Leader)
	}
}
