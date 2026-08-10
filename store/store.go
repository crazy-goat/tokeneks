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
  cache_write_1h INTEGER NOT NULL DEFAULT 0,
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

CREATE TABLE IF NOT EXISTS model_price (
  provider       TEXT    NOT NULL,
  model          TEXT    NOT NULL,
  name           TEXT,
  input          REAL    NOT NULL DEFAULT 0,
  output         REAL    NOT NULL DEFAULT 0,
  cache_read     REAL    NOT NULL DEFAULT 0,
  cache_write    REAL    NOT NULL DEFAULT 0,
  cache_write_1h REAL    NOT NULL DEFAULT 0,
  source         TEXT    NOT NULL,
  updated_at     INTEGER NOT NULL,
  -- effective_from/effective_to give a row a validity window in ms epoch,
  -- so the same (provider, model) can carry more than one rate over time
  -- (see prices_derive.go's time-segmented derivation). 0 means unbounded
  -- on that side: effective_from=0 is "since forever", effective_to=0 is
  -- "still in effect" -- the same open-ended convention claude.go's
  -- claudePriceWindow already uses for Anthropic's dated price changes.
  -- effective_from is part of the primary key (not effective_to) because
  -- windows for one (provider, model) are non-overlapping by construction,
  -- so its start alone already identifies the row.
  effective_from INTEGER NOT NULL DEFAULT 0,
  effective_to   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (provider, model, effective_from)
);
CREATE INDEX IF NOT EXISTS idx_model_price_model ON model_price(model);
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

	var hasCacheWrite1h int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('message') WHERE name = 'cache_write_1h'`,
	).Scan(&hasCacheWrite1h); err != nil {
		return err
	}
	if hasCacheWrite1h == 0 {
		// Defaults to 0 for every existing row, which is exactly the pre-fix
		// behavior (all cache writes priced at the 5m rate) — a `sync --force`
		// is needed to backfill real values from the source JSONL.
		if _, err := db.Exec(
			`ALTER TABLE message ADD COLUMN cache_write_1h INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return err
		}
	}

	var hasEffectiveFrom int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('model_price') WHERE name = 'effective_from'`,
	).Scan(&hasEffectiveFrom); err != nil {
		return err
	}
	if hasEffectiveFrom == 0 {
		if err := migrateModelPriceEffectiveWindow(db); err != nil {
			return err
		}
	}
	return nil
}

// migrateModelPriceEffectiveWindow adds effective_from/effective_to to
// model_price and extends its primary key to include effective_from. A
// plain ALTER TABLE ADD COLUMN (the pattern used elsewhere in migrate)
// can't do this: sqlite has no "add a column to the primary key", so the
// table is rebuilt under a transaction instead — rename it aside, recreate
// it with the new shape, copy every row across, drop the old one.
//
// Every pre-existing row (models.dev syncs, and any derived row from
// before time-segmented derivation existed) becomes a fully open window
// (effective_from=0, effective_to=0): that is exactly the one rate it
// already applied at every timestamp, so running this migration changes
// no report's numbers by itself.
func migrateModelPriceEffectiveWindow(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`ALTER TABLE model_price RENAME TO model_price_old`,
		`CREATE TABLE model_price (
			provider       TEXT    NOT NULL,
			model          TEXT    NOT NULL,
			name           TEXT,
			input          REAL    NOT NULL DEFAULT 0,
			output         REAL    NOT NULL DEFAULT 0,
			cache_read     REAL    NOT NULL DEFAULT 0,
			cache_write    REAL    NOT NULL DEFAULT 0,
			cache_write_1h REAL    NOT NULL DEFAULT 0,
			source         TEXT    NOT NULL,
			updated_at     INTEGER NOT NULL,
			effective_from INTEGER NOT NULL DEFAULT 0,
			effective_to   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (provider, model, effective_from)
		)`,
		`INSERT INTO model_price
		   (provider, model, name, input, output, cache_read, cache_write, cache_write_1h,
		    source, updated_at, effective_from, effective_to)
		 SELECT provider, model, name, input, output, cache_read, cache_write, cache_write_1h,
		        source, updated_at, 0, 0
		 FROM model_price_old`,
		`DROP TABLE model_price_old`,
		`CREATE INDEX IF NOT EXISTS idx_model_price_model ON model_price(model)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrateModelPriceEffectiveWindow: %w", err)
		}
	}
	return tx.Commit()
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
