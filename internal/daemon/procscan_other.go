//go:build !linux && !darwin

package daemon

import "time"

// Process scans are not implemented here (Windows runs have no leftover
// background shells to find this way); cleanup relies on live runs only.
func processCwds() []string                         { return nil }
func sessionProcesses(string, []int) []int          { return nil }
func stopProcesses(pids []int, _ time.Duration) int { return len(pids) }

// openPathsUnder cannot tell here: run temp folders are kept.
func openPathsUnder([]string) ([]string, bool) { return nil, false }
