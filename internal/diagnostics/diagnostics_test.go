package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Synthetic, obviously fake credentials assembled at runtime so this file
// itself does not look like it contains secrets.
var (
	fakeNodeTok  = "pyn_" + strings.Repeat("A1b2", 11)
	fakeOwnerTok = "pyc_" + strings.Repeat("Zz09", 11)
	fakeGitHub   = "ghp_" + strings.Repeat("x", 36)
	fakeJWT      = "eyJ" + strings.Repeat("a", 12) + ".eyJ" + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12)
)

func TestScannerFindsSecretsNotMarkers(t *testing.T) {
	s := NewScanner()
	text := strings.Join([]string{
		"token=" + fakeNodeTok,
		"Authorization: Bearer " + fakeOwnerTok,
		`{"refresh_token":"rt-synthetic-0123456789"}`,
		"gh " + fakeGitHub,
		"jwt " + fakeJWT,
		"-----BEGIN EC PRIVATE KEY-----",
		"mail synth.user@example.com",
	}, "\n")
	got := map[string]int{}
	for _, f := range s.ScanString("t", text) {
		got[f.Rule]++
		if strings.Contains(f.String(), fakeNodeTok) || strings.Contains(f.Preview, fakeOwnerTok[4:]) {
			t.Fatalf("finding leaks the value: %v", f)
		}
	}
	for _, r := range []string{RulePyreToken, RuleBearer, RuleOAuth, RuleJWT, RulePrivateKey, RuleEmail} {
		if got[r] == 0 {
			t.Errorf("rule %s found nothing in %v", r, got)
		}
	}
	clean := "pyn_[REDACTED] Bearer [REDACTED] realm=pyre node_01K0000000000000000000000 ver_01K00 " +
		`WWW-Authenticate: Bearer realm="pyre"`
	if f := s.ScanString("clean", clean); len(f) != 0 {
		t.Fatalf("false positives on redaction markers/IDs: %v", f)
	}
}

func TestScannerKnownValuesAndAllow(t *testing.T) {
	s := NewScanner().AddKnown("ticket", "opaque-synthetic-value-42").Allow(`@example\.com$`)
	f := s.ScanString("x", "a opaque-synthetic-value-42 b opaque-synthetic-value-42 c me@example.com")
	if len(f) != 2 || !strings.HasPrefix(f[0].Rule, RuleKnown) {
		t.Fatalf("findings = %v", f)
	}
	s.IgnorePII = true
	if f := s.ScanString("y", "x@corp.test.org"); len(f) != 0 {
		t.Fatalf("pii not ignored: %v", f)
	}
}

func TestRedact(t *testing.T) {
	in := "Bearer opaque.Value-0123456789 Bearer " + fakeOwnerTok + " node " + fakeNodeTok + ` {"access_token":"abcdefghijkl"} ` + fakeGitHub + " u@corp.example.org"
	out := Redact(in)
	if f := NewScanner().ScanString("redacted", out); len(f) != 0 {
		t.Fatalf("redacted text still has findings %v: %q", f, out)
	}
	for _, want := range []string{"Bearer [REDACTED]", "Bearer pyc_[REDACTED]", "pyn_[REDACTED]", `"access_token":"[REDACTED]"`, "[EMAIL]"} {
		if !strings.Contains(out, want) {
			t.Errorf("redacted output %q lacks %q", out, want)
		}
	}
}

func TestScanSQLiteAndDir(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "x.db")
	if err := os.WriteFile(db, []byte("header\x00\x01"+fakeNodeTok+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-wal", []byte("wal "+fakeOwnerTok), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewScanner()
	f, err := s.ScanSQLite(db)
	if err != nil || len(Secrets(f)) != 2 {
		t.Fatalf("sqlite scan = %v, %v", f, err)
	}
	f, err = s.ScanDir(dir)
	if err != nil || len(f) != 2 {
		t.Fatalf("dir scan = %v, %v", f, err)
	}
}

func TestRingBoundsAndCounts(t *testing.T) {
	r := NewRing(3, 1<<10, Redact)
	logger := slog.New(slog.NewTextHandler(r, nil))
	for i := 0; i < 10; i++ {
		logger.Info("line", "i", i, "tok", fakeNodeTok)
	}
	lines := r.Lines()
	if len(lines) != 3 || !strings.Contains(lines[2], "i=9") {
		t.Fatalf("lines = %q", lines)
	}
	if strings.Contains(r.String(), fakeNodeTok) {
		t.Fatal("ring retained an unredacted token")
	}
	st := r.Stats()
	if st.TotalLines != 10 || st.Dropped != 7 || st.TotalBytes == 0 {
		t.Fatalf("stats = %+v", st)
	}
	// partial lines
	r.Reset()
	_, _ = r.Write([]byte("abc"))
	_, _ = r.Write([]byte("def\nghi\n"))
	if got := r.Lines(); len(got) != 2 || got[0] != "abcdef" {
		t.Fatalf("partial handling = %q", got)
	}
	// byte bound
	r2 := NewRing(100, 10, nil)
	r2.Add("0123456789")
	r2.Add("abc")
	if got := r2.Lines(); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("byte bound = %q", got)
	}
}

func TestSampleSelfAndProcess(t *testing.T) {
	s1, err := SampleSelf()
	if err != nil {
		t.Fatal(err)
	}
	if s1.RSSBytes <= 0 || s1.PID != os.Getpid() {
		t.Fatalf("self sample = %+v", s1)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	x := 0
	for time.Now().Before(deadline) {
		x++
	}
	_ = x
	s2, err := SampleSelf()
	if err != nil {
		t.Fatal(err)
	}
	if pct := CPUPercent(s1, s2); pct <= 10 {
		t.Fatalf("busy loop measured %.1f%% CPU", pct)
	}
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start sleep:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	p, err := SampleProcess(cmd.Process.Pid)
	if err != nil || p.RSSBytes <= 0 {
		t.Fatalf("child sample = %+v, %v", p, err)
	}
	tree, err := SampleTree(os.Getpid())
	if err != nil || tree.Procs < 2 || tree.RSSBytes < p.RSSBytes {
		t.Fatalf("tree sample = %+v, %v", tree, err)
	}
}

func TestParsePSTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:01.50":     1500 * time.Millisecond,
		"01:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"1-00:00:01":  24*time.Hour + time.Second,
		"12:34":       12*time.Minute + 34*time.Second,
		"00:00:00.25": 250 * time.Millisecond,
	} {
		got, err := parsePSTime(in)
		if err != nil || got != want {
			t.Errorf("parsePSTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestBuilderIsRedacted(t *testing.T) {
	b := NewBuilder("node_synthetic", 5)
	b.AddKnownSecret("opaque-node-secret-123")
	b.SetLimit("max_outbox", 100000)
	b.SetQueue("outbox", func(context.Context) (int, error) { return 7, nil })
	b.SetQueue("broken", func(context.Context) (int, error) {
		return 0, errors.New("db error near " + fakeNodeTok)
	})
	b.MaxRuns = 4
	b.ActiveRuns = func() int { return 2 }
	b.RecordError("dial hub: Authorization: Bearer " + fakeOwnerTok + " opaque-node-secret-123")
	for i := 0; i < 10; i++ {
		b.RecordError(fmt.Sprintf("e%d", i))
	}
	d, err := b.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	if f := NewScanner().AddKnown("node", "opaque-node-secret-123").ScanBytes("diag", raw); len(f) != 0 {
		t.Fatalf("diagnostics leak: %v\n%s", f, raw)
	}
	if d.Queues["outbox"] != 7 || d.Queues["broken"] != -1 || d.Limits["max_outbox"] != 100000 {
		t.Fatalf("queues/limits = %+v %+v", d.Queues, d.Limits)
	}
	if len(d.Errors) != 6 { // 5 retained + broken queue error
		t.Fatalf("errors = %q", d.Errors)
	}
	if d.Resources.CPUCount <= 0 || d.Resources.PyreRSSMB <= 0 || d.Resources.ActiveRuns != 2 || d.Resources.MaxRuns != 4 {
		t.Fatalf("resources = %+v", d.Resources)
	}
	out, err := b.Handler()(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.(contracts.Diagnostics); !ok {
		t.Fatalf("handler returned %T", out)
	}
}

func TestParseVMStat(t *testing.T) {
	out := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:                               1000.\nPages active:                             5.\nPages inactive:                           1000.\nPages speculative:                        48.\n"
	if got := parseVMStat(out); got != (2048*16384)>>20 {
		t.Fatalf("parseVMStat = %d", got)
	}
}
