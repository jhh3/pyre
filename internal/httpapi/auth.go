package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"sync"

	"github.com/jhh3/pyre/internal/contracts"
)

// StaticTokens is a simple in-memory contracts.Authenticator for tests,
// the mock hub, and development. Tokens are compared in constant time by
// hash. It is not a substitute for the hub's token store.
type StaticTokens struct {
	mu     sync.RWMutex
	tokens map[[32]byte]contracts.Principal
	// Fallback, if set, is consulted for tokens not in the table.
	Fallback func(token string) (contracts.Principal, bool)
}

// NewStaticTokens returns an authenticator for the given token→principal map.
func NewStaticTokens(m map[string]contracts.Principal) *StaticTokens {
	s := &StaticTokens{tokens: map[[32]byte]contracts.Principal{}}
	for t, p := range m {
		s.Add(t, p)
	}
	return s
}

// Add registers (or replaces) a token.
func (s *StaticTokens) Add(token string, p contracts.Principal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[[32]byte]contracts.Principal{}
	}
	s.tokens[sha256.Sum256([]byte(token))] = p
}

// Remove revokes a token.
func (s *StaticTokens) Remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, sha256.Sum256([]byte(token)))
}

// Authenticate implements contracts.Authenticator.
func (s *StaticTokens) Authenticate(_ context.Context, bearer string) (contracts.Principal, error) {
	h := sha256.Sum256([]byte(bearer))
	s.mu.RLock()
	var found contracts.Principal
	ok := false
	for k, p := range s.tokens {
		if subtle.ConstantTimeCompare(k[:], h[:]) == 1 {
			found, ok = p, true
		}
	}
	fb := s.Fallback
	s.mu.RUnlock()
	if ok {
		return found, nil
	}
	if fb != nil {
		if p, ok := fb(bearer); ok {
			return p, nil
		}
	}
	return contracts.Principal{}, contracts.Errorf(contracts.ErrUnauthorized, "invalid credential")
}
