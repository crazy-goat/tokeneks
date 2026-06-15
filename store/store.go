// Package store provides a local sqlite-backed store for session data
// ingested from various AI agents (Claude, OpenCode, Pi).
//
// Three tables: session, message, tool_call. All times are milliseconds
// since the Unix epoch.
package store

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS session (
  agent          TEXT    NOT NULL,
  session_id     TEXT    NOT NULL,
  project        TEXT,
  parent_id      TEXT,
  created_at     INTEGER NOT NULL,
  last_activity  INTEGER NOT NULL,
  source_mtime   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (agent, session_id)
);
CREATE INDEX IF NOT EXISTS idx_session_agent_time ON session(agent, last_activity);
CREATE INDEX IF NOT EXISTS idx_session_parent     ON session(agent, parent_id) WHERE parent_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS message (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  agent         TEXT    NOT NULL,
  session_id    TEXT    NOT NULL,
  msg_index     INTEGER NOT NULL,
  role          TEXT    NOT NULL,
  content       TEXT,
  model         TEXT,
  provider      TEXT,
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read    INTEGER NOT NULL DEFAULT 0,
  cache_write   INTEGER NOT NULL DEFAULT 0,
  cost          REAL    NOT NULL DEFAULT 0,
  stop_reason   TEXT,
  thinking      TEXT,
  response      TEXT,
  tool_call_id  TEXT,
  created_at    INTEGER NOT NULL,
  FOREIGN KEY (agent, session_id) REFERENCES session(agent, session_id) ON DELETE CASCADE,
  UNIQUE (agent, session_id, msg_index)
);
CREATE INDEX IF NOT EXISTS idx_message_session ON message(agent, session_id, msg_index);
CREATE INDEX IF NOT EXISTS idx_message_role    ON message(agent, role, created_at);

CREATE TABLE IF NOT EXISTS tool_call (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id   INTEGER NOT NULL,
  call_id      TEXT    NOT NULL,
  name         TEXT    NOT NULL,
  input        TEXT,
  error        INTEGER NOT NULL DEFAULT 0,
  status       TEXT,
  duration_ms  INTEGER,
  FOREIGN KEY (message_id) REFERENCES message(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_tool_message ON tool_call(message_id);
CREATE INDEX IF NOT EXISTS idx_tool_name    ON tool_call(name);
`

// Store wraps a sqlite database with the tokeneks schema.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the store at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store.Open schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store.Open migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate brings older databases up to the current schema. Idempotent.
func migrate(db *sql.DB) error {
	var hasSourceMTime int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('session') WHERE name = 'source_mtime'`,
	).Scan(&hasSourceMTime); err != nil {
		return err
	}
	if hasSourceMTime == 0 {
		if _, err := db.Exec(
			`ALTER TABLE session ADD COLUMN source_mtime INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB returns the underlying *sql.DB for advanced use (e.g. transactions).
// Most callers should use the typed methods on Store instead.
func (s *Store) DB() *sql.DB { return s.db }
