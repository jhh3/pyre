package backup

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/skillmanager"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/toolchain"
	"github.com/jhh3/outfit/internal/vault"
	"golang.org/x/crypto/argon2"
)

const (
	format         = "outfit-backup/v1"
	legacyFormat   = "cw-backup/v1"
	kdfName        = "argon2id"
	kdfTime        = uint32(3)
	kdfMemory      = uint32(64 * 1024)
	kdfParallelism = uint8(4)
)

type Package struct {
	Manifest kit.Package `json:"manifest"`
	Trust    string      `json:"trust"`
	Source   string      `json:"source"`
}

type Connection struct {
	Name             string            `json:"name"`
	ConnectorKey     string            `json:"connectorKey"`
	SelectedMode     string            `json:"selectedMode"`
	CredentialMethod string            `json:"credentialMethod"`
	Status           string            `json:"status"`
	CredentialID     string            `json:"credentialId"`
	SafeIdentity     map[string]string `json:"safeIdentity"`
}

type Credential struct {
	ID, ConnectorKey, MethodID, DisplayName, Fingerprint string
	Environment, AccountID, AccountName, PrincipalName   string
	Scopes                                               []string
	CreatedAt, UpdatedAt, LastVerifiedAt, LastUsedAt     time.Time
	Fields                                               map[string]string
}

type Tool struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

type Payload struct {
	CreatedAt   time.Time       `json:"createdAt"`
	Packages    []Package       `json:"packages"`
	Connections []Connection    `json:"connections"`
	Credentials []Credential    `json:"credentials"`
	Tools       []Tool          `json:"tools"`
	Skills      map[string]bool `json:"skillPreferences"`
}

type Envelope struct {
	Format          string    `json:"format"`
	CreatedAt       time.Time `json:"createdAt"`
	ConnectionCount int       `json:"connectionCount"`
	KDF             string    `json:"kdf"`
	KDFTime         uint32    `json:"kdfTime"`
	KDFMemory       uint32    `json:"kdfMemoryKiB"`
	KDFParallelism  uint8     `json:"kdfParallelism"`
	Salt            string    `json:"salt"`
	Nonce           string    `json:"nonce"`
	Ciphertext      string    `json:"ciphertext"`
}

// Why a connection was left out of a backup.
const (
	SkipMissingSecret = "missing-secret" // its vault secret no longer exists
	SkipOAuth         = "oauth"          // excluded on request (BuildOptions.ExcludeOAuth)
)

// SkippedConnection is a connection Build left out of a backup.
type SkippedConnection struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// BuildOptions tune Build.
type BuildOptions struct {
	// ExcludeOAuth leaves out connections whose credential is an
	// Outfit-managed OAuth grant. Copies of one refresh-token family on
	// several machines invalidate each other when the provider rotates it,
	// so fleet copies leave them out unless asked.
	ExcludeOAuth bool
}

// Build collects the portable payload. A connection whose credential secret
// no longer exists in the vault cannot be restored anywhere, so it is left
// out and reported in skipped instead of failing the whole backup; any other
// vault error (a locked keychain) still fails.
func Build(ctx context.Context, store *state.Store, secrets vault.Store, tools toolchain.Manager, skills skillmanager.Manager, options BuildOptions) (Payload, []SkippedConnection, error) {
	connections, err := store.Connections(ctx)
	if err != nil {
		return Payload{}, nil, err
	}
	payload := Payload{CreatedAt: time.Now().UTC()}
	installed, err := store.Packages(ctx)
	if err != nil {
		return Payload{}, nil, err
	}
	packagesByKey := map[string]kit.Package{}
	for _, item := range installed {
		packagesByKey[item.Key] = item.Package
	}
	oauth := func(connectorKey, methodID string) bool {
		method, ok := kit.FindCredentialMethod(packagesByKey[connectorKey], methodID)
		return ok && method.OAuth != nil
	}
	credentials, err := store.Credentials(ctx)
	if err != nil {
		return Payload{}, nil, err
	}
	missing := map[string]string{}
	for _, credential := range credentials {
		if options.ExcludeOAuth && oauth(credential.ConnectorKey, credential.MethodID) {
			continue
		}
		fields := map[string]string{}
		if credential.VaultRef != "" {
			fields, err = secrets.Get(ctx, credential.VaultRef)
			if vault.IsNotFound(err) {
				missing[credential.ID] = "credential " + credential.DisplayName + " has no secret in the vault (" + credential.VaultRef + ")"
				continue
			}
			if err != nil {
				return Payload{}, nil, fmt.Errorf("read credential %s: %w", credential.DisplayName, err)
			}
		}
		payload.Credentials = append(payload.Credentials, Credential{ID: credential.ID, ConnectorKey: credential.ConnectorKey, MethodID: credential.MethodID, DisplayName: credential.DisplayName, Fingerprint: credential.Fingerprint, Environment: credential.Environment, AccountID: credential.AccountID, AccountName: credential.AccountName, PrincipalName: credential.PrincipalName, Scopes: credential.Scopes, CreatedAt: credential.CreatedAt, UpdatedAt: credential.UpdatedAt, LastVerifiedAt: credential.LastVerifiedAt, LastUsedAt: credential.LastUsedAt, Fields: fields})
	}
	var skipped []SkippedConnection
	packageKeys := map[string]bool{}
	included := map[string]bool{}
	for _, connection := range connections {
		if options.ExcludeOAuth && oauth(connection.ConnectorKey, connection.CredentialMethod) {
			skipped = append(skipped, SkippedConnection{Name: connection.Name, Kind: SkipOAuth, Reason: "OAuth grant (" + connection.CredentialMethod + "); copies of one refresh token invalidate each other"})
			continue
		}
		if reason, ok := missing[connection.CredentialID]; ok {
			skipped = append(skipped, SkippedConnection{Name: connection.Name, Kind: SkipMissingSecret, Reason: reason})
			continue
		}
		included[connection.Name] = true
		payload.Connections = append(payload.Connections, Connection{Name: connection.Name, ConnectorKey: connection.ConnectorKey, SelectedMode: connection.SelectedMode, CredentialMethod: connection.CredentialMethod, Status: connection.Status, CredentialID: connection.CredentialID, SafeIdentity: connection.SafeIdentity})
		packageKeys[connection.ConnectorKey] = true
	}
	for _, item := range installed {
		if packageKeys[item.Key] {
			payload.Packages = append(payload.Packages, Package{Manifest: item.Package, Trust: item.Trust, Source: item.Source})
		}
	}
	receipts, err := tools.List()
	if err != nil {
		return Payload{}, nil, err
	}
	seen := map[string]bool{}
	for _, receipt := range receipts {
		if seen[receipt.ID] {
			continue
		}
		current, currentErr := tools.Resolve(receipt.ID)
		if currentErr == nil {
			payload.Tools = append(payload.Tools, Tool{ID: current.ID, Version: current.Version, Platform: current.Platform})
			seen[receipt.ID] = true
		}
	}
	preferences, err := skills.ExportPreferences()
	if err != nil {
		return Payload{}, nil, err
	}
	payload.Skills = map[string]bool{}
	for key, enabled := range preferences {
		if name, _, _ := strings.Cut(key, "|"); included[name] {
			payload.Skills[key] = enabled
		}
	}
	return payload, skipped, nil
}

func Write(path, passphrase string, payload Payload) error {
	return writeWithFormat(path, passphrase, payload, format)
}

func writeWithFormat(path, passphrase string, payload Payload, backupFormat string) error {
	if passphrase == "" {
		return errors.New("backup passphrase is required")
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key := argon2.IDKey([]byte(passphrase), salt, kdfTime, kdfMemory, kdfParallelism, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, []byte(backupFormat))
	envelope := Envelope{Format: backupFormat, CreatedAt: payload.CreatedAt, ConnectionCount: len(payload.Connections), KDF: kdfName, KDFTime: kdfTime, KDFMemory: kdfMemory, KDFParallelism: kdfParallelism, Salt: base64.RawStdEncoding.EncodeToString(salt), Nonce: base64.RawStdEncoding.EncodeToString(nonce), Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext)}
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".outfit-backup-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func Inspect(path string) (Envelope, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return Envelope{}, err
	}
	var envelope Envelope
	if err := json.Unmarshal(bytes, &envelope); err != nil {
		return Envelope{}, err
	}
	if envelope.Format != format && envelope.Format != legacyFormat {
		return Envelope{}, fmt.Errorf("unsupported backup format %q", envelope.Format)
	}
	if envelope.KDF != kdfName || envelope.KDFTime == 0 || envelope.KDFMemory < 32*1024 || envelope.KDFMemory > 1024*1024 || envelope.KDFParallelism == 0 || envelope.KDFParallelism > 32 {
		return Envelope{}, errors.New("unsupported or unsafe backup KDF parameters")
	}
	return envelope, nil
}

func Read(path, passphrase string) (Payload, error) {
	envelope, err := Inspect(path)
	if err != nil {
		return Payload{}, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(envelope.Salt)
	if err != nil {
		return Payload{}, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return Payload{}, err
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return Payload{}, err
	}
	key := argon2.IDKey([]byte(passphrase), salt, envelope.KDFTime, envelope.KDFMemory, envelope.KDFParallelism, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return Payload{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Payload{}, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(envelope.Format))
	if err != nil {
		return Payload{}, errors.New("backup passphrase is incorrect or the file was modified")
	}
	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return Payload{}, err
	}
	migrateLegacyPayload(&payload)
	return payload, nil
}

func migrateLegacyPayload(payload *Payload) {
	for index := range payload.Connections {
		if payload.Connections[index].CredentialMethod == "cw-oauth" {
			payload.Connections[index].CredentialMethod = "outfit-oauth"
		}
	}
	for index := range payload.Credentials {
		if payload.Credentials[index].MethodID == "cw-oauth" {
			payload.Credentials[index].MethodID = "outfit-oauth"
		}
	}
	for index := range payload.Packages {
		pkg := &payload.Packages[index].Manifest
		if strings.HasPrefix(pkg.APIVersion, "cw.jhh3.dev/") {
			pkg.APIVersion = "outfit.jhh3.dev/" + strings.TrimPrefix(pkg.APIVersion, "cw.jhh3.dev/")
		}
		for methodIndex := range pkg.CredentialMethods {
			method := &pkg.CredentialMethods[methodIndex]
			if method.ID == "cw-oauth" {
				method.ID = "outfit-oauth"
			}
			if method.Custody == "cw" {
				method.Custody = "outfit"
			}
		}
		for modeIndex := range pkg.Modes {
			mode := &pkg.Modes[modeIndex]
			if mode.DefaultCredentialMethod == "cw-oauth" {
				mode.DefaultCredentialMethod = "outfit-oauth"
			}
			for methodIndex, methodID := range mode.CredentialMethods {
				if methodID == "cw-oauth" {
					mode.CredentialMethods[methodIndex] = "outfit-oauth"
				}
			}
		}
	}
}
