package transport

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Secret prefixes. Every Pyre secret is "<prefix><43 chars base64url>"
// (256 bits of entropy). Prefixes make secrets recognisable to Redact and to
// secret scanners; they are not secret themselves.
const (
	PrefixOwnerToken   = "pyc_" // owner client credential (CLI, TUI, Android)
	PrefixNodeToken    = "pyn_" // enrolled node daemon credential
	PrefixSessionToken = "pys_" // agent-session scoped credential
	PrefixTicket       = "pyt_" // one-time, purpose-bound ticket
	PrefixEnrollSecret = "pye_" // one-time node enrollment secret
	PrefixPairingCode  = "pyp_" // one-time client pairing code
)

const secretBytes = 32

// NewSecret returns a new high-entropy secret with the given prefix.
func NewSecret(prefix string) (string, error) {
	var b [secretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("transport: crypto/rand: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashSecret returns the lowercase hex SHA-256 of a secret. Only this hash is
// ever persisted.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// SecretMatches reports, in constant time, whether secret hashes to
// storedHash.
func SecretMatches(storedHash, secret string) bool {
	got := HashSecret(secret)
	return subtle.ConstantTimeCompare([]byte(storedHash), []byte(got)) == 1
}

// tokenKind maps a credential prefix to the principal kind it may carry.
func tokenKind(token string) (contracts.PrincipalKind, bool) {
	switch {
	case strings.HasPrefix(token, PrefixOwnerToken):
		return contracts.PrincipalOwner, true
	case strings.HasPrefix(token, PrefixNodeToken):
		return contracts.PrincipalNode, true
	case strings.HasPrefix(token, PrefixSessionToken):
		return contracts.PrincipalSession, true
	}
	return "", false
}

func prefixFor(kind contracts.PrincipalKind) (string, bool) {
	switch kind {
	case contracts.PrincipalOwner:
		return PrefixOwnerToken, true
	case contracts.PrincipalNode:
		return PrefixNodeToken, true
	case contracts.PrincipalSession:
		return PrefixSessionToken, true
	}
	return "", false
}

// BearerToken extracts the token from an "Authorization: Bearer <token>"
// header, or "" if absent or malformed.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}
