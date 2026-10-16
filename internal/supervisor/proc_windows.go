//go:build windows

package supervisor

import (
	"time"

	"github.com/jhh3/pyre/internal/sysstats"
)

// cpuTime is this process's kernel+user time (GetProcessTimes).
func cpuTime() time.Duration {
	cpu, _, err := sysstats.NativeSelf()
	if err != nil {
		return 0
	}
	return cpu
}
