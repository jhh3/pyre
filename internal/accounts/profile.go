package accounts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/osutil"
)

// Link is one composed profile entry.
type Link struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

// ProfileInfo describes a prepared profile. It never contains credentials.
type ProfileInfo struct {
	AccountID string            `json:"account_id"`
	Harness   contracts.Harness `json:"harness"`
	Dir       string            `json:"dir"` // canonical absolute path
	GrantID   string            `json:"grant_id"`
	// LoginCommand is the exact command the owner runs to create a fresh,
	// independent device login inside the profile. Pyre never copies a login.
	LoginCommand string `json:"login_command"`
	// KeychainService is the macOS keychain item Claude will use (Claude only).
	KeychainService string   `json:"keychain_service,omitempty"`
	Linked          []Link   `json:"linked"`    // mirrored dirs: per-entry links to the user's entries (read-only sharing)
	Copied          []string `json:"copied"`    // per-launch copies of the user's files (writes stay in the profile)
	Shared          []Link   `json:"shared"`    // symlinks into Pyre's shared transcript store
	Generated       []string `json:"generated"` // profile-local files Pyre wrote (no credentials)
	Skipped         []string `json:"skipped"`   // composable items absent in the user's config
	Conflicts       []string `json:"conflicts"` // existing profile entries left untouched
	// Unavailable lists owner configuration left out of the profile because
	// Pyre cannot read it (e.g. a plugin marketplace under a macOS-protected
	// folder, which would block the harness at startup).
	Unavailable []string `json:"unavailable,omitempty"`
}

type sharedItem struct {
	name string
	dir  bool
}

var (
	// claudeShared is everything `claude --resume <uuid>` reads back about a
	// conversation (transcripts, rewind checkpoints, todo and task lists,
	// plans, per-session hook env, shell snapshots, prompt history). Every
	// Claude profile on a node links these to <SharedRoot>/claude, so a
	// session continues natively under any account (sharedstore.go). Login
	// state (.credentials.json, keychain, .claude.json) and the live-process
	// registry (sessions/) stay per profile.
	claudeShared = []sharedItem{{"projects", true}, {"history.jsonl", false}, {"file-history", true}, {"todos", true},
		{"plans", true}, {"tasks", true}, {"session-env", true}, {"shell-snapshots", true}}
	codexShared = []sharedItem{{"sessions", true}, {"archived_sessions", true}, {"session_index.jsonl", false}}
	// Credential stores are never created, copied, linked or read by
	// composition.
	credentialNames = map[string]bool{".credentials.json": true, "auth.json": true, "oauth-token": true}
)

// transcriptStore names the shared store entry that native resume depends on.
func transcriptStore(h contracts.Harness) string {
	if h == contracts.HarnessCodex {
		return "sessions"
	}
	return "projects"
}

// CreateProfile prepares the isolated profile for (account, harness) on this
// node and registers its device grant (state unknown until the owner logs in
// with LoginCommand and the identity verifies). It is idempotent.
func (s *Service) CreateProfile(ctx context.Context, account contracts.Account, harness contracts.Harness) (ProfileInfo, error) {
	kind := contracts.CredClaudeProfileStore
	if harness == contracts.HarnessCodex {
		kind = contracts.CredCodexHomeFile
	}
	return s.createProfile(ctx, account, harness, kind)
}

func (s *Service) createProfile(ctx context.Context, account contracts.Account, harness contracts.Harness, kind contracts.CredentialKind) (ProfileInfo, error) {
	var info ProfileInfo
	if contracts.ProviderFor(harness) == "" {
		return info, contracts.Errorf(contracts.ErrInvalid, "harness %q has no subscription profile", harness)
	}
	if account.Provider != contracts.ProviderFor(harness) {
		return info, contracts.Errorf(contracts.ErrInvalid, "account provider %q does not serve harness %q", account.Provider, harness)
	}
	if account.Kind != "" && account.Kind != "subscription" {
		return info, contracts.Errorf(contracts.ErrInvalid, "only subscription accounts get profiles (API billing is never a target)")
	}
	if err := safeSegment(account.AccountID); err != nil {
		return info, contracts.Errorf(contracts.ErrInvalid, "%v", err)
	}
	if err := s.UpsertAccount(ctx, account); err != nil {
		return info, err
	}

	root, err := ensureDir(s.cfg.ProfileRoot)
	if err != nil {
		return info, fmt.Errorf("accounts: profile root: %w", err)
	}
	userClaude := canonicalOrClean(s.cfg.UserClaudeDir)
	userCodex := canonicalOrClean(s.cfg.UserCodexDir)
	for _, u := range []string{userClaude, userCodex} {
		if within(root, u) {
			return info, contracts.Errorf(contracts.ErrInvalid, "profile root %s is inside the default config %s; refusing", root, u)
		}
	}
	if _, err := ensureDir(filepath.Join(root, account.AccountID)); err != nil {
		return info, err
	}
	sub := string(harness)
	if kind == contracts.CredOAuthToken {
		sub = s.tokenProfileSub(ctx, account.AccountID, harness) // tokenmerge.go
	}
	dir, err := ensureDir(filepath.Join(root, account.AccountID, sub))
	if err != nil {
		return info, err
	}
	for _, u := range []string{userClaude, userCodex} {
		if within(dir, u) || within(u, dir) {
			return info, contracts.Errorf(contracts.ErrInvalid, "profile %s overlaps the default config %s; refusing", dir, u)
		}
	}
	sharedRoot, err := ensureDir(filepath.Join(s.cfg.SharedRoot, string(harness)))
	if err != nil {
		return info, fmt.Errorf("accounts: shared store: %w", err)
	}

	info = ProfileInfo{AccountID: account.AccountID, Harness: harness, Dir: dir}
	switch harness {
	case contracts.HarnessClaude:
		s.composeFromOwner(&info, dir, userClaude, harness)
		s.composeShared(&info, dir, sharedRoot, claudeShared)
		if err := s.mergeClaudeJSON(&info, dir); err != nil {
			return info, err
		}
		info.KeychainService = ClaudeKeychainService(dir)
		info.LoginCommand = fmt.Sprintf("CLAUDE_CONFIG_DIR=%s claude auth login --claudeai", shellQuote(dir))
	case contracts.HarnessCodex:
		s.composeFromOwner(&info, dir, userCodex, harness)
		s.composeShared(&info, dir, sharedRoot, codexShared)
		if err := s.writeCodexConfig(&info, dir, userCodex); err != nil {
			return info, err
		}
		info.LoginCommand = fmt.Sprintf(`CODEX_HOME=%s codex -c 'cli_auth_credentials_store="file"' login --device-auth`, shellQuote(dir))
	}

	if kind == CredRotaCheckout || kind == contracts.CredOAuthToken {
		info.LoginCommand = "" // no login: the rota snapshot is checked out / the hub's token is injected on use
	}
	g, err := s.ensureGrant(ctx, account, harness, dir, info.LoginCommand, kind)
	if err != nil {
		return info, err
	}
	info.GrantID = g.GrantID
	s.log.Info("accounts: profile prepared", "account", account.AccountID, "harness", string(harness), "dir", dir)
	for _, u := range info.Unavailable {
		s.log.Warn("accounts: profile composition: owner configuration left out", "profile", dir, "reason", u)
	}
	return info, nil
}

func (s *Service) ensureGrant(ctx context.Context, account contracts.Account, harness contracts.Harness, dir, loginCmd string, kind contracts.CredentialKind) (grantRow, error) {
	g, err := s.getGrant(ctx, s.db, account.AccountID, harness)
	if err == nil && g.Kind != kind && kind == contracts.CredOAuthToken {
		// A token for an account with a rota/device login here: the login
		// becomes the token's fallback (tokenmerge.go).
		return s.convertToToken(ctx, g, dir)
	}
	if err == nil && g.Kind == contracts.CredOAuthToken && kind != contracts.CredOAuthToken {
		// The account runs on its token here; this login is (re)recorded as
		// the fallback used when the token goes away.
		f := grantFallback{GrantID: g.GrantID, Kind: kind, ProfileDir: dir, ProfileRef: account.AccountID + "/" + string(harness), RefreshOwner: "harness", Detail: "login required: " + loginCmd}
		if harness == contracts.HarnessCodex {
			f.MaxProcs = 1
		}
		if kind == CredRotaCheckout {
			f.ProfileRef, f.Detail = "rota:"+account.RotaRef, "rota snapshot "+account.RotaRef+"; checked in"
		}
		return g, s.putFallback(ctx, f)
	}
	if err == nil {
		if g.ProfileDir != dir {
			return g, contracts.Errorf(contracts.ErrConflict, "grant already bound to profile %s", g.ProfileDir).WithRef(contracts.KindGrant, g.GrantID)
		}
		if g.Kind != kind {
			return g, contracts.Errorf(contracts.ErrConflict, "account already has a %s grant on this node", g.Kind).WithRef(contracts.KindGrant, g.GrantID)
		}
		return g, nil
	}
	if !contracts.IsCode(err, contracts.ErrNotFound) {
		return g, err
	}
	g = grantRow{DeviceGrant: contracts.DeviceGrant{
		GrantID:      contracts.NewID(contracts.PrefixGrant),
		AccountID:    account.AccountID,
		NodeID:       s.cfg.NodeID,
		Harness:      harness,
		ProfileRef:   account.AccountID + "/" + string(harness),
		ProfileDir:   dir,
		RefreshOwner: "harness",
		State:        contracts.GrantUnknown,
		Detail:       "login required: " + loginCmd,
		ObservedAt:   s.now(),
	}}
	g.Kind = kind
	if harness == contracts.HarnessCodex {
		g.MaxProcs = 1 // one Codex process per home, whatever the credential source
	}
	if kind == CredRotaCheckout {
		g.ProfileRef = "rota:" + account.RotaRef
		g.Detail = "rota snapshot " + account.RotaRef + "; checked in"
	}
	if kind == contracts.CredOAuthToken {
		g.RefreshOwner, g.Detail = "none", "token held by the hub; injected at launch"
	}
	g.Mechanism = mechanismFor(g.Kind)
	if err := insertGrant(ctx, s.db, g); err != nil {
		return g, err
	}
	s.report(ctx, contracts.ReportGrant, g.DeviceGrant)
	return g, nil
}

// composeShared links transcript stores into Pyre's shared store so a
// session started under one account can be resumed under another.
func (s *Service) composeShared(info *ProfileInfo, dir, sharedRoot string, items []sharedItem) {
	for _, it := range items {
		target := filepath.Join(sharedRoot, it.name)
		if it.dir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				info.Conflicts = append(info.Conflicts, it.name+": "+err.Error())
				continue
			}
		} else if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				info.Conflicts = append(info.Conflicts, it.name+": "+err.Error())
				continue
			}
			f.Close()
		}
		if filepath.Base(sharedRoot) == string(contracts.HarnessClaude) {
			s.adoptShared(info, filepath.Join(dir, it.name), target, it.dir) // sharedstore.go
		}
		if s.link(info, filepath.Join(dir, it.name), target) {
			info.Shared = append(info.Shared, Link{Name: it.name, Target: target})
		}
	}
}

// link creates p -> target unless p exists. An existing correct link is
// accepted; anything else is recorded as a conflict and left untouched.
func (s *Service) link(info *ProfileInfo, p, target string) bool {
	fi, err := os.Lstat(p)
	if err == nil {
		if fi.Mode()&osutil.LinkModes != 0 {
			if cur, _ := os.Readlink(p); cur == target {
				return true
			}
		}
		info.Conflicts = append(info.Conflicts, filepath.Base(p)+": exists and is not the expected link; left untouched")
		return false
	}
	if err := osutil.Symlink(target, p); err != nil {
		info.Conflicts = append(info.Conflicts, filepath.Base(p)+": "+err.Error())
		return false
	}
	return true
}

// mergeClaudeJSON writes the profile's .claude.json with the user's
// user-scope mcpServers merged in (never a symlink: the file also carries
// this profile's oauthAccount and onboarding state).
func (s *Service) mergeClaudeJSON(info *ProfileInfo, dir string) error {
	p := filepath.Join(dir, ".claude.json")
	prof := map[string]json.RawMessage{}
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &prof); err != nil {
			info.Conflicts = append(info.Conflicts, ".claude.json: unparseable; left untouched")
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var userServers map[string]json.RawMessage
	if s.cfg.UserClaudeJSON != "" {
		if b, err := os.ReadFile(s.cfg.UserClaudeJSON); err == nil {
			var user map[string]json.RawMessage
			if json.Unmarshal(b, &user) == nil {
				_ = json.Unmarshal(user["mcpServers"], &userServers)
				// Non-secret first-run state, so an unattended TUI in a fresh
				// profile does not stop at onboarding screens.
				for _, k := range onboardingKeys {
					if v, ok := user[k]; ok && string(v) != "null" {
						if _, have := prof[k]; !have {
							prof[k] = v
						}
					}
				}
			}
		}
	}
	merged := map[string]json.RawMessage{}
	if raw, ok := prof["mcpServers"]; ok {
		_ = json.Unmarshal(raw, &merged)
	}
	for k, v := range userServers {
		merged[k] = v
	}
	_, existed := prof["mcpServers"]
	if len(merged) > 0 || existed {
		b, _ := json.Marshal(merged)
		prof["mcpServers"] = b
	}
	out, err := json.MarshalIndent(prof, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p, out, 0o600); err != nil {
		return err
	}
	info.Generated = append(info.Generated, ".claude.json")
	return nil
}

// onboardingKeys are non-secret ~/.claude.json keys copied into a new
// profile (never oauthAccount, never credentials): first-run state and the
// owner's answers to startup modals. claudeInChromeDefaultEnabled is the
// answer to "Claude in Chrome extension detected", which Claude 2.1.289
// otherwise shows on a profile's second launch and which blocks an
// unattended TUI (and the initial prompt) until a human answers.
// hasCompletedClaudeInChromeOnboarding is the owner having seen the
// "Claude in Chrome" introduction, which Claude 2.1.285 (laptop) otherwise
// shows as a blocking modal on a fresh profile's first launch.
// autoUpdates is deliberately absent: Claude migrates a config
// autoUpdates=false into env.DISABLE_AUTOUPDATER in settings.json; the
// updater is disabled through the launch environment instead
// (ClaudeProfileEnv).
var onboardingKeys = []string{"hasCompletedOnboarding", "lastOnboardingVersion", "theme", "claudeInChromeDefaultEnabled",
	"hasCompletedClaudeInChromeOnboarding"}

// ClaudeProfileEnv returns the environment every Claude launch in a Pyre
// profile gets. DISABLE_AUTOUPDATER=1 stops Claude's native self-update,
// which installs into and repoints the owner's own Claude installation, and
// (unless FORCE_AUTOUPDATE_PLUGINS is set) plugin marketplace auto-updates,
// which would write into the owner's shared plugin directories. A Pyre run
// never writes the owner's files; Pyre's update manager handles Claude
// versions. Nothing is written to any config file.
func ClaudeProfileEnv() map[string]string {
	return map[string]string{"DISABLE_AUTOUPDATER": "1"}
}

const codexManagedMarker = "# pyre-managed: account profile config"

// writeCodexConfig writes the profile's config.toml: the file credential
// store setting (so the device login lands in this home's auth.json, never
// the OS keyring) followed by the user's config.toml content. A
// non-Pyre-managed config.toml already in the profile is left untouched.
func (s *Service) writeCodexConfig(info *ProfileInfo, dir, userCodex string) error {
	p := filepath.Join(dir, "config.toml")
	if b, err := os.ReadFile(p); err == nil && !bytes.HasPrefix(b, []byte(codexManagedMarker)) {
		info.Conflicts = append(info.Conflicts, "config.toml: exists and is not Pyre-managed; left untouched (login command passes -c cli_auth_credentials_store=\"file\")")
		return nil
	}
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&osutil.LinkModes != 0 {
		info.Conflicts = append(info.Conflicts, "config.toml: is a symlink; left untouched")
		return nil
	}
	var user []byte
	src := ""
	if userCodex != "" {
		src = filepath.Join(userCodex, "config.toml")
		if b, err := os.ReadFile(src); err == nil {
			user = b
		}
	}
	var buf bytes.Buffer
	sum := sha256.Sum256(user)
	fmt.Fprintf(&buf, "%s (regenerated by pyre; edit the source instead)\n", codexManagedMarker)
	if len(user) > 0 {
		fmt.Fprintf(&buf, "# source: %s sha256:%s\n", src, hex.EncodeToString(sum[:]))
	}
	buf.WriteString("cli_auth_credentials_store = \"file\"\n")
	if len(user) > 0 {
		buf.WriteString("\n# ---- adopted from the user's config.toml ----\n")
		buf.Write(stripRootKey(user, "cli_auth_credentials_store"))
		if !bytes.HasSuffix(user, []byte("\n")) {
			buf.WriteByte('\n')
		}
	}
	if err := writeFileAtomic(p, buf.Bytes(), 0o600); err != nil {
		return err
	}
	info.Generated = append(info.Generated, "config.toml")
	return nil
}

// stripRootKey removes `key = ...` lines from the TOML root table (before
// the first table header) so the managed value is the only one.
func stripRootKey(b []byte, key string) []byte {
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	inRoot := true
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			inRoot = false
		}
		if inRoot {
			if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == key {
				out.WriteString("# (pyre) overridden: " + key + "\n")
				continue
			}
		}
		out.WriteString(line + "\n")
	}
	return out.Bytes()
}

func writeFileAtomic(p string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := fileperm.RestrictFile(tmp, mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, p)
}

// sharesTranscriptStore reports whether two profiles of a harness resolve
// their transcript store to the same directory (required for native resume).
func sharesTranscriptStore(h contracts.Harness, a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	name := transcriptStore(h)
	ra, err1 := Canonicalize(filepath.Join(a, name))
	rb, err2 := Canonicalize(filepath.Join(b, name))
	return err1 == nil && err2 == nil && ra == rb
}
