package hub

import "github.com/jhh3/pyre/internal/storage/sqlitex"

// Migrations for namespace "hub". Append-only: never edit a released step.
var Migrations = []sqlitex.Migration{{Namespace: "hub", Version: 1, Name: "core", SQL: `
CREATE TABLE hub_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE sessions (
	session_id TEXT PRIMARY KEY,
	harness TEXT NOT NULL,
	title TEXT NOT NULL,
	project_id TEXT,
	created_by TEXT NOT NULL,
	created_at TEXT NOT NULL,
	current_run_id TEXT,
	lineage TEXT NOT NULL,
	tags TEXT NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
CREATE INDEX sessions_created ON sessions(created_at, session_id);
CREATE TABLE runs (
	run_id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	node_id TEXT NOT NULL,
	state TEXT NOT NULL,
	run TEXT NOT NULL,
	origin_seq INTEGER NOT NULL DEFAULT 0,
	start_command_id TEXT,
	started_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX runs_session ON runs(session_id, started_at);
CREATE INDEX runs_node ON runs(node_id, state);
CREATE TABLE node_state (
	node_id TEXT PRIMARY KEY,
	hello TEXT,
	status TEXT,
	connected INTEGER NOT NULL DEFAULT 0,
	connected_since TEXT,
	last_seen_at TEXT,
	protocol INTEGER NOT NULL DEFAULT 0,
	committed_report_seq INTEGER NOT NULL DEFAULT 0,
	last_command_seq INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
CREATE TABLE node_reports (
	node_id TEXT NOT NULL,
	origin_seq INTEGER NOT NULL,
	kind TEXT NOT NULL,
	processed_at TEXT NOT NULL,
	PRIMARY KEY (node_id, origin_seq)
);
CREATE TABLE report_rejects (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id TEXT NOT NULL,
	origin_seq INTEGER NOT NULL,
	kind TEXT NOT NULL,
	error TEXT NOT NULL,
	payload TEXT,
	at TEXT NOT NULL
);
CREATE TABLE commands (
	command_id TEXT PRIMARY KEY,
	node_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	kind TEXT NOT NULL,
	actor TEXT NOT NULL,
	issued_at TEXT NOT NULL,
	expires_at TEXT,
	payload TEXT NOT NULL,
	state TEXT NOT NULL,
	ref_kind TEXT,
	ref_id TEXT,
	sent_at TEXT,
	acked_at TEXT,
	completed_at TEXT,
	outcome TEXT,
	result TEXT,
	error TEXT,
	UNIQUE (node_id, seq)
);
CREATE INDEX commands_pending ON commands(node_id, state, seq);
CREATE INDEX commands_ref ON commands(ref_kind, ref_id);
CREATE TABLE leases (
	lease_id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	holder TEXT NOT NULL,
	holder_key TEXT NOT NULL,
	acquired_at TEXT NOT NULL,
	renewed_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	takeover_of TEXT,
	released_at TEXT
);
CREATE INDEX leases_run ON leases(run_id, released_at, expires_at);
CREATE TABLE approvals (
	approval_id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	node_id TEXT,
	state TEXT NOT NULL,
	approval TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX approvals_session ON approvals(session_id, state, created_at);
CREATE INDEX approvals_run ON approvals(run_id, state);
CREATE TABLE accounts (
	account_id TEXT PRIMARY KEY,
	provider TEXT NOT NULL,
	account TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE account_observations (
	account_id TEXT NOT NULL,
	reporting_node TEXT NOT NULL,
	observed_at TEXT NOT NULL,
	observation TEXT NOT NULL,
	PRIMARY KEY (account_id, reporting_node)
);
CREATE TABLE device_grants (
	grant_id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL,
	node_id TEXT NOT NULL,
	generation INTEGER NOT NULL,
	grant_json TEXT NOT NULL,
	observed_at TEXT NOT NULL
);
CREATE INDEX device_grants_account ON device_grants(account_id);
CREATE TABLE account_assignments (
	assignment_id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	node_id TEXT NOT NULL,
	released_at TEXT,
	assignment TEXT NOT NULL
);
CREATE INDEX account_assignments_account ON account_assignments(account_id, released_at);
CREATE TABLE fallback_plans (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	from_run_id TEXT NOT NULL,
	node_id TEXT NOT NULL,
	plan TEXT NOT NULL,
	planned_at TEXT NOT NULL
);
CREATE INDEX fallback_plans_session ON fallback_plans(session_id, id);
CREATE TABLE environments (
	generation_id TEXT PRIMARY KEY,
	node_id TEXT NOT NULL,
	generation TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE node_connections (
	node_id TEXT PRIMARY KEY,
	connections TEXT NOT NULL,
	reported_at TEXT NOT NULL
);
CREATE TABLE session_activity (
	session_id TEXT PRIMARY KEY,
	run_id TEXT,
	line TEXT NOT NULL,
	at TEXT NOT NULL
);
`}, {Namespace: "hub", Version: 2, Name: "account_leases", SQL: `
CREATE TABLE account_leases (
	lease_id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL,
	node_id TEXT NOT NULL,
	holder TEXT NOT NULL,
	acquired_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	released_at TEXT
);
CREATE INDEX account_leases_account ON account_leases(account_id, released_at);
`}}
