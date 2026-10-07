// Package credentials holds Pyre's credential-reference model, the node-local
// secret Store implementations (macOS Keychain via `security`, Linux Secret
// Service via `secret-tool`, and an explicit opt-in 0600 file store), the
// launch-time resolver that turns connection grants into a run's environment,
// redaction helpers, and the sealed cross-node provisioning primitive.
//
// Invariants:
//   - A Ref names where a secret lives; it never contains the value and is
//     safe to persist, log, and send to the hub.
//   - A Secret value refuses JSON/text serialization and formats as
//     "[REDACTED]", so it cannot leak through logs, records, or errors by
//     accident.
//   - A locked or unavailable OS store yields an actionable *StoreError; there
//     is never a silent plaintext fallback.
package credentials

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Redacted is the placeholder written wherever a secret value would appear.
const Redacted = "[REDACTED]"

// ServicePrefix prefixes every Pyre-owned OS credential-store service name so
// Pyre never reads or overwrites items owned by other tools (including Outfit).
const ServicePrefix = "pyre:"

// Backend names.
const (
	BackendKeychain      = "keychain"
	BackendSecretService = "secret-service"
	BackendFile          = "file"
	BackendMemory        = "memory" // tests and fakes only
)

// Secret is an in-memory credential value. Its zero value is empty.
type Secret struct{ v []byte }

// NewSecret wraps a value.
func NewSecret(value string) Secret { return Secret{v: []byte(value)} }

// SecretFromBytes copies b into a Secret.
func SecretFromBytes(b []byte) Secret { return Secret{v: append([]byte(nil), b...)} }

// Reveal returns the plaintext. Call it only at the point of use (process
// environment, provider call); never pass the result to a logger or record.
func (s Secret) Reveal() string { return string(s.v) }

// Bytes returns a copy of the plaintext bytes.
func (s Secret) Bytes() []byte { return append([]byte(nil), s.v...) }

// Len reports the value length in bytes.
func (s Secret) Len() int { return len(s.v) }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return len(s.v) == 0 }

// Wipe zeroes the backing array.
func (s *Secret) Wipe() {
	for i := range s.v {
		s.v[i] = 0
	}
	s.v = nil
}

func (Secret) String() string   { return Redacted }
func (Secret) GoString() string { return "credentials.Secret(" + Redacted + ")" }

// Format makes every fmt verb print the placeholder.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Redacted) }

// ErrSerializeSecret is returned by Secret's marshalers.
var ErrSerializeSecret = errors.New("credentials: refusing to serialize a secret value")

func (Secret) MarshalJSON() ([]byte, error) { return nil, ErrSerializeSecret }
func (Secret) MarshalText() ([]byte, error) { return nil, ErrSerializeSecret }

// Ref identifies a secret in a node-local store. It is metadata, not a value.
type Ref struct {
	Backend string `json:"backend"`
	Service string `json:"service"` // always ServicePrefix + ...
	Account string `json:"account"`
}

var refPart = regexp.MustCompile(`^[A-Za-z0-9._:@-]{1,200}$`)

// NewRef builds the conventional reference for one field of a connection:
// service "pyre:<connection_id>", account "<field>".
func NewRef(backend, connectionID, field string) Ref {
	return Ref{Backend: backend, Service: ServicePrefix + connectionID, Account: field}
}

// Validate checks the reference shape. Restricting characters keeps refs safe
// to pass as CLI arguments and inside `security -i` command lines.
func (r Ref) Validate() error {
	switch r.Backend {
	case BackendKeychain, BackendSecretService, BackendFile, BackendMemory:
	default:
		return fmt.Errorf("credentials: unknown backend %q", r.Backend)
	}
	if !strings.HasPrefix(r.Service, ServicePrefix) || !refPart.MatchString(r.Service) {
		return fmt.Errorf("credentials: service %q must start with %q and use [A-Za-z0-9._:@-]", r.Service, ServicePrefix)
	}
	if !refPart.MatchString(r.Account) || strings.HasPrefix(r.Account, "-") {
		return fmt.Errorf("credentials: account %q must use [A-Za-z0-9._:@-] and not start with '-'", r.Account)
	}
	return nil
}

// String renders "backend:service/account", e.g. "keychain:pyre:conn_x/GH_TOKEN".
func (r Ref) String() string { return r.Backend + ":" + r.Service + "/" + r.Account }

// Key is the backend-independent identity used by file/memory stores.
func (r Ref) Key() string { return r.Service + "/" + r.Account }

// ParseRef parses the String form.
func ParseRef(s string) (Ref, error) {
	backend, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Ref{}, fmt.Errorf("credentials: malformed ref %q", s)
	}
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return Ref{}, fmt.Errorf("credentials: malformed ref %q", s)
	}
	r := Ref{Backend: backend, Service: rest[:i], Account: rest[i+1:]}
	return r, r.Validate()
}
