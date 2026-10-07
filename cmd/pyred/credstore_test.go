package main

import (
	"testing"

	"github.com/jhh3/pyre/internal/credentials"
)

func TestCredentialConfig(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	cfg, err := credentialConfig(runOpts{}, get)
	if err != nil || cfg.Backend != "" || cfg.Keychain != "" {
		t.Fatalf("default: %+v %v", cfg, err)
	}
	env[EnvCredentialBackend] = "keychain"
	env[EnvCredentialKeychain] = "/tmp/synthetic-test.keychain-db"
	cfg, err = credentialConfig(runOpts{}, get)
	if err != nil || cfg.Backend != credentials.BackendKeychain || cfg.Keychain != "/tmp/synthetic-test.keychain-db" {
		t.Fatalf("env: %+v %v", cfg, err)
	}
	// flags win over the environment
	cfg, err = credentialConfig(runOpts{credBackend: "auto", credKeychain: "/tmp/other.keychain-db"}, get)
	if err != nil || cfg.Backend != "auto" || cfg.Keychain != "/tmp/other.keychain-db" {
		t.Fatalf("flags: %+v %v", cfg, err)
	}
	// never the plaintext file store, and no keychain path for Secret Service
	if _, err := credentialConfig(runOpts{credBackend: "file"}, get); err == nil {
		t.Fatal("file backend must not be selectable by flag")
	}
	if _, err := credentialConfig(runOpts{credBackend: "secret-service", credKeychain: "/tmp/x"}, get); err == nil {
		t.Fatal("keychain path with secret-service must be refused")
	}
	fs := runFlags(&runOpts{})
	if fs.Lookup("credential-backend") == nil || fs.Lookup("credential-keychain") == nil || fs.Lookup("rota-home") == nil {
		t.Fatal("missing flags")
	}
}
