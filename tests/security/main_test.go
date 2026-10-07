package security

import (
	"os"
	"testing"

	"github.com/jhh3/pyre/tests/resilience/stack"
)

// TestMain lets the test binary act as the shell-capable test daemon
// (stack.RunTestDaemon) for real-stack scenarios.
func TestMain(m *testing.M) {
	if os.Getenv(stack.RoleEnv) == stack.RoleDaemon {
		os.Exit(stack.RunTestDaemon(os.Args[1:]))
	}
	code := m.Run()
	stack.Cleanup()
	os.Exit(code)
}
