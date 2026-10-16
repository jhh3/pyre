package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	_ "modernc.org/sqlite"
)

const (
	currentSchemaVersion = 5
	busyRetryWindow      = 12 * time.Second
)

type Store struct {
	db    *sql.DB
	paths Paths
}

type InstalledPackage struct {
	Key         string
	Version     string
	Digest      string
	Source      string
	Trust       string
	Package     kit.Package
	InstalledAt time.Time
}

type Connection struct {
	ID               string
	Name             string
	ConnectorKey     string
	SelectedMode     string
	CredentialMethod string
	Status           string
	CredentialID     string
	CredentialRef    string
	SafeIdentity     map[string]string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type CredentialRecord struct {
	ID, ConnectorKey, MethodID, VaultRef, DisplayName, Fingerprint string
	Environment, AccountID, AccountName, PrincipalName, Status     string
	Scopes                                                         []string
	CreatedAt, UpdatedAt, LastVerifiedAt, LastUsedAt               time.Time
	Consumers                                                      []string
}

type Cleanup struct {
	ID            string
	CredentialRef string
	Reason        string
	Attempts      int
	CreatedAt     time.Time
}

type ContextItem struct {
	Handle         string         `json:"handle"`
	URL            string         `json:"url"`
	ConnectionName string         `json:"connection"`
	ConnectorKey   string         `json:"connector"`
	Operation      string         `json:"operation"`
	ResourceType   string         `json:"resourceType"`
	Title          string         `json:"title"`
	Body           string         `json:"body"`
	Arguments      map[string]any `json:"arguments"`
	Provenance     map[string]any `json:"provenance"`
	FetchedAt      time.Time      `json:"fetchedAt"`
	ExpiresAt      time.Time      `json:"expiresAt"`
}

type Tool struct {
	ConnectionID, Name, Title, Description, Class, SourceName string
	InputSchema                                               map[string]any
}

type AuditEntry struct {
	ID                                      int64
	At                                      time.Time
	ConnectionID, Operation, Class, Outcome string
	Detail                                  map[string]any
}

type CacheConfig struct {
	Enabled    bool `json:"enabled"`
	TTLSeconds int  `json:"ttlSeconds"`
}

func Open(ctx context.Context, paths Paths) (*Store, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", paths.Database+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(12000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, paths: paths}
	if err := store.configureJournal(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(paths.Database, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Paths() Paths { return s.paths }

func (s *Store) configureJournal(ctx context.Context) error {
	return retryBusy(ctx, func() error {
		var mode string
		if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			return fmt.Errorf("inspect state journal mode: %w", err)
		}
		if strings.EqualFold(mode, "wal") {
			return nil
		}
		if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
			return fmt.Errorf("enable state WAL: %w", err)
		}
		if !strings.EqualFold(mode, "wal") {
			return fmt.Errorf("enable state WAL: sqlite selected %q", mode)
		}
		return nil
	})
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version)
	if err == nil && version >= currentSchemaVersion {
		return nil
	}
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return err
	}
	return retryBusy(ctx, func() error { return s.migrateOnce(ctx) })
}

func (s *Store) migrateOnce(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var version int
	if err := conn.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err == nil {
		if version >= currentSchemaVersion {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return err
			}
			committed = true
			return nil
		}
	} else if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);`,
		`INSERT INTO schema_version(version) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM schema_version);`,
		`CREATE TABLE IF NOT EXISTS connector_packages (
			key TEXT PRIMARY KEY, version TEXT NOT NULL, digest TEXT NOT NULL, source TEXT NOT NULL,
			trust TEXT NOT NULL CHECK (trust IN ('builtin','trusted','untrusted')), manifest_json TEXT NOT NULL,
			installed_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS connections (
			id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, connector_key TEXT NOT NULL REFERENCES connector_packages(key),
			selected_mode TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('active','broken','disabled','removed')),
			credential_ref TEXT NOT NULL DEFAULT '', safe_identity_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS discovered_tools (
			connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE, name TEXT NOT NULL,
			description TEXT NOT NULL, class TEXT NOT NULL, input_schema_json TEXT NOT NULL, source_name TEXT NOT NULL,
			PRIMARY KEY(connection_id, name)
		);`,
		`CREATE TABLE IF NOT EXISTS secret_cleanup (
			id TEXT PRIMARY KEY, credential_ref TEXT NOT NULL UNIQUE, reason TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, last_error TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, connection_id TEXT, operation TEXT,
			class TEXT, outcome TEXT NOT NULL, detail_json TEXT NOT NULL DEFAULT '{}'
		);`,
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value_json TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS context_items (
			handle TEXT PRIMARY KEY, url TEXT NOT NULL, connection_name TEXT NOT NULL, resource_type TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, fetched_at TEXT NOT NULL, expires_at TEXT NOT NULL,
			connector_key TEXT NOT NULL DEFAULT '', operation TEXT NOT NULL DEFAULT '',
			arguments_json TEXT NOT NULL DEFAULT '{}', provenance_json TEXT NOT NULL DEFAULT '{}'
		);`,
		`CREATE INDEX IF NOT EXISTS context_items_url ON context_items(url,connection_name,expires_at);`,
		`UPDATE schema_version SET version = 1 WHERE version < 1;`,
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate state: %w", err)
		}
	}
	if err := conn.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version < 2 {
		for _, column := range []struct{ name, definition string }{
			{"connector_key", `TEXT NOT NULL DEFAULT ''`},
			{"operation", `TEXT NOT NULL DEFAULT ''`},
			{"arguments_json", `TEXT NOT NULL DEFAULT '{}'`},
			{"provenance_json", `TEXT NOT NULL DEFAULT '{}'`},
		} {
			if err := ensureColumn(ctx, conn, "context_items", column.name, column.definition); err != nil {
				return err
			}
		}
		fts := []string{
			`CREATE VIRTUAL TABLE IF NOT EXISTS context_items_fts USING fts5(handle UNINDEXED,title,body,connection_name UNINDEXED,connector_key UNINDEXED,operation UNINDEXED,resource_type UNINDEXED,content='context_items',content_rowid='rowid',tokenize='unicode61');`,
			`CREATE TRIGGER IF NOT EXISTS context_items_fts_insert AFTER INSERT ON context_items BEGIN INSERT INTO context_items_fts(rowid,handle,title,body,connection_name,connector_key,operation,resource_type) VALUES(new.rowid,new.handle,new.title,new.body,new.connection_name,new.connector_key,new.operation,new.resource_type); END;`,
			`CREATE TRIGGER IF NOT EXISTS context_items_fts_delete AFTER DELETE ON context_items BEGIN INSERT INTO context_items_fts(context_items_fts,rowid,handle,title,body,connection_name,connector_key,operation,resource_type) VALUES('delete',old.rowid,old.handle,old.title,old.body,old.connection_name,old.connector_key,old.operation,old.resource_type); END;`,
			`CREATE TRIGGER IF NOT EXISTS context_items_fts_update AFTER UPDATE ON context_items BEGIN INSERT INTO context_items_fts(context_items_fts,rowid,handle,title,body,connection_name,connector_key,operation,resource_type) VALUES('delete',old.rowid,old.handle,old.title,old.body,old.connection_name,old.connector_key,old.operation,old.resource_type); INSERT INTO context_items_fts(rowid,handle,title,body,connection_name,connector_key,operation,resource_type) VALUES(new.rowid,new.handle,new.title,new.body,new.connection_name,new.connector_key,new.operation,new.resource_type); END;`,
			`INSERT INTO context_items_fts(context_items_fts) VALUES('rebuild');`,
			`UPDATE schema_version SET version=2;`,
		}
		for _, statement := range fts {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate searchable context: %w", err)
			}
		}
	}
	if err := ensureColumn(ctx, conn, "connections", "credential_method", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE connections SET status='broken' WHERE credential_method='' AND status='active'`); err != nil {
		return fmt.Errorf("quarantine pre-alpha2 connections: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE schema_version SET version=3 WHERE version<3`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS credentials (
		id TEXT PRIMARY KEY, connector_key TEXT NOT NULL REFERENCES connector_packages(key), method_id TEXT NOT NULL,
		vault_ref TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL, fingerprint TEXT NOT NULL DEFAULT '',
		environment TEXT NOT NULL DEFAULT '', account_id TEXT NOT NULL DEFAULT '', account_name TEXT NOT NULL DEFAULT '',
		principal_name TEXT NOT NULL DEFAULT '', scopes_json TEXT NOT NULL DEFAULT '[]',
		status TEXT NOT NULL CHECK (status IN ('candidate','active','broken','revoked','cleanup-pending')),
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_verified_at TEXT, last_used_at TEXT
	)`); err != nil {
		return fmt.Errorf("create credential records: %w", err)
	}
	if err := ensureColumn(ctx, conn, "connections", "credential_id", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `INSERT INTO credentials(id,connector_key,method_id,vault_ref,display_name,status,created_at,updated_at,last_verified_at)
		SELECT 'cred_' || id,connector_key,credential_method,credential_ref,name,
			CASE WHEN status='active' THEN 'active' ELSE 'broken' END,created_at,updated_at,CASE WHEN status='active' THEN ? ELSE NULL END
		FROM connections WHERE credential_id='' AND credential_method<>''
		ON CONFLICT(id) DO NOTHING`, now); err != nil {
		return fmt.Errorf("migrate credential records: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE connections SET credential_id='cred_' || id WHERE credential_id='' AND credential_method<>''`); err != nil {
		return fmt.Errorf("link migrated credential records: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE schema_version SET version=4 WHERE version<4`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE connections SET credential_method='outfit-oauth' WHERE credential_method='cw-oauth'`); err != nil {
		return fmt.Errorf("rename legacy connection credential method: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE credentials SET method_id='outfit-oauth' WHERE method_id='cw-oauth'`); err != nil {
		return fmt.Errorf("rename legacy credential method: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE connector_packages SET manifest_json=replace(manifest_json,'cw.jhh3.dev/','outfit.jhh3.dev/') WHERE manifest_json LIKE '%cw.jhh3.dev/%'`); err != nil {
		return fmt.Errorf("rename legacy connector schema: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE connector_packages SET manifest_json=replace(replace(manifest_json,'"custody":"cw"','"custody":"outfit"'),'"cw-oauth"','"outfit-oauth"') WHERE manifest_json LIKE '%"custody":"cw"%' OR manifest_json LIKE '%"cw-oauth"%'`); err != nil {
		return fmt.Errorf("rename legacy connector contracts: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE schema_version SET version=5 WHERE version<5`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

func retryBusy(ctx context.Context, fn func() error) error {
	deadline := time.Now().Add(busyRetryWindow)
	delay := 20 * time.Millisecond
	for {
		err := fn()
		if err == nil || !sqliteBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 250*time.Millisecond {
			delay *= 2
			if delay > 250*time.Millisecond {
				delay = 250 * time.Millisecond
			}
		}
	}
}

func sqliteBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") || strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked")
}

type schemaExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func ensureColumn(ctx context.Context, executor schemaExecutor, table, name, definition string) error {
	rows, err := executor.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var index, notNull, primaryKey int
		var column, kind string
		var defaultValue any
		if err := rows.Scan(&index, &column, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		found = found || column == name
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = executor.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+name+` `+definition)
	return err
}

func (s *Store) InstallPackage(ctx context.Context, loaded kit.Loaded, trust string) error {
	if trust != "builtin" && trust != "trusted" && trust != "untrusted" {
		return fmt.Errorf("invalid package trust %q", trust)
	}
	var digest, source, installedTrust string
	err := s.db.QueryRowContext(ctx, `SELECT digest,source,trust FROM connector_packages WHERE key=?`, loaded.Package.Key).Scan(&digest, &source, &installedTrust)
	if err == nil && digest == loaded.Digest && source == loaded.Source && installedTrust == trust {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return retryBusy(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO connector_packages(key,version,digest,source,trust,manifest_json,installed_at)
			VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET version=excluded.version,digest=excluded.digest,source=excluded.source,
				trust=excluded.trust,manifest_json=excluded.manifest_json,installed_at=excluded.installed_at
			WHERE connector_packages.version<>excluded.version OR connector_packages.digest<>excluded.digest
				OR connector_packages.source<>excluded.source OR connector_packages.trust<>excluded.trust
				OR connector_packages.manifest_json<>excluded.manifest_json`,
			loaded.Package.Key, loaded.Package.Version, loaded.Digest, loaded.Source, trust, string(loaded.Raw), time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
}

func (s *Store) Packages(ctx context.Context) ([]InstalledPackage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,version,digest,source,trust,manifest_json,installed_at FROM connector_packages ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []InstalledPackage
	for rows.Next() {
		var item InstalledPackage
		var manifest, installed string
		if err := rows.Scan(&item.Key, &item.Version, &item.Digest, &item.Source, &item.Trust, &manifest, &installed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(manifest), &item.Package); err != nil {
			return nil, err
		}
		item.InstalledAt, _ = time.Parse(time.RFC3339Nano, installed)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Package(ctx context.Context, key string) (InstalledPackage, error) {
	var item InstalledPackage
	var manifest, installed string
	err := s.db.QueryRowContext(ctx, `SELECT key,version,digest,source,trust,manifest_json,installed_at FROM connector_packages WHERE key=?`, key).
		Scan(&item.Key, &item.Version, &item.Digest, &item.Source, &item.Trust, &manifest, &installed)
	if err != nil {
		return InstalledPackage{}, err
	}
	if err := json.Unmarshal([]byte(manifest), &item.Package); err != nil {
		return InstalledPackage{}, err
	}
	item.InstalledAt, _ = time.Parse(time.RFC3339Nano, installed)
	return item, nil
}

func (s *Store) Connections(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, connectionColumns+` WHERE c.status <> 'removed' ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Connection
	for rows.Next() {
		item, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Connection(ctx context.Context, name string) (Connection, error) {
	return scanConnection(s.db.QueryRowContext(ctx, connectionColumns+` WHERE c.name=?`, name))
}

func (s *Store) MergeConnectionIdentity(ctx context.Context, id string, values map[string]string) error {
	var encoded string
	if err := s.db.QueryRowContext(ctx, `SELECT safe_identity_json FROM connections WHERE id=? AND status='active'`, id).Scan(&encoded); err != nil {
		return err
	}
	identity := map[string]string{}
	if err := json.Unmarshal([]byte(encoded), &identity); err != nil {
		return err
	}
	for key, value := range values {
		if value == "" {
			delete(identity, key)
		} else {
			identity[key] = value
		}
	}
	updated, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE connections SET safe_identity_json=?,updated_at=? WHERE id=? AND status='active' AND safe_identity_json=?`, string(updated), time.Now().UTC().Format(time.RFC3339Nano), id, encoded)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return errors.New("connection identity changed concurrently; retry")
	}
	return nil
}

const connectionColumns = `SELECT c.id,c.name,c.connector_key,c.selected_mode,c.credential_method,c.status,c.credential_id,
	COALESCE(cr.vault_ref,c.credential_ref),c.safe_identity_json,c.created_at,c.updated_at
	FROM connections c LEFT JOIN credentials cr ON cr.id=c.credential_id`

type scanner interface{ Scan(...any) error }

func scanConnection(row scanner) (Connection, error) {
	var item Connection
	var identity, created, updated string
	if err := row.Scan(&item.ID, &item.Name, &item.ConnectorKey, &item.SelectedMode, &item.CredentialMethod, &item.Status, &item.CredentialID, &item.CredentialRef, &identity, &created, &updated); err != nil {
		return Connection{}, err
	}
	if err := json.Unmarshal([]byte(identity), &item.SafeIdentity); err != nil {
		return Connection{}, err
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return item, nil
}

func (s *Store) CreateConnection(ctx context.Context, connection Connection) error {
	return s.CreateConnectionWithTools(ctx, connection, nil)
}

func (s *Store) CreateConnectionWithTools(ctx context.Context, connection Connection, tools []Tool) error {
	identity, err := json.Marshal(connection.SafeIdentity)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if connection.CredentialID == "" {
		connection.CredentialID = "cred_" + connection.ID
	}
	if err := insertCredentialTx(ctx, tx, CredentialRecord{ID: connection.CredentialID, ConnectorKey: connection.ConnectorKey, MethodID: connection.CredentialMethod, VaultRef: connection.CredentialRef, DisplayName: connection.Name, Fingerprint: connection.SafeIdentity["fingerprint"], Environment: connection.SafeIdentity["environment"], AccountID: connection.SafeIdentity["account_id"], AccountName: connection.SafeIdentity["account_name"], PrincipalName: connection.SafeIdentity["principal"], Scopes: splitSafeList(connection.SafeIdentity["scopes"]), Status: "active", LastVerifiedAt: time.Now().UTC()}); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO connections(id,name,connector_key,selected_mode,credential_method,status,credential_id,credential_ref,safe_identity_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		connection.ID, connection.Name, connection.ConnectorKey, connection.SelectedMode, connection.CredentialMethod, connection.Status, connection.CredentialID, connection.CredentialRef, string(identity), now, now)
	if err != nil {
		return err
	}
	if err := replaceToolsTx(ctx, tx, connection.ID, tools); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AttachConnectionWithCredential(ctx context.Context, connection Connection, credentialID string, tools []Tool) error {
	identity, err := json.Marshal(connection.SafeIdentity)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var connectorKey, methodID, vaultRef string
	if err := tx.QueryRowContext(ctx, `SELECT connector_key,method_id,vault_ref FROM credentials WHERE id=? AND status='active'`, credentialID).Scan(&connectorKey, &methodID, &vaultRef); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("credential is not active")
		}
		return err
	}
	if connectorKey != connection.ConnectorKey || methodID != connection.CredentialMethod {
		return errors.New("credential connector or method does not match the connection")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO connections(id,name,connector_key,selected_mode,credential_method,status,credential_id,credential_ref,safe_identity_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, connection.ID, connection.Name, connection.ConnectorKey, connection.SelectedMode, connection.CredentialMethod, connection.Status, credentialID, vaultRef, string(identity), now, now); err != nil {
		return err
	}
	if err := replaceToolsTx(ctx, tx, connection.ID, tools); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE credentials SET last_verified_at=?,updated_at=? WHERE id=? AND status='active'`, now, now, credentialID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SwitchConnection(ctx context.Context, name, expectedRef, mode, credentialMethod, newRef, status, cleanupID string, identity map[string]string, tools []Tool) (bool, bool, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return false, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	var connectionID, oldCredentialID string
	if err := tx.QueryRowContext(ctx, `SELECT id,credential_id FROM connections WHERE name=? AND credential_ref=? AND status<>'removed'`, name, expectedRef).Scan(&connectionID, &oldCredentialID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	newCredentialID := "cred_" + cleanupID
	var connectorKey string
	if err := tx.QueryRowContext(ctx, `SELECT connector_key FROM connections WHERE id=?`, connectionID).Scan(&connectorKey); err != nil {
		return false, false, err
	}
	if err := insertCredentialTx(ctx, tx, CredentialRecord{ID: newCredentialID, ConnectorKey: connectorKey, MethodID: credentialMethod, VaultRef: newRef, DisplayName: name, Fingerprint: identity["fingerprint"], Environment: identity["environment"], AccountID: identity["account_id"], AccountName: identity["account_name"], PrincipalName: identity["principal"], Scopes: splitSafeList(identity["scopes"]), Status: "active", LastVerifiedAt: time.Now().UTC()}); err != nil {
		return false, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE connections SET selected_mode=?,credential_method=?,credential_id=?,credential_ref=?,status=?,safe_identity_json=?,updated_at=? WHERE name=? AND credential_id=? AND credential_ref=? AND status <> 'removed'`,
		mode, credentialMethod, newCredentialID, newRef, status, string(encoded), time.Now().UTC().Format(time.RFC3339Nano), name, oldCredentialID, expectedRef)
	if err != nil {
		return false, false, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return false, false, nil
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections WHERE credential_id=? AND status<>'removed'`, oldCredentialID).Scan(&remaining); err != nil {
		return false, false, err
	}
	cleanupOld := remaining == 0
	if cleanupOld && expectedRef != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret_cleanup(id,credential_ref,reason,created_at) VALUES(?,?,?,?)`, cleanupID, expectedRef, "superseded connection credential", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return false, false, err
		}
	}
	if cleanupOld && oldCredentialID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE credentials SET status='cleanup-pending',updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), oldCredentialID); err != nil {
			return false, false, err
		}
	}
	if err := replaceToolsTx(ctx, tx, connectionID, tools); err != nil {
		return false, false, err
	}
	return true, cleanupOld, tx.Commit()
}

// PurgeRemovedConnection deletes the row of a removed connection (its
// discovered tools cascade) so its name can be used again. An active
// connection is never touched.
func (s *Store) PurgeRemovedConnection(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE name=? AND status='removed'`, name)
	return err
}

func (s *Store) RemoveConnection(ctx context.Context, name, expectedRef, cleanupID string) (bool, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	var credentialID string
	if err := tx.QueryRowContext(ctx, `SELECT credential_id FROM connections WHERE name=? AND credential_ref=? AND status<>'removed'`, name, expectedRef).Scan(&credentialID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE connections SET status='removed',credential_ref='',updated_at=? WHERE name=? AND credential_id=? AND credential_ref=? AND status <> 'removed'`, time.Now().UTC().Format(time.RFC3339Nano), name, credentialID, expectedRef)
	if err != nil {
		return false, false, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return false, false, nil
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections WHERE credential_id=? AND status<>'removed'`, credentialID).Scan(&remaining); err != nil {
		return false, false, err
	}
	cleanupCredential := remaining == 0
	if cleanupCredential && expectedRef != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret_cleanup(id,credential_ref,reason,created_at) VALUES(?,?,?,?)`, cleanupID, expectedRef, "removed connection credential", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return false, false, err
		}
	}
	if cleanupCredential && credentialID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE credentials SET status=CASE WHEN vault_ref='' THEN 'revoked' ELSE 'cleanup-pending' END,updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), credentialID); err != nil {
			return false, false, err
		}
		if expectedRef == "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE id=? AND vault_ref='' AND NOT EXISTS (SELECT 1 FROM connections c WHERE c.credential_id=credentials.id AND c.status<>'removed')`, credentialID); err != nil {
				return false, false, err
			}
		}
	}
	return true, cleanupCredential, tx.Commit()
}

func (s *Store) AddCleanup(ctx context.Context, id, ref, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO secret_cleanup(id,credential_ref,reason,created_at) VALUES(?,?,?,?) ON CONFLICT(credential_ref) DO UPDATE SET reason=excluded.reason`, id, ref, reason, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func insertCredentialTx(ctx context.Context, tx *sql.Tx, credential CredentialRecord) error {
	if credential.ID == "" || credential.ConnectorKey == "" || credential.MethodID == "" {
		return errors.New("credential record requires id, connector, and method")
	}
	if credential.Status == "" {
		credential.Status = "active"
	}
	now := time.Now().UTC()
	created := credential.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := credential.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	scopes, err := json.Marshal(credential.Scopes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO credentials(id,connector_key,method_id,vault_ref,display_name,fingerprint,environment,account_id,account_name,principal_name,scopes_json,status,created_at,updated_at,last_verified_at,last_used_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, credential.ID, credential.ConnectorKey, credential.MethodID, credential.VaultRef, credential.DisplayName, credential.Fingerprint, credential.Environment, credential.AccountID, credential.AccountName, credential.PrincipalName, string(scopes), credential.Status, created.Format(time.RFC3339Nano), updated.Format(time.RFC3339Nano), nullableTime(credential.LastVerifiedAt), nullableTime(credential.LastUsedAt))
	return err
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func splitSafeList(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func (s *Store) Credentials(ctx context.Context) ([]CredentialRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cr.id,cr.connector_key,cr.method_id,cr.vault_ref,cr.display_name,cr.fingerprint,cr.environment,cr.account_id,cr.account_name,cr.principal_name,cr.scopes_json,cr.status,cr.created_at,cr.updated_at,cr.last_verified_at,cr.last_used_at,
		COALESCE(GROUP_CONCAT(CASE WHEN c.status<>'removed' THEN c.name END, char(31)),'')
		FROM credentials cr LEFT JOIN connections c ON c.credential_id=cr.id
		GROUP BY cr.id ORDER BY cr.connector_key,cr.display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CredentialRecord
	for rows.Next() {
		item, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Credential(ctx context.Context, id string) (CredentialRecord, error) {
	return scanCredential(s.db.QueryRowContext(ctx, `SELECT cr.id,cr.connector_key,cr.method_id,cr.vault_ref,cr.display_name,cr.fingerprint,cr.environment,cr.account_id,cr.account_name,cr.principal_name,cr.scopes_json,cr.status,cr.created_at,cr.updated_at,cr.last_verified_at,cr.last_used_at,
		COALESCE(GROUP_CONCAT(CASE WHEN c.status<>'removed' THEN c.name END, char(31)),'')
		FROM credentials cr LEFT JOIN connections c ON c.credential_id=cr.id WHERE cr.id=? GROUP BY cr.id`, id))
}

func scanCredential(row scanner) (CredentialRecord, error) {
	var item CredentialRecord
	var scopes, created, updated, consumers string
	var verified, used sql.NullString
	if err := row.Scan(&item.ID, &item.ConnectorKey, &item.MethodID, &item.VaultRef, &item.DisplayName, &item.Fingerprint, &item.Environment, &item.AccountID, &item.AccountName, &item.PrincipalName, &scopes, &item.Status, &created, &updated, &verified, &used, &consumers); err != nil {
		return CredentialRecord{}, err
	}
	_ = json.Unmarshal([]byte(scopes), &item.Scopes)
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if verified.Valid {
		item.LastVerifiedAt, _ = time.Parse(time.RFC3339Nano, verified.String)
	}
	if used.Valid {
		item.LastUsedAt, _ = time.Parse(time.RFC3339Nano, used.String)
	}
	item.Consumers = splitSafeList(strings.ReplaceAll(consumers, string(rune(31)), ","))
	return item, nil
}

func (s *Store) TouchCredential(ctx context.Context, id string, verified bool) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if verified {
		_, err := s.db.ExecContext(ctx, `UPDATE credentials SET last_verified_at=?,last_used_at=?,updated_at=? WHERE id=?`, now, now, now, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE credentials SET last_used_at=?,updated_at=? WHERE id=?`, now, now, id)
	return err
}

// RotateCredentialSecret atomically moves every active consumer of one
// credential to a newly vaulted OAuth grant. The old ref remains in the
// cleanup journal until its ciphertext is actually destroyed.
func (s *Store) RotateCredentialSecret(ctx context.Context, credentialID, expectedRef, newRef, cleanupID string, safeSettings map[string]string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var currentRef string
	if err := tx.QueryRowContext(ctx, `SELECT vault_ref FROM credentials WHERE id=? AND status='active'`, credentialID).Scan(&currentRef); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if currentRef != expectedRef {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,safe_identity_json FROM connections WHERE credential_id=? AND status<>'removed'`, credentialID)
	if err != nil {
		return false, err
	}
	type identityUpdate struct{ id, encoded string }
	var identities []identityUpdate
	for rows.Next() {
		var id, encoded string
		if err := rows.Scan(&id, &encoded); err != nil {
			rows.Close()
			return false, err
		}
		identity := map[string]string{}
		if err := json.Unmarshal([]byte(encoded), &identity); err != nil {
			rows.Close()
			return false, err
		}
		for key, value := range safeSettings {
			identity["setting."+key] = value
		}
		updated, err := json.Marshal(identity)
		if err != nil {
			rows.Close()
			return false, err
		}
		identities = append(identities, identityUpdate{id: id, encoded: string(updated)})
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE credentials SET vault_ref=?,updated_at=? WHERE id=? AND vault_ref=? AND status='active'`, newRef, now, credentialID, expectedRef)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return false, nil
	}
	for _, identity := range identities {
		result, err := tx.ExecContext(ctx, `UPDATE connections SET credential_ref=?,safe_identity_json=?,updated_at=? WHERE id=? AND credential_id=? AND credential_ref=? AND status<>'removed'`, newRef, identity.encoded, now, identity.id, credentialID, expectedRef)
		if err != nil {
			return false, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return false, nil
		}
	}
	if expectedRef != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret_cleanup(id,credential_ref,reason,created_at) VALUES(?,?,?,?)`, cleanupID, expectedRef, "rotated OAuth grant", now); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *Store) Cleanups(ctx context.Context) ([]Cleanup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,credential_ref,reason,attempts,created_at FROM secret_cleanup ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Cleanup
	for rows.Next() {
		var item Cleanup
		var created string
		if err := rows.Scan(&item.ID, &item.CredentialRef, &item.Reason, &item.Attempts, &created); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) FinishCleanup(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ref string
	if err := tx.QueryRowContext(ctx, `SELECT credential_ref FROM secret_cleanup WHERE id=?`, id).Scan(&ref); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM secret_cleanup WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE vault_ref=? AND NOT EXISTS (SELECT 1 FROM connections c WHERE c.credential_id=credentials.id AND c.status<>'removed')`, ref); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailCleanup(ctx context.Context, id string, failure error) error {
	_, err := s.db.ExecContext(ctx, `UPDATE secret_cleanup SET attempts=attempts+1,last_error=? WHERE id=?`, failure.Error(), id)
	return err
}

func (s *Store) Audit(ctx context.Context, connectionID, operation, class, outcome string, detail map[string]any) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO audit(at,connection_id,operation,class,outcome,detail_json) VALUES(?,?,?,?,?,?)`, time.Now().UTC().Format(time.RFC3339Nano), emptyNull(connectionID), emptyNull(operation), emptyNull(class), outcome, string(encoded))
	return err
}

func (s *Store) Audits(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,at,COALESCE(connection_id,''),COALESCE(operation,''),COALESCE(class,''),outcome,detail_json FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AuditEntry
	for rows.Next() {
		var item AuditEntry
		var at, detail string
		if err := rows.Scan(&item.ID, &at, &item.ConnectionID, &item.Operation, &item.Class, &item.Outcome, &detail); err != nil {
			return nil, err
		}
		item.At, _ = time.Parse(time.RFC3339Nano, at)
		_ = json.Unmarshal([]byte(detail), &item.Detail)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ReplaceTools(ctx context.Context, connectionID string, tools []Tool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replaceToolsTx(ctx, tx, connectionID, tools); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceToolsTx(ctx context.Context, tx *sql.Tx, connectionID string, tools []Tool) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM discovered_tools WHERE connection_id=?`, connectionID); err != nil {
		return err
	}
	for _, tool := range tools {
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO discovered_tools(connection_id,name,description,class,input_schema_json,source_name) VALUES(?,?,?,?,?,?)`, connectionID, tool.Name, tool.Description, tool.Class, string(encoded), tool.SourceName); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Tools(ctx context.Context, connectionID string) ([]Tool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT connection_id,name,description,class,input_schema_json,source_name FROM discovered_tools WHERE connection_id=? ORDER BY name`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Tool
	for rows.Next() {
		var item Tool
		var encoded string
		if err := rows.Scan(&item.ConnectionID, &item.Name, &item.Description, &item.Class, &encoded, &item.SourceName); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(encoded), &item.InputSchema)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Tool(ctx context.Context, connectionID, name string) (Tool, error) {
	var item Tool
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT connection_id,name,description,class,input_schema_json,source_name FROM discovered_tools WHERE connection_id=? AND name=?`, connectionID, name).Scan(&item.ConnectionID, &item.Name, &item.Description, &item.Class, &encoded, &item.SourceName)
	if err != nil {
		return Tool{}, err
	}
	_ = json.Unmarshal([]byte(encoded), &item.InputSchema)
	return item, nil
}

func emptyNull(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

func (s *Store) Setting(ctx context.Context, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key,value_json) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json`, key, string(encoded))
	return err
}

func (s *Store) ReadSetting(ctx context.Context, key string, value any) error {
	var encoded string
	if err := s.db.QueryRowContext(ctx, `SELECT value_json FROM settings WHERE key=?`, key).Scan(&encoded); err != nil {
		return err
	}
	return json.Unmarshal([]byte(encoded), value)
}

func (s *Store) PutContext(ctx context.Context, item ContextItem) error {
	arguments, err := json.Marshal(item.Arguments)
	if err != nil {
		return err
	}
	provenance, err := json.Marshal(item.Provenance)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO context_items(handle,url,connection_name,connector_key,operation,resource_type,title,body,arguments_json,provenance_json,fetched_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(handle) DO UPDATE SET url=excluded.url,connection_name=excluded.connection_name,connector_key=excluded.connector_key,operation=excluded.operation,resource_type=excluded.resource_type,title=excluded.title,body=excluded.body,arguments_json=excluded.arguments_json,provenance_json=excluded.provenance_json,fetched_at=excluded.fetched_at,expires_at=excluded.expires_at`,
		item.Handle, item.URL, item.ConnectionName, item.ConnectorKey, item.Operation, item.ResourceType, item.Title, item.Body, string(arguments), string(provenance), item.FetchedAt.UTC().Format(time.RFC3339Nano), item.ExpiresAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) CachedContext(ctx context.Context, rawURL, connectionName string, now time.Time) (ContextItem, error) {
	return scanContext(s.db.QueryRowContext(ctx, contextColumns+` WHERE url=? AND connection_name=? AND expires_at>? ORDER BY fetched_at DESC LIMIT 1`, rawURL, connectionName, now.UTC().Format(time.RFC3339Nano)))
}

func (s *Store) Context(ctx context.Context, handle string) (ContextItem, error) {
	return scanContext(s.db.QueryRowContext(ctx, contextColumns+` WHERE handle=?`, handle))
}

func (s *Store) Contexts(ctx context.Context, limit int) ([]ContextItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, contextColumns+` ORDER BY fetched_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ContextItem
	for rows.Next() {
		item, err := scanContext(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CountContexts(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_items`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func scanContext(row scanner) (ContextItem, error) {
	var item ContextItem
	var arguments, provenance, fetched, expires string
	err := row.Scan(&item.Handle, &item.URL, &item.ConnectionName, &item.ConnectorKey, &item.Operation, &item.ResourceType, &item.Title, &item.Body, &arguments, &provenance, &fetched, &expires)
	if err != nil {
		return ContextItem{}, err
	}
	_ = json.Unmarshal([]byte(arguments), &item.Arguments)
	_ = json.Unmarshal([]byte(provenance), &item.Provenance)
	item.FetchedAt, _ = time.Parse(time.RFC3339Nano, fetched)
	item.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	return item, nil
}

const contextColumns = `SELECT handle,url,connection_name,connector_key,operation,resource_type,title,body,arguments_json,provenance_json,fetched_at,expires_at FROM context_items`

func (s *Store) SearchContexts(ctx context.Context, query, connection string, now time.Time, includeExpired bool, limit int) ([]ContextItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	where := ` WHERE context_items_fts MATCH ?`
	query = plainFTSQuery(query)
	if query == "" {
		return nil, nil
	}
	arguments := []any{query}
	if connection != "" {
		where += ` AND c.connection_name=?`
		arguments = append(arguments, connection)
	}
	if !includeExpired {
		where += ` AND c.expires_at>?`
		arguments = append(arguments, now.UTC().Format(time.RFC3339Nano))
	}
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT c.handle,c.url,c.connection_name,c.connector_key,c.operation,c.resource_type,c.title,c.body,c.arguments_json,c.provenance_json,c.fetched_at,c.expires_at FROM context_items_fts JOIN context_items c ON c.rowid=context_items_fts.rowid`+where+` ORDER BY bm25(context_items_fts),c.fetched_at DESC LIMIT ?`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ContextItem
	for rows.Next() {
		item, err := scanContext(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func plainFTSQuery(value string) string {
	fields := strings.Fields(value)
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.ReplaceAll(field, `"`, `""`)
		quoted = append(quoted, `"`+field+`"`)
	}
	return strings.Join(quoted, " ")
}

func (s *Store) PurgeExpiredContexts(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM context_items WHERE expires_at<=?`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) PurgeAllContexts(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM context_items`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
