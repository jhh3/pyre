package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// One account per identity (W-tokmerge). A `claude setup-token` added with
// an older CLI created a second, token-only account with the label of the
// person's existing (rota / per-machine login) account. The hub folds such
// a token-only account into the same-provider, same-label account that has
// no token: the encrypted token moves, references are repointed, and the
// token-only account is replaced by an alias (old ID -> surviving ID) so
// nodes and running sessions that still name the old ID keep working:
//
//   - accounts.token.fetch and accounts.tokens resolve an alias (a node
//     learns the surviving ID from the answer's account_id);
//   - node reports naming the old ID (observations, assignments, runs,
//     fallback plans) are recorded under the surviving ID; grant reports
//     for the old ID are dropped (the node re-registers the surviving
//     account and reports that grant);
//   - an explicit account policy naming the old ID resolves to the
//     surviving one.
//
// The pass is idempotent and runs at hub start, during maintenance and
// after every accounts.set_token. It never folds when the choice is
// ambiguous (several same-label accounts without a token, both accounts
// holding a token, different provider identities, or a disabled target).

// mergeMigrationSQL adds the alias table (additive, hub_tokens namespace).
const mergeMigrationSQL = `
CREATE TABLE account_aliases (
	old_id TEXT PRIMARY KEY,
	new_id TEXT NOT NULL,
	reason TEXT NOT NULL,
	merged_at TEXT NOT NULL
);
CREATE INDEX account_aliases_new ON account_aliases(new_id);
`

// AccountMerge reports one folded account.
type AccountMerge struct {
	From   string `json:"from"`
	Into   string `json:"into"`
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// resolveAccountAlias returns the surviving account ID for id (id itself
// when it is not an alias). Aliases are kept flat (one hop).
func (h *Hub) resolveAccountAlias(ctx context.Context, q queryer, id string) string {
	if id == "" {
		return id
	}
	var to string
	if err := q.QueryRowContext(ctx, `SELECT new_id FROM account_aliases WHERE old_id=?`, id).Scan(&to); err == nil && to != "" {
		return to
	}
	return id
}

// isAccountAlias reports whether id was folded into another account.
func (h *Hub) isAccountAlias(ctx context.Context, q queryer, id string) bool {
	return h.resolveAccountAlias(ctx, q, id) != id
}

type mergeCand struct {
	a        contracts.Account
	hasToken bool
}

// FoldTokenAccounts runs one idempotent fold pass and returns what it
// merged. Ambiguous cases are skipped (and reported in skipped).
func (h *Hub) FoldTokenAccounts(ctx context.Context) (merged []AccountMerge, skipped []string, err error) {
	var all []mergeCand
	rows, err := h.db.QueryContext(ctx, `SELECT a.account, t.account_id IS NOT NULL FROM accounts a LEFT JOIN account_tokens t ON t.account_id=a.account_id ORDER BY a.account_id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var body string
		var tok bool
		if err := rows.Scan(&body, &tok); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var a contracts.Account
		if jerr := json.Unmarshal([]byte(body), &a); jerr != nil {
			continue
		}
		all = append(all, mergeCand{a: a, hasToken: tok})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	key := func(a contracts.Account) string {
		return string(a.Provider) + "\x00" + strings.ToLower(strings.TrimSpace(a.Label))
	}
	groups := map[string][]mergeCand{}
	for _, c := range all {
		if strings.TrimSpace(c.a.Label) == "" {
			continue
		}
		groups[key(c.a)] = append(groups[key(c.a)], c)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		var tokenOnly, plain []contracts.Account
		for _, c := range g {
			switch {
			case c.hasToken && c.a.RotaRef == "":
				tokenOnly = append(tokenOnly, c.a)
			case !c.hasToken:
				plain = append(plain, c.a)
			}
		}
		if len(tokenOnly) == 0 {
			continue
		}
		if len(plain) != 1 {
			if len(plain) > 1 {
				skipped = append(skipped, "label "+g[0].a.Label+": several accounts without a token ("+joinIDs(plain)+"); not merged")
			}
			continue
		}
		into := plain[0]
		from := tokenOnly[0] // the oldest by ID order; a second one stays (into then holds a token)
		switch {
		case into.Disabled && !from.Disabled:
			skipped = append(skipped, "label "+into.Label+": "+into.AccountID+" is disabled; not merged")
			continue
		case from.ProviderRef != "" && into.ProviderRef != "" && from.ProviderRef != into.ProviderRef:
			skipped = append(skipped, "label "+into.Label+": different provider identities; not merged")
			continue
		}
		m, err := h.mergeAccounts(ctx, from.AccountID, into.AccountID, "token-only account folded into the same-label account")
		if err != nil {
			skipped = append(skipped, "label "+into.Label+": "+contracts.AsError(err).Message)
			continue
		}
		merged = append(merged, m)
	}
	return merged, skipped, nil
}

func joinIDs(as []contracts.Account) string {
	ids := make([]string, len(as))
	for i, a := range as {
		ids[i] = a.AccountID
	}
	return strings.Join(ids, ", ")
}

// mergeAccounts folds account from (token-only) into account into in one
// transaction. Safe with running sessions: assignments and runs are
// repointed, and the alias keeps every later report and fetch naming from
// working.
func (h *Hub) mergeAccounts(ctx context.Context, fromID, intoID, reason string) (AccountMerge, error) {
	var out AccountMerge
	err := h.tx(ctx, func(tx *sql.Tx) error {
		from, err := h.getAccount(ctx, tx, fromID)
		if err != nil {
			return err
		}
		into, err := h.getAccount(ctx, tx, intoID)
		if err != nil {
			return err
		}
		if from.Provider != into.Provider {
			return contracts.Errorf(contracts.ErrConflict, "accounts %s and %s are of different providers", fromID, intoID)
		}
		_, fromTok, err := h.getTokenRow(ctx, tx, fromID)
		if err != nil {
			return err
		}
		_, intoTok, err := h.getTokenRow(ctx, tx, intoID)
		if err != nil {
			return err
		}
		if fromTok && intoTok {
			return contracts.Errorf(contracts.ErrConflict, "both %s and %s hold a token", fromID, intoID)
		}
		now := persistence.Time(h.now())
		exec := func(q string, args ...any) error {
			_, err := tx.ExecContext(ctx, q, args...)
			return err
		}
		steps := []struct {
			q    string
			args []any
		}{
			// The encrypted token (never decrypted here).
			{`UPDATE account_tokens SET account_id=?, updated_at=updated_at WHERE account_id=?`, []any{intoID, fromID}},
			// Usage: keep the newest observation per node under the survivor.
			{`INSERT INTO account_observations(account_id,reporting_node,observed_at,observation)
				SELECT ?, reporting_node, observed_at, json_set(observation,'$.account_id',?) FROM account_observations WHERE account_id=?
				ON CONFLICT(account_id,reporting_node) DO UPDATE SET observed_at=excluded.observed_at, observation=excluded.observation
				WHERE excluded.observed_at > account_observations.observed_at`, []any{intoID, intoID, fromID}},
			{`DELETE FROM account_observations WHERE account_id=?`, []any{fromID}},
			// Node grants of the old account: the node re-registers the
			// survivor and reports that grant; later reports for the old ID
			// are dropped.
			{`DELETE FROM device_grants WHERE account_id=?`, []any{fromID}},
			// Running and past work.
			{`UPDATE account_assignments SET account_id=?, assignment=json_set(assignment,'$.account_id',?) WHERE account_id=?`, []any{intoID, intoID, fromID}},
			{`UPDATE account_leases SET account_id=? WHERE account_id=?`, []any{intoID, fromID}},
			{`UPDATE runs SET run=json_set(run,'$.account_id',?) WHERE json_extract(run,'$.account_id')=?`, []any{intoID, fromID}},
			{`UPDATE fallback_plans SET plan=json_set(plan,'$.from_account_id',?) WHERE json_extract(plan,'$.from_account_id')=?`, []any{intoID, fromID}},
			{`UPDATE fallback_plans SET plan=json_set(plan,'$.to_account_id',?) WHERE json_extract(plan,'$.to_account_id')=?`, []any{intoID, fromID}},
			// Aliases stay one hop.
			{`UPDATE account_aliases SET new_id=? WHERE new_id=?`, []any{intoID, fromID}},
			{`INSERT INTO account_aliases(old_id,new_id,reason,merged_at) VALUES(?,?,?,?)
				ON CONFLICT(old_id) DO UPDATE SET new_id=excluded.new_id, reason=excluded.reason, merged_at=excluded.merged_at`, []any{fromID, intoID, reason, now}},
			{`DELETE FROM accounts WHERE account_id=?`, []any{fromID}},
		}
		for _, s := range steps {
			if err := exec(s.q, s.args...); err != nil {
				return err
			}
		}
		if into.ProviderRef == "" {
			into.ProviderRef = from.ProviderRef
		}
		if into.Plan == "" {
			into.Plan = from.Plan
		}
		if from.Reserve() && !into.Reserve() {
			// The owner put this person's account in reserve: the
			// survivor stays there.
			into.Tier = contracts.TierReserve
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountTierChanged, Automatic: true, AccountID: intoID,
				From: string(contracts.TierNormal), To: string(contracts.TierReserve), Reason: "folded from " + fromID + " (reserve)"})
		}
		// PerMachineIndependent stays the survivor's: a token needs no fleet
		// lease anyway, and a rota login behind it keeps its exclusivity.
		if err := h.putAccountTx(ctx, tx, into); err != nil { // account.changed
			return err
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountFolded, Automatic: true, AccountID: intoID, From: from.Label, FromAcct: fromID, ToAcct: intoID, Reason: reason})
		out = AccountMerge{From: fromID, Into: intoID, Label: into.Label, Reason: reason}
		return nil
	})
	return out, err
}

// foldAfterSet runs the fold pass after a token was stored and returns the
// account the token now belongs to.
func (h *Hub) foldAfterSet(ctx context.Context, accountID string) string {
	if _, _, err := h.FoldTokenAccounts(ctx); err != nil {
		return accountID
	}
	return h.resolveAccountAlias(ctx, h.db, accountID)
}
