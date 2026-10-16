package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/jhh3/pyre/internal/fileperm"
)

// FileStore is the explicit opt-in plaintext store: one JSON file, mode 0600,
// inside a 0700 directory. It exists for headless hosts without a usable OS
// store and is never chosen automatically. Permissions are verified on every
// access; a broader mode is refused rather than repaired silently.
type FileStore struct {
	dir string
	mu  sync.Mutex
}

const fileStoreName = "credentials.json"

// NewFileStore opens (creating) the store. optIn must be true; it represents
// the owner's explicit configuration choice.
func NewFileStore(dir string, optIn bool) (*FileStore, error) {
	if !optIn {
		return nil, storeErr(KindRefused, BackendFile, "open", Ref{}, "the plaintext file store requires explicit opt-in (credentials.backend=file and allow_file_store=true)", "")
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, storeErr(KindInvalid, BackendFile, "open", Ref{}, "configure an absolute directory for the file store", "")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, storeErr(KindUnavailable, BackendFile, "open", Ref{}, "check directory permissions", Diagnostic(nil, []byte(err.Error())))
	}
	fsStore := &FileStore{dir: dir}
	if err := fsStore.checkPerms(); err != nil {
		return nil, err
	}
	return fsStore, nil
}

func (f *FileStore) Backend() string { return BackendFile }

func (f *FileStore) path() string { return filepath.Join(f.dir, fileStoreName) }

func (f *FileStore) checkPerms() error {
	st, err := os.Stat(f.dir)
	if err != nil {
		return storeErr(KindUnavailable, BackendFile, "open", Ref{}, "check the file store directory", Diagnostic(nil, []byte(err.Error())))
	}
	if desc, bad := fileperm.Exposed(f.dir, st); bad {
		return storeErr(KindRefused, BackendFile, "open", Ref{}, fmt.Sprintf("%s (currently %s); Pyre refuses group/world-accessible credential directories", fixCmd(f.dir, true), modeDesc(st, desc)), "")
	}
	fst, err := os.Stat(f.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return storeErr(KindUnavailable, BackendFile, "open", Ref{}, "check the file store", Diagnostic(nil, []byte(err.Error())))
	}
	if desc, bad := fileperm.Exposed(f.path(), fst); bad {
		return storeErr(KindRefused, BackendFile, "open", Ref{}, fmt.Sprintf("%s (currently %s); Pyre refuses group/world-readable credential files", fixCmd(f.path(), false), modeDesc(fst, desc)), "")
	}
	return nil
}

func (f *FileStore) load() (map[string]string, error) {
	if err := f.checkPerms(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(f.path())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, storeErr(KindUnavailable, BackendFile, "read", Ref{}, "check the file store", Diagnostic(nil, []byte(err.Error())))
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, storeErr(KindInvalid, BackendFile, "read", Ref{}, "the file store is corrupt; restore it from backup or re-provision", "")
	}
	return m, nil
}

func (f *FileStore) save(m map[string]string) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return storeErr(KindInvalid, BackendFile, "write", Ref{}, "", "encode")
	}
	tmp, err := os.CreateTemp(f.dir, ".credentials-*")
	if err != nil {
		return storeErr(KindUnavailable, BackendFile, "write", Ref{}, "check the file store directory", Diagnostic(nil, []byte(err.Error())))
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := fileperm.RestrictFile(tmp, 0o600); err == nil {
		if _, err = tmp.Write(raw); err == nil {
			err = tmp.Sync()
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(name, f.path())
		}
		if err == nil {
			return nil
		}
	} else {
		tmp.Close()
	}
	return storeErr(KindUnavailable, BackendFile, "write", Ref{}, "check the file store directory", "atomic replace failed")
}

func (f *FileStore) check(ref Ref, op string) error {
	if ref.Backend != BackendFile {
		return storeErr(KindInvalid, BackendFile, op, ref, "", "ref belongs to backend "+ref.Backend)
	}
	if err := ref.Validate(); err != nil {
		return storeErr(KindInvalid, BackendFile, op, ref, "", err.Error())
	}
	return nil
}

func (f *FileStore) Get(_ context.Context, ref Ref) (Secret, error) {
	if err := f.check(ref, "get"); err != nil {
		return Secret{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return Secret{}, err
	}
	enc, ok := m[ref.Key()]
	if !ok {
		return Secret{}, storeErr(KindNotFound, BackendFile, "get", ref, "provision the credential on this node first", "")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return Secret{}, storeErr(KindInvalid, BackendFile, "get", ref, "re-provision this credential", "stored value is not valid encoding")
	}
	return SecretFromBytes(raw), nil
}

func (f *FileStore) Put(_ context.Context, ref Ref, value Secret) error {
	if err := f.check(ref, "put"); err != nil {
		return err
	}
	if value.IsZero() {
		return storeErr(KindInvalid, BackendFile, "put", ref, "", "empty value")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	m[ref.Key()] = base64.StdEncoding.EncodeToString(value.v)
	return f.save(m)
}

func (f *FileStore) Delete(_ context.Context, ref Ref) error {
	if err := f.check(ref, "delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	if _, ok := m[ref.Key()]; !ok {
		return nil
	}
	delete(m, ref.Key())
	return f.save(m)
}

// MemoryStore is the fake credential store for tests. It can simulate a locked
// or unavailable backend and counts operations.
type MemoryStore struct {
	mu          sync.Mutex
	BackendName string // defaults to "memory"
	values      map[string][]byte
	Locked      bool
	Unavailable bool
	Gets        int
}

// NewMemoryStore returns an empty fake store using backend "memory".
func NewMemoryStore() *MemoryStore { return &MemoryStore{values: map[string][]byte{}} }

func (m *MemoryStore) Backend() string {
	if m.BackendName != "" {
		return m.BackendName
	}
	return BackendMemory
}

func (m *MemoryStore) gate(ref Ref, op string) error {
	if m.Locked {
		return storeErr(KindLocked, m.Backend(), op, ref, "unlock the fake store", "")
	}
	if m.Unavailable {
		return storeErr(KindUnavailable, m.Backend(), op, ref, "fake store unavailable", "")
	}
	if err := ref.Validate(); err != nil {
		return storeErr(KindInvalid, m.Backend(), op, ref, "", err.Error())
	}
	return nil
}

// SetLocked toggles the simulated lock.
func (m *MemoryStore) SetLocked(v bool) { m.mu.Lock(); m.Locked = v; m.mu.Unlock() }

func (m *MemoryStore) Get(_ context.Context, ref Ref) (Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Gets++
	if err := m.gate(ref, "get"); err != nil {
		return Secret{}, err
	}
	v, ok := m.values[ref.Key()]
	if !ok {
		return Secret{}, storeErr(KindNotFound, m.Backend(), "get", ref, "provision the credential first", "")
	}
	return SecretFromBytes(v), nil
}

func (m *MemoryStore) Put(_ context.Context, ref Ref, value Secret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(ref, "put"); err != nil {
		return err
	}
	if m.values == nil {
		m.values = map[string][]byte{}
	}
	m.values[ref.Key()] = value.Bytes()
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, ref Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(ref, "delete"); err != nil {
		return err
	}
	delete(m.values, ref.Key())
	return nil
}

// Len reports the number of stored values.
func (m *MemoryStore) Len() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.values) }
