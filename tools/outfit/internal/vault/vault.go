package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/outfit/internal/state"
	keyring "github.com/zalando/go-keyring"
)

const (
	service       = "dev.jhh3.outfit"
	legacyService = "dev.jhh3.cw"
)

var (
	keyringGet    = keyring.Get
	keyringSet    = keyring.Set
	keyringDelete = keyring.Delete
)

type Store interface {
	Put(context.Context, map[string]string) (string, error)
	Get(context.Context, string) (map[string]string, error)
	Delete(context.Context, string) error
}

// ErrNotFound reports a secret reference with no stored value (a deleted
// keychain item or a missing file-vault entry).
var ErrNotFound = errors.New("vault secret not found")

// IsNotFound reports whether err means the referenced secret does not exist.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// backendMarker records, inside the vault directory, which backend new
// secrets go to when OUTFIT_VAULT_BACKEND is unset. Only an explicit choice
// (`outfit vault use`, SetBackend) writes it; nothing writes it implicitly.
const backendMarker = "backend"

// Backend names the backend new secrets go to: OUTFIT_VAULT_BACKEND when
// set, else the choice recorded in the vault directory, else the OS
// keychain.
func Backend(paths state.Paths) string {
	backend, _ := BackendSource(paths)
	return backend
}

// BackendSource is Backend plus where the choice came from: "env",
// "recorded" (vault/backend) or "default".
func BackendSource(paths state.Paths) (string, string) {
	if value := os.Getenv("OUTFIT_VAULT_BACKEND"); value != "" {
		return value, "env"
	}
	if raw, err := os.ReadFile(filepath.Join(paths.Vault, backendMarker)); err == nil {
		switch strings.TrimSpace(string(raw)) {
		case "file":
			return "file", "recorded"
		case "keyring":
			return "keyring", "recorded"
		}
	}
	return "keyring", "default"
}

// SetBackend records the backend for runs without OUTFIT_VAULT_BACKEND.
// Existing secrets stay where they are and remain readable: reads look in
// the file vault for a ref it holds and in the keychain otherwise.
func SetBackend(paths state.Paths, backend string) error {
	if backend != "file" && backend != "keyring" {
		return fmt.Errorf("vault backend must be file or keyring")
	}
	if err := os.MkdirAll(paths.Vault, 0o700); err != nil {
		return err
	}
	if backend == "file" {
		if _, err := openFile(paths.Vault); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(paths.Vault, backendMarker), []byte(backend+"\n"), 0o600)
}

// Open returns the vault. New secrets go to the selected backend (Backend);
// a secret is read from wherever it lives: the file vault when it holds the
// ref, else the OS keychain. A backend choice therefore never hides secrets
// stored under the other one.
func Open(paths state.Paths) (Store, error) {
	backend := Backend(paths)
	dual := &Dual{backend: backend, keyring: &Keyring{}}
	switch backend {
	case "keyring":
		if _, err := os.Stat(filepath.Join(paths.Vault, "master.key")); err == nil {
			file, err := openFile(paths.Vault)
			if err != nil {
				return nil, err
			}
			dual.file = file
		}
	case "file":
		file, err := openFile(paths.Vault)
		if err != nil {
			return nil, err
		}
		dual.file = file
	default:
		return nil, fmt.Errorf("unsupported OUTFIT_VAULT_BACKEND; use keyring or explicit file")
	}
	return dual, nil
}

// Dual writes to one backend and reads from both (see Open).
type Dual struct {
	backend string
	keyring *Keyring
	file    *File // nil when this home has no file vault
}

// Backend is the backend new secrets go to.
func (d *Dual) Backend() string { return d.backend }

func (d *Dual) inFile(ref string) bool {
	if d.file == nil {
		return false
	}
	_, err := os.Stat(d.file.path(ref))
	return err == nil
}

// WriteError is a failure to store a secret: the backend itself refused
// (a locked or unavailable keychain, an unwritable file vault). Callers that
// store many secrets treat it as systemic and stop.
type WriteError struct{ Err error }

func (e *WriteError) Error() string { return "store secret: " + e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }

// IsWriteError reports whether err is (or wraps) a WriteError.
func IsWriteError(err error) bool {
	var target *WriteError
	return errors.As(err, &target)
}

func (d *Dual) Put(ctx context.Context, value map[string]string) (string, error) {
	var ref string
	var err error
	if d.backend == "file" {
		ref, err = d.file.Put(ctx, value)
	} else {
		ref, err = d.keyring.Put(ctx, value)
	}
	if err != nil {
		return "", &WriteError{Err: err}
	}
	return ref, nil
}

func (d *Dual) Get(ctx context.Context, ref string) (map[string]string, error) {
	if d.inFile(ref) {
		return d.file.Get(ctx, ref)
	}
	value, err := d.keyring.Get(ctx, ref)
	if err != nil && d.backend == "file" && !IsNotFound(err) {
		// The selected vault does not hold it and the keychain cannot be
		// asked (no secret service): the secret is missing here.
		return nil, fmt.Errorf("file vault: %w: %s (keychain not available: %v)", ErrNotFound, ref, err)
	}
	return value, err
}

func (d *Dual) Delete(ctx context.Context, ref string) error {
	if d.inFile(ref) {
		return d.file.Delete(ctx, ref)
	}
	err := d.keyring.Delete(ctx, ref)
	if err != nil && d.backend == "file" {
		return nil // nothing in the file vault; the keychain is not available
	}
	return err
}

type Keyring struct{}

func (k *Keyring) Put(_ context.Context, value map[string]string) (string, error) {
	ref, err := randomRef()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if err := keyringSet(service, ref, string(encoded)); err != nil {
		return "", fmt.Errorf("OS keychain: %w", err)
	}
	return ref, nil
}

func (k *Keyring) Get(_ context.Context, ref string) (map[string]string, error) {
	encoded, err := keyringGet(service, ref)
	if errors.Is(err, keyring.ErrNotFound) {
		encoded, err = keyringGet(legacyService, ref)
		if err == nil {
			if setErr := keyringSet(service, ref, encoded); setErr != nil {
				return nil, fmt.Errorf("migrate legacy OS keychain secret: %w", setErr)
			}
			_ = keyringDelete(legacyService, ref)
		}
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, fmt.Errorf("OS keychain: %w: %s", ErrNotFound, ref)
	}
	if err != nil {
		return nil, fmt.Errorf("OS keychain: %w", err)
	}
	return decode(encoded)
}

func (k *Keyring) Delete(_ context.Context, ref string) error {
	err := keyringDelete(service, ref)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("OS keychain: %w", err)
	}
	err = keyringDelete(legacyService, ref)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("legacy OS keychain: %w", err)
	}
	return nil
}

type File struct {
	root string
	key  []byte
}

type fileEnvelope struct {
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func openFile(root string) (*File, error) {
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(root, "master.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		if err := writeExclusive(keyPath, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("file vault master key has invalid length")
	}
	return &File{root: root, key: key}, nil
}

func (f *File) Put(_ context.Context, value map[string]string) (string, error) {
	ref, err := randomRef()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(f.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nil, nonce, encoded, []byte(ref))
	envelope, _ := json.Marshal(fileEnvelope{Nonce: base64.RawStdEncoding.EncodeToString(nonce), Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext)})
	if err := writeExclusive(f.path(ref), envelope, 0o600); err != nil {
		return "", err
	}
	return ref, nil
}

func (f *File) Get(_ context.Context, ref string) (map[string]string, error) {
	raw, err := os.ReadFile(f.path(ref))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("file vault: %w: %s", ErrNotFound, ref)
	}
	if err != nil {
		return nil, err
	}
	var envelope fileEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(f.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(ref))
	if err != nil {
		return nil, fmt.Errorf("decrypt file vault secret: %w", err)
	}
	return decode(string(plain))
}

func (f *File) Delete(_ context.Context, ref string) error {
	err := os.Remove(f.path(ref))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (f *File) path(ref string) string { return filepath.Join(f.root, "secrets", ref+".json") }

func decode(encoded string) (map[string]string, error) {
	var value map[string]string
	if err := json.Unmarshal([]byte(encoded), &value); err != nil {
		return nil, err
	}
	return value, nil
}

func randomRef() (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return "outfitsec_" + hex.EncodeToString(raw), nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}
