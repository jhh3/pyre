package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// readState remembers when the owner last opened each thread in the web
// inbox (the hub has no per-thread read mark for the owner: attention
// items are per ask or per session). It lives in <state-dir>/read.json.
type readState struct {
	mu      sync.Mutex
	path    string
	Threads map[string]time.Time `json:"threads"`
}

// readKeep bounds the file: marks older than this are dropped on save
// (the done box only reaches back 14 days).
const readKeep = 60 * 24 * time.Hour

func loadReadState(dir string) (*readState, error) {
	r := &readState{Threads: map[string]time.Time{}}
	if dir == "" {
		return r, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r.path = filepath.Join(dir, "read.json")
	b, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, r); err != nil {
		// A damaged file only loses read marks; start over.
		r.Threads = map[string]time.Time{}
	}
	if r.Threads == nil {
		r.Threads = map[string]time.Time{}
	}
	return r, nil
}

func (r *readState) readAt(thread string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Threads[thread]
}

// mark records that thread was read at t and saves the file.
func (r *readState) mark(thread string, t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.Before(r.Threads[thread]) {
		return nil
	}
	r.Threads[thread] = t
	if r.path == "" {
		return nil
	}
	for k, v := range r.Threads {
		if t.Sub(v) > readKeep {
			delete(r.Threads, k)
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
