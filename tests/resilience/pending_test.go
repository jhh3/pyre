package resilience

import "testing"

// Scenarios whose system under test is not available as a black box yet.
// They are listed so `go test -v` reports them as SKIP (never as passes).
// See README.md for the planned assertions.

func TestPendingAdapterCrashPoints(t *testing.T) {
	t.Skip("PENDING cmd/pyred adapter wiring: cmd/pyred/adapters.go registers only the mock adapter, so claude/codex crash points " +
		"(around durable writes / native dispatch, isolated tmux sockets) cannot be driven through a real daemon yet; the shell adapter " +
		"is exercised through the test-binary daemon role (TestStackDaemonCrashRecoversLiveShellRun)")
}

func TestPendingAccountsThroughDaemon(t *testing.T) {
	t.Skip("PENDING integration wiring: provider-usage-unavailable and simultaneous account selection (A13) are covered in-package by " +
		"internal/accounts (TestUsagePollingBoundedAndRetryAfter, TestUsageProbeNeverRefreshes, TestStaleUsageIsNotFresh, " +
		"TestSimultaneousSelectionRace, TestCodexLeaseConflict — all passing on c8bc980); the black-box path through pyred " +
		"(account.select / account.observe node actions with a fake usage endpoint) needs pyred integrations configurable for an isolated HOME")
}

func TestPendingMachineSleep(t *testing.T) {
	t.Skip("PENDING: machine sleep (SIGSTOP of pyred for longer than the hub heartbeat timeout, then SIGCONT) — harness ready (stack.Proc.Signal); " +
		"assertion: stale-not-idle during the pause, no duplicate run on resume")
}
