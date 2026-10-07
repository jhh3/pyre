package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// RuntimeStats are Go runtime counters of the current process. They make
// goroutine, heap and file-descriptor leaks visible in long soaks without a
// profiling endpoint.
type RuntimeStats struct {
	Goroutines  int     `json:"goroutines"`
	HeapAllocMB float64 `json:"heap_alloc_mb"`
	HeapObjects uint64  `json:"heap_objects"`
	NumGC       uint32  `json:"num_gc"`
	// OpenFDs counts this process's open file descriptors (-1 if the
	// platform offers no /proc/self/fd or /dev/fd listing).
	OpenFDs int `json:"open_fds"`
}

// SampleRuntime reads the current process's Go runtime counters. It calls
// runtime.ReadMemStats (a brief stop-the-world), so call it on demand or at
// most every few seconds, not per request.
func SampleRuntime() RuntimeStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return RuntimeStats{
		Goroutines:  runtime.NumGoroutine(),
		HeapAllocMB: round1(float64(ms.HeapAlloc) / (1 << 20)),
		HeapObjects: ms.HeapObjects,
		NumGC:       ms.NumGC,
		OpenFDs:     OpenFDs(),
	}
}

// OpenFDs returns the number of open file descriptors of the current
// process, excluding the one used to list them; -1 if unknown.
func OpenFDs() int {
	for _, d := range []string{"/proc/self/fd", "/dev/fd"} {
		ents, err := os.ReadDir(d)
		if err == nil && len(ents) > 0 {
			return len(ents) - 1
		}
	}
	return -1
}

// RuntimeStatsFile is a RuntimeStats snapshot written to a file for an
// external sampler.
type RuntimeStatsFile struct {
	PID int       `json:"pid"`
	At  time.Time `json:"at"`
	RuntimeStats
}

// WriteRuntimeStats atomically replaces path with a JSON snapshot of the
// current process's runtime counters.
func WriteRuntimeStats(path string) error {
	b, err := json.Marshal(RuntimeStatsFile{PID: os.Getpid(), At: time.Now().UTC(), RuntimeStats: SampleRuntime()})
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
