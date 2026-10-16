//go:build windows

package diagnostics

import (
	"time"

	"github.com/jhh3/pyre/internal/sysstats"
)

// selfCPU is this process's kernel+user time (GetProcessTimes).
func selfCPU() (time.Duration, error) {
	cpu, _, err := sysstats.NativeSelf()
	return cpu, err
}
