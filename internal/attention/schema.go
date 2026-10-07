package attention

import "github.com/jhh3/pyre/internal/storage/sqlitex"

// Namespace is the sqlitex migration namespace owned by this package.
const Namespace = "attention"

// Migrations returns every migration this package needs, including the
// shared operations table used for idempotent mutations. Pass them to
// sqlitex.Migrate (NewStore does this itself).
func Migrations() []sqlitex.Migration {
	return []sqlitex.Migration{sqlitex.OperationsMigration, migration1}
}

var migration1 = sqlitex.Migration{Namespace: Namespace, Version: 1, Name: "attention inbox", SQL: `
CREATE TABLE attention_items (
	attention_id   TEXT PRIMARY KEY,
	kind           TEXT NOT NULL,
	severity       TEXT NOT NULL,
	group_key      TEXT NOT NULL,
	object_kind    TEXT NOT NULL,
	object_id      TEXT NOT NULL,
	session_id     TEXT NOT NULL DEFAULT '',
	project_id     TEXT,
	title          TEXT NOT NULL,
	summary        TEXT NOT NULL DEFAULT '',
	state          TEXT NOT NULL,
	count          INTEGER NOT NULL DEFAULT 1,
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	snoozed_until  TEXT,
	deep_link      TEXT NOT NULL,
	notification   TEXT,
	one_shot       INTEGER NOT NULL DEFAULT 0,
	-- transition_seq increments each time the item (re)enters "open"; one
	-- push is attempted per transition.
	transition_seq INTEGER NOT NULL DEFAULT 0,
	last_cursor    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX attention_items_group ON attention_items(group_key, state);
CREATE INDEX attention_items_list ON attention_items(state, updated_at, attention_id);
CREATE INDEX attention_items_session ON attention_items(session_id);
CREATE INDEX attention_items_snooze ON attention_items(state, snoozed_until);

-- Event IDs already applied (dedup of re-sent events); pruned to a bound.
CREATE TABLE attention_seen (
	event_id TEXT PRIMARY KEY,
	cursor   INTEGER NOT NULL,
	seen_at  TEXT NOT NULL
);
CREATE INDEX attention_seen_cursor ON attention_seen(cursor);

-- Persisted event-stream cursor per consumer.
CREATE TABLE attention_cursor (
	consumer   TEXT PRIMARY KEY,
	cursor     INTEGER NOT NULL,
	updated_at TEXT NOT NULL
);

-- Explicit record of event ranges that could not be replayed
-- (reset_required) and how they were reconciled.
CREATE TABLE attention_gaps (
	gap_id      INTEGER PRIMARY KEY AUTOINCREMENT,
	from_cursor INTEGER NOT NULL,
	to_cursor   INTEGER NOT NULL,
	recorded_at TEXT NOT NULL,
	detail      TEXT NOT NULL
);

CREATE TABLE attention_prefs (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	prefs      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

-- Device push registrations. device_token is a provider push token, never a
-- Pyre credential.
CREATE TABLE attention_push (
	provider     TEXT NOT NULL,
	client_id    TEXT NOT NULL,
	device_token TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL,
	PRIMARY KEY (provider, client_id)
);

-- Transactional outbox of notification jobs, written in the same
-- transaction as the item transition that caused them.
CREATE TABLE attention_notify (
	attention_id    TEXT NOT NULL,
	seq             INTEGER NOT NULL,
	action          TEXT NOT NULL, -- "notify" | "dismiss"
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_at TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	done            INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (attention_id, seq, action)
);
CREATE INDEX attention_notify_due ON attention_notify(done, next_attempt_at);
`}
