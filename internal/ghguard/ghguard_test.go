package ghguard

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestRewriteArgs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"run watch 123", "run watch 123 --interval 60"},
		{"run watch 123 --exit-status", "run watch 123 --exit-status --interval 60"},
		{"run watch 123 -i 5", "run watch 123 -i 5"},
		{"run watch 123 -i5", "run watch 123 -i5"},
		{"run watch --interval=90 123", "run watch --interval=90 123"},
		{"run watch 123 --interval 90", "run watch 123 --interval 90"},
		{"pr checks 7 --watch", "pr checks 7 --watch --interval 60"},
		{"pr checks 7 --watch=true -R o/r", "pr checks 7 --watch=true -R o/r --interval 60"},
		{"pr checks 7 --watch --interval 30", "pr checks 7 --watch --interval 30"},
		{"pr checks 7", "pr checks 7"}, // no --watch: one call, no interval flag
		{"run view 123", "run view 123"},
		{"api repos/o/r/commits/abc/check-runs", "api repos/o/r/commits/abc/check-runs"},
		{"run watch 123 -- x", "run watch 123 --interval 60 -- x"},
		{"", ""},
	}
	for _, c := range cases {
		got := strings.Join(RewriteArgs(strings.Fields(c.in)), " ")
		if got != c.want {
			t.Errorf("RewriteArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExemptAndCommand(t *testing.T) {
	for _, in := range []string{"", "--version", "version", "help pr", "pr view --help", "pr -h", "api rate_limit", "api /rate_limit", "config get git_protocol", "completion -s zsh"} {
		if !Exempt(strings.Fields(in)) {
			t.Errorf("Exempt(%q) = false", in)
		}
	}
	for _, in := range []string{"api repos/o/r/commits/abc/check-runs", "pr view 3", "run watch 1", "api graphql -f query=x", "auth status"} {
		if Exempt(strings.Fields(in)) {
			t.Errorf("Exempt(%q) = true", in)
		}
	}
	if got := Command(strings.Fields("api repos/o/r/pulls --paginate")); got != "api" {
		t.Errorf("Command(api ...) = %q", got)
	}
	if got := Command(strings.Fields("pr view 3 --json state")); got != "pr view" {
		t.Errorf("Command(pr view) = %q", got)
	}
}

func TestIsShimName(t *testing.T) {
	for _, n := range []string{"gh", "/x/ghshim/gh", "gh.exe", "GH.EXE"} {
		if !IsShimName(n) {
			t.Errorf("IsShimName(%q) = false", n)
		}
	}
	for _, n := range []string{"pyre", "/x/pyre", "ghx", "pyre.exe"} {
		if IsShimName(n) {
			t.Errorf("IsShimName(%q) = true", n)
		}
	}
}

// A tight sequential loop gets its burst, then is slowed to PerMinute;
// parallel calls beyond MaxWait of borrowed tokens are refused.
func TestLimiterThrottlesThenRefuses(t *testing.T) {
	l := NewLimiter()
	now := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	for i := 0; i < DefaultBurst; i++ {
		if d := l.Take("ses_a", "run_a", now); d.Wait != 0 || d.Refused {
			t.Fatalf("burst call %d: %+v", i, d)
		}
	}
	// Sequential: each call after the burst waits ~2 s (30/min).
	seq := now
	for i := 0; i < 10; i++ {
		d := l.Take("ses_a", "run_a", seq)
		if d.Refused || d.Wait < 1900*time.Millisecond || d.Wait > 2100*time.Millisecond {
			t.Fatalf("sequential call %d: %+v", i, d)
		}
		seq = seq.Add(d.Wait)
	}
	// Parallel at one instant: up to MaxWait of borrowing, then refusals.
	b := NewLimiter()
	for i := 0; i < DefaultBurst; i++ {
		b.Take("ses_b", "", now)
	}
	var waits []time.Duration
	refused := 0
	for i := 0; i < 10; i++ {
		d := b.Take("ses_b", "", now)
		if d.Refused {
			refused++
			if d.LastMinute < DefaultBurst {
				t.Fatalf("refusal counts %d calls in the last minute", d.LastMinute)
			}
			continue
		}
		waits = append(waits, d.Wait)
	}
	if len(waits) != 5 || waits[4] != DefaultMaxWait || refused != 5 {
		t.Fatalf("parallel: waits %v refused %d", waits, refused)
	}
	// Other sessions are unaffected.
	if d := b.Take("ses_c", "", now); d.Wait != 0 || d.Refused {
		t.Fatalf("other session: %+v", d)
	}
	// Tokens refill: a minute later 30 calls go straight through.
	later := now.Add(time.Minute + DefaultMaxWait)
	for i := 0; i < 30; i++ {
		if d := b.Take("ses_b", "", later); d.Wait != 0 || d.Refused {
			t.Fatalf("refilled call %d: %+v", i, d)
		}
	}
	top, total := b.Top(later, 5)
	if len(top) != 2 || top[0].SessionID != "ses_b" || top[0].Calls != DefaultBurst+5+30 || top[0].Throttled != 5 || top[0].Refused != 5 || total != DefaultBurst+5+30+1 {
		t.Fatalf("top %+v total %d", top, total)
	}
	// An hour on, the counts are gone.
	if top, total := b.Top(later.Add(2*time.Hour), 5); len(top) != 0 || total != 0 {
		t.Fatalf("after an hour: %+v %d", top, total)
	}
}

func TestLimiterWarnEvery(t *testing.T) {
	l := NewLimiter()
	now := time.Now()
	if !l.Warn("s", now, 5*time.Minute) || l.Warn("s", now.Add(time.Minute), 5*time.Minute) || !l.Warn("s", now.Add(6*time.Minute), 5*time.Minute) {
		t.Fatal("warn cadence")
	}
}

func TestFindRealSkipsShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root := t.TempDir()
	self := filepath.Join(root, "bin", "pyre")
	shim := filepath.Join(root, "ghshim")
	dup := filepath.Join(root, "dup") // another link to pyre, not the shim dir
	real := filepath.Join(root, "real")
	for _, d := range []string{filepath.Dir(self), shim, dup, real} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "gh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{shim, dup} {
		if err := os.Symlink(self, filepath.Join(d, "gh")); err != nil {
			t.Fatal(err)
		}
	}
	path := strings.Join([]string{shim, dup, filepath.Join(root, "missing"), real}, string(os.PathListSeparator))
	got, err := FindReal(path, shim, self)
	if err != nil || got != filepath.Join(real, "gh") {
		t.Fatalf("FindReal = %q, %v", got, err)
	}
	if _, err := FindReal(strings.Join([]string{shim, dup}, string(os.PathListSeparator)), shim, self); err == nil {
		t.Fatal("found a gh with only the shim on PATH")
	}
}

func TestRateLimitAndFleetLines(t *testing.T) {
	now := time.Date(2026, 11, 3, 22, 50, 0, 0, time.UTC) // 2:50 PM PT
	b, err := RateLimit([]byte(`{"resources":{"core":{"limit":5000,"used":4200,"remaining":800,"reset":1793833260},
		"graphql":{"limit":5000,"used":10,"remaining":4990,"reset":1793833260}},"rate":{}}`), now)
	if err != nil || b.Core.Remaining != 800 || b.GraphQL.Remaining != 4990 || b.Core.Reset.IsZero() || !b.Low() {
		t.Fatalf("RateLimit = %+v, %v", b, err)
	}
	b.TopSessions = []contracts.GitHubCaller{{SessionID: "ses_x", Calls: 400, Throttled: 30, Refused: 2}}
	old := contracts.GitHubBudget{Core: contracts.GitHubLimit{Limit: 5000, Remaining: 5000}, ObservedAt: now.Add(-time.Hour),
		TopSessions: []contracts.GitHubCaller{{SessionID: "ses_y", Calls: 9}}}
	lines := FleetLines([]contracts.Node{{Name: "old", Resources: contracts.NodeResources{GitHub: &old}},
		{Name: "server", Resources: contracts.NodeResources{GitHub: &b}}, {Name: "none"}}, 5)
	want := []string{"core 800/5000 left", "graphql 4990/5000", "sampled on server at 2:50 PM PT", "LOW",
		"ses_x on server 400 (30 delayed, 2 refused); ses_y on old 9"}
	all := strings.Join(lines, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("FleetLines missing %q:\n%s", w, all)
		}
	}
	if FleetLines([]contracts.Node{{Name: "none"}}, 5) != nil {
		t.Error("lines without any budget")
	}
	if !slices.ContainsFunc(lines, func(s string) bool { return strings.HasPrefix(s, "GitHub API budget") }) {
		t.Error("no budget line")
	}
}
