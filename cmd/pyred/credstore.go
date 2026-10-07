package main

import (
	"fmt"

	"github.com/jhh3/pyre/internal/credentials"
)

// Environment overrides for the store that holds Pyre-provisioned connection
// credentials (the flags --credential-backend / --credential-keychain win).
const (
	EnvCredentialBackend  = "PYRE_CREDENTIAL_BACKEND"
	EnvCredentialKeychain = "PYRE_CREDENTIAL_KEYCHAIN"
)

// credentialConfig selects the node's credential store. Only OS stores are
// selectable here: the plaintext file store is never chosen by a daemon flag
// or environment variable.
func credentialConfig(o runOpts, getenv func(string) string) (credentials.Config, error) {
	cfg := credentials.Config{Backend: o.credBackend, Keychain: o.credKeychain}
	if cfg.Backend == "" {
		cfg.Backend = getenv(EnvCredentialBackend)
	}
	if cfg.Keychain == "" {
		cfg.Keychain = getenv(EnvCredentialKeychain)
	}
	switch cfg.Backend {
	case "", "auto", credentials.BackendKeychain, credentials.BackendSecretService:
	default:
		return credentials.Config{}, fmt.Errorf("credential backend %q: use auto, keychain or secret-service", cfg.Backend)
	}
	if cfg.Keychain != "" && cfg.Backend == credentials.BackendSecretService {
		return credentials.Config{}, fmt.Errorf("--credential-keychain applies to the keychain backend, not %s", cfg.Backend)
	}
	return cfg, nil
}
