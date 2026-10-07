package accounts

// Rota checkout: run Pyre sessions on the accounts rota already holds on a
// node, by TRANSFERRING a snapshot's login into an isolated Pyre profile for
// the duration of use and transferring it back afterwards.
//
// Invariant: at any moment exactly one store owns a refresh family on a
// node — the rota snapshot (no lock) or the Pyre profile (lock present).
//
//	checkout: lock (O_EXCL) -> fleet lease -> live-default check -> write the
//	          snapshot credential into the profile store
//	in use:   the harness refreshes inside the profile; a bounded poller
//	          copies rotated tokens back into the snapshot (rota format,
//	          other keys preserved) so rota's copy never goes stale
//	check-in: final write-back -> delete the profile credential -> release
//	          the fleet lease -> remove the lock
//
// Rota (pyre-integration branch) honours the lock: no refresh, usage
// refresh, switch or export of a locked account. The account that is the
// node's live default login is never checked out. Credentials are never
// logged, stored in the database, reported or returned.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// CredRotaCheckout is the credential kind for a grant backed by a rota
// snapshot checked out into a Pyre profile.
const CredRotaCheckout = contracts.CredRotaCheckout

// Node actions for checkouts.
const (
	ActionAccountCheckouts = contracts.ActionAccountCheckouts
	ActionAccountCheckin   = contracts.ActionAccountCheckin
)

// FleetLeaser is the hub-side lease on an account's refresh family. Rota
// snapshots may have been copied between machines (rota export/import), so
// an account is one family fleet-wide: at most one node may have it checked
// out. The daemon wires this to the hub (proposed accounts.lease.* API).
type FleetLeaser interface {
	AcquireAccountLease(ctx context.Context, req FleetLeaseRequest) (FleetLease, error)
	RenewAccountLease(ctx context.Context, leaseID string, ttl time.Duration) (FleetLease, error)
	ReleaseAccountLease(ctx context.Context, leaseID string) error
}

// FleetLeaseRequest asks for an account's refresh-family lease.
type FleetLeaseRequest struct {
	AccountID string
	NodeID    string
	Holder    string // "rota-checkout:<grant_id>"
	TTL       time.Duration
}

// FleetLease is a granted lease.
type FleetLease struct {
	LeaseID   string
	AccountID string
	NodeID    string
	ExpiresAt time.Time
}

// HomeBusyChecker reports whether a harness process still uses a profile
// (Codex adapter: the per-home app-server is running). Check-in waits until
// it is idle so no process can refresh after the credential moved back.
type HomeBusyChecker interface {
	HomeBusy(ctx context.Context, profileDir string) (bool, error)
}

// RotaConfig enables rota checkouts on a node.
type RotaConfig struct {
	Enabled bool
	// ClaudeAccountsDir is rota's Claude snapshot dir (default ~/.claude/accounts).
	ClaudeAccountsDir string
	// CodexHome is rota's CODEX_HOME (default Config.UserCodexDir); snapshots
	// live in <CodexHome>/accounts/<name>/auth.json, the live default login
	// in <CodexHome>/auth.json.
	CodexHome string
	// LiveClaudeJSON is the ~/.claude.json naming the node's live default
	// Claude login, which is never checked out (default
	// Config.UserClaudeJSON). Set it with ClaudeAccountsDir when rota lives
	// under another home than the daemon's (an isolated-HOME node).
	LiveClaudeJSON string
	// Fleet is the hub lease. nil or unreachable => checkout denied, unless
	// the account is listed in Independent.
	Fleet FleetLeaser
	// Independent lists account IDs the owner marked as a per-machine
	// independent login (never exported/imported), needing no fleet lease.
	Independent map[string]bool
	// HomeBusy optionally reports a still-running harness on a profile.
	HomeBusy HomeBusyChecker
	// ProfileUsers lists live processes bound to a profile through
	// CLAUDE_CONFIG_DIR / CODEX_HOME (default accounts.ProfileUsers; Linux).
	// Check-in waits for them: a helper that outlived the run (e.g. a memory
	// tool's detached enrichment calling `claude -p`) could otherwise refresh
	// the credential after it moved back to rota.
	ProfileUsers func(ctx context.Context, profileDir string) (pids []int, supported bool, err error)
	// SyncInterval bounds write-back latency (default 10s, min 2s).
	SyncInterval time.Duration
	// LeaseTTL is the fleet lease TTL (default 30m; renewed at half-life).
	LeaseTTL time.Duration
	// User is the keychain account attribute (default $USER).
	User string
}

// CheckoutView is the nonsecret state of a checkout.
type CheckoutView struct {
	GrantID      string            `json:"grant_id"`
	AccountID    string            `json:"account_id"`
	Harness      contracts.Harness `json:"harness"`
	RotaName     string            `json:"rota_name"`
	ProfileDir   string            `json:"profile_dir"`
	LockPath     string            `json:"lock_path"`
	State        string            `json:"state"` // active | pending_checkin | released
	FleetLeaseID string            `json:"fleet_lease_id,omitempty"`
	CheckedOutAt time.Time         `json:"checked_out_at"`
	LastSyncAt   *time.Time        `json:"last_sync_at,omitempty"`
	Detail       string            `json:"detail,omitempty"`
}

// LockInfo is the content of a rota snapshot lock file (no secrets).
type LockInfo struct {
	Holder       string    `json:"holder"` // "pyre"
	NodeID       string    `json:"node_id"`
	Hostname     string    `json:"hostname"`
	PID          int       `json:"pid"`
	AccountID    string    `json:"account_id"`
	GrantID      string    `json:"grant_id"`
	ProfileDir   string    `json:"profile_dir"`
	RunIDs       []string  `json:"run_ids"`
	CheckedOutAt time.Time `json:"checked_out_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func rotaMigration() string {
	return `
CREATE TABLE accounts_checkout (
	grant_id       TEXT PRIMARY KEY,
	account_id     TEXT NOT NULL,
	harness        TEXT NOT NULL,
	rota_name      TEXT NOT NULL,
	snapshot_path  TEXT NOT NULL,
	lock_path      TEXT NOT NULL,
	profile_dir    TEXT NOT NULL,
	fleet_lease_id TEXT NOT NULL DEFAULT '',
	fleet_expires  TEXT NOT NULL DEFAULT '',
	state          TEXT NOT NULL,
	checked_out_at TEXT NOT NULL,
	last_sync_at   TEXT NOT NULL DEFAULT '',
	detail         TEXT NOT NULL DEFAULT ''
);`
}

const (
	coActive   = "active"
	coPending  = "pending_checkin"
	coReleased = "released"
)

type checkoutRow struct {
	CheckoutView
	SnapshotPath string
	FleetExpires time.Time
}

func (s *Service) rotaDefaults() {
	r := &s.cfg.Rota
	home, _ := os.UserHomeDir()
	if r.ClaudeAccountsDir == "" && home != "" {
		r.ClaudeAccountsDir = filepath.Join(home, ".claude", "accounts")
	}
	if r.CodexHome == "" {
		r.CodexHome = s.cfg.UserCodexDir
	}
	if r.LiveClaudeJSON == "" {
		r.LiveClaudeJSON = s.cfg.UserClaudeJSON
	}
	if r.ProfileUsers == nil {
		r.ProfileUsers = ProfileUsers
	}
	if r.SyncInterval <= 0 {
		r.SyncInterval = 10 * time.Second
	}
	if r.SyncInterval < 2*time.Second && !s.cfg.TestMode {
		r.SyncInterval = 2 * time.Second
	}
	if r.LeaseTTL <= 0 {
		r.LeaseTTL = 30 * time.Minute
	}
	if r.User == "" {
		r.User = os.Getenv("USER")
	}
}

// ---------- paths and snapshot formats ----------

func validRotaName(n string) bool {
	if n == "" || strings.Trim(n, ".") == "" || len(n) > 200 {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._@-", c)) {
			return false
		}
	}
	return true
}

// snapshotPaths returns (snapshot file, lock file) for a rota account name.
func (s *Service) snapshotPaths(h contracts.Harness, name string) (string, string, error) {
	if !validRotaName(name) {
		return "", "", contracts.Errorf(contracts.ErrInvalid, "invalid rota account name")
	}
	switch h {
	case contracts.HarnessClaude:
		d := s.cfg.Rota.ClaudeAccountsDir
		return filepath.Join(d, name+".json"), filepath.Join(d, name+".pyre-lock"), nil
	case contracts.HarnessCodex:
		d := filepath.Join(s.cfg.Rota.CodexHome, "accounts", name)
		return filepath.Join(d, "auth.json"), filepath.Join(d, "pyre-lock"), nil
	}
	return "", "", contracts.Errorf(contracts.ErrInvalid, "harness %s has no rota snapshots", h)
}

// claudeSnapshot is the rota snapshot: {"name", "claudeAiOauth", "oauthAccount", "savedAt", "authDead"?}.
type claudeSnapshot struct {
	raw          map[string]json.RawMessage
	oauth        map[string]json.RawMessage // claudeAiOauth, all keys preserved
	accountUUID  string
	refreshToken secret
	accessToken  secret
	expiresAt    int64
	dead         bool
}

func readClaudeSnapshot(p string) (*claudeSnapshot, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	sn := &claudeSnapshot{}
	if err := json.Unmarshal(b, &sn.raw); err != nil {
		return nil, fmt.Errorf("snapshot unparseable")
	}
	if json.Unmarshal(sn.raw["claudeAiOauth"], &sn.oauth) != nil || sn.oauth == nil {
		return nil, fmt.Errorf("snapshot has no claudeAiOauth")
	}
	var o struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
	}
	_ = json.Unmarshal(sn.raw["claudeAiOauth"], &o)
	sn.accessToken, sn.refreshToken, sn.expiresAt = secret{o.AccessToken}, secret{o.RefreshToken}, o.ExpiresAt
	var acct struct {
		AccountUUID string `json:"accountUuid"`
	}
	_ = json.Unmarshal(sn.raw["oauthAccount"], &acct)
	sn.accountUUID = acct.AccountUUID
	_, sn.dead = sn.raw["authDead"]
	return sn, nil
}

// claudeProfileOAuth reads the profile store's claudeAiOauth object (all
// keys) plus its tokens, from keychain (macOS) or file.
func (s *Service) claudeProfileOAuth(ctx context.Context, dir string) (map[string]json.RawMessage, storeInfo, error) {
	si := s.readClaudeStore(ctx, grantRow{DeviceGrant: contracts.DeviceGrant{ProfileDir: dir, Kind: CredRotaCheckout}})
	if si.Locked != "" {
		return nil, si, errors.New(si.Locked)
	}
	if !si.Present {
		return nil, si, nil
	}
	raw, err := s.readClaudeStoreRaw(ctx, dir)
	if err != nil {
		return nil, si, err
	}
	var top map[string]json.RawMessage
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || json.Unmarshal(top["claudeAiOauth"], &o) != nil {
		return nil, si, fmt.Errorf("profile credential unparseable")
	}
	return o, si, nil
}

// readClaudeStoreRaw returns the raw JSON of the profile's credential
// (keychain item preferred on macOS, then file). Never logged.
func (s *Service) readClaudeStoreRaw(ctx context.Context, dir string) ([]byte, error) {
	if s.cfg.GOOS == "darwin" {
		out, code, err := s.runSecurity(ctx, "find-generic-password", "-s", ClaudeKeychainService(dir), "-w")
		if err == nil && code == 0 {
			b := bytes.TrimSpace(out)
			if len(b) > 0 && b[0] != '{' {
				if dec, derr := hex.DecodeString(string(b)); derr == nil {
					b = dec
				}
			}
			var probe storeInfo
			parseClaudeStore(b, &probe)
			if probe.Present {
				return b, nil
			}
		}
	}
	return os.ReadFile(filepath.Join(dir, ".credentials.json"))
}

// storeFile labels the profile credential store in checkout details.
const storeFile = "file"

// writeClaudeProfileStore writes {"claudeAiOauth": oauth} into the
// profile's 0600 <dir>/.credentials.json (in the 0700 profile, the same
// protection as rota's own snapshot; deleted at check-in), on every OS.
//
// Pyre never creates a macOS keychain item for a profile: an item created by
// pyred (a LaunchAgent) needs a keychain ACL approval before the `claude`
// binary may read it, so Claude blocked on an invisible GUI prompt with a
// blank pane and no SessionStart. With no item for its CLAUDE_CONFIG_DIR,
// Claude reads (and refreshes) the file. A leftover Pyre-created item for
// this profile would shadow the file, so it is moved out of the way first
// (removed; the caller writes the current credential); if it cannot be
// removed (locked keychain) the checkout is refused.
func (s *Service) writeClaudeProfileStore(ctx context.Context, dir string, oauth map[string]json.RawMessage) (string, error) {
	ob, _ := json.Marshal(oauth)
	payload, _ := json.Marshal(map[string]json.RawMessage{"claudeAiOauth": ob})
	if s.cfg.GOOS == "darwin" {
		if err := s.removeProfileKeychainItem(ctx, dir); err != nil {
			return "", err
		}
	}
	return storeFile, writeFileAtomic(filepath.Join(dir, ".credentials.json"), payload, 0o600)
}

// removeProfileKeychainItem deletes the keychain item Claude would use for
// exactly this CLAUDE_CONFIG_DIR ("Claude Code-credentials-<sha256(dir)[:8]>").
// The owner's default "Claude Code-credentials" item and items of other
// config dirs are never addressed. Attribute lookup works on a locked
// keychain; deletion of an existing item may not.
func (s *Service) removeProfileKeychainItem(ctx context.Context, dir string) error {
	svc := ClaudeKeychainService(dir)
	_, code, err := s.runSecurity(ctx, "find-generic-password", "-s", svc)
	if err != nil || code == 44 {
		return nil // no item (or no security tool: Claude cannot use one either)
	}
	if code != 0 {
		return fmt.Errorf("cannot check for a keychain item %q (security exit %d)", svc, code)
	}
	_, dcode, derr := s.runSecurity(ctx, "delete-generic-password", "-s", svc)
	if derr != nil || dcode != 0 && dcode != 44 {
		return fmt.Errorf("a keychain item %q for the profile shadows its credential file and could not be removed (security exit %d); unlock the login keychain", svc, dcode)
	}
	s.log.Info("accounts: removed the profile's keychain item; the profile uses its credential file", "profile", dir, "item", svc)
	return nil
}

// MigrateProfileKeychain moves credentials that earlier Pyre versions stored
// in macOS keychain items for rota-checkout profiles into each profile's
// 0600 .credentials.json and deletes those items (one item per profile:
// "Claude Code-credentials-<hash of that profile dir>"). A checked-out
// profile keeps its live credential (the item's content, unless the file is
// at least as new); for a checked-in profile the item is a leftover (the
// rota snapshot is authoritative) and is only deleted. An unreadable item
// (locked keychain) is left for the next start. Nothing secret is logged.
func (s *Service) MigrateProfileKeychain(ctx context.Context) {
	if s.cfg.GOOS != "darwin" {
		return
	}
	gs, err := s.listGrants(ctx, s.db, "")
	if err != nil {
		return
	}
	for _, g := range gs {
		if g.Kind != CredRotaCheckout || g.Harness != contracts.HarnessClaude || g.ProfileDir == "" {
			continue
		}
		s.comu.Lock()
		s.migrateOne(ctx, g)
		s.comu.Unlock()
	}
}

func (s *Service) migrateOne(ctx context.Context, g grantRow) {
	dir := g.ProfileDir
	svc := ClaudeKeychainService(dir)
	if _, code, err := s.runSecurity(ctx, "find-generic-password", "-s", svc); err != nil || code != 0 {
		return
	}
	if s.checkedOut(ctx, g.GrantID) {
		out, code, err := s.runSecurity(ctx, "find-generic-password", "-s", svc, "-w")
		if err != nil || code != 0 {
			s.log.Warn("accounts: profile keychain item unreadable; migration to the credential file deferred (unlock the login keychain)", "profile", dir, "security_exit", code)
			return
		}
		b := bytes.TrimSpace(out)
		if len(b) > 0 && b[0] != '{' {
			if dec, derr := hex.DecodeString(string(b)); derr == nil {
				b = dec
			}
		}
		var item, file storeInfo
		parseClaudeStore(b, &item)
		fb, ferr := os.ReadFile(filepath.Join(dir, ".credentials.json"))
		if ferr == nil {
			parseClaudeStore(fb, &file)
		}
		if item.Present && (!file.Present || item.ExpiresAt.After(file.ExpiresAt)) {
			if err := writeFileAtomic(filepath.Join(dir, ".credentials.json"), b, 0o600); err != nil {
				s.log.Warn("accounts: profile keychain migration: credential file write failed; item kept", "profile", dir, "err", err)
				return
			}
		}
	}
	if _, dcode, derr := s.runSecurity(ctx, "delete-generic-password", "-s", svc); derr != nil || dcode != 0 && dcode != 44 {
		s.log.Warn("accounts: profile keychain item could not be deleted; retried at the next start", "profile", dir, "security_exit", dcode)
		return
	}
	s.log.Info("accounts: migrated the profile's keychain item to its credential file", "profile", dir, "item", svc)
}

// deleteClaudeProfileStore removes the profile credential from keychain and file.
func (s *Service) deleteClaudeProfileStore(ctx context.Context, dir string) error {
	if s.cfg.GOOS == "darwin" {
		_, code, err := s.runSecurity(ctx, "delete-generic-password", "-s", ClaudeKeychainService(dir))
		if err != nil || (code != 0 && code != 44) {
			return fmt.Errorf("keychain delete failed (exit %d)", code)
		}
	}
	if err := os.Remove(filepath.Join(dir, ".credentials.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ---------- locks ----------

func readLock(p string) (*LockInfo, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var l LockInfo
	if json.Unmarshal(b, &l) != nil {
		return &LockInfo{}, nil // unparseable lock still blocks
	}
	return &l, nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (s *Service) newLock(g grantRow, runID string) LockInfo {
	host, _ := os.Hostname()
	now := s.now()
	return LockInfo{Holder: "pyre", NodeID: s.cfg.NodeID, Hostname: host, PID: os.Getpid(), AccountID: g.AccountID,
		GrantID: g.GrantID, ProfileDir: g.ProfileDir, RunIDs: []string{runID}, CheckedOutAt: now, UpdatedAt: now}
}

// createLock atomically creates the lock file (O_EXCL).
func createLock(p string, l LockInfo) error {
	b, _ := json.MarshalIndent(l, "", "  ")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(p)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(p)
		return err
	}
	return f.Close()
}

func updateLock(p string, l LockInfo) error {
	b, _ := json.MarshalIndent(l, "", "  ")
	return writeFileAtomic(p, append(b, '\n'), 0o600)
}

// ---------- live default identity ----------

// liveDefaultIdentity returns the provider account id of the node's live
// default login (non-secret, decoded locally). "" = no default login.
func (s *Service) liveDefaultIdentity(h contracts.Harness) (string, error) {
	switch h {
	case contracts.HarnessClaude:
		if s.cfg.Rota.LiveClaudeJSON == "" {
			return "", nil
		}
		b, err := os.ReadFile(s.cfg.Rota.LiveClaudeJSON)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("cannot read the default Claude identity: %v", err)
		}
		var c struct {
			OAuthAccount *struct {
				AccountUUID string `json:"accountUuid"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(b, &c) != nil {
			return "", fmt.Errorf("default ~/.claude.json unparseable; refusing checkout")
		}
		if c.OAuthAccount == nil {
			return "", nil
		}
		return c.OAuthAccount.AccountUUID, nil
	case contracts.HarnessCodex:
		if s.cfg.Rota.CodexHome == "" {
			return "", nil
		}
		p := filepath.Join(s.cfg.Rota.CodexHome, "auth.json")
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		si, ident := readCodexStore(s.cfg.Rota.CodexHome)
		if si.Exists && si.Present && ident == "" {
			return "", fmt.Errorf("default Codex identity undecodable; refusing checkout")
		}
		return ident, nil
	}
	return "", nil
}

// snapshotIdentity returns (identity, dead, error) of a rota snapshot.
func (s *Service) snapshotIdentity(h contracts.Harness, snapPath string) (string, bool, error) {
	switch h {
	case contracts.HarnessClaude:
		sn, err := readClaudeSnapshot(snapPath)
		if err != nil {
			return "", false, err
		}
		if sn.refreshToken.empty() {
			return sn.accountUUID, true, nil
		}
		return sn.accountUUID, sn.dead, nil
	case contracts.HarnessCodex:
		si, ident := readCodexStore(filepath.Dir(snapPath))
		if !si.Exists {
			return "", false, fs.ErrNotExist
		}
		return ident, !si.Present, nil
	}
	return "", false, fmt.Errorf("unsupported harness")
}

// ---------- profile + grant ----------

// CreateRotaProfile prepares the isolated profile for an account backed by
// the rota snapshot named by account.RotaRef and registers a rota-checkout
// grant. Nothing is checked out until a run is assigned.
func (s *Service) CreateRotaProfile(ctx context.Context, account contracts.Account, harness contracts.Harness) (ProfileInfo, error) {
	if !s.cfg.Rota.Enabled {
		return ProfileInfo{}, contracts.Errorf(contracts.ErrUnsupported, "rota checkouts are not enabled on this node")
	}
	snap, _, err := s.snapshotPaths(harness, account.RotaRef)
	if err != nil {
		return ProfileInfo{}, err
	}
	if _, err := os.Stat(snap); err != nil {
		return ProfileInfo{}, contracts.Errorf(contracts.ErrNotFound, "rota snapshot %q not found on this node", account.RotaRef)
	}
	return s.createProfile(ctx, account, harness, CredRotaCheckout)
}

// ---------- checkout ----------

func (s *Service) getCheckout(ctx context.Context, q querier, grantID string) (*checkoutRow, error) {
	var c checkoutRow
	var harness, fexp, at, sync string
	err := q.QueryRowContext(ctx, `SELECT grant_id,account_id,harness,rota_name,snapshot_path,lock_path,profile_dir,fleet_lease_id,fleet_expires,state,checked_out_at,last_sync_at,detail
		FROM accounts_checkout WHERE grant_id=?`, grantID).Scan(&c.GrantID, &c.AccountID, &harness, &c.RotaName, &c.SnapshotPath, &c.LockPath,
		&c.ProfileDir, &c.FleetLeaseID, &fexp, &c.State, &at, &sync, &c.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Harness = contracts.Harness(harness)
	c.FleetExpires, c.CheckedOutAt = pt(fexp), pt(at)
	if sync != "" {
		t := pt(sync)
		c.LastSyncAt = &t
	}
	return &c, nil
}

func (s *Service) listCheckouts(ctx context.Context, states ...string) ([]checkoutRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT grant_id FROM accounts_checkout ORDER BY grant_id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []checkoutRow
	for _, id := range ids {
		c, err := s.getCheckout(ctx, s.db, id)
		if err != nil || c == nil {
			continue
		}
		if len(states) == 0 || contains(states, c.State) {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (s *Service) putCheckout(ctx context.Context, c checkoutRow) error {
	sync := ""
	if c.LastSyncAt != nil {
		sync = ft(*c.LastSyncAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts_checkout(grant_id,account_id,harness,rota_name,snapshot_path,lock_path,profile_dir,fleet_lease_id,fleet_expires,state,checked_out_at,last_sync_at,detail)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(grant_id) DO UPDATE SET account_id=excluded.account_id, harness=excluded.harness,
		rota_name=excluded.rota_name, snapshot_path=excluded.snapshot_path, lock_path=excluded.lock_path, profile_dir=excluded.profile_dir,
		fleet_lease_id=excluded.fleet_lease_id, fleet_expires=excluded.fleet_expires, state=excluded.state, checked_out_at=excluded.checked_out_at,
		last_sync_at=excluded.last_sync_at, detail=excluded.detail`,
		c.GrantID, c.AccountID, string(c.Harness), c.RotaName, c.SnapshotPath, c.LockPath, c.ProfileDir, c.FleetLeaseID, ft(c.FleetExpires),
		c.State, ft(c.CheckedOutAt), sync, c.Detail)
	return err
}

// checkedOut reports whether the grant is checked out on this node.
func (s *Service) checkedOut(ctx context.Context, grantID string) bool {
	c, err := s.getCheckout(ctx, s.db, grantID)
	return err == nil && c != nil && c.State != coReleased
}

// verifyRotaGrant checks a not-yet-checked-out rota grant against its
// snapshot: present, not dead, identity == ProviderRef, not the live
// default login, not locked by another holder.
func (s *Service) verifyRotaGrant(a contracts.Account, g grantRow) verdict {
	v := verdict{Grant: g, Outcome: contracts.FallbackIntervention}
	now := s.now()
	set := func(state contracts.GrantState, detail string) {
		if v.Grant.State != state || v.Grant.Detail != detail {
			v.Grant.State, v.Grant.Detail, v.Grant.ObservedAt, v.Changed = state, detail, now, true
		}
	}
	switch g.State {
	case contracts.GrantDead, contracts.GrantRevoked, contracts.GrantExpired:
		v.Reason = fmt.Sprintf("grant %s; re-auth in rota (`rota reauth`) then reverify", g.State)
		return v
	}
	snap, lock, err := s.snapshotPaths(g.Harness, a.RotaRef)
	if err != nil {
		v.Reason = "account has no valid rota_ref"
		return v
	}
	if l, err := readLock(lock); err == nil {
		if l.NodeID != s.cfg.NodeID || l.GrantID != g.GrantID {
			v.Outcome = contracts.FallbackWaiting
			v.Reason = fmt.Sprintf("rota account in use by Pyre on node %s", l.NodeID)
			return v
		}
	}
	ident, dead, err := s.snapshotIdentity(g.Harness, snap)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		set(contracts.GrantUnknown, "rota snapshot missing")
		v.Reason = "rota snapshot missing on this node"
		return v
	case err != nil:
		v.Reason = "rota snapshot unreadable: " + err.Error()
		return v
	case dead:
		set(contracts.GrantDead, "rota snapshot marked dead or has no refresh token; `rota reauth` required")
		v.Reason = v.Grant.Detail
		return v
	}
	live, err := s.liveDefaultIdentity(g.Harness)
	if err != nil {
		v.Reason = err.Error()
		return v
	}
	if ident != "" && ident == live {
		v.Reason = "rota account is the node's live default login; never checked out"
		return v
	}
	if a.ProviderRef == "" {
		v.Reason = fmt.Sprintf("account has no provider_ref; confirm the rota snapshot identity %q first", ident)
		return v
	}
	if ident != a.ProviderRef {
		set(v.Grant.State, fmt.Sprintf("identity mismatch: expected %s, rota snapshot has %s", a.ProviderRef, ident))
		v.Reason = v.Grant.Detail
		return v
	}
	if g.State != contracts.GrantValid {
		if !g.EverValid {
			v.Grant.Generation++
		}
		v.Grant.EverValid, v.Grant.IdentityRef = true, ident
		set(contracts.GrantValid, "rota snapshot verified locally (checked in)")
	}
	v.OK = true
	return v
}

// ensureCheckout transfers the rota login into the grant's profile for
// runID (no-op when this node already holds it; the run is added to the lock).
func (s *Service) ensureCheckout(ctx context.Context, a contracts.Account, g grantRow, runID string) error {
	s.comu.Lock()
	defer s.comu.Unlock()
	if c, err := s.getCheckout(ctx, s.db, g.GrantID); err != nil {
		return err
	} else if c != nil && c.State != coReleased {
		if l, err := readLock(c.LockPath); err == nil && l.NodeID == s.cfg.NodeID {
			if !contains(l.RunIDs, runID) {
				l.RunIDs = append(l.RunIDs, runID)
			}
			l.PID, l.UpdatedAt = os.Getpid(), s.now()
			_ = updateLock(c.LockPath, *l)
		}
		if c.State == coPending {
			c.State, c.Detail = coActive, "re-used before check-in"
			return s.putCheckout(ctx, *c)
		}
		return nil
	}
	snap, lockPath, err := s.snapshotPaths(g.Harness, a.RotaRef)
	if err != nil {
		return err
	}
	// 1. Exclusive node-local lock (also what rota honours).
	if err := createLock(lockPath, s.newLock(g, runID)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			l, _ := readLock(lockPath)
			node := "?"
			if l != nil {
				node = l.NodeID
			}
			return contracts.Errorf(contracts.ErrConflict, "rota account %s is checked out by Pyre on node %s", a.RotaRef, node).
				WithDetail("outcome", string(contracts.FallbackWaiting))
		}
		return fmt.Errorf("rota lock: %w", err)
	}
	unlock := func() { os.Remove(lockPath) }
	// 2. Fleet refresh-family lease (deny by default).
	var lease FleetLease
	if !s.cfg.Rota.Independent[a.AccountID] {
		if s.cfg.Rota.Fleet == nil {
			unlock()
			return contracts.Errorf(contracts.ErrUnavailable, "no hub lease service; rota checkout of %s denied (mark the account per-machine independent to allow)", a.AccountID).
				WithDetail("outcome", string(contracts.FallbackIntervention))
		}
		lease, err = s.cfg.Rota.Fleet.AcquireAccountLease(ctx, FleetLeaseRequest{AccountID: a.AccountID, NodeID: s.cfg.NodeID,
			Holder: "rota-checkout:" + g.GrantID, TTL: s.cfg.Rota.LeaseTTL})
		if err != nil {
			unlock()
			outcome := contracts.FallbackIntervention
			if contracts.IsCode(err, contracts.ErrConflict) {
				outcome = contracts.FallbackWaiting
			}
			return contracts.Errorf(contracts.ErrUnavailable, "fleet lease for %s denied: %s", a.AccountID, redact(err.Error())).
				WithDetail("outcome", string(outcome))
		}
	}
	fail := func(e error) error {
		if lease.LeaseID != "" {
			_ = s.cfg.Rota.Fleet.ReleaseAccountLease(context.WithoutCancel(ctx), lease.LeaseID)
		}
		unlock()
		return e
	}
	// 3. Re-check under the lock: never the live default; snapshot still valid.
	ident, dead, err := s.snapshotIdentity(g.Harness, snap)
	if err != nil || dead {
		return fail(contracts.Errorf(contracts.ErrUnavailable, "rota snapshot not usable").WithDetail("outcome", string(contracts.FallbackIntervention)))
	}
	if live, lerr := s.liveDefaultIdentity(g.Harness); lerr != nil || (ident != "" && ident == live) {
		return fail(contracts.Errorf(contracts.ErrForbidden, "refusing to check out the live default login").WithDetail("outcome", string(contracts.FallbackIntervention)))
	}
	if ident != a.ProviderRef {
		return fail(contracts.Errorf(contracts.ErrUnavailable, "rota snapshot identity changed").WithDetail("outcome", string(contracts.FallbackIntervention)))
	}
	// 4. Transfer into the profile store.
	store, err := s.transferIn(ctx, g, snap)
	if err != nil {
		_ = s.removeProfileCredential(context.WithoutCancel(ctx), g)
		return fail(contracts.Errorf(contracts.ErrUnavailable, "checkout write failed: %s", redact(err.Error())).WithDetail("outcome", string(contracts.FallbackIntervention)))
	}
	now := s.now()
	c := checkoutRow{CheckoutView: CheckoutView{GrantID: g.GrantID, AccountID: a.AccountID, Harness: g.Harness, RotaName: a.RotaRef,
		ProfileDir: g.ProfileDir, LockPath: lockPath, State: coActive, FleetLeaseID: lease.LeaseID, CheckedOutAt: now, LastSyncAt: &now,
		Detail: "checked out; profile store: " + store},
		SnapshotPath: snap, FleetExpires: lease.ExpiresAt}
	if err := s.putCheckout(ctx, c); err != nil {
		_ = s.removeProfileCredential(context.WithoutCancel(ctx), g)
		return fail(err)
	}
	s.log.Info("accounts: rota account checked out", "account", a.AccountID, "rota", a.RotaRef, "profile", g.ProfileDir, "store", store)
	return nil
}

// transferIn writes the snapshot's credential into the profile store and
// returns which store holds it.
func (s *Service) transferIn(ctx context.Context, g grantRow, snap string) (string, error) {
	switch g.Harness {
	case contracts.HarnessClaude:
		sn, err := readClaudeSnapshot(snap)
		if err != nil {
			return "", err
		}
		store, err := s.writeClaudeProfileStore(ctx, g.ProfileDir, sn.oauth)
		if err != nil {
			return "", err
		}
		// Identity for Claude and for Pyre's pre-launch check (non-secret).
		if acct, ok := sn.raw["oauthAccount"]; ok {
			p := filepath.Join(g.ProfileDir, ".claude.json")
			m := map[string]json.RawMessage{}
			if b, err := os.ReadFile(p); err == nil {
				_ = json.Unmarshal(b, &m)
			}
			m["oauthAccount"] = acct
			out, _ := json.MarshalIndent(m, "", "  ")
			if err := writeFileAtomic(p, out, 0o600); err != nil {
				return "", err
			}
		}
		return store, nil
	case contracts.HarnessCodex:
		b, err := os.ReadFile(snap)
		if err != nil {
			return "", err
		}
		return storeFile, writeFileAtomic(filepath.Join(g.ProfileDir, "auth.json"), b, 0o600)
	}
	return "", fmt.Errorf("unsupported harness")
}

func (s *Service) removeProfileCredential(ctx context.Context, g grantRow) error {
	switch g.Harness {
	case contracts.HarnessClaude:
		return s.deleteClaudeProfileStore(ctx, g.ProfileDir)
	case contracts.HarnessCodex:
		if err := os.Remove(filepath.Join(g.ProfileDir, "auth.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ---------- write-back ----------

// writeBack copies a newer profile credential into the rota snapshot,
// preserving rota's format and other keys. It never writes a blank or older
// credential, and never overwrites a snapshot someone else advanced.
func (s *Service) writeBack(ctx context.Context, c *checkoutRow) (bool, error) {
	if !s.lockOurs(c) {
		return false, errLockRevoked
	}
	switch c.Harness {
	case contracts.HarnessClaude:
		prof, _, err := s.claudeProfileOAuth(ctx, c.ProfileDir)
		if err != nil {
			return false, err
		}
		if prof == nil {
			return false, fmt.Errorf("profile credential missing or blanked; not written back (rota will report the login state itself)")
		}
		sn, err := readClaudeSnapshot(c.SnapshotPath)
		if err != nil {
			return false, err
		}
		var pr struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		}
		pb, _ := json.Marshal(prof)
		_ = json.Unmarshal(pb, &pr)
		if pr.RefreshToken == "" {
			return false, fmt.Errorf("profile credential has no refresh token; not written back")
		}
		if pr.RefreshToken == sn.refreshToken.reveal() && pr.AccessToken == sn.accessToken.reveal() {
			return false, nil // in sync
		}
		if pr.ExpiresAt < sn.expiresAt {
			return false, fmt.Errorf("rota snapshot is newer than the profile credential (changed while checked out); not overwritten")
		}
		merged := map[string]json.RawMessage{}
		for k, v := range sn.oauth {
			merged[k] = v // keep rota-only keys (refreshTokenExpiresAt, ...)
		}
		for k, v := range prof {
			merged[k] = v
		}
		mb, _ := json.Marshal(merged)
		sn.raw["claudeAiOauth"] = mb
		return true, writeRotaJSON(c.SnapshotPath, sn.raw)
	case contracts.HarnessCodex:
		pb, err := os.ReadFile(filepath.Join(c.ProfileDir, "auth.json"))
		if err != nil {
			return false, fmt.Errorf("profile auth.json missing; not written back")
		}
		sb, err := os.ReadFile(c.SnapshotPath)
		if err != nil {
			return false, err
		}
		if bytes.Equal(pb, sb) {
			return false, nil
		}
		psi, pid := readCodexStore(c.ProfileDir)
		if !psi.Present {
			return false, fmt.Errorf("profile auth.json has no login; not written back")
		}
		_, sid := readCodexStore(filepath.Dir(c.SnapshotPath))
		if sid != "" && pid != sid {
			return false, fmt.Errorf("profile identity differs from rota snapshot; not written back")
		}
		var pl, sl struct {
			LastRefresh string `json:"last_refresh"`
		}
		_ = json.Unmarshal(pb, &pl)
		_ = json.Unmarshal(sb, &sl)
		if pl.LastRefresh != "" && sl.LastRefresh != "" && pl.LastRefresh < sl.LastRefresh {
			return false, fmt.Errorf("rota snapshot is newer than the profile credential; not overwritten")
		}
		return true, writeSnapshotBytes(c.SnapshotPath, pb)
	}
	return false, nil
}

var errLockRevoked = errors.New("rota lock removed or taken over (rota pyre-unlock?); the rota snapshot is authoritative again, nothing written back")

// lockOurs reports whether the snapshot lock is still this checkout's.
func (s *Service) lockOurs(c *checkoutRow) bool {
	l, err := readLock(c.LockPath)
	return err == nil && l.NodeID == s.cfg.NodeID && l.GrantID == c.GrantID
}

// writeRotaJSON writes a snapshot as rota does (json indent 2 + newline,
// 0600, atomic rename) through a Pyre-unique temp name.
func writeRotaJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeSnapshotBytes(p, append(b, '\n'))
}

func writeSnapshotBytes(p string, b []byte) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".pyre-tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ---------- check-in ----------

// maybeCheckin checks a rota grant back in when no run (and no probe) uses
// it on this node.
func (s *Service) maybeCheckin(ctx context.Context, grantID string) {
	s.comu.Lock()
	defer s.comu.Unlock()
	c, err := s.getCheckout(ctx, s.db, grantID)
	if err != nil || c == nil || c.State == coReleased {
		return
	}
	if err := s.checkinLocked(ctx, c, false); err != nil {
		s.log.Warn("accounts: rota check-in deferred", "account", c.AccountID, "err", redact(err.Error()))
	}
}

// checkinLocked performs check-in (caller holds comu).
func (s *Service) checkinLocked(ctx context.Context, c *checkoutRow, force bool) error {
	act, err := s.activeAssignments(ctx, s.db, c.AccountID)
	if err != nil {
		return err
	}
	for _, a := range act {
		// Token runs on the same (converted) grant never use the checkout.
		if a.GrantID == c.GrantID && a.CredentialKind != contracts.CredOAuthToken {
			return contracts.Errorf(contracts.ErrConflict, "account still assigned to run %s", a.RunID)
		}
	}
	if l, _ := getLease(ctx, s.db, c.GrantID); l != nil {
		c.State, c.Detail = coPending, "process lease still held"
		_ = s.putCheckout(ctx, *c)
		return fmt.Errorf("process lease held by %s", l.Holder)
	}
	if s.cfg.Rota.HomeBusy != nil && !force {
		if busy, err := s.cfg.Rota.HomeBusy.HomeBusy(ctx, c.ProfileDir); err != nil || busy {
			c.State, c.Detail = coPending, "harness process still running on the profile"
			_ = s.putCheckout(ctx, *c)
			return fmt.Errorf("profile busy")
		}
	}
	if n := s.profileHolds(c.ProfileDir); n > 0 && !force {
		c.State, c.Detail = coPending, fmt.Sprintf("harness process(es) of this daemon still use the profile (%d hold(s), e.g. a transcript capture); checking in once they finish", n)
		_ = s.putCheckout(ctx, *c)
		return fmt.Errorf("profile held")
	}
	if s.cfg.Rota.ProfileUsers != nil && !force {
		if pids, ok, err := s.cfg.Rota.ProfileUsers(ctx, c.ProfileDir); ok && (err != nil || len(pids) > 0) {
			c.State, c.Detail = coPending, "harness process(es) still bound to the profile (pid "+pidList(pids)+"); checking in once they exit"
			if err != nil {
				c.Detail = "harness process check failed: " + redact(err.Error())
			}
			_ = s.putCheckout(ctx, *c)
			return fmt.Errorf("profile in use")
		}
	}
	// Final write-back. A refusal (newer snapshot / blank profile) is
	// recorded but does not block returning ownership to rota.
	if _, err := s.writeBack(ctx, c); err != nil {
		c.Detail = "final write-back skipped: " + redact(err.Error())
		s.log.Warn("accounts: rota write-back skipped at check-in", "account", c.AccountID, "err", redact(err.Error()))
	}
	g, err := s.grantByID(ctx, s.db, c.GrantID)
	if err != nil {
		return err
	}
	g.ProfileDir = c.ProfileDir // the checkout's profile (the grant may since have become a token grant)
	if err := s.removeProfileCredential(ctx, g); err != nil {
		c.State, c.Detail = coPending, "could not remove profile credential: "+redact(err.Error())
		_ = s.putCheckout(ctx, *c)
		return err // keep the lock: the profile may still hold a live copy
	}
	if c.FleetLeaseID != "" && s.cfg.Rota.Fleet != nil {
		if err := s.cfg.Rota.Fleet.ReleaseAccountLease(ctx, c.FleetLeaseID); err != nil {
			s.log.Warn("accounts: fleet lease release failed (expires on its own)", "account", c.AccountID, "err", redact(err.Error()))
		}
	}
	if s.lockOurs(c) {
		if err := os.Remove(c.LockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} // else: the lock was removed/taken over; never delete someone else's lock
	now := s.now()
	c.State, c.LastSyncAt = coReleased, &now
	if c.Detail == "" || strings.HasPrefix(c.Detail, "process lease") || strings.HasPrefix(c.Detail, "harness process") || strings.HasPrefix(c.Detail, "checked out") {
		c.Detail = "checked in"
	}
	s.log.Info("accounts: rota account checked in", "account", c.AccountID, "rota", c.RotaName)
	return s.putCheckout(ctx, *c)
}

// ---------- background sync and recovery ----------

// SyncCheckouts runs one write-back/renew/check-in pass.
func (s *Service) SyncCheckouts(ctx context.Context) {
	cs, err := s.listCheckouts(ctx, coActive, coPending)
	if err != nil {
		return
	}
	for i := range cs {
		c := &cs[i]
		s.comu.Lock()
		cur, _ := s.getCheckout(ctx, s.db, c.GrantID)
		if cur == nil || cur.State == coReleased {
			s.comu.Unlock()
			continue
		}
		c = cur
		if wrote, err := s.writeBack(ctx, c); err != nil {
			c.Detail = "write-back: " + redact(err.Error())
		} else {
			now := s.now()
			c.LastSyncAt = &now
			if wrote {
				c.Detail = "rotated credential written back to rota"
				s.log.Info("accounts: rota write-back", "account", c.AccountID)
			}
		}
		if c.FleetLeaseID != "" && s.cfg.Rota.Fleet != nil && s.now().After(c.FleetExpires.Add(-s.cfg.Rota.LeaseTTL/2)) {
			if l, err := s.cfg.Rota.Fleet.RenewAccountLease(ctx, c.FleetLeaseID, s.cfg.Rota.LeaseTTL); err == nil {
				c.FleetExpires = l.ExpiresAt
			} else {
				s.log.Warn("accounts: fleet lease renew failed", "account", c.AccountID, "err", redact(err.Error()))
			}
		}
		_ = s.putCheckout(ctx, *c)
		if c.State == coPending {
			_ = s.checkinLocked(ctx, c, false)
		}
		s.comu.Unlock()
	}
}

// RunRotaSync polls checkouts until ctx ends (bounded: one pass per
// SyncInterval, sequential).
func (s *Service) RunRotaSync(ctx context.Context) {
	if !s.cfg.Rota.Enabled {
		return
	}
	t := time.NewTicker(s.cfg.Rota.SyncInterval)
	defer t.Stop()
	for {
		s.SyncCheckouts(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RecoverRotaCheckouts runs at daemon start: assignments of runs that are no
// longer live are released, idle checkouts are checked in (with write-back),
// and lock files this node left behind without a database record are
// resolved by writing back the orphaned profile credential if newer.
func (s *Service) RecoverRotaCheckouts(ctx context.Context, live func(runID string) bool) error {
	if !s.cfg.Rota.Enabled {
		return nil
	}
	s.MigrateProfileKeychain(ctx)
	acts, err := s.activeAssignments(ctx, s.db, "")
	if err != nil {
		return err
	}
	for _, a := range acts {
		if a.CredentialKind == CredRotaCheckout && (live == nil || !live(a.RunID)) {
			if _, err := s.ReleaseRun(ctx, a.RunID); err != nil {
				s.log.Warn("accounts: recover release", "run", a.RunID, "err", err)
			}
		}
	}
	cs, _ := s.listCheckouts(ctx, coActive, coPending)
	for i := range cs {
		s.maybeCheckin(ctx, cs[i].GrantID)
	}
	return s.recoverOrphanLocks(ctx)
}

func (s *Service) recoverOrphanLocks(ctx context.Context) error {
	var locks []string
	if d := s.cfg.Rota.ClaudeAccountsDir; d != "" {
		m, _ := filepath.Glob(filepath.Join(d, "*.pyre-lock"))
		locks = append(locks, m...)
	}
	if d := s.cfg.Rota.CodexHome; d != "" {
		m, _ := filepath.Glob(filepath.Join(d, "accounts", "*", "pyre-lock"))
		locks = append(locks, m...)
	}
	s.comu.Lock()
	defer s.comu.Unlock()
	for _, lp := range locks {
		l, err := readLock(lp)
		if err != nil || l.NodeID != s.cfg.NodeID || l.GrantID == "" {
			continue // another node's (or foreign) lock: leave it
		}
		if c, _ := s.getCheckout(ctx, s.db, l.GrantID); c != nil && c.State != coReleased {
			continue // tracked; handled by check-in
		}
		if l.PID != os.Getpid() && pidAlive(l.PID) {
			continue // a live process (another daemon instance?) holds it
		}
		g, err := s.grantByID(ctx, s.db, l.GrantID)
		if err != nil {
			continue
		}
		g.ProfileDir = s.rotaProfileDir(ctx, g) // tokenmerge.go
		h := g.Harness
		snap := ""
		if h == contracts.HarnessClaude {
			snap = strings.TrimSuffix(lp, ".pyre-lock") + ".json"
		} else {
			snap = filepath.Join(filepath.Dir(lp), "auth.json")
		}
		c := &checkoutRow{CheckoutView: CheckoutView{GrantID: g.GrantID, AccountID: g.AccountID, Harness: h, ProfileDir: g.ProfileDir,
			LockPath: lp, State: coPending, CheckedOutAt: l.CheckedOutAt, Detail: "recovered orphan lock"}, SnapshotPath: snap}
		if _, err := s.writeBack(ctx, c); err != nil {
			s.log.Warn("accounts: orphan write-back skipped", "account", g.AccountID, "err", redact(err.Error()))
		}
		if err := s.removeProfileCredential(ctx, g); err != nil {
			continue
		}
		os.Remove(lp)
		c.State = coReleased
		_ = s.putCheckout(ctx, *c)
		s.log.Info("accounts: recovered orphaned rota checkout", "account", g.AccountID)
	}
	return nil
}

// Checkouts lists checkouts (non-secret).
func (s *Service) Checkouts(ctx context.Context) ([]CheckoutView, error) {
	cs, err := s.listCheckouts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CheckoutView, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.CheckoutView)
	}
	return out, nil
}

// Checkin is the owner-initiated check-in of an idle account.
func (s *Service) Checkin(ctx context.Context, accountID string) (CheckoutView, error) {
	gs, err := s.listGrants(ctx, s.db, accountID)
	if err != nil {
		return CheckoutView{}, err
	}
	for _, g := range gs {
		if g.Kind != CredRotaCheckout {
			continue
		}
		s.comu.Lock()
		c, err := s.getCheckout(ctx, s.db, g.GrantID)
		if err == nil && c != nil && c.State != coReleased {
			err = s.checkinLocked(ctx, c, false)
		}
		s.comu.Unlock()
		if err != nil {
			return CheckoutView{}, err
		}
		if c != nil {
			c2, _ := s.getCheckout(ctx, s.db, g.GrantID)
			return c2.CheckoutView, nil
		}
	}
	return CheckoutView{}, contracts.Errorf(contracts.ErrNotFound, "no rota checkout for account").WithRef(contracts.KindAccount, accountID)
}

// probeRotaSnapshot reads the usage of a checked-in Claude rota account with
// the snapshot's current access token (read-only GET; never refreshes; an
// expired token yields unknown). Codex snapshots are not probed (that would
// start a Codex process on rota's home).
func (s *Service) rotaSnapshotToken(g grantRow, a contracts.Account) (storeInfo, string) {
	snap, lock, err := s.snapshotPaths(g.Harness, a.RotaRef)
	if err != nil {
		return storeInfo{}, "invalid rota_ref"
	}
	if l, err := readLock(lock); err == nil && l.NodeID != s.cfg.NodeID {
		return storeInfo{}, "in use by Pyre on node " + l.NodeID + "; its observations come from that node"
	}
	if g.Harness != contracts.HarnessClaude {
		return storeInfo{}, "codex rota account not checked out; usage needs its app-server (unknown until checked out)"
	}
	sn, err := readClaudeSnapshot(snap)
	if err != nil {
		return storeInfo{}, "rota snapshot unreadable"
	}
	si := storeInfo{Exists: true, Present: !sn.accessToken.empty(), Source: "rota-snapshot", AccessToken: sn.accessToken}
	if sn.expiresAt > 0 {
		si.ExpiresAt = time.UnixMilli(sn.expiresAt).UTC()
	}
	return si, ""
}

// releaseRotaHook runs after an assignment release (outside the DB tx).
func (s *Service) releaseRotaHook(ctx context.Context, a assignmentRow) {
	if a.CredentialKind != CredRotaCheckout || a.GrantID == "" {
		return
	}
	if c, _ := s.getCheckout(ctx, s.db, a.GrantID); c != nil {
		if l, err := readLock(c.LockPath); err == nil && l.NodeID == s.cfg.NodeID {
			var keep []string
			for _, r := range l.RunIDs {
				if r != a.RunID {
					keep = append(keep, r)
				}
			}
			l.RunIDs, l.UpdatedAt = keep, s.now()
			_ = updateLock(c.LockPath, *l)
		}
	}
	s.maybeCheckin(ctx, a.GrantID)
}

// ActionAccountRegister registers an account on this node and prepares its
// profile: RegisterParams -> RegisterResult.
const ActionAccountRegister = contracts.ActionAccountRegister

// RegisterParams mirrors a hub account onto this node.
type RegisterParams = contracts.AccountRegisterParams

// RegisterResult is nonsecret.
type RegisterResult struct {
	Profile     ProfileInfo           `json:"profile"`
	ProviderRef string                `json:"provider_ref,omitempty"`
	Grant       contracts.DeviceGrant `json:"grant"`
	Verified    bool                  `json:"verified"`
	Detail      string                `json:"detail,omitempty"`
}

// Register mirrors an account onto the node and prepares its profile. A
// rota account must not be the node's live default login.
func (s *Service) Register(ctx context.Context, p RegisterParams) (RegisterResult, error) {
	var res RegisterResult
	a := p.Account
	if a.AccountID == "" || p.Harness == "" {
		return res, contracts.Errorf(contracts.ErrInvalid, "account.account_id and harness are required")
	}
	if a.Provider == "" {
		a.Provider = contracts.ProviderFor(p.Harness)
	}
	switch p.Source {
	case "rota":
		if !s.cfg.Rota.Enabled {
			return res, contracts.Errorf(contracts.ErrUnsupported, "rota checkouts are not enabled on this node (pyred --rota-checkout)")
		}
		snap, _, err := s.snapshotPaths(p.Harness, a.RotaRef)
		if err != nil {
			return res, err
		}
		ident, dead, err := s.snapshotIdentity(p.Harness, snap)
		if err != nil {
			return res, contracts.Errorf(contracts.ErrNotFound, "rota snapshot not usable on this node")
		}
		if dead {
			return res, contracts.Errorf(contracts.ErrUnavailable, "rota marks this account dead; `rota reauth` first")
		}
		live, err := s.liveDefaultIdentity(p.Harness)
		if err != nil {
			return res, contracts.Errorf(contracts.ErrUnavailable, "%v", err)
		}
		if ident != "" && ident == live {
			return res, contracts.Errorf(contracts.ErrForbidden, "rota account is the node's live default login; it is never checked out")
		}
		switch {
		case a.ProviderRef == "" && p.BindRotaIdentity:
			a.ProviderRef = ident
		case a.ProviderRef != "" && a.ProviderRef != ident:
			return res, contracts.Errorf(contracts.ErrConflict, "provider_ref does not match the rota snapshot identity")
		}
		info, err := s.CreateRotaProfile(ctx, a, p.Harness)
		if err != nil {
			return res, err
		}
		if err := s.UpsertAccount(ctx, a); err != nil {
			return res, err
		}
		res.Profile, res.ProviderRef = info, a.ProviderRef
	case contracts.SourceToken:
		return s.registerToken(ctx, a, p.Harness) // token.go
	case "login", "":
		info, err := s.CreateProfile(ctx, a, p.Harness)
		if err != nil {
			return res, err
		}
		res.Profile, res.ProviderRef = info, a.ProviderRef
	default:
		return res, contracts.Errorf(contracts.ErrInvalid, "source must be rota, login or token")
	}
	g, err := s.getGrant(ctx, s.db, a.AccountID, p.Harness)
	if err != nil {
		return res, err
	}
	if g.Kind == contracts.CredOAuthToken {
		// The account runs on its hub token here; this login was recorded
		// as the fallback (tokenmerge.go).
		res.Grant, res.Verified = g.DeviceGrant, g.State == contracts.GrantValid
		res.Detail = "the account's token is used on this node; this " + p.Source + " login is kept as the fallback"
		return res, nil
	}
	var v verdict
	if g.Kind == CredRotaCheckout && !s.checkedOut(ctx, g.GrantID) {
		v = s.verifyRotaGrant(a, g)
	} else {
		v = s.verifyGrant(ctx, a, g)
	}
	s.persistVerdict(ctx, v)
	if !v.Changed {
		s.report(ctx, contracts.ReportGrant, v.Grant.DeviceGrant)
	}
	res.Grant, res.Verified, res.Detail = v.Grant.DeviceGrant, v.OK, v.Reason
	return res, nil
}
