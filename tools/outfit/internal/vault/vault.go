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

func Open(paths state.Paths) (Store, error) {
	switch os.Getenv("OUTFIT_VAULT_BACKEND") {
	case "", "keyring":
		return &Keyring{}, nil
	case "file":
		return openFile(paths.Vault)
	default:
		return nil, fmt.Errorf("unsupported OUTFIT_VAULT_BACKEND; use keyring or explicit file")
	}
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
