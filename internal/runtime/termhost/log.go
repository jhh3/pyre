package termhost

import "os"

// maxLog is the size at which the host log is rotated (one old copy).
const maxLog = 1 << 20

// openLog opens the host log for appending (0600), rotating a large one.
func openLog(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > maxLog {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

// OpenLog is openLog for the host process, which opens the log itself when
// started through Task Scheduler (no stdio to inherit).
func OpenLog(path string) (*os.File, error) { return openLog(path) }
