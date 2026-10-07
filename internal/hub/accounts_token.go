package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"filippo.io/age"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Token accounts (contracts/accounts_token.go). The hub keeps the only copy
// of each token, encrypted to an age X25519 identity stored next to the
// database (DataDir/secrets/account-tokens.key, 0600). The database holds
// ciphertext only; hub backups (hub.db, blobs, tls) never include the key,
// so a backup restored elsewhere carries no usable token: run `pyre
// accounts add-token` again there. No read API returns a token; a node
// fetches it (node principal, allowed node) and keeps it in memory.

var (
	_ contracts.AccountTokenAPI     = (*Hub)(nil)
	_ contracts.AccountTokenFetcher = (*Hub)(nil)
)

// TokenKeyFile is the token-encryption identity, relative to DataDir.
const TokenKeyFile = "secrets/account-tokens.key"

// tokenMigrations live in their own namespace (additive; schema.go is
// untouched).
var tokenMigrations = []sqlitex.Migration{{Namespace: "hub_tokens", Version: 1, Name: "account_tokens", SQL: `
CREATE TABLE account_tokens (
	account_id TEXT PRIMARY KEY,
	provider TEXT NOT NULL,
	ciphertext BLOB NOT NULL,
	nodes TEXT NOT NULL,
	added_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`}, {Namespace: "hub_tokens", Version: 2, Name: "account_aliases", SQL: mergeMigrationSQL}}

type tokenVault struct {
	mu  sync.Mutex
	id  *age.X25519Identity
	dir string
}

// vault returns the hub's token vault.
func (h *Hub) vault(context.Context) (*tokenVault, error) {
	if h.tokens.dir == "" {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "token accounts need a hub data directory")
	}
	return &h.tokens, nil
}

// identity loads or creates the X25519 identity (0600 in a 0700 dir).
func (v *tokenVault) identity() (*age.X25519Identity, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.id != nil {
		return v.id, nil
	}
	p := filepath.Join(v.dir, TokenKeyFile)
	b, err := os.ReadFile(p)
	switch {
	case err == nil:
		if st, serr := os.Stat(p); serr == nil && st.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("hub: %s is readable by others (mode %o); chmod 600", p, st.Mode().Perm())
		}
		// The file is an age identity file: "#" comment lines plus one key.
		var id *age.X25519Identity
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parsed, perr := age.ParseX25519Identity(line)
			if perr != nil || id != nil {
				id = nil
				break
			}
			id = parsed
		}
		if id == nil {
			return nil, fmt.Errorf("hub: token key %s is not an age X25519 identity", p)
		}
		v.id = id
		return id, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(filepath.Dir(p), 0o700)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(f, "# pyre hub account-token key (age X25519); never share\n"+id.String()+"\n"); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	v.id = id
	return id, nil
}

func (v *tokenVault) seal(token string) ([]byte, error) {
	id, err := v.identity()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, token); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (v *tokenVault) open(ct []byte) (string, error) {
	id, err := v.identity()
	if err != nil {
		return "", err
	}
	r, err := age.Decrypt(bytes.NewReader(ct), id)
	if err != nil {
		return "", contracts.Errorf(contracts.ErrUnavailable, "stored token cannot be decrypted with this hub's key; run `pyre accounts add-token` again")
	}
	b, err := io.ReadAll(io.LimitReader(r, contracts.MaxAccountToken+1))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type tokenRow struct {
	accountID, provider string
	ct                  []byte
	nodes               []string
	added, updated      string
}

func (h *Hub) getTokenRow(ctx context.Context, q queryer, accountID string) (tokenRow, bool, error) {
	var r tokenRow
	var nodes string
	err := q.QueryRowContext(ctx, `SELECT account_id,provider,ciphertext,nodes,added_at,updated_at FROM account_tokens WHERE account_id=?`, accountID).
		Scan(&r.accountID, &r.provider, &r.ct, &nodes, &r.added, &r.updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	_ = json.Unmarshal([]byte(nodes), &r.nodes)
	return r, true, nil
}

// checkToken validates a token's shape without echoing it.
func checkToken(p contracts.Provider, tok string) error {
	switch {
	case tok == "":
		return contracts.Errorf(contracts.ErrInvalid, "token is required")
	case len(tok) > contracts.MaxAccountToken:
		return contracts.Errorf(contracts.ErrInvalid, "token is longer than %d bytes", contracts.MaxAccountToken)
	case strings.ContainsAny(tok, " \t\r\n\x00"):
		return contracts.Errorf(contracts.ErrInvalid, "token must be one line without spaces")
	case p == contracts.ProviderAnthropic && !strings.HasPrefix(tok, "sk-ant-oat"):
		return contracts.Errorf(contracts.ErrInvalid, "an anthropic token comes from `claude setup-token` and starts with sk-ant-oat")
	}
	return nil
}

// SetAccountToken stores an account's token (owner only) and emits
// account.changed.
func (h *Hub) SetAccountToken(ctx context.Context, req contracts.SetAccountTokenRequest) (contracts.AccountTokenStatus, error) {
	var out contracts.AccountTokenStatus
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if !isOwner(p) {
		return out, contracts.Errorf(contracts.ErrForbidden, "only the owner manages account tokens")
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	if !contracts.ValidID(req.AccountID, contracts.PrefixAccount) {
		return out, contracts.Errorf(contracts.ErrInvalid, "account_id %q is malformed", req.AccountID)
	}
	req.AccountID = h.resolveAccountAlias(ctx, h.db, req.AccountID)
	nodes := append([]string(nil), req.Nodes...)
	sort.Strings(nodes)
	for _, n := range nodes {
		if !contracts.ValidID(n, contracts.PrefixNode) {
			return out, contracts.Errorf(contracts.ErrInvalid, "node %q is malformed", n)
		}
	}
	v, err := h.vault(ctx)
	if err != nil {
		return out, err
	}
	a, err := h.getAccount(ctx, h.db, req.AccountID)
	if err != nil {
		return out, err
	}
	if err := checkToken(a.Provider, req.Token); err != nil {
		return out, err
	}
	ct, err := v.seal(req.Token)
	if err != nil {
		return out, err
	}
	// The idempotency record keeps a digest of the request; digest a form
	// that carries a hash of the token, never the token.
	sum := sha256.Sum256([]byte(req.Token))
	digestForm := struct {
		AccountID string   `json:"account_id"`
		TokenHash string   `json:"token_sha256"`
		Nodes     []string `json:"nodes"`
	}{req.AccountID, hex.EncodeToString(sum[:]), nodes}
	err = h.mutate(ctx, req.OperationID, contracts.OpSetAccountToken, p, digestForm, &out, func(tx *sql.Tx) (any, error) {
		a, err := h.getAccount(ctx, tx, req.AccountID)
		if err != nil {
			return nil, err
		}
		now := persistence.Time(h.now())
		nj := persistence.JSON(nodes)
		if nodes == nil {
			nj = "[]"
		}
		_, had, err := h.getTokenRow(ctx, tx, a.AccountID)
		if err != nil {
			return nil, err
		}
		ar := auditRec{Action: contracts.AuditAccountTokenAdded, Actor: actorOf(p), AccountID: a.AccountID}
		if had {
			ar.Action = contracts.AuditAccountTokenReplaced
		}
		if len(nodes) > 0 {
			ar.Reason = fmt.Sprintf("limited to %d machine(s)", len(nodes))
		}
		h.auditTx(ctx, tx, ar)
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_tokens(account_id,provider,ciphertext,nodes,added_at,updated_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(account_id) DO UPDATE SET provider=excluded.provider, ciphertext=excluded.ciphertext, nodes=excluded.nodes, updated_at=excluded.updated_at`,
			a.AccountID, string(a.Provider), ct, nj, now, now); err != nil {
			return nil, err
		}
		// A token login is not a refresh family: every node may use it at
		// once. An account that also has a rota login keeps its own setting:
		// the rota fallback still needs its fleet lease.
		if a.RotaRef == "" {
			a.PerMachineIndependent = true
		}
		if err := h.putAccountTx(ctx, tx, a); err != nil {
			return nil, err
		}
		st, err := h.tokenStatus(ctx, tx, a.AccountID)
		return st, err
	})
	if err != nil {
		return out, err
	}
	// One account per identity: a token-only account with the label of an
	// account without a token is folded into it (accounts_merge.go).
	if into := h.foldAfterSet(ctx, out.AccountID); into != out.AccountID {
		if st, serr := h.tokenStatus(ctx, h.db, into); serr == nil {
			out = st
		}
	}
	return out, nil
}

// RemoveAccountToken deletes an account's token (owner only). Nodes drop
// their in-memory copy at their next fetch (not found => grant revoked).
func (h *Hub) RemoveAccountToken(ctx context.Context, req contracts.RemoveAccountTokenRequest) (contracts.AccountTokenStatus, error) {
	var out contracts.AccountTokenStatus
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if !isOwner(p) {
		return out, contracts.Errorf(contracts.ErrForbidden, "only the owner manages account tokens")
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	if _, err := h.vault(ctx); err != nil {
		return out, err
	}
	req.AccountID = h.resolveAccountAlias(ctx, h.db, req.AccountID)
	err = h.mutate(ctx, req.OperationID, contracts.OpRemoveAccountToken, p, req, &out, func(tx *sql.Tx) (any, error) {
		a, err := h.getAccount(ctx, tx, req.AccountID)
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM account_tokens WHERE account_id=?`, a.AccountID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountTokenRemoved, Actor: actorOf(p), AccountID: a.AccountID})
		}
		if err := h.putAccountTx(ctx, tx, a); err != nil {
			return nil, err
		}
		return contracts.AccountTokenStatus{AccountID: a.AccountID, Provider: a.Provider}, nil
	})
	return out, err
}

// ListAccountTokens returns token status (never bytes) for every account
// with a token, or one account.
func (h *Hub) ListAccountTokens(ctx context.Context, req contracts.ListAccountTokensRequest) ([]contracts.AccountTokenStatus, error) {
	if _, err := principal(ctx); err != nil {
		return nil, err
	}
	if _, err := h.vault(ctx); err != nil {
		return nil, err
	}
	q := `SELECT account_id FROM account_tokens`
	var args []any
	if req.AccountID != "" {
		// An alias answers for its surviving account (whose ID the caller
		// then reads from the status).
		q += ` WHERE account_id=?`
		args = append(args, h.resolveAccountAlias(ctx, h.db, req.AccountID))
	}
	rows, err := h.db.QueryContext(ctx, q+` ORDER BY account_id`, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []contracts.AccountTokenStatus{}
	for _, id := range ids {
		st, err := h.tokenStatus(ctx, h.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// tokenStatus builds the status of a stored token; the newest
// messages-probe observation from any node decides verified/rejected.
func (h *Hub) tokenStatus(ctx context.Context, q queryer, accountID string) (contracts.AccountTokenStatus, error) {
	r, ok, err := h.getTokenRow(ctx, q, accountID)
	if err != nil {
		return contracts.AccountTokenStatus{}, err
	}
	st := contracts.AccountTokenStatus{AccountID: accountID}
	if !ok {
		return st, nil
	}
	st.Provider, st.Set, st.Nodes = contracts.Provider(r.provider), true, r.nodes
	st.AddedAt, st.UpdatedAt = persistence.ParseTime(r.added), persistence.ParseTime(r.updated)
	rows, err := q.QueryContext(ctx, `SELECT observation FROM account_observations WHERE account_id=?`, accountID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	var newest *contracts.AccountObservation
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return st, err
		}
		var o contracts.AccountObservation
		if json.Unmarshal([]byte(body), &o) != nil || !strings.HasPrefix(o.Source, "token:") && o.Source != contracts.UsageSourceMessagesProbe {
			continue
		}
		if o.ObservedAt.Before(st.UpdatedAt) {
			continue // about the previous token
		}
		if newest == nil || o.ObservedAt.After(newest.ObservedAt) {
			cp := o
			newest = &cp
		}
		if o.Eligibility != contracts.EligibleUnusable && o.Confidence == "high" && (st.VerifiedAt == nil || o.ObservedAt.After(*st.VerifiedAt)) {
			t := o.ObservedAt
			st.VerifiedAt = &t
		}
	}
	if newest != nil && newest.Eligibility == contracts.EligibleUnusable {
		st.Rejected, st.Detail = true, newest.Detail
	}
	return st, rows.Err()
}

// FetchAccountToken hands the token to a node allowed to use it.
func (h *Hub) FetchAccountToken(ctx context.Context, req contracts.FetchAccountTokenRequest) (contracts.FetchedAccountToken, error) {
	var out contracts.FetchedAccountToken
	p, err := requireNode(ctx)
	if err != nil {
		return out, err
	}
	v, err := h.vault(ctx)
	if err != nil {
		return out, err
	}
	// A folded account's ID fetches the surviving account's token; the
	// answer's account_id tells the node which account it now is.
	a, err := h.getAccount(ctx, h.db, h.resolveAccountAlias(ctx, h.db, req.AccountID))
	if err != nil {
		return out, err
	}
	if a.Disabled {
		return out, contracts.Errorf(contracts.ErrForbidden, "account is disabled").WithRef(contracts.KindAccount, a.AccountID)
	}
	r, ok, err := h.getTokenRow(ctx, h.db, a.AccountID)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, contracts.Errorf(contracts.ErrNotFound, "account has no token").WithRef(contracts.KindAccount, a.AccountID).WithDetail("token", "absent")
	}
	if len(r.nodes) > 0 {
		allowed := false
		for _, n := range r.nodes {
			allowed = allowed || n == p.ID
		}
		if !allowed {
			return out, contracts.Errorf(contracts.ErrForbidden, "this node may not use the account's token").WithRef(contracts.KindAccount, a.AccountID)
		}
	}
	tok, err := v.open(r.ct)
	if err != nil {
		return out, err
	}
	// One entry per machine and token version (nodes re-fetch periodically).
	h.auditTx(ctx, h.db, auditRec{Action: contracts.AuditAccountTokenSynced, Automatic: true, NodeID: p.ID, AccountID: a.AccountID,
		Dedupe: "tokensync:" + a.AccountID + ":" + p.ID + ":" + r.updated})
	return contracts.FetchedAccountToken{AccountID: a.AccountID, Provider: a.Provider, Token: tok, UpdatedAt: persistence.ParseTime(r.updated)}, nil
}
