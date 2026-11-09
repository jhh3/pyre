package contracts

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func repoRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestNewIDSortableUniquePrefixed(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 5000; i++ {
		id := NewID(PrefixMessage)
		if !ValidID(id, PrefixMessage) || len(id) != len("msg_")+26 {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate %s", id)
		}
		if prev != "" && id <= prev {
			t.Fatalf("not monotonic: %s <= %s", id, prev)
		}
		seen[id] = true
		prev = id
	}
	a := "x_" + newULID(time.UnixMilli(1))
	b := "x_" + newULID(time.UnixMilli(2))
	if !(a < b) {
		t.Fatalf("time order: %s %s", a, b)
	}
	if IDPrefix("ses_abc") != "ses" || IDPrefix("nope") != "" {
		t.Fatal("IDPrefix")
	}
	if ValidID("ses_../etc", PrefixSession) {
		t.Fatal("path chars must be invalid")
	}
}

func TestIDTimeAndDerivedID(t *testing.T) {
	// The real incident's operation and message IDs decode to their mint times.
	if at, ok := IDTime("op_…"); !ok || !at.Equal(time.Date(2026, 10, 23, 22, 58, 2, 976e6, time.UTC)) {
		t.Fatalf("IDTime = %v %v", at, ok)
	}
	for _, id := range []string{"op_enc-r1-reply-1", "msg_legacy-ws-1", "", "op_01M5NFRM30PSGM2XGCTV96X17!"} {
		if _, ok := IDTime(id); ok {
			t.Fatalf("IDTime(%q) should not decode", id)
		}
	}
	now := time.UnixMilli(time.Now().UnixMilli()).UTC()
	a := DerivedID(PrefixMessage, now, "k")
	if a != DerivedID(PrefixMessage, now, "k") || a == DerivedID(PrefixMessage, now, "k2") || !ValidID(a, PrefixMessage) || len(a) != len("msg_")+26 {
		t.Fatalf("DerivedID not deterministic, distinct and well-formed: %s", a)
	}
	if at, ok := IDTime(a); !ok || !at.Equal(now) {
		t.Fatalf("DerivedID time = %v, want %v", at, now)
	}
	if z := DerivedID(PrefixMessage, time.Time{}, "k"); !strings.HasPrefix(z, "msg_0000000000") {
		t.Fatalf("zero time = %s", z)
	}
}

func TestErrorMapping(t *testing.T) {
	e := Errorf(ErrUnavailable, "hub down")
	if !e.Retryable || e.Code.HTTPStatus() != 503 {
		t.Fatal("unavailable mapping")
	}
	if !IsCode(e, ErrUnavailable) || CodeOf(nil) != "" {
		t.Fatal("IsCode")
	}
	b, _ := json.Marshal(ErrorBody{Error: Errorf(ErrConflict, "x").WithRef(KindMessage, "msg_1")})
	if !strings.Contains(string(b), `"code":"conflict"`) {
		t.Fatal(string(b))
	}
}

func TestFixturesDecodeStrictly(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "tests", "fixtures", "contracts")
	bundles, err := LoadFixtureDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) < 10 {
		t.Fatalf("expected >=10 fixture bundles, got %d", len(bundles))
	}
	for _, b := range bundles {
		for _, x := range b.Exchanges {
			if x.Response == nil && x.Error == nil {
				t.Errorf("%s/%s: exchange needs response or error", b.Name, x.Name)
			}
		}
	}
}

func TestProviderNeverCrosses(t *testing.T) {
	if ProviderFor(HarnessClaude) == ProviderFor(HarnessCodex) {
		t.Fatal("providers must differ")
	}
	if ProviderFor(HarnessShell) != "" {
		t.Fatal("shell has no provider")
	}
}

func TestEvidenceClassificationV16(t *testing.T) {
	if !AcceptanceEvidence(EvidenceClaudeInboxRead) || !ConsumptionEvidence(EvidenceClaudeInboxRead) || !PullEvidence(EvidenceClaudeInboxRead) {
		t.Fatal("inbox read must be acceptance, consumption and pull evidence")
	}
	for _, e := range []EvidenceCategory{EvidenceClaudeMonitorNotified, EvidenceTerminalWrite, EvidenceNone, "unknown.kind"} {
		if AcceptanceEvidence(e) || ConsumptionEvidence(e) || PullEvidence(e) {
			t.Fatalf("%s must never establish acceptance", e)
		}
	}
	if PullEvidence(EvidenceClaudePromptHook) || ConsumptionEvidence(EvidenceClaudeQueue) {
		t.Fatal("push evidence classified as pull/consumption")
	}
	got := WithoutCaps([]string{CapInputQueue, CapInputMonitor, CapStop}, " input.monitor ,x")
	if len(got) != 2 || HasCap(got, CapInputMonitor) {
		t.Fatalf("WithoutCaps = %v", got)
	}
	if len(WithoutCaps([]string{CapStop}, "")) != 1 {
		t.Fatal("empty removal list changed caps")
	}
}
