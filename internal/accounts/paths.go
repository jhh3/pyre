package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Canonicalize returns the absolute, cleaned path with every symlink
// resolved. The Claude macOS keychain item name hashes the exact
// CLAUDE_CONFIG_DIR string, so "/tmp/x" and "/private/tmp/x" (or a doubled
// slash) would address different logins; Pyre always stores and launches with
// the canonical form. The path must exist.
func Canonicalize(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

// ensureDir creates p (and parents) with mode 0700 when missing, tightens an
// existing directory to 0700, and returns its canonical path.
func ensureDir(p string) (string, error) {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(p, 0o700); err != nil {
		return "", err
	}
	return Canonicalize(p)
}

// ClaudeKeychainService is the macOS keychain service name Claude Code uses
// for a CLAUDE_CONFIG_DIR (accounts report E1): the first 8 hex characters of
// sha256 over the exact directory string. Pass a canonical path.
func ClaudeKeychainService(profileDir string) string {
	sum := sha256.Sum256([]byte(profileDir))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
}

// safeSegment validates an account ID used as a directory name.
func safeSegment(s string) error {
	if s == "" || s == "." || s == ".." || len(s) > 200 {
		return fmt.Errorf("invalid path segment %q", s)
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-' || c == '_' || c == '.' || c == ':') {
			return fmt.Errorf("invalid path segment %q", s)
		}
	}
	return nil
}

// within reports whether p is root or below it (both canonical).
func within(p, root string) bool {
	if root == "" {
		return false
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// canonicalOrClean canonicalizes p when it exists, else returns it cleaned
// and absolute (used for refusal checks against paths that may not exist).
func canonicalOrClean(p string) string {
	if p == "" {
		return ""
	}
	if c, err := Canonicalize(p); err == nil {
		return c
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Clean(abs)
}

// harnessEnvVar is the profile-binding environment variable per harness.
func harnessEnvVar(h contracts.Harness) string {
	switch h {
	case contracts.HarnessClaude:
		return "CLAUDE_CONFIG_DIR"
	case contracts.HarnessCodex:
		return "CODEX_HOME"
	}
	return ""
}

// harnessForProvider maps a subscription provider to its agent harness.
func harnessForProvider(p contracts.Provider) contracts.Harness {
	switch p {
	case contracts.ProviderAnthropic:
		return contracts.HarnessClaude
	case contracts.ProviderOpenAI:
		return contracts.HarnessCodex
	}
	return ""
}

// shellQuote single-quotes s for display in a copy-pasteable command.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-:@+=", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
