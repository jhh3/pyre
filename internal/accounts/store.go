package accounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Migrations returns the "accounts" namespace migrations (node database).
// Nothing stored here is a credential: grants hold the profile path and
// state, observations hold utilization numbers.
func Migrations() []sqlitex.Migration {
	return []sqlitex.Migration{v1Migration(), {
		Namespace: "accounts",
		Version:   2,
		Name:      "rota_checkouts",
		SQL:       rotaMigration(),
	}, {
		Namespace: "accounts",
		Version:   3,
		Name:      "token_fallback_and_aliases",
		SQL:       tokenMergeMigration(), // tokenmerge.go
	}}
}

func v1Migration() sqlitex.Migration {
	return sqlitex.Migration{
		Namespace: "accounts",
		Version:   1,
		Name:      "accounts_grants_assignments_leases_observations",
		SQL: `
CREATE TABLE accounts_account (
	account_id TEXT PRIMARY KEY,
	provider   TEXT NOT NULL,
	body       TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE accounts_grant (
	grant_id      TEXT PRIMARY KEY,
	account_id    TEXT NOT NULL,
	harness       TEXT NOT NULL,
	kind          TEXT NOT NULL,
	profile_dir   TEXT NOT NULL,
	profile_ref   TEXT NOT NULL,
	generation    INTEGER NOT NULL DEFAULT 0,
	max_procs     INTEGER NOT NULL DEFAULT 0,
	refresh_owner TEXT NOT NULL,
	state         TEXT NOT NULL,
	detail        TEXT NOT NULL DEFAULT '',
	observed_at   TEXT NOT NULL,
	ever_valid    INTEGER NOT NULL DEFAULT 0,
	identity_ref  TEXT NOT NULL DEFAULT '',
	UNIQUE (account_id, harness)
);
CREATE TABLE accounts_assignment (
	assignment_id    TEXT PRIMARY KEY,
	run_id           TEXT NOT NULL UNIQUE,
	session_id       TEXT NOT NULL DEFAULT '',
	account_id       TEXT NOT NULL DEFAULT '',
	grant_id         TEXT NOT NULL DEFAULT '',
	grant_generation INTEGER NOT NULL DEFAULT 0,
	credential_kind  TEXT NOT NULL,
	lease            TEXT NOT NULL DEFAULT '',
	reason           TEXT NOT NULL,
	assigned_at      TEXT NOT NULL,
	released_at      TEXT NOT NULL DEFAULT '',
	freshness        TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX accounts_assignment_active ON accounts_assignment(account_id, released_at);
CREATE TABLE accounts_lease (
	grant_id    TEXT PRIMARY KEY,
	holder      TEXT NOT NULL,
	refcount    INTEGER NOT NULL,
	acquired_at TEXT NOT NULL
);
CREATE TABLE accounts_observation (
	account_id  TEXT PRIMARY KEY,
	observed_at TEXT NOT NULL,
	body        TEXT NOT NULL
);
CREATE TABLE accounts_cooldown (
	account_id TEXT PRIMARY KEY,
	until      TEXT NOT NULL,
	reason     TEXT NOT NULL
);
CREATE TABLE accounts_poll (
	account_id TEXT PRIMARY KEY,
	next_at    TEXT NOT NULL,
	failures   INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE accounts_fallback (
	from_run_id TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL,
	planned_at  TEXT NOT NULL,
	body        TEXT NOT NULL
);`,
	}
}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func ft(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return sqlitex.FormatTime(t)
}

func pt(s string) time.Time {
	t, _ := sqlitex.ParseTime(s)
	return t
}

// ---------- accounts ----------

// UpsertAccount records (or updates) an account definition on this node.
// Account records are owned by the hub; the daemon mirrors them here.
func (s *Service) UpsertAccount(ctx context.Context, a contracts.Account) error {
	if a.AccountID == "" || safeSegment(a.AccountID) != nil {
		return contracts.Errorf(contracts.ErrInvalid, "invalid account id %q", a.AccountID)
	}
	if a.Provider != contracts.ProviderAnthropic && a.Provider != contracts.ProviderOpenAI {
		return contracts.Errorf(contracts.ErrInvalid, "unknown provider %q", a.Provider)
	}
	if a.Kind == "" {
		a.Kind = "subscription"
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	if a.ProviderRef == "" {
		// A verified identity is never unbound by a copy that lacks it
		// (e.g. a hub record mirrored before the binding reached the hub).
		if cur, err := getAccount(ctx, s.db, a.AccountID); err == nil {
			a.ProviderRef = cur.ProviderRef
		}
	}
	b, _ := json.Marshal(a)
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts_account(account_id,provider,body,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET provider=excluded.provider, body=excluded.body, updated_at=excluded.updated_at`,
		a.AccountID, string(a.Provider), string(b), ft(s.now()))
	return err
}

// Account returns one account.
func (s *Service) Account(ctx context.Context, id string) (contracts.Account, error) {
	return getAccount(ctx, s.db, id)
}

func getAccount(ctx context.Context, q querier, id string) (contracts.Account, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT body FROM accounts_account WHERE account_id=?`, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Account{}, contracts.Errorf(contracts.ErrNotFound, "account not found").WithRef(contracts.KindAccount, id)
	}
	if err != nil {
		return contracts.Account{}, err
	}
	var a contracts.Account
	err = json.Unmarshal([]byte(body), &a)
	return a, err
}

func listAccounts(ctx context.Context, q querier, provider contracts.Provider) ([]contracts.Account, error) {
	query := `SELECT body FROM accounts_account`
	var args []any
	if provider != "" {
		query += ` WHERE provider=?`
		args = append(args, string(provider))
	}
	query += ` ORDER BY account_id`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.Account
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var a contracts.Account
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------- grants ----------

type grantRow struct {
	contracts.DeviceGrant
	EverValid   bool
	IdentityRef string // last verified nonsecret provider account id
}

const grantCols = `grant_id,account_id,harness,kind,profile_dir,profile_ref,generation,max_procs,refresh_owner,state,detail,observed_at,ever_valid,identity_ref`

func scanGrant(sc interface{ Scan(...any) error }, nodeID string) (grantRow, error) {
	var g grantRow
	var harness, kind, state, observed string
	var ever int
	err := sc.Scan(&g.GrantID, &g.AccountID, &harness, &kind, &g.ProfileDir, &g.ProfileRef, &g.Generation,
		&g.MaxProcs, &g.RefreshOwner, &state, &g.Detail, &observed, &ever, &g.IdentityRef)
	if err != nil {
		return g, err
	}
	g.NodeID = nodeID
	g.Harness = contracts.Harness(harness)
	g.Kind = contracts.CredentialKind(kind)
	g.State = contracts.GrantState(state)
	g.ObservedAt = pt(observed)
	g.EverValid = ever != 0
	g.Mechanism = mechanismFor(g.Kind)
	return g, nil
}

func mechanismFor(k contracts.CredentialKind) string {
	switch k {
	case contracts.CredClaudeProfileStore:
		return "CLAUDE_CONFIG_DIR isolated profile (keychain item or .credentials.json)"
	case contracts.CredClaudeTokenFD:
		return "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR setup-token (no refresh)"
	case contracts.CredCodexHomeFile:
		return "CODEX_HOME isolated home (auth.json, file store, one process)"
	case contracts.CredOAuthToken:
		return "hub-held access token in the run's private environment; isolated profile without credentials; no refresh, no lease"
	case CredRotaCheckout:
		return "rota snapshot checked out into an isolated profile (single owner; written back on refresh and release)"
	}
	return string(k)
}

func (s *Service) getGrant(ctx context.Context, q querier, accountID string, h contracts.Harness) (grantRow, error) {
	row := q.QueryRowContext(ctx, `SELECT `+grantCols+` FROM accounts_grant WHERE account_id=? AND harness=?`, accountID, string(h))
	g, err := scanGrant(row, s.cfg.NodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return g, contracts.Errorf(contracts.ErrNotFound, "no %s profile for this account on this node", h).WithRef(contracts.KindAccount, accountID)
	}
	return g, err
}

func (s *Service) grantByID(ctx context.Context, q querier, id string) (grantRow, error) {
	row := q.QueryRowContext(ctx, `SELECT `+grantCols+` FROM accounts_grant WHERE grant_id=?`, id)
	g, err := scanGrant(row, s.cfg.NodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return g, contracts.Errorf(contracts.ErrNotFound, "grant not found").WithRef(contracts.KindGrant, id)
	}
	return g, err
}

func (s *Service) listGrants(ctx context.Context, q querier, accountID string) ([]grantRow, error) {
	query := `SELECT ` + grantCols + ` FROM accounts_grant`
	var args []any
	if accountID != "" {
		query += ` WHERE account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY account_id, harness`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []grantRow
	for rows.Next() {
		g, err := scanGrant(rows, s.cfg.NodeID)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func insertGrant(ctx context.Context, q querier, g grantRow) error {
	ever := 0
	if g.EverValid {
		ever = 1
	}
	_, err := q.ExecContext(ctx, `INSERT INTO accounts_grant(`+grantCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.GrantID, g.AccountID, string(g.Harness), string(g.Kind), g.ProfileDir, g.ProfileRef, g.Generation, g.MaxProcs,
		g.RefreshOwner, string(g.State), g.Detail, ft(g.ObservedAt), ever, g.IdentityRef)
	return err
}

// updateGrantState persists a state change. Generation is passed explicitly
// so a reverification can bump it atomically.
func updateGrantState(ctx context.Context, q querier, g grantRow) error {
	ever := 0
	if g.EverValid {
		ever = 1
	}
	_, err := q.ExecContext(ctx, `UPDATE accounts_grant SET state=?, detail=?, observed_at=?, generation=?, ever_valid=?, identity_ref=?, profile_dir=? WHERE grant_id=?`,
		string(g.State), g.Detail, ft(g.ObservedAt), g.Generation, ever, g.IdentityRef, g.ProfileDir, g.GrantID)
	return err
}

// ---------- assignments ----------

type assignmentRow struct {
	contracts.AccountAssignment
	CredentialKind contracts.CredentialKind
}

const asgCols = `assignment_id,run_id,session_id,account_id,grant_id,grant_generation,credential_kind,lease,reason,assigned_at,released_at,freshness`

func scanAssignment(sc interface{ Scan(...any) error }, nodeID string) (assignmentRow, error) {
	var a assignmentRow
	var kind, assigned, released, fresh string
	err := sc.Scan(&a.AssignmentID, &a.RunID, &a.SessionID, &a.AccountID, &a.GrantID, &a.GrantGeneration, &kind,
		&a.Lease, &a.Reason, &assigned, &released, &fresh)
	if err != nil {
		return a, err
	}
	a.NodeID = nodeID
	a.CredentialKind = contracts.CredentialKind(kind)
	a.AssignedAt = pt(assigned)
	if released != "" {
		t := pt(released)
		a.ReleasedAt = &t
	}
	_ = json.Unmarshal([]byte(fresh), &a.Freshness)
	return a, nil
}

func insertAssignment(ctx context.Context, q querier, a assignmentRow) error {
	fb, _ := json.Marshal(a.Freshness)
	_, err := q.ExecContext(ctx, `INSERT INTO accounts_assignment(`+asgCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.AssignmentID, a.RunID, a.SessionID, a.AccountID, a.GrantID, a.GrantGeneration, string(a.CredentialKind),
		a.Lease, a.Reason, ft(a.AssignedAt), "", string(fb))
	return err
}

func (s *Service) assignmentForRun(ctx context.Context, q querier, runID string) (assignmentRow, error) {
	row := q.QueryRowContext(ctx, `SELECT `+asgCols+` FROM accounts_assignment WHERE run_id=?`, runID)
	a, err := scanAssignment(row, s.cfg.NodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return a, contracts.Errorf(contracts.ErrNotFound, "no account assignment for run").WithRef(contracts.KindRun, runID)
	}
	return a, err
}

func (s *Service) activeAssignments(ctx context.Context, q querier, accountID string) ([]assignmentRow, error) {
	query := `SELECT ` + asgCols + ` FROM accounts_assignment WHERE released_at=''`
	var args []any
	if accountID != "" {
		query += ` AND account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY assigned_at`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []assignmentRow
	for rows.Next() {
		a, err := scanAssignment(rows, s.cfg.NodeID)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------- leases ----------

type leaseRow struct {
	GrantID  string
	Holder   string
	Refcount int
}

func getLease(ctx context.Context, q querier, grantID string) (*leaseRow, error) {
	var l leaseRow
	err := q.QueryRowContext(ctx, `SELECT grant_id,holder,refcount FROM accounts_lease WHERE grant_id=?`, grantID).Scan(&l.GrantID, &l.Holder, &l.Refcount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// acquireLease takes (or joins, for the same holder) the grant's process
// lease. A different holder conflicts.
func acquireLease(ctx context.Context, q querier, grantID, holder string, now time.Time) error {
	l, err := getLease(ctx, q, grantID)
	if err != nil {
		return err
	}
	if l == nil {
		_, err = q.ExecContext(ctx, `INSERT INTO accounts_lease(grant_id,holder,refcount,acquired_at) VALUES(?,?,1,?)`, grantID, holder, ft(now))
		return err
	}
	if l.Holder != holder {
		return contracts.Errorf(contracts.ErrConflict, "process lease held by %q (one Codex process per home)", l.Holder).WithRef(contracts.KindGrant, grantID)
	}
	_, err = q.ExecContext(ctx, `UPDATE accounts_lease SET refcount=refcount+1 WHERE grant_id=?`, grantID)
	return err
}

func releaseLease(ctx context.Context, q querier, grantID, holder string) error {
	l, err := getLease(ctx, q, grantID)
	if err != nil || l == nil || l.Holder != holder {
		return err
	}
	if l.Refcount <= 1 {
		_, err = q.ExecContext(ctx, `DELETE FROM accounts_lease WHERE grant_id=?`, grantID)
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE accounts_lease SET refcount=refcount-1 WHERE grant_id=?`, grantID)
	return err
}

// ---------- observations / cooldowns / poll state ----------

func getObservation(ctx context.Context, q querier, accountID string) (*contracts.AccountObservation, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT body FROM accounts_observation WHERE account_id=?`, accountID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var o contracts.AccountObservation
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// putObservation stores obs unless a newer observation is already stored.
func putObservation(ctx context.Context, q querier, o contracts.AccountObservation) error {
	b, _ := json.Marshal(o)
	_, err := q.ExecContext(ctx, `INSERT INTO accounts_observation(account_id,observed_at,body) VALUES(?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET observed_at=excluded.observed_at, body=excluded.body
		WHERE excluded.observed_at >= accounts_observation.observed_at`,
		o.AccountID, ft(o.ObservedAt), string(b))
	return err
}

type cooldown struct {
	Until  time.Time
	Reason string
}

func getCooldown(ctx context.Context, q querier, accountID string) (*cooldown, error) {
	var until, reason string
	err := q.QueryRowContext(ctx, `SELECT until,reason FROM accounts_cooldown WHERE account_id=?`, accountID).Scan(&until, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cooldown{Until: pt(until), Reason: reason}, nil
}

func putCooldown(ctx context.Context, q querier, accountID string, until time.Time, reason string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO accounts_cooldown(account_id,until,reason) VALUES(?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET until=excluded.until, reason=excluded.reason`, accountID, ft(until), reason)
	return err
}

type pollState struct {
	NextAt    time.Time
	Failures  int
	LastError string
}

func getPoll(ctx context.Context, q querier, accountID string) (pollState, error) {
	var p pollState
	var next string
	err := q.QueryRowContext(ctx, `SELECT next_at,failures,last_error FROM accounts_poll WHERE account_id=?`, accountID).Scan(&next, &p.Failures, &p.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return pollState{}, nil
	}
	p.NextAt = pt(next)
	return p, err
}

func putPoll(ctx context.Context, q querier, accountID string, p pollState) error {
	_, err := q.ExecContext(ctx, `INSERT INTO accounts_poll(account_id,next_at,failures,last_error) VALUES(?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET next_at=excluded.next_at, failures=excluded.failures, last_error=excluded.last_error`,
		accountID, ft(p.NextAt), p.Failures, p.LastError)
	return err
}

// ---------- fallback plans ----------

func getFallback(ctx context.Context, q querier, fromRunID string) (*contracts.FallbackPlan, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT body FROM accounts_fallback WHERE from_run_id=?`, fromRunID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p contracts.FallbackPlan
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func putFallback(ctx context.Context, q querier, p contracts.FallbackPlan) error {
	b, _ := json.Marshal(p)
	_, err := q.ExecContext(ctx, `INSERT INTO accounts_fallback(from_run_id,session_id,planned_at,body) VALUES(?,?,?,?)
		ON CONFLICT(from_run_id) DO NOTHING`, p.FromRunID, p.SessionID, ft(p.PlannedAt), string(b))
	return err
}
