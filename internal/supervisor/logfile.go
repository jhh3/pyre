package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingFile is a size-bounded log file: when a write would exceed
// MaxBytes the file is rotated to path.1 (path.1 -> path.2 ...), keeping at
// most Backups old files. Total disk use is bounded by
// MaxBytes*(Backups+1). Files are created 0600.
type RotatingFile struct {
	Path     string
	MaxBytes int64 // default 5 MiB
	Backups  int   // default 3

	mu   sync.Mutex
	f    *os.File
	size int64
}

func (r *RotatingFile) limits() (int64, int) {
	m, b := r.MaxBytes, r.Backups
	if m <= 0 {
		m = 5 << 20
	}
	if b <= 0 {
		b = 3
	}
	return m, b
}

func (r *RotatingFile) open() error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *RotatingFile) rotate() error {
	_, backups := r.limits()
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	for i := backups - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.Path, i), fmt.Sprintf("%s.%d", r.Path, i+1))
	}
	if err := os.Rename(r.Path, r.Path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.open()
}

// Write implements io.Writer. A single write larger than MaxBytes is
// truncated to MaxBytes.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	max, _ := r.limits()
	n := len(p)
	if int64(len(p)) > max {
		p = p[:max]
	}
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size+int64(len(p)) > max && r.size > 0 {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	w, err := r.f.Write(p)
	r.size += int64(w)
	if err != nil {
		return w, err
	}
	return n, nil
}

// Close closes the current file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
