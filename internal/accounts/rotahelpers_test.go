package accounts

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeFleet is the hub refresh-family lease (one node per account).
type fakeFleet struct {
	mu          sync.Mutex
	held        map[string]FleetLease // account -> lease
	unreachable bool
	acquires    int
	releases    int
	renews      int
}

func newFakeFleet() *fakeFleet { return &fakeFleet{held: map[string]FleetLease{}} }

func (f *fakeFleet) AcquireAccountLease(_ context.Context, r FleetLeaseRequest) (FleetLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable {
		return FleetLease{}, contracts.Errorf(contracts.ErrUnavailable, "hub unreachable")
	}
	if l, ok := f.held[r.AccountID]; ok && l.NodeID != r.NodeID {
		return FleetLease{}, contracts.Errorf(contracts.ErrConflict, "account leased by node %s", l.NodeID)
	}
	f.acquires++
	l := FleetLease{LeaseID: fmt.Sprintf("flease_%d", f.acquires), AccountID: r.AccountID, NodeID: r.NodeID, ExpiresAt: time.Now().Add(r.TTL)}
	f.held[r.AccountID] = l
	return l, nil
}

func (f *fakeFleet) RenewAccountLease(_ context.Context, id string, ttl time.Duration) (FleetLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, l := range f.held {
		if l.LeaseID == id {
			l.ExpiresAt = time.Now().Add(ttl)
			f.held[k] = l
			f.renews++
			return l, nil
		}
	}
	return FleetLease{}, contracts.Errorf(contracts.ErrNotFound, "no lease")
}

func (f *fakeFleet) ReleaseAccountLease(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, l := range f.held {
		if l.LeaseID == id {
			delete(f.held, k)
			f.releases++
		}
	}
	return nil
}

func (f *fakeFleet) holder(acct string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[acct].NodeID
}

type fakeBusy struct {
	mu   sync.Mutex
	busy map[string]bool
}

func (b *fakeBusy) HomeBusy(_ context.Context, dir string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.busy[dir], nil
}

type rotaEnv struct {
	*testEnv
	claudeAcc string
	fleet     *fakeFleet
}

func newRotaEnv(t *testing.T, mod ...func(*Config)) *rotaEnv {
	t.Helper()
	fleet := newFakeFleet()
	var claudeAcc string
	all := append([]func(*Config){func(c *Config) {
		claudeAcc = filepath.Join(filepath.Dir(c.UserClaudeDir), ".claude", "accounts")
		c.Rota = RotaConfig{Enabled: true, ClaudeAccountsDir: claudeAcc, CodexHome: c.UserCodexDir, Fleet: fleet,
			SyncInterval: time.Second, LeaseTTL: time.Minute, User: "synthetic-user"}
	}}, mod...)
	e := newEnv(t, all...)
	os.MkdirAll(claudeAcc, 0o700)
	return &rotaEnv{testEnv: e, claudeAcc: claudeAcc, fleet: fleet}
}

func rotaName(label string) string { return "acct-" + label + "@synthetic.invalid" }

// writeRotaClaude writes a rota Claude snapshot in rota's format.
func (r *rotaEnv) writeRotaClaude(label, gen string, expiresAt time.Time) string {
	r.t.Helper()
	p := filepath.Join(r.claudeAcc, rotaName(label)+".json")
	snap := map[string]any{
		"name": rotaName(label),
		"claudeAiOauth": map[string]any{
			"accessToken": "pyre-synthetic-" + label + "-access-" + gen, "refreshToken": "pyre-synthetic-" + label + "-refresh-" + gen,
			"expiresAt": expiresAt.UnixMilli(), "scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max",
			"refreshTokenExpiresAt": time.Now().Add(50 * 24 * time.Hour).UnixMilli(),
		},
		"oauthAccount": map[string]any{"accountUuid": claudeUUID(label), "emailAddress": rotaName(label)},
		"savedAt":      time.Now().UnixMilli(),
	}
	b, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *rotaEnv) writeRotaCodex(label string) string {
	r.t.Helper()
	d := filepath.Join(r.cfg.UserCodexDir, "accounts", rotaName(label))
	os.MkdirAll(d, 0o700)
	loginCodex(r.t, d, label, false)
	return filepath.Join(d, "auth.json")
}

// addRotaAccount registers an account backed by a rota snapshot.
func (r *rotaEnv) addRotaAccount(id string, h contracts.Harness, label string, independent bool) ProfileInfo {
	r.t.Helper()
	a := contracts.Account{AccountID: id, Provider: contracts.ProviderFor(h), Label: label, Kind: "subscription", RotaRef: rotaName(label)}
	if h == contracts.HarnessClaude {
		a.ProviderRef = claudeUUID(label)
	} else {
		a.ProviderRef = codexAcct(label)
	}
	if independent {
		r.svc.cfg.Rota.Independent = map[string]bool{id: true}
	}
	info, err := r.svc.CreateRotaProfile(context.Background(), a, h)
	if err != nil {
		r.t.Fatalf("CreateRotaProfile: %v", err)
	}
	return info
}

type claudeSnapFile struct {
	Name          string         `json:"name"`
	ClaudeAiOauth map[string]any `json:"claudeAiOauth"`
	OAuthAccount  map[string]any `json:"oauthAccount"`
	SavedAt       int64          `json:"savedAt"`
}

func readSnap(t *testing.T, p string) claudeSnapFile {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var s claudeSnapFile
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("snapshot no longer valid JSON: %v", err)
	}
	return s
}

// profileCred returns the profile credential (file or fake keychain) or nil.
func (r *rotaEnv) profileCred(dir, kcDir string) map[string]any {
	r.t.Helper()
	var b []byte
	if kcDir != "" {
		h, err := os.ReadFile(filepath.Join(kcDir, ClaudeKeychainService(dir)))
		if err == nil {
			b, _ = hex.DecodeString(string(h))
		}
	}
	if b == nil {
		if fb, err := os.ReadFile(filepath.Join(dir, ".credentials.json")); err == nil {
			b = fb
		}
	}
	if b == nil {
		return nil
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	o, _ := m["claudeAiOauth"].(map[string]any)
	return o
}

// simulateClaudeRefresh rewrites the profile credential as Claude does after
// a refresh (its own object: no rota-only keys).
func (r *rotaEnv) simulateClaudeRefresh(dir, kcDir, label, gen string, exp time.Time) {
	r.t.Helper()
	cred := map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "pyre-synthetic-" + label + "-access-" + gen, "refreshToken": "pyre-synthetic-" + label + "-refresh-" + gen,
		"expiresAt": exp.UnixMilli(), "scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max"}}
	b, _ := json.Marshal(cred)
	if kcDir != "" {
		os.WriteFile(filepath.Join(kcDir, ClaudeKeychainService(dir)), []byte(hex.EncodeToString(b)), 0o600)
		return
	}
	os.WriteFile(filepath.Join(dir, ".credentials.json"), b, 0o600)
}

// writeKeychainShim is a fake macOS `security` that also understands the
// `security -i` add command Claude and Pyre use (hex payload on stdin).
// <kc>/locked models a locked login keychain with the exit codes observed
// over ssh on macOS 26 (laptop): attribute lookups still work, reading a
// secret or deleting an existing item fails with 36
// (errSecInteractionNotAllowed), a missing item is 44, and writes fail.
func writeKeychainShim(t *testing.T, dir string) (shim, kc, logf string) {
	t.Helper()
	kc = filepath.Join(dir, "kc")
	os.MkdirAll(kc, 0o700)
	logf = filepath.Join(dir, "security.log")
	shim = filepath.Join(dir, "security")
	script := fmt.Sprintf(`#!/bin/sh
KC='%s'; LOG='%s'
if [ "$1" = "-i" ]; then
  line=$(cat)
  verb=$(printf '%%s' "$line" | awk '{print $1}')
  svc=$(printf '%%s' "$line" | sed -n 's/.*-s "\([^"]*\)".*/\1/p')
  hexv=$(printf '%%s' "$line" | sed -n 's/.*-X "\([0-9a-fA-F]*\)".*/\1/p')
  echo "-i:$verb $svc" >> "$LOG"
  if [ -f "$KC/locked" ]; then echo "security: SecKeychainItemCreateFromContent: User interaction is not allowed. returned -25308"; exit 36; fi
  printf '%%s' "$hexv" > "$KC/$svc"
  exit 0
fi
svc=""; prev=""
for a in "$@"; do [ "$prev" = "-s" ] && svc="$a"; prev="$a"; done
echo "$1 $svc" >> "$LOG"
wflag=""; for a in "$@"; do [ "$a" = "-w" ] && wflag=1; done
if [ -f "$KC/locked" ]; then
  case "$1" in
    find-generic-password)
      [ -f "$KC/$svc" ] || exit 44
      [ -n "$wflag" ] && exit 36
      echo "keychain: attributes of $svc"; exit 0 ;;
    delete-generic-password)
      [ -f "$KC/$svc" ] || exit 44
      exit 36 ;;
  esac
  exit 36
fi
case "$1" in
  find-generic-password)
    if [ -f "$KC/$svc" ]; then cat "$KC/$svc"; exit 0; fi
    echo "security: The specified item could not be found in the keychain." >&2; exit 44 ;;
  delete-generic-password)
    if [ -f "$KC/$svc" ]; then rm -f "$KC/$svc"; exit 0; fi
    exit 44 ;;
esac
exit 1
`, kc, logf)
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return shim, kc, logf
}
