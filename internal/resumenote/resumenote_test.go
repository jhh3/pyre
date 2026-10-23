package resumenote

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestIDsStableAndValid(t *testing.T) {
	id := MessageID("ses_synth-a", "run_01JSYNTH")
	if id != "msg_resumed-synth-a-01JSYNTH" || id != MessageID("ses_synth-a", "run_01JSYNTH") {
		t.Fatalf("message ID = %q", id)
	}
	if !contracts.ValidID(MessageID("ses_a b/c", "run_x+y"), contracts.PrefixMessage) {
		t.Fatal("message ID with odd characters is invalid")
	}
	if !contracts.ValidID(OperationID("ses_a", "run_b"), contracts.PrefixOperation) {
		t.Fatal("operation ID invalid")
	}
}

func TestAfterCrashText(t *testing.T) {
	died := time.Date(2026, 10, 20, 7, 34, 0, 0, time.UTC) // 12:34 AM PDT
	up := died.Add(9 * time.Minute)
	got := AfterCrash(died, up)
	for _, want := range []string{"Pyre restarted after a crash", "died at about Oct 20, 12:34 AM P", "resumed you at Oct 20, 12:43 AM P",
		"(watchers, Monitors, dev servers, tunnels, shells) is gone", "git status", "check in with it before continuing"} {
		if !strings.Contains(got, want) {
			t.Errorf("note lacks %q:\n%s", want, got)
		}
	}
	if s := AfterCrash(time.Time{}, up); strings.Contains(s, "died at") || !strings.Contains(s, "your previous process died and Pyre resumed you") {
		t.Errorf("unknown death time: %s", s)
	}
	if s := AfterReboot("server"); !strings.HasPrefix(s, "[from the owner] server has restarted") || !strings.Contains(s, lostBackground) {
		t.Errorf("reboot note: %s", s)
	}
}
