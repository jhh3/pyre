// Package rota is Pyre's read-only inventory adapter for the user's Rota
// installation (github.com/jhh3/rota). It answers "is rota here, which
// accounts has the owner saved, and what did rota last see for usage" for
// display and for owner-confirmed account mapping.
//
// Safety contract (the accounts spike report §4, decision I08):
//
//   - Rota snapshots are live logins. This package lists snapshot NAMES from
//     directory entries only; it never opens a snapshot file
//     (~/.claude/accounts/<name>.json, $CODEX_HOME/accounts/<name>/auth.json)
//     and never seeds a Pyre profile from one.
//   - The only rota command it can execute is `rota --help` (verified in the
//     rota source to print the module docstring and exit; no network, no
//     file access). `rota`, `rota usage`, `list`, `whoami`, `switch`, `save`,
//     `reauth`, `export`, `codex ...` are never run: they read secrets,
//     refresh tokens, or rewrite the live login.
//   - The usage cache (~/.claude/accounts/.usage-cache.json) holds only
//     {name: {fetchedAt, data: <usage endpoint response>}} — utilization
//     numbers, reset times and extra-usage credit totals, no credentials
//     (rota cache_store/usage_for). Only the utilization windows are kept;
//     everything else is discarded. Hints are always stale and display-only.
package rota

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/integrations/bundled"
	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/contracts"
)

// Options locates rota's files. Zero values use rota's own defaults for the
// user's default (ambient) login.
type Options struct {
	// RotaPath is the rota executable; default: the bundled rota of this Pyre
	// install (install.sh --with-tools), else a PATH lookup.
	RotaPath string
	// ClaudeAccountsDir is rota's Claude snapshot directory
	// (default ~/.claude/accounts).
	ClaudeAccountsDir string
	// CodexHome is the Codex home whose accounts/ rota manages
	// (default $CODEX_HOME or ~/.codex).
	CodexHome string
	// RunHelp also runs `rota --help` to confirm the executable works. The
	// file check alone is the default.
	RunHelp bool
	// MaxCacheBytes bounds the usage cache read (default 1 MiB).
	MaxCacheBytes int64
}

func (o *Options) defaults() {
	home, _ := os.UserHomeDir()
	if o.ClaudeAccountsDir == "" && home != "" {
		o.ClaudeAccountsDir = filepath.Join(home, ".claude", "accounts")
	}
	if o.CodexHome == "" {
		if v := os.Getenv("CODEX_HOME"); v != "" {
			o.CodexHome = v
		} else if home != "" {
			o.CodexHome = filepath.Join(home, ".codex")
		}
	}
	if o.MaxCacheBytes <= 0 {
		o.MaxCacheBytes = 1 << 20
	}
}

// UsageHint is rota's last good usage sample for one saved Claude account.
type UsageHint struct {
	Name      string                  `json:"name"`
	FetchedAt time.Time               `json:"fetched_at"`
	Windows   []contracts.UsageWindow `json:"windows"`
}

// Inventory is what Pyre learns about a node's rota without touching any
// credential.
type Inventory struct {
	Status         contracts.IntegrationStatus `json:"status"`
	ClaudeAccounts []string                    `json:"claude_accounts"` // snapshot names
	CodexAccounts  []string                    `json:"codex_accounts"`  // snapshot directory names
	UsageHints     []UsageHint                 `json:"usage_hints,omitempty"`
	Notes          []string                    `json:"notes,omitempty"`
}

var reVersion = regexp.MustCompile(`(?m)^\s*(?:__version__|VERSION)\s*=\s*["']([^"']+)["']`)

// Detect reports rota's presence. It reads the executable file (a Python
// script) to fingerprint it; with RunHelp it also runs `rota --help`.
func Detect(ctx context.Context, opts Options) contracts.IntegrationStatus {
	opts.defaults()
	st := contracts.IntegrationStatus{}
	p := opts.RotaPath
	if p == "" {
		p = bundled.Path("rota")
	}
	if p == "" {
		lp, err := exec.LookPath("rota")
		if err != nil {
			st.Health = "not installed"
			return st
		}
		p = lp
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		st.Health = "not installed"
		return st
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.Mode().IsRegular() {
		st.Health = "not a regular file"
		return st
	}
	st.Present, st.Path = true, real
	f, err := os.Open(real)
	if err != nil {
		st.Health = "unreadable: " + err.Error()
		return st
	}
	head, _ := io.ReadAll(io.LimitReader(f, 2<<20))
	f.Close()
	sum := sha256.Sum256(head)
	st.Revision = "sha256:" + hex.EncodeToString(sum[:])[:16]
	if m := reVersion.FindSubmatch(head); m != nil {
		st.Version = string(m[1])
	}
	if !bytes.Contains(head[:min(len(head), 4096)], []byte("rota")) {
		st.Health = "unrecognized executable"
		return st
	}
	st.Health = "ok (file)"
	if opts.RunHelp {
		out, err := runHelp(ctx, real)
		switch {
		case err != nil:
			st.Health = "help failed: " + err.Error()
		case !strings.HasPrefix(strings.TrimSpace(out), "rota"):
			st.Health = "unexpected help output"
		default:
			st.Health = "ok"
		}
	}
	return st
}

// runHelp is the ONLY rota invocation in Pyre. Exact argv; minimal
// environment with a throwaway HOME so nothing user-specific is even
// resolvable; short timeout.
func runHelp(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--help")
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "HOME=" + os.TempDir(), "LANG=C.UTF-8"}
	cmd.Dir = os.TempDir()
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if out.Len() > 64<<10 {
		return out.String()[:64<<10], nil
	}
	return out.String(), nil
}

// Scan inventories rota on this node: presence, snapshot names and usage
// cache hints. Snapshot files are listed, never opened.
func Scan(ctx context.Context, opts Options) (Inventory, error) {
	opts.defaults()
	inv := Inventory{Status: Detect(ctx, opts), ClaudeAccounts: []string{}, CodexAccounts: []string{}}
	names, err := claudeSnapshotNames(opts.ClaudeAccountsDir)
	if err != nil {
		inv.Notes = append(inv.Notes, "claude snapshots: "+err.Error())
	}
	inv.ClaudeAccounts = names
	cnames, err := codexSnapshotNames(opts.CodexHome)
	if err != nil {
		inv.Notes = append(inv.Notes, "codex snapshots: "+err.Error())
	}
	inv.CodexAccounts = cnames
	if opts.ClaudeAccountsDir != "" {
		hints, err := ReadUsageCache(filepath.Join(opts.ClaudeAccountsDir, ".usage-cache.json"), opts.MaxCacheBytes)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			inv.Notes = append(inv.Notes, "usage cache: "+err.Error())
		default:
			inv.UsageHints = hints
		}
	}
	return inv, nil
}

// claudeSnapshotNames lists <dir>/*.json entry names (rota's
// store_accounts glob), excluding dotfiles such as the usage cache.
func claudeSnapshotNames(dir string) ([]string, error) {
	out := []string{}
	if dir == "" {
		return out, nil
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") || e.IsDir() {
			continue
		}
		out = append(out, strings.TrimSuffix(n, ".json"))
	}
	sort.Strings(out)
	return out, nil
}

// codexSnapshotNames lists <home>/accounts/<name>/ directories that hold an
// auth.json (presence via lstat; content never read).
func codexSnapshotNames(home string) ([]string, error) {
	out := []string{}
	if home == "" {
		return out, nil
	}
	dir := filepath.Join(home, "accounts")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, e.Name(), "auth.json")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ReadUsageCache parses rota's usage cache, keeping only utilization windows
// and fetch times. The live-login pseudo account "(current)" is skipped.
func ReadUsageCache(path string, maxBytes int64) ([]UsageHint, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("usage cache larger than %d bytes; ignored", maxBytes)
	}
	var raw map[string]struct {
		FetchedAt float64         `json:"fetchedAt"` // unix ms
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("usage cache unparseable")
	}
	var out []UsageHint
	for name, ent := range raw {
		if name == "(current)" || len(ent.Data) == 0 {
			continue
		}
		ws, err := accounts.ParseClaudeUsage(ent.Data)
		if err != nil {
			continue
		}
		h := UsageHint{Name: name, Windows: ws}
		if ent.FetchedAt > 0 {
			h.FetchedAt = time.UnixMilli(int64(ent.FetchedAt)).UTC()
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Observation converts a hint into a display-only observation: always
// stale, low confidence, eligibility unknown (never capacity).
func (h UsageHint) Observation(accountID, nodeID string) contracts.AccountObservation {
	return contracts.AccountObservation{
		AccountID: accountID, Provider: contracts.ProviderAnthropic, ReportingNode: nodeID,
		ObservedAt: h.FetchedAt, Source: "rota:usage-cache", Confidence: "low",
		Eligibility: contracts.EligibleUnknown, Windows: h.Windows, Stale: true,
		Detail: "rota cached sample (display only; not used for admission)",
	}
}

// Mapping links a rota snapshot name to a Pyre account. Only Confirmed
// mappings (Account.RotaRef already equal to the name) are authoritative;
// the rest are suggestions for the owner.
type Mapping struct {
	RotaName  string             `json:"rota_name"`
	Provider  contracts.Provider `json:"provider"`
	AccountID string             `json:"account_id,omitempty"`
	Confirmed bool               `json:"confirmed"`
	Reason    string             `json:"reason"`
}

// SuggestMappings proposes rota-name -> account links by RotaRef (confirmed)
// or by label (suggested). Unmatched names are returned without an account.
func SuggestMappings(inv Inventory, accts []contracts.Account) []Mapping {
	var out []Mapping
	add := func(names []string, p contracts.Provider) {
		for _, n := range names {
			m := Mapping{RotaName: n, Provider: p, Reason: "no Pyre account matches; create one and confirm the mapping"}
			for _, a := range accts {
				if a.Provider != p {
					continue
				}
				if a.RotaRef == n {
					m.AccountID, m.Confirmed, m.Reason = a.AccountID, true, "account rota_ref"
					break
				}
				if m.AccountID == "" && (strings.EqualFold(a.Label, n) || strings.EqualFold(a.Label, localPart(n))) {
					m.AccountID, m.Reason = a.AccountID, "label match (owner must confirm)"
				}
			}
			out = append(out, m)
		}
	}
	add(inv.ClaudeAccounts, contracts.ProviderAnthropic)
	add(inv.CodexAccounts, contracts.ProviderOpenAI)
	return out
}

func localPart(s string) string {
	if i := strings.IndexByte(s, '@'); i > 0 {
		return s[:i]
	}
	return s
}

// ApplyMapping records an owner-confirmed mapping on the account (label
// link only; it grants nothing and copies nothing).
func ApplyMapping(a contracts.Account, rotaName string) contracts.Account {
	a.RotaRef = rotaName
	return a
}
