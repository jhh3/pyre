package store

import "github.com/jhh3/pyre/internal/storage/sqlitex"

// Migrations for namespace "messages". Append-only.
var Migrations = []sqlitex.Migration{{Namespace: "messages", Version: 1, Name: "messages", SQL: `
CREATE TABLE messages (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id TEXT NOT NULL UNIQUE,
	recipient_kind TEXT NOT NULL,
	recipient_id TEXT NOT NULL,
	inbox_cursor INTEGER NOT NULL,
	sender_kind TEXT NOT NULL,
	sender_id TEXT NOT NULL,
	thread_id TEXT,
	project_id TEXT,
	in_reply_to TEXT,
	reissues TEXT,
	risk_accepted_by TEXT,
	mode TEXT NOT NULL,
	content_digest TEXT NOT NULL,
	envelope TEXT NOT NULL,
	storage TEXT NOT NULL,
	stored_at TEXT NOT NULL,
	runtime TEXT NOT NULL,
	runtime_reason TEXT,
	bound_run_id TEXT,
	ack TEXT NOT NULL,
	ack_ref TEXT,
	outcome_ref TEXT,
	expires_at TEXT,
	updated_at TEXT NOT NULL,
	UNIQUE (recipient_kind, recipient_id, inbox_cursor)
);
CREATE INDEX messages_sender ON messages(sender_kind, sender_id, seq);
CREATE INDEX messages_thread ON messages(thread_id, seq);
CREATE INDEX messages_runtime ON messages(runtime, expires_at);
CREATE INDEX messages_reply ON messages(in_reply_to);
CREATE TABLE message_inbox_heads (
	recipient_kind TEXT NOT NULL,
	recipient_id TEXT NOT NULL,
	last_cursor INTEGER NOT NULL,
	PRIMARY KEY (recipient_kind, recipient_id)
);
CREATE TABLE message_receipts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id TEXT NOT NULL,
	node_id TEXT NOT NULL,
	origin_seq INTEGER NOT NULL,
	receipt TEXT NOT NULL,
	observed_at TEXT NOT NULL
);
CREATE UNIQUE INDEX message_receipts_origin ON message_receipts(node_id, origin_seq) WHERE origin_seq > 0;
CREATE INDEX message_receipts_msg ON message_receipts(message_id, id);
`}, {Namespace: "messages", Version: 2, Name: "consumed_at", SQL: `
ALTER TABLE messages ADD COLUMN consumed_at TEXT;
`}}
